package app

import (
	"context"
	"strings"
	"testing"
)

// A queued job that already created a workflow before its publish failed is
// later replaced by a newer push. The current head must get its own review
// instead of inheriting the old workflow and being treated as superseded.
func TestReplacedQueuedReviewCreatesCurrentWorkflow(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)

	oldJob := f.seedRunFor(t, "job-replaced-queued", reviewTestHead, "mr_open")
	oldWorkflow := createPendingReview(t, f, oldJob.ID, jobGeneration(t, oldJob))

	newHead := strings.Repeat("c", 40)
	setIncrementalHead(f, newHead, "other.go")
	currentJob := f.seedRunFor(t, "job-replaced-queued", newHead, "mr_update")
	if err := f.service.Handle(ctx, currentJob); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	workflow, err := f.db.GetWorkflowByJob(ctx, currentJob.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.ID == oldWorkflow {
		t.Fatalf("job still maps to the old workflow %s", oldWorkflow)
	}
	if !strings.EqualFold(workflow.HeadSHA, newHead) {
		t.Fatalf("workflow head = %q, want %q", workflow.HeadSHA, newHead)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("summary notes = %d, want exactly one review for the new head", f.gitlab.noteCount())
	}
	old, err := f.db.GetWorkflow(ctx, oldWorkflow)
	if err != nil {
		t.Fatalf("old workflow history was deleted: %v", err)
	}
	if old.ID != oldWorkflow {
		t.Fatalf("old workflow id = %q, want %q", old.ID, oldWorkflow)
	}
}
