package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func publishedFinding(id, severity, decision string) store.Finding {
	return store.Finding{
		ID:         id,
		OriginNode: "synthesize",
		Severity:   severity,
		Category:   "logic",
		Title:      "finding " + id,
		Body:       "body " + id,
		Decision:   decision,
	}
}

func mustPublication(t *testing.T, s *Store, projectID, mrIID int64, workflowID, key string) {
	t.Helper()
	if _, err := s.UpsertPublication(context.Background(), store.Publication{
		ProjectID:   projectID,
		MRIID:       mrIID,
		Kind:        "summary",
		PublishKey:  key,
		WorkflowID:  workflowID,
		Fingerprint: "fp-" + key,
	}); err != nil {
		t.Fatalf("UpsertPublication: %v", err)
	}
}

// HasPublishedSevereFinding looks only at the most recent summary and only at
// findings that were actually published or marked uncertain.
func TestHasPublishedSevereFindingUsesLatestPublishedReview(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
	})
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	wfA, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(first.JobID, first.Generation, 42, 7, headA))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	finishWorkflow(t, s, wfA, 1, clock.Now())
	mustPublication(t, s, 42, 7, wfA, "summary")
	if err := s.ReplaceFindings(ctx, wfA, []store.Finding{publishedFinding("f-a", "P1", "publish")}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	severe, err := s.HasPublishedSevereFinding(ctx, 42, 7)
	if err != nil || !severe {
		t.Fatalf("severe = %v, %v, want true", severe, err)
	}

	// Dropped findings were refuted or dismissed; they do not force a full
	// review. Marked findings are unresolved and do.
	if err := s.ReplaceFindings(ctx, wfA, []store.Finding{publishedFinding("f-a", "P0", "drop")}); err != nil {
		t.Fatalf("ReplaceFindings drop: %v", err)
	}
	if severe, err = s.HasPublishedSevereFinding(ctx, 42, 7); err != nil || severe {
		t.Fatalf("dropped finding severe = %v, %v, want false", severe, err)
	}
	if err := s.ReplaceFindings(ctx, wfA, []store.Finding{publishedFinding("f-a", "P0", "mark")}); err != nil {
		t.Fatalf("ReplaceFindings mark: %v", err)
	}
	if severe, err = s.HasPublishedSevereFinding(ctx, 42, 7); err != nil || !severe {
		t.Fatalf("marked finding severe = %v, %v, want true", severe, err)
	}

	clock.Advance(time.Second)
	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
	})
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	wfB, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(second.JobID, second.Generation, 42, 7, headB))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	finishWorkflow(t, s, wfB, 1, clock.Now())
	mustPublication(t, s, 42, 7, wfB, "summary")
	if err := s.ReplaceFindings(ctx, wfB, []store.Finding{publishedFinding("f-b", "P2", "publish")}); err != nil {
		t.Fatalf("ReplaceFindings second: %v", err)
	}
	// The older workflow still holds P0, but the latest published review is
	// clean apart from a P2, so incremental review stays allowed.
	if severe, err = s.HasPublishedSevereFinding(ctx, 42, 7); err != nil || severe {
		t.Fatalf("latest review severe = %v, %v, want false", severe, err)
	}
}

func TestHasPublishedSevereFindingTreatsFailedSummaryAsUnknown(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"),
	})
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	wfA, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(first.JobID, first.Generation, 42, 7, reviewHead("a")))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	finishWorkflow(t, s, wfA, 1, clock.Now())
	mustPublication(t, s, 42, 7, wfA, "summary-a")
	if err := s.ReplaceFindings(ctx, wfA, []store.Finding{publishedFinding("f-a", "P1", "publish")}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}

	clock.Advance(time.Second)
	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("b"),
	})
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	wfB, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(second.JobID, second.Generation, 42, 7, reviewHead("b")))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	rev := mustAppendEvent(t, s, wfB, 1, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, clock.Now()))
	rev = mustAppendEvent(t, s, wfB, rev, queuedEvent(t, "a", 1, clock.Now()))
	rev = mustAppendEvent(t, s, wfB, rev, startedEvent(t, "a", 1, "p", "m", clock.Now()))
	rev = mustAppendEvent(t, s, wfB, rev, testEvent(t, dag.EventNodeFailed, dag.NodeFailedPayload{
		NodeID: "a", Attempt: 1, Error: "agent failed",
	}, clock.Now()))
	rev = mustAppendEvent(t, s, wfB, rev, queuedEvent(t, "b", 1, clock.Now()))
	rev = mustAppendEvent(t, s, wfB, rev, startedEvent(t, "b", 1, "p", "m", clock.Now()))
	rev = mustAppendEvent(t, s, wfB, rev, testEvent(t, dag.EventNodeFailed, dag.NodeFailedPayload{
		NodeID: "b", Attempt: 1, Error: "agent failed",
	}, clock.Now()))
	if _, err := s.Append(ctx, wfB, rev, testEvent(t, dag.EventWorkflowFailed, dag.WorkflowTerminalPayload{Reason: "agent failed"}, clock.Now())); err != nil {
		t.Fatalf("fail second workflow: %v", err)
	}
	mustPublication(t, s, 42, 7, wfB, "summary-b")

	severe, err := s.HasPublishedSevereFinding(ctx, 42, 7)
	if err != nil || !severe {
		t.Fatalf("failed summary history = %v, %v, want true (unknown)", severe, err)
	}
}

// A completed workflow can publish an empty summary when its verdict is
// INCOMPLETE. That summary is not evidence that older P0/P1 findings were
// rechecked, so incremental history must stay unsafe.
func TestHasPublishedSevereFindingTreatsIncompleteSummaryAsUnknown(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"),
	})
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	wfA, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(first.JobID, first.Generation, 42, 7, reviewHead("a")))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	finishWorkflow(t, s, wfA, 1, clock.Now())
	if err := s.SetWorkflowVerdict(ctx, wfA, "BLOCKING"); err != nil {
		t.Fatalf("set first verdict: %v", err)
	}
	mustPublication(t, s, 42, 7, wfA, "summary-a")
	if err := s.ReplaceFindings(ctx, wfA, []store.Finding{publishedFinding("f-a", "P0", "publish")}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}

	clock.Advance(time.Second)
	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("b"),
	})
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	wfB, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(second.JobID, second.Generation, 42, 7, reviewHead("b")))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	finishWorkflow(t, s, wfB, 1, clock.Now())
	if err := s.SetWorkflowVerdict(ctx, wfB, "INCOMPLETE"); err != nil {
		t.Fatalf("set second verdict: %v", err)
	}
	mustPublication(t, s, 42, 7, wfB, "summary-b")

	severe, err := s.HasPublishedSevereFinding(ctx, 42, 7)
	if err != nil || !severe {
		t.Fatalf("incomplete summary history = %v, %v, want true (unknown)", severe, err)
	}

	// A clean completed PASS review still allows incremental narrowing.
	if err := s.SetWorkflowVerdict(ctx, wfB, "PASS"); err != nil {
		t.Fatalf("set second verdict PASS: %v", err)
	}
	if severe, err = s.HasPublishedSevereFinding(ctx, 42, 7); err != nil || severe {
		t.Fatalf("clean PASS summary history = %v, %v, want false", severe, err)
	}
}

func TestHasPublishedSevereFindingWithoutPublication(t *testing.T) {
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	severe, err := s.HasPublishedSevereFinding(context.Background(), 42, 7)
	if err != nil || severe {
		t.Fatalf("severe = %v, %v, want false", severe, err)
	}
}
