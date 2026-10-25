package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Startup recovery and a lease-reclaimed job can drive the same workflow.
// Recovery must take application ownership before preparing the worktree and
// registering its executor; the losing handler must not unregister or delete
// the winner's resources.
func TestRecoverAndRunReviewShareExecutorOwnership(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedRunFor(t, "job-ownership", reviewTestHead, "mr_open")
	workflowID := createPendingReview(t, f, job.ID, jobGeneration(t, job))
	runDir := filepath.Join(f.manager.Get(42).Root, "runs", job.ID)

	block := make(chan struct{})
	f.runner.block = block
	f.runner.started = make(chan struct{})
	recoverDone := make(chan error, 1)
	go func() { recoverDone <- f.service.Recover(ctx) }()
	select {
	case <-f.runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not start the review")
	}

	err := f.service.RunReview(ctx, job)
	if err == nil {
		t.Fatal("a competing handler reported success while recovery owned the workflow")
	}
	if _, lookupErr := f.executors.lookup(workflowID); lookupErr != nil {
		t.Fatalf("losing handler removed the owner's executor: %v", lookupErr)
	}
	if info, statErr := os.Stat(runDir); statErr != nil || !info.IsDir() {
		t.Fatalf("losing handler removed the owner's worktree %q: %v", runDir, statErr)
	}

	close(block)
	if err := <-recoverDone; err != nil {
		t.Fatalf("Recover: %v", err)
	}
	workflow, err := f.db.GetWorkflow(ctx, workflowID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if workflow.Status != "completed" {
		t.Fatalf("workflow status = %q, want completed", workflow.Status)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("summary notes = %d, want 1", f.gitlab.noteCount())
	}
}

// Two handlers for the same leased job must not both prepare the worktree or
// publish. The second is rejected as a retryable ownership conflict.
func TestConcurrentRunReviewSameJobHasOneOwner(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedRunFor(t, "job-concurrent", reviewTestHead, "mr_open")

	block := make(chan struct{})
	f.runner.block = block
	f.runner.started = make(chan struct{})
	firstDone := make(chan error, 1)
	go func() { firstDone <- f.service.Handle(ctx, job) }()
	select {
	case <-f.runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first handler did not start the review")
	}

	second := f.service.Handle(ctx, job)
	if second == nil {
		t.Fatal("second handler reported success for an owned workflow")
	}
	close(block)
	if err := <-firstDone; err != nil {
		t.Fatalf("first handler: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != "completed" {
		t.Fatalf("workflow status = %q, want completed", workflow.Status)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("summary notes = %d, want 1", f.gitlab.noteCount())
	}
}
