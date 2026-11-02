package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestIssue161DeleteOnboardingRetryExactAndAtomic(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	keys := []string{"workspace.onboard:42:retry", "workspace.onboard:4:retry", "workspace.onboard:420:retry", "workspace.onboard:42:retry:extra"}
	for _, key := range keys {
		if _, _, err := s.Enqueue(ctx, store.Job{Kind: "workspace.onboard", Key: key, Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER issue161_fail_delete BEFORE DELETE ON projects BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, 42); err == nil {
		t.Fatal("delete should fail at project row")
	}
	if _, err := s.GetProject(ctx, 42); err != nil {
		t.Fatalf("rollback lost project: %v", err)
	}
	for _, key := range append(keys, "workspace.onboard:42") {
		if n := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = ?`, key); n != 1 {
			t.Errorf("rollback lost %s: count=%d", key, n)
		}
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER issue161_fail_delete`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		want := 1
		if i == 0 {
			want = 0
		}
		if n := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = ?`, key); n != want {
			t.Errorf("after delete %s: count=%d, want %d", key, n, want)
		}
	}
}

func TestIssue161ReviewAttemptFencesAcrossGenerations(t *testing.T) {
	for _, oldState := range []string{"complete", "running", "queued"} {
		t.Run(oldState, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, newFakeClock())
			mustProject(t, s, 42)
			headA, headB := reviewHead("a"), reviewHead("b")
			first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: headA})
			if err != nil {
				t.Fatal(err)
			}
			var old *store.Job
			for attempt := 1; attempt <= 2; attempt++ {
				old, err = s.Claim(ctx, "old", []string{reviewRunKind}, time.Hour)
				if err != nil || old == nil || old.Attempts != attempt {
					t.Fatalf("claim old: %+v %v", old, err)
				}
				goal := store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: first.Generation, Attempt: attempt, HeadSHA: headA, State: store.ReviewStatusRunning}
				if written, err := s.SetReviewStatus(ctx, goal); err != nil || !written {
					t.Fatalf("old status: %v %v", written, err)
				}
				if attempt == 1 || oldState == "queued" {
					if err := s.Fail(ctx, *old, errors.New("retry"), nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			goal := store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: first.Generation, Attempt: 1, HeadSHA: headA, State: store.ReviewStatusPass}
			before := mustReviewStatus(t, s, 42, 7)
			if written, err := s.SetReviewStatus(ctx, goal); err != nil || written {
				t.Fatalf("lower attempt write: %v %v", written, err)
			}
			if status := mustReviewStatus(t, s, 42, 7); status.Version != before.Version || status.State != store.ReviewStatusRunning {
				t.Fatalf("lower attempt changed status: %+v", status)
			}
			goal.Attempt, goal.State = 0, store.ReviewStatusRunning
			if written, err := s.SetReviewStatus(ctx, goal); err != nil || !written {
				t.Fatalf("same generation zero attempt: %v %v", written, err)
			}
			if status := mustReviewStatus(t, s, 42, 7); status.Attempt != 2 {
				t.Fatalf("same generation lost attempt: %+v", status)
			}
			if oldState == "complete" {
				goal := store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: first.Generation, Attempt: 2, HeadSHA: headA, State: store.ReviewStatusPass}
				if written, err := s.SetReviewStatus(ctx, goal); err != nil || !written {
					t.Fatalf("old pass: %v %v", written, err)
				}
				if err := s.Complete(ctx, *old); err != nil {
					t.Fatal(err)
				}
			}
			second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: headB})
			if err != nil || second.Generation != first.Generation+1 {
				t.Fatalf("successor: %+v %v", second, err)
			}
			queued := mustReviewStatus(t, s, 42, 7)
			if queued.Attempt != 0 || queued.State != store.ReviewStatusQueued {
				t.Fatalf("successor queued: %+v", queued)
			}
			stale := store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: first.Generation, Attempt: 2, HeadSHA: headA, State: store.ReviewStatusPass}
			if written, err := s.SetReviewStatus(ctx, stale); written || !errors.Is(err, store.ErrSuperseded) {
				t.Fatalf("stale: %v %v", written, err)
			}
			if oldState == "running" {
				if blocked, err := s.Claim(ctx, "new", []string{reviewRunKind}, time.Hour); err != nil || blocked != nil {
					t.Fatalf("unfenced claim: %+v %v", blocked, err)
				}
				if err := s.Fail(ctx, *old, errors.New("superseded"), nil); err != nil {
					t.Fatal(err)
				}
				if job, err := s.GetJob(ctx, old.ID); err != nil || job.State != store.JobCancelled {
					t.Fatalf("old cancellation: %+v %v", job, err)
				}
			}
			job, err := s.Claim(ctx, "new", []string{reviewRunKind}, time.Hour)
			wantAttempt := 1
			if oldState == "queued" {
				wantAttempt = 3
			}
			if err != nil || job == nil || job.ID != second.JobID || job.Attempts != wantAttempt {
				t.Fatalf("new claim: %+v %v", job, err)
			}
			goal = store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: second.Generation, Attempt: job.Attempts, HeadSHA: headB, State: store.ReviewStatusRunning}
			if written, err := s.SetReviewStatus(ctx, goal); err != nil || !written {
				t.Fatalf("new running: %v %v", written, err)
			}
			goal.State = store.ReviewStatusPass
			if written, err := s.SetReviewStatus(ctx, goal); err != nil || !written {
				t.Fatalf("new pass: %v %v", written, err)
			}
			goal.State = store.ReviewStatusFailed
			if written, err := s.SetReviewStatus(ctx, goal); err != nil || written {
				t.Fatalf("terminal fence: %v %v", written, err)
			}
			if job.Attempts > 1 {
				goal.Attempt--
				if written, err := s.SetReviewStatus(ctx, goal); err != nil || written {
					t.Fatalf("attempt fence: %v %v", written, err)
				}
			}
			if status := mustReviewStatus(t, s, 42, 7); status.State != store.ReviewStatusPass || status.Attempt != job.Attempts {
				t.Fatalf("final: %+v", status)
			}
		})
	}
}

func TestIssue161CancellationGenerationAttempt(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	goal := store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: first.Generation, Attempt: 3, HeadSHA: head, State: store.ReviewStatusRunning}
	if written, err := s.SetReviewStatus(ctx, goal); err != nil || !written {
		t.Fatalf("running: %v %v", written, err)
	}
	cancel, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, Cancel: true})
	if err != nil {
		t.Fatal(err)
	}
	if status := mustReviewStatus(t, s, 42, 7); status.Generation != cancel.Generation || status.Attempt != 0 || status.HeadSHA != head {
		t.Fatalf("cancel queued: %+v", status)
	}
	goal.Generation, goal.Attempt, goal.HeadSHA, goal.State = cancel.Generation, 0, "", store.ReviewStatusCancelled
	if written, err := s.SetReviewStatus(ctx, goal); err != nil || !written {
		t.Fatalf("cancel: %v %v", written, err)
	}
	if status := mustReviewStatus(t, s, 42, 7); status.State != store.ReviewStatusCancelled || status.Attempt != 0 || status.HeadSHA != head {
		t.Fatalf("cancel status: %+v", status)
	}
}
