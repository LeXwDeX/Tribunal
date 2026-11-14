package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestStatusConvergesAcrossGenerations(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	enqueue := func() int64 {
		t.Helper()
		r, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head, Payload: json.RawMessage(`{"trigger":"mr_update"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return r.Generation
	}
	first := enqueue()
	if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: first, HeadSHA: f.head, State: reviewStatusPass}); err != nil || !ok {
		t.Fatalf("pass: %v %v", ok, err)
	}
	drainReviewJobs(t, f, "review.status")
	second := enqueue()
	drainReviewJobs(t, f, "review.status")
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := reviewStatusBody(reviewStatusUpdate{State: reviewStatusQueued, HeadSHA: f.head, Generation: second, EstimateMinutes: 5})
	if got := f.gitlab.notes[0].Body; got != want || row.AppliedVersion != row.Version || row.BodyHash != sha256Hex(got) {
		t.Fatalf("remote and receipt do not match queued goal: body=%q row=%+v", got, row)
	}
}

func TestStatusGoalAttemptOrderingThroughJob(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	r, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head})
	if err != nil {
		t.Fatal(err)
	}
	goal := store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: r.Generation, Attempt: 1, HeadSHA: f.head, State: reviewStatusRunning}
	if ok, err := f.db.SetReviewStatus(ctx, goal); err != nil || !ok {
		t.Fatalf("running: %v %v", ok, err)
	}
	drainReviewJobs(t, f, "review.status")
	goal.State = reviewStatusQueued
	if ok, err := f.db.SetReviewStatus(ctx, goal); err != nil || ok {
		t.Fatalf("same attempt queued: %v %v", ok, err)
	}
	goal.Attempt = 2
	if ok, err := f.db.SetReviewStatus(ctx, goal); err != nil || !ok {
		t.Fatalf("higher attempt queued: %v %v", ok, err)
	}
	drainReviewJobs(t, f, "review.status")
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if body := f.gitlab.notes[0].Body; parseStatusPhase(body) != reviewStatusPhase(reviewStatusQueued) || row.BodyHash != sha256Hex(body) {
		t.Fatalf("retry not visible: %q %+v", body, row)
	}
}

type lateStatusGitLab struct {
	*reviewGitLab
	entered chan struct{}
	release chan struct{}
	done    chan struct{}
}

type blockedLiveStatusGitLab struct {
	*reviewGitLab
	entered chan struct{}
	release chan struct{}
}

func (g *blockedLiveStatusGitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	if parseStatusPhase(body) == reviewStatusPhase(reviewStatusRunning) {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.reviewGitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
}

func TestStatusWaiterReadsGoalAfterPreviousWriterReleases(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	r, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head})
	if err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")
	g := &blockedLiveStatusGitLab{reviewGitLab: f.gitlab, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	})
	f.service.status = newStatusReporter(g, nil)
	goal := store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: r.Generation, Attempt: 1, HeadSHA: f.head, State: reviewStatusRunning}
	if ok, err := f.db.SetReviewStatus(ctx, goal); err != nil || !ok {
		t.Fatalf("running: %v %v", ok, err)
	}
	old, err := f.db.Claim(ctx, "old", []string{"review.status"}, time.Nanosecond)
	if err != nil || old == nil {
		t.Fatalf("old claim: %v %v", old, err)
	}
	oldDone := make(chan error, 1)
	go func() { oldDone <- f.service.RunStatusJob(ctx, old) }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old writer did not start")
	}
	newer, err := f.db.Claim(ctx, "new", []string{"review.status"}, time.Minute)
	if err != nil || newer == nil {
		t.Fatalf("new claim: %v %v", newer, err)
	}
	waiting := &waitContext{Context: ctx, waiting: make(chan struct{})}
	newDone := make(chan error, 1)
	go func() { newDone <- f.service.RunStatusJob(waiting, newer) }()
	select {
	case <-waiting.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("new writer did not wait")
	}
	goal.State = reviewStatusPass
	if ok, err := f.db.SetReviewStatus(ctx, goal); err != nil || !ok {
		t.Fatalf("pass: %v %v", ok, err)
	}
	close(g.release)
	for _, done := range []<-chan error{oldDone, newDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("writer did not finish")
		}
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := f.gitlab.ListMRNotes(ctx, "token", 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	body := notes[0].Body
	if parseStatusPhase(body) != reviewStatusPhase(reviewStatusPass) || row.AppliedVersion != row.Version || row.BodyHash != sha256Hex(body) {
		t.Fatalf("waiter used stale goal: %q %+v", body, row)
	}
}

func (g *lateStatusGitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	if parseStatusPhase(body) != reviewStatusPhase(reviewStatusRunning) {
		return g.reviewGitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
	}
	go func() {
		<-g.release
		_ = g.reviewGitLab.UpdateMRNote(context.Background(), token, projectID, iid, noteID, body)
		close(g.done)
	}()
	close(g.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestStatusLateCompletionAfterCancellationRepairs(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	r, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head})
	if err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")
	g := &lateStatusGitLab{reviewGitLab: f.gitlab, entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	f.service.status = newStatusReporter(g, nil)
	goal := store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: r.Generation, Attempt: 1, HeadSHA: f.head, State: reviewStatusRunning}
	if ok, err := f.db.SetReviewStatus(ctx, goal); err != nil || !ok {
		t.Fatalf("running: %v %v", ok, err)
	}
	old, err := f.db.Claim(ctx, "old", []string{"review.status"}, time.Nanosecond)
	if err != nil || old == nil {
		t.Fatalf("claim: %v %v", old, err)
	}
	oldCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- f.service.RunStatusJob(oldCtx, old) }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old request did not start")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled job: %v", err)
	}
	goal.State = reviewStatusPass
	if ok, err := f.db.SetReviewStatus(ctx, goal); err != nil || !ok {
		t.Fatalf("pass: %v %v", ok, err)
	}
	drainReviewJobs(t, f, "review.status")
	close(g.release)
	<-g.done
	// A new service has no in-memory locks or scan cursor from the old process.
	// The row is successfully applied, so the repair comes from the
	// low-frequency self-heal scan after its interval elapsed (#459).
	oldService := f.service
	f.service = &ReviewService{store: f.db, workspace: f.manager, cipher: oldService.cipher,
		gitlab: f.gitlab, status: testStatusReporter(f.gitlab), logger: oldService.logger,
		now: func() time.Time { return time.Now().Add(reviewStatusSelfHealInterval + time.Minute) }}
	if err := f.service.ReconcileAppliedStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := g.ListMRNotes(ctx, "token", 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if body := notes[0].Body; parseStatusPhase(body) != reviewStatusPhase(reviewStatusPass) || row.BodyHash != sha256Hex(body) {
		t.Fatalf("late request not repaired: %q %+v", body, row)
	}
	if f.runner.count() != 0 {
		t.Fatal("status recovery ran the model")
	}
}

func TestStatusVerificationScansBeyondFirstHundred(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	for iid := int64(1); iid <= 101; iid++ {
		_, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: iid, HeadSHA: f.head})
		if err != nil {
			t.Fatal(err)
		}
		row, err := f.db.GetReviewStatus(ctx, 42, iid)
		if err != nil {
			t.Fatal(err)
		}
		// The self-heal scan only revisits terminal rows (#459); this test is
		// about paging beyond the first hundred, so make every row terminal.
		if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
			ProjectID: 42, MRIID: iid, Generation: row.Generation,
			Attempt: row.Attempt, HeadSHA: f.head, State: store.ReviewStatusPass,
		}); err != nil || !ok {
			t.Fatalf("set pass: %v %v", ok, err)
		}
		row, err = f.db.GetReviewStatus(ctx, 42, iid)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.MarkReviewStatusApplied(ctx, 42, iid, int64(row.Version), iid, "hash", ""); err != nil {
			t.Fatal(err)
		}
	}
	for {
		job, err := f.db.Claim(ctx, "drain", []string{"review.status"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		if err := f.db.Complete(ctx, *job); err != nil {
			t.Fatal(err)
		}
	}
	f.service.now = func() time.Time { return time.Now().Add(reviewStatusSelfHealInterval + time.Minute) }
	if err := f.service.ReconcileAppliedStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for {
		job, err := f.db.Claim(ctx, "verify", []string{"review.status"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		var p reviewSideEffectPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			t.Fatal(err)
		}
		seen[p.MRIID] = true
		if err := f.db.Complete(ctx, *job); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 101 || !seen[101] {
		t.Fatalf("verification starved rows: got %d, last=%v", len(seen), seen[101])
	}
}

func TestStatusVerificationShortPageRepeatsNextCycle(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	_, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head})
	if err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 2; cycle++ {
		if err := f.service.enqueueStatusVerification(ctx, 42); err != nil {
			t.Fatal(err)
		}
		job, err := f.db.Claim(ctx, "verification", []string{"review.status"}, time.Minute)
		if err != nil || job == nil {
			t.Fatalf("cycle %d missing job: %v %v", cycle, job, err)
		}
		if err := f.db.Complete(ctx, *job); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusVerificationFullPageWrapsWithoutEmptyCycle(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	for iid := int64(1); iid <= 100; iid++ {
		if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: iid, HeadSHA: f.head}); err != nil {
			t.Fatal(err)
		}
	}
	for cycle := 0; cycle < 2; cycle++ {
		if err := f.service.enqueueStatusVerification(ctx, 42); err != nil {
			t.Fatal(err)
		}
		count := 0
		for {
			job, err := f.db.Claim(ctx, "verification", []string{"review.status"}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if job == nil {
				break
			}
			count++
			if err := f.db.Complete(ctx, *job); err != nil {
				t.Fatal(err)
			}
		}
		if count != 100 {
			t.Fatalf("cycle %d jobs = %d, want 100", cycle, count)
		}
	}
}

type failingStatusEnqueueStore struct {
	ReviewStore
	fail bool
}

func (s *failingStatusEnqueueStore) Enqueue(ctx context.Context, job store.Job) (string, bool, error) {
	if s.fail && job.Key == "review.status:42:1" {
		return "", false, errors.New("test enqueue failure")
	}
	return s.ReviewStore.Enqueue(ctx, job)
}

type rejectGlobalStatusListStore struct{ ReviewStore }

func (s *rejectGlobalStatusListStore) ListPendingReviewStatuses(context.Context) ([]store.ReviewStatus, error) {
	return nil, errors.New("global status enumeration must not be used")
}

func TestReconciliationUsesScopedStatusScan(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head}); err != nil {
		t.Fatal(err)
	}
	f.service.store = &rejectGlobalStatusListStore{ReviewStore: f.db}
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStatusVerificationPagesAndEnqueueFailure(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	for iid := int64(1); iid <= 150; iid++ {
		if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: iid, HeadSHA: f.head}); err != nil {
			t.Fatal(err)
		}
	}
	drain := func() map[int64]bool {
		t.Helper()
		seen := map[int64]bool{}
		for {
			job, err := f.db.Claim(ctx, "scan", []string{"review.status"}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if job == nil {
				return seen
			}
			var p reviewSideEffectPayload
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				t.Fatal(err)
			}
			seen[p.MRIID] = true
			if err := f.db.Complete(ctx, *job); err != nil {
				t.Fatal(err)
			}
		}
	}
	drain()
	failing := &failingStatusEnqueueStore{ReviewStore: f.db, fail: true}
	f.service.store = failing
	if err := f.service.enqueueStatusVerification(ctx, 42); err == nil {
		t.Fatal("expected enqueue failure")
	}
	first := drain()
	if len(first) != 99 || first[1] || !first[100] || first[101] {
		t.Fatalf("first page = %v", first)
	}
	if err := f.service.enqueueStatusVerification(ctx, 42); err != nil {
		t.Fatal(err)
	}
	second := drain()
	if len(second) != 50 || !second[101] || !second[150] || second[1] {
		t.Fatalf("second page = %v", second)
	}
	failing.fail = false
	if err := f.service.enqueueStatusVerification(ctx, 42); err != nil {
		t.Fatal(err)
	}
	third := drain()
	if len(third) != 100 || !third[1] || !third[100] || third[101] {
		t.Fatalf("retry page = %v", third)
	}
}

func TestReconciliationPagesEachProject(t *testing.T) {
	for _, kind := range []string{"pending reaction", "stale eyes", "unfinished", "queued cancellation"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			seedProject(t, f.db, f.service.cipher, 43, "whsec", "{}", "default")
			for _, projectID := range []int64{42, 43} {
				for iid := int64(1); iid <= 101; iid++ {
					r, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: projectID, MRIID: iid, HeadSHA: f.head, Payload: json.RawMessage(`{"trigger":"command","note_id":555}`)})
					if err != nil {
						t.Fatal(err)
					}
					if kind == "unfinished" {
						if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{ProjectID: projectID, MRIID: iid, Generation: r.Generation, Attempt: 1, HeadSHA: f.head, State: reviewStatusRunning}); err != nil || !ok {
							t.Fatalf("running: %v %v", ok, err)
						}
					}
					if kind == "queued cancellation" {
						if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: projectID, MRIID: iid, Cancel: true}); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			for _, jobKind := range []string{"review.run", "review.cancel", "review.reaction"} {
				for {
					job, err := f.db.Claim(ctx, "crashed", []string{jobKind}, time.Hour)
					if err != nil {
						t.Fatal(err)
					}
					if job == nil {
						break
					}
					var p reviewSideEffectPayload
					if err := json.Unmarshal(job.Payload, &p); err != nil {
						t.Fatal(err)
					}
					if jobKind == "review.reaction" || ((p.ProjectID == 42 || p.ProjectID == 43) && p.MRIID == 101) {
						if err := f.db.Complete(ctx, *job); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if kind != "pending reaction" {
				f.service.now = func() time.Time { return time.Now().Add(3 * time.Hour) }
			}
			for cycle := 0; cycle < 2; cycle++ {
				if err := f.service.ReconcileSideEffects(ctx); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "unfinished", "queued cancellation":
				want := reviewStatusFailed
				if kind == "queued cancellation" {
					want = reviewStatusCancelled
				}
				for _, projectID := range []int64{42, 43} {
					row, err := f.db.GetReviewStatus(ctx, projectID, 101)
					if err != nil {
						t.Fatal(err)
					}
					if row.State != want {
						t.Fatalf("project %d later page status = %s, want %s", projectID, row.State, want)
					}
				}
			case "stale eyes":
				for _, projectID := range []int64{42, 43} {
					row, err := f.db.GetReviewReaction(ctx, projectID, 101, 555)
					if err != nil || row.Desired != reviewAwardNone {
						t.Fatalf("project %d later page reaction = %+v %v", projectID, row, err)
					}
				}
			case "pending reaction":
				seen := map[int64]bool{}
				for {
					job, err := f.db.Claim(ctx, "reconciled", []string{"review.reaction"}, time.Minute)
					if err != nil {
						t.Fatal(err)
					}
					if job == nil {
						break
					}
					var p reviewSideEffectPayload
					if err := json.Unmarshal(job.Payload, &p); err != nil {
						t.Fatal(err)
					}
					if p.MRIID == 101 {
						seen[p.ProjectID] = true
					}
				}
				if !seen[42] || !seen[43] {
					t.Fatalf("later page pending reactions missing: %v", seen)
				}
			}
		})
	}
}

func TestStatusWriterLockWaitHonorsCancellation(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	_, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head})
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.db.Claim(ctx, "status", []string{"review.status"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim: %v %v", job, err)
	}
	unlock, err := f.service.statusLocks.lock(ctx, "42:7")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waiting := &waitContext{Context: waitCtx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- f.service.RunStatusJob(waiting, job) }()
	select {
	case <-waiting.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("status job did not reach a lock wait")
	}
	f.service.statusLocks.mu.Lock()
	entry := f.service.statusLocks.entries["42:7"]
	refs := 0
	if entry != nil {
		refs = entry.refs
	}
	f.service.statusLocks.mu.Unlock()
	if refs != 2 {
		t.Fatalf("status lock references = %d, want holder and waiter", refs)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled status job did not exit while lock was held")
	}
	f.gitlab.mu.Lock()
	writes, notes := len(f.gitlab.statusUpdates), len(f.gitlab.notes)
	f.gitlab.mu.Unlock()
	if writes != 0 || notes != 0 {
		t.Fatalf("cancelled waiter wrote status: writes=%d notes=%d", writes, notes)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || row.AppliedVersion >= row.Version {
		t.Fatalf("cancelled waiter settled status: %+v, %v", row, err)
	}
}

func TestStatusVerificationSettlesTerminalError(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	_, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head})
	if err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")
	staleBody := reviewStatusMarkerLine + "\nstale"
	f.gitlab.notes[0].Body = staleBody
	pending, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: pending.Generation,
		Attempt: pending.Attempt + 1, HeadSHA: f.head, State: reviewStatusRunning,
	}); err != nil || !ok {
		t.Fatalf("pending status: %v %v", ok, err)
	}
	f.gitlab.statusNoteErr = &gitlab.Error{Op: "update_note", Status: 403}
	drainReviewJobs(t, f, "review.status")
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastError == "" || row.AppliedVersion != row.Version {
		t.Fatalf("terminal failure not settled: %+v", row)
	}
	rows, err := f.db.ListReviewStatusesForVerification(ctx, 42, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("terminal failure still eligible: %+v %v", rows, err)
	}
	// Restoring permission alone does not retry the settled version; a new
	// generation is one way to publish a new goal.
	f.gitlab.statusNoteErr = nil
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")
	if f.gitlab.notes[0].Body != staleBody {
		t.Fatal("terminal version retried automatically")
	}
	if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head}); err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")
	if got := parseStatusPhase(f.gitlab.notes[0].Body); got != reviewStatusPhase(reviewStatusQueued) {
		t.Fatalf("new generation did not recover: %s", got)
	}
}

func TestTerminalStatusErrorCanBeSupersededByWorkflowRecovery(t *testing.T) {
	for _, phase := range []string{reviewStatusRunning, reviewStatusPublishing} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.service.status = testStatusReporter(f.gitlab)
			job := f.seedReview(t, "completed-workflow")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			claimed, err := f.db.Claim(ctx, "completed", []string{"review.run"}, time.Minute)
			if err != nil || claimed == nil {
				t.Fatalf("claim: %v %v", claimed, err)
			}
			if err := f.db.Complete(ctx, *claimed); err != nil {
				t.Fatal(err)
			}
			row, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: row.Generation, Attempt: row.Attempt + 1, HeadSHA: f.head, State: phase}); err != nil || !ok {
				t.Fatalf("set %s: %v %v", phase, ok, err)
			}
			drainReviewJobs(t, f, "review.status")
			pending, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
				ProjectID: 42, MRIID: 7, Generation: pending.Generation,
				Attempt: pending.Attempt, HeadSHA: f.head, State: phase,
			}); err != nil || !ok {
				t.Fatalf("pending status: %v %v", ok, err)
			}
			for i := range f.gitlab.notes {
				if strings.HasPrefix(f.gitlab.notes[i].Body, reviewStatusMarkerPrefix) {
					f.gitlab.notes[i].Body = reviewStatusMarkerLine + "\nstale"
				}
			}
			f.gitlab.statusNoteErr = &gitlab.Error{Op: "update_note", Status: 403}
			drainReviewJobs(t, f, "review.status")
			failed, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || failed.LastError == "" || failed.State != phase {
				t.Fatalf("terminal error: %+v %v", failed, err)
			}
			f.service.now = func() time.Time { return time.Now().Add(3 * time.Hour) }
			if err := f.service.ReconcileSideEffects(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || recovered.State != reviewStatusPass || recovered.Version <= failed.Version || recovered.LastError != "" || recovered.AppliedVersion >= recovered.Version {
				t.Fatalf("completed workflow did not replace failed version: %+v %v", recovered, err)
			}
		})
	}
}

func TestStatusReconcilesLateRemoteOverwriteAfterApplied(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	r, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head, Payload: json.RawMessage(`{"trigger":"mr_update"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: r.Generation, HeadSHA: f.head, State: reviewStatusPass}); err != nil || !ok {
		t.Fatalf("pass: %v %v", ok, err)
	}
	drainReviewJobs(t, f, "review.status")
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	// An old request completes remotely after the new receipt was committed.
	f.gitlab.notes[0].Body = reviewStatusBody(reviewStatusUpdate{State: reviewStatusRunning, HeadSHA: f.head, Generation: r.Generation})
	f.service.now = func() time.Time { return time.Now().Add(reviewStatusSelfHealInterval + time.Minute) }
	if err := f.service.ReconcileAppliedStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")
	after, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if body := f.gitlab.notes[0].Body; parseStatusPhase(body) != reviewStatusPhase(reviewStatusPass) || after.BodyHash != sha256Hex(body) || after.AppliedVersion != after.Version || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("late overwrite not repaired: body=%q row=%+v", body, after)
	}
}
