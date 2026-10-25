package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// A queued review job keeps its ID when a newer push replaces its payload. The
// old job -> workflow mapping must be detached in the same transaction so the
// current generation can create its own workflow instead of being rejected as
// superseded.
func TestQueuedReviewReplacementDetachesStaleJobMapping(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
		Payload: []byte(`{"trigger":"mr_open"}`),
	})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	wfA, reused, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(first.JobID, first.Generation, 42, 7, headA))
	if err != nil || reused {
		t.Fatalf("create old workflow = %q reused=%v err=%v", wfA, reused, err)
	}

	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
		Payload: []byte(`{"trigger":"mr_update"}`),
	})
	if err != nil {
		t.Fatalf("replacement enqueue: %v", err)
	}
	if second.JobID != first.JobID {
		t.Fatalf("queued replacement job = %q, want retained %q", second.JobID, first.JobID)
	}
	if second.Generation != first.Generation+1 {
		t.Fatalf("replacement generation = %d, want %d", second.Generation, first.Generation+1)
	}

	mapped, err := s.GetWorkflowByJob(ctx, first.JobID)
	if err != nil || mapped.ID != wfA {
		t.Fatalf("stale mapping should remain until the current generation creates: %+v, %v", mapped, err)
	}
	// A previous review that already finished keeps its slot free for the
	// current generation.
	finishWorkflow(t, s, wfA, 1, time.Now())

	wfB, reused, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(second.JobID, second.Generation, 42, 7, headB))
	if err != nil {
		t.Fatalf("create current workflow: %v", err)
	}
	if reused || wfB == wfA {
		t.Fatalf("current workflow = %q reused=%v, want a new workflow", wfB, reused)
	}
	mapped, err = s.GetWorkflowByJob(ctx, second.JobID)
	if err != nil || mapped.ID != wfB || mapped.HeadSHA != headB {
		t.Fatalf("current mapping = %+v, %v", mapped, err)
	}
	oldDossier, err := s.GetWorkflowDossier(ctx, wfA)
	if err != nil {
		t.Fatalf("old dossier: %v", err)
	}
	if oldDossier.JobID != "" || oldDossier.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("old dossier = %+v, want detached and skipped", oldDossier)
	}
	if _, err := s.GetWorkflow(ctx, wfA); err != nil {
		t.Fatalf("old workflow history was deleted: %v", err)
	}

	// A same-generation retry still reuses the workflow.
	again, reused, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(second.JobID, second.Generation, 42, 7, headB))
	if err != nil || !reused || again != wfB {
		t.Fatalf("same-generation retry = %q reused=%v err=%v", again, reused, err)
	}
}

// A running review keeps its own job mapping when a queued successor is
// replaced. Only the mapping of the replaced queued job is detached.
func TestQueuedReplacementPreservesRunningSuccessorMapping(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")
	headC := reviewHead("c")

	runningJob, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
	})
	if err != nil {
		t.Fatalf("enqueue running: %v", err)
	}
	claimed, err := s.Claim(ctx, "worker-1", []string{reviewRunKind}, time.Hour)
	if err != nil || claimed == nil || claimed.ID != runningJob.JobID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}

	queuedJob, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
	})
	if err != nil {
		t.Fatalf("enqueue successor: %v", err)
	}
	if queuedJob.JobID == runningJob.JobID {
		t.Fatal("successor reused the running job id")
	}
	queuedWF, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(queuedJob.JobID, queuedJob.Generation, 42, 7, headB))
	if err != nil {
		t.Fatalf("create successor workflow: %v", err)
	}

	third, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headC,
	})
	if err != nil {
		t.Fatalf("replace successor: %v", err)
	}
	if third.JobID != queuedJob.JobID {
		t.Fatalf("replacement job = %q, want %q", third.JobID, queuedJob.JobID)
	}

	running, err := s.GetJob(ctx, runningJob.JobID)
	if err != nil || running.State != store.JobRunning || running.Attempts != claimed.Attempts {
		t.Fatalf("running job changed: %+v, %v", running, err)
	}
	if _, err := s.GetWorkflowByJob(ctx, runningJob.JobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("running job gained a mapping it never had: %v", err)
	}
	if _, err := s.GetWorkflowByJob(ctx, queuedJob.JobID); err != nil {
		t.Fatalf("replacement enqueue should not detach by itself: %v", err)
	}
	finishWorkflow(t, s, queuedWF, 1, time.Now())
	currentWF, reused, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(third.JobID, third.Generation, 42, 7, headC))
	if err != nil || reused || currentWF == queuedWF {
		t.Fatalf("current workflow = %q reused=%v err=%v", currentWF, reused, err)
	}
	remapped, err := s.GetWorkflowByJob(ctx, queuedJob.JobID)
	if err != nil || remapped.ID != currentWF {
		t.Fatalf("current mapping = %+v, %v", remapped, err)
	}
	if _, err := s.GetWorkflow(ctx, queuedWF); err != nil {
		t.Fatalf("replaced successor history was deleted: %v", err)
	}
}

// A new generation can carry the same head (for example a retried review).
// The stale mapping is still detached so the current generation gets a fresh
// workflow while the older one remains for audit.
func TestSameHeadNewGenerationCreatesFreshWorkflow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
	})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	wfA, reused, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(first.JobID, first.Generation, 42, 7, head))
	if err != nil || reused {
		t.Fatalf("old workflow = %q reused=%v err=%v", wfA, reused, err)
	}
	finishWorkflow(t, s, wfA, 1, time.Now())

	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
	})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if second.JobID != first.JobID || second.Generation != first.Generation+1 {
		t.Fatalf("second = %+v, want the same job with a new generation", second)
	}
	wfB, reused, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(second.JobID, second.Generation, 42, 7, head))
	if err != nil {
		t.Fatalf("create fresh workflow: %v", err)
	}
	if reused || wfB == wfA {
		t.Fatalf("workflow = %q reused=%v, want a fresh workflow", wfB, reused)
	}
	if _, err := s.GetWorkflow(ctx, wfA); err != nil {
		t.Fatalf("old workflow history was deleted: %v", err)
	}
}
