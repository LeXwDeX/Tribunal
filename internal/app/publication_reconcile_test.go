package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// A retryable publication error that was just recorded must wait for the
// backoff before the periodic scan touches the status note again (#510).
func TestReconcileSideEffectsKeepsRecentPendingPublication(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	workflow := reviewWithFailedNote(t, f, "job-publication-backoff")
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || before.State != reviewStatusFailed {
		t.Fatalf("failed publication status = %+v, %v", before, err)
	}
	f.gitlab.noteErr = errors.New("publication retried before the backoff")

	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	if got := workflowPublication(t, f, workflow.ID); got.PublicationState != store.WorkflowPublicationPending ||
		got.PublicationError != publicationRetryReason {
		t.Fatalf("publication = %q/%q, want pending retry", got.PublicationState, got.PublicationError)
	}
	after, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || after.State != reviewStatusFailed || after.Attempt != before.Attempt {
		t.Fatalf("status = %+v, %v; want the failed row untouched", after, err)
	}
}

// An unknown discussion outcome has its own bounded terminal-age path; the
// periodic publication scan must not flip its status or retry it (#510).
func TestReconcileSideEffectsSkipsUnknownPublicationOutcome(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	workflow := reviewWithFailedNote(t, f, "job-publication-unknown")
	if err := f.db.SetWorkflowPublicationState(ctx, workflow.ID, store.WorkflowPublicationPending, publicationUnknownReason); err != nil {
		t.Fatal(err)
	}
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	f.service.now = func() time.Time { return time.Now().UTC().Add(publicationRetryInterval + time.Minute) }
	f.gitlab.noteErr = errors.New("unknown outcome retried by the publication scan")

	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	if got := workflowPublication(t, f, workflow.ID); got.PublicationState != store.WorkflowPublicationPending ||
		got.PublicationError != publicationUnknownReason {
		t.Fatalf("publication = %q/%q, want the unknown outcome kept", got.PublicationState, got.PublicationError)
	}
	after, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || after.State != reviewStatusFailed || after.Attempt != before.Attempt {
		t.Fatalf("status = %+v, %v; want the failed row untouched", after, err)
	}
}

// A failed execution whose failure note is still pending is retried by the
// same periodic scan after the backoff (#510).
func TestReconcileSideEffectsRetriesPendingFailurePublication(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	f.gitlab.noteErr = errors.New("gitlab: create note failed")
	job := f.seedReview(t, "job-failure-publication-reconcile")
	if err := f.service.RunReview(ctx, job); err == nil {
		t.Fatal("RunReview unexpectedly published")
	}
	f.gitlab.noteErr = nil
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != string(dag.WorkflowFailed) || workflow.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("workflow = %s/%s, want failed with pending publication", workflow.Status, workflow.PublicationState)
	}
	f.service.now = func() time.Time { return time.Now().UTC().Add(publicationRetryInterval + time.Minute) }

	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	if got := workflowPublication(t, f, workflow.ID); got.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("publication = %q/%q, want published", got.PublicationState, got.PublicationError)
	}
}
