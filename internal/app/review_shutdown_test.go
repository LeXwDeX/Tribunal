package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// interruptReview starts a review job, cancels its context while the first
// agent call is running (a worker shutdown), and returns the workflow ID.
func interruptReview(t *testing.T, f *reviewFixture, job *jobs.Job) string {
	t.Helper()
	f.runner.setBlock(make(chan struct{}), make(chan struct{}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.service.Handle(ctx, job) }()
	select {
	case <-f.runner.startedSignal():
	case <-time.After(5 * time.Second):
		t.Fatal("review did not start")
	}
	cancel()
	select {
	case err := <-done:
		// A nil error would let the worker complete the job.
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted Handle error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handle did not stop after shutdown")
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	snapshot, err := f.engine.Snapshot(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Terminal() {
		t.Fatalf("shutdown left workflow %s (%s), want it unfinished", snapshot.Status, snapshot.CancelReason)
	}
	if got := f.gitlab.noteCount(); got != 0 {
		t.Fatalf("interrupted review published %d notes", got)
	}
	// The restarted process runs without the blocking agent.
	f.runner.setBlock(nil, nil)
	return workflow.ID
}

func assertReviewPublished(t *testing.T, f *reviewFixture, wfID string) {
	t.Helper()
	snapshot, err := f.engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Status != dag.WorkflowCompleted {
		t.Fatalf("resumed workflow status = %s (%s), want completed", snapshot.Status, snapshot.CancelReason)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), wfID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier: %v", err)
	}
	if record.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("publication state = %q, want published", record.PublicationState)
	}
	if f.gitlab.noteCount() == 0 {
		t.Fatal("resumed review did not publish a note")
	}
}

func TestReviewShutdownResumesOnJobRetry(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-shutdown-retry")
	wfID := interruptReview(t, f, job)

	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("retried Handle: %v", err)
	}
	assertReviewPublished(t, f, wfID)
}

func TestReviewShutdownResumesOnStartupRecovery(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-shutdown-recover")
	wfID := interruptReview(t, f, job)

	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertReviewPublished(t, f, wfID)
}
