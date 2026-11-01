package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func commandReplay(projectID, mrIID, noteID int64, command, head string) store.ReviewCommand {
	return store.ReviewCommand{
		ProjectID: projectID, MRIID: mrIID, NoteID: noteID, Command: command,
		Cancel: command == "cancel", HeadSHA: head,
		Payload: json.RawMessage(fmt.Sprintf(`{"trigger":"command","note_id":%d}`, noteID)),
	}
}

func TestEnqueueReviewLatestDeduplicatesCommandReplay(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	headA := reviewHead("a")
	headB := reviewHead("b")

	run, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "run", headA))
	if err != nil || run.Generation != 1 || run.Duplicate {
		t.Fatalf("run = %+v, %v", run, err)
	}
	cancel, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 556, "cancel", ""))
	if err != nil || cancel.Generation != 2 || cancel.Duplicate {
		t.Fatalf("cancel = %+v, %v", cancel, err)
	}
	controlBefore, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	statusBefore := mustReviewStatus(t, s, 42, 7)
	reactionBefore := mustReviewReaction(t, s, 42, 7, 555)
	jobBefore, err := s.GetJob(ctx, run.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// A replayed command keeps the original cancellation and generation even
	// when the worker observed a different head before enqueueing again.
	replayed, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "run", headB))
	if err != nil {
		t.Fatalf("replayed run: %v", err)
	}
	if !replayed.Duplicate || replayed.JobID != run.JobID || replayed.Generation != 0 {
		t.Fatalf("replayed run = %+v", replayed)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.Generation != controlBefore.Generation || !control.Cancelled || control.HeadSHA != controlBefore.HeadSHA {
		t.Fatalf("control after replay = %+v, want %+v", control, controlBefore)
	}
	jobAfter, err := s.GetJob(ctx, run.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if string(jobAfter.Payload) != string(jobBefore.Payload) || jobAfter.State != jobBefore.State ||
		!jobAfter.UpdatedAt.Equal(jobBefore.UpdatedAt) {
		t.Fatalf("replay changed queued job: before=%+v after=%+v", jobBefore, jobAfter)
	}
	if status := mustReviewStatus(t, s, 42, 7); status.Version != statusBefore.Version ||
		status.Generation != statusBefore.Generation || status.State != statusBefore.State {
		t.Fatalf("replay changed status: before=%+v after=%+v", statusBefore, status)
	}
	if reaction := mustReviewReaction(t, s, 42, 7, 555); reaction.Version != reactionBefore.Version {
		t.Fatalf("replay changed reaction: before=%+v after=%+v", reactionBefore, reaction)
	}

	replayedCancel, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 556, "cancel", ""))
	if err != nil || !replayedCancel.Duplicate || replayedCancel.JobID != cancel.JobID {
		t.Fatalf("replayed cancel = %+v, %v", replayedCancel, err)
	}

	// A different command, note, MR, or project is a new identity.
	distinct := []struct {
		name string
		cmd  store.ReviewCommand
		want int64
	}{
		{"same note other command", commandReplay(42, 7, 556, "run", headA), 3},
		{"other note same command", commandReplay(42, 7, 557, "run", headA), 4},
		{"other MR", commandReplay(42, 8, 555, "run", headA), 1},
		{"other project", commandReplay(43, 7, 555, "run", headA), 1},
	}
	for _, tt := range distinct {
		result, err := s.EnqueueReviewLatest(ctx, tt.cmd)
		if err != nil || result.Duplicate || result.Generation != tt.want {
			t.Fatalf("%s = %+v, %v; want generation %d", tt.name, result, err, tt.want)
		}
	}
}

func TestEnqueueReviewLatestCommandReplayRollsBackIdentity(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	// Fail after the identity row, control bump and side effects are written:
	// the whole transaction must roll back, including the replay identity.
	bad := commandReplay(42, 7, 555, "run", head)
	bad.Payload = json.RawMessage(`{"trigger":"command","generation":9}`)
	if _, err := s.EnqueueReviewLatest(ctx, bad); err == nil {
		t.Fatal("reserved generation was accepted")
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM review_command_replays`,
		`SELECT COUNT(*) FROM review_controls`,
		`SELECT COUNT(*) FROM review_status`,
		`SELECT COUNT(*) FROM review_reactions`,
		`SELECT COUNT(*) FROM jobs WHERE kind IN ('review.run', 'review.cancel')`,
	} {
		if got := rowCount(t, s, query); got != 0 {
			t.Fatalf("%s = %d, want 0", query, got)
		}
	}

	// The same identity is still available after a failed attempt.
	retry, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "run", head))
	if err != nil || retry.Duplicate || retry.Generation != 1 {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
}

func TestEnqueueReviewLatestCommandReplayConcurrent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	const workers = 8
	results := make(chan store.ReviewEnqueueResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "run", head))
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent enqueue: %v", err)
		}
	}
	accepted, duplicated := 0, 0
	for result := range results {
		if result.Duplicate {
			duplicated++
		} else {
			accepted++
			if result.Generation != 1 {
				t.Fatalf("accepted generation = %d", result.Generation)
			}
		}
	}
	if accepted != 1 || duplicated != workers-1 {
		t.Fatalf("accepted=%d duplicated=%d", accepted, duplicated)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 1 || control.Cancelled {
		t.Fatalf("control = %+v, %v", control, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = 'review:42:7'`); got != 1 {
		t.Fatalf("review jobs = %d, want 1", got)
	}
}

func TestDeleteProjectRemovesCommandReplayIdentities(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	head := reviewHead("a")
	for _, projectID := range []int64{42, 43} {
		if _, err := s.EnqueueReviewLatest(ctx, commandReplay(projectID, 7, 555, "run", head)); err != nil {
			t.Fatalf("enqueue project %d: %v", projectID, err)
		}
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays WHERE project_id = 42`); got != 0 {
		t.Fatalf("deleted project identities = %d", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays WHERE project_id = 43`); got != 1 {
		t.Fatalf("other project identities = %d", got)
	}
	mustProject(t, s, 42)
	// A re-registered project must not reuse a deleted project's identity.
	result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "run", head))
	if err != nil || result.Duplicate || result.Generation != 1 {
		t.Fatalf("re-registered enqueue = %+v, %v", result, err)
	}
}
