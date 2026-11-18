package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// publishedReview runs one review to a successful publication.
func publishedReview(t *testing.T, f *reviewFixture, jobID string) *store.Workflow {
	t.Helper()
	ctx := context.Background()
	job := f.seedReview(t, jobID)
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowCompleted) || workflow.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("workflow = %s/%s, want completed and published", workflow.Status, workflow.PublicationState)
	}
	return workflow
}

// stalePublicationViewStore reports the publication state a periodic scan read
// before it took the publication lock, so the scan can be tested against a
// publication that completed in between (#584).
type stalePublicationViewStore struct {
	ReviewStore
	workflowID string
}

func (s stalePublicationViewStore) GetWorkflow(ctx context.Context, id string) (*store.Workflow, error) {
	wf, err := s.ReviewStore.GetWorkflow(ctx, id)
	if err != nil || wf == nil || id != s.workflowID {
		return wf, err
	}
	stale := *wf
	stale.PublicationState = store.WorkflowPublicationPending
	return &stale, nil
}

// A scan that listed a workflow while it was pending must not publish it again
// after the job published it, roll its status back to publishing, or move the
// published row back to pending when the duplicate attempt fails (#584).
func TestPublishPendingSkipsPublicationCompletedAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	workflow := publishedReview(t, f, "job-584-published-scan")
	record, err := f.db.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier: %v", err)
	}
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	item := store.PendingPublication{
		WorkflowID: workflow.ID, ProjectID: 42, MRIID: 7, HeadSHA: workflow.HeadSHA,
		Generation: record.Generation, Status: string(dag.WorkflowCompleted),
	}
	f.service.store = stalePublicationViewStore{ReviewStore: f.db, workflowID: workflow.ID}
	f.gitlab.mrErr = &gitlab.Error{Op: "get_merge_request", Status: 503}

	advanced, err := f.service.publishPendingItem(ctx, item)
	if err != nil {
		t.Fatalf("publishPendingItem: %v", err)
	}
	if !advanced {
		t.Fatal("publishPendingItem reported no progress for a publishable snapshot")
	}
	got := workflowPublication(t, f, workflow.ID)
	if got.PublicationState != store.WorkflowPublicationPublished || got.PublicationError != "" {
		t.Fatalf("publication = %q/%q, want the published result kept", got.PublicationState, got.PublicationError)
	}
	after, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if after.State != before.State || after.Attempt != before.Attempt || after.Detail != before.Detail {
		t.Fatalf("status = %+v, want %+v unchanged", after, before)
	}
	if effects := countPublicationEffects(f); effects.summaries != 1 {
		t.Fatalf("summary notes = %d, want the single published summary", effects.summaries)
	}
}

// A retryable publication failure must never overwrite a published outcome;
// the duplicate attempt that raced the successful one cannot make the review
// publishable again (#584).
func TestPublicationFailureDoesNotDowngradePublishedState(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	workflow := publishedReview(t, f, "job-584-published-state")

	if err := f.publisher.recordPublicationFailure(ctx, workflow.ID, errors.New("transient upstream failure")); err != nil {
		t.Fatalf("recordPublicationFailure: %v", err)
	}
	got := workflowPublication(t, f, workflow.ID)
	if got.PublicationState != store.WorkflowPublicationPublished || got.PublicationError != "" {
		t.Fatalf("publication = %q/%q, want the published result kept", got.PublicationState, got.PublicationError)
	}
}

// A recovery cancel for an invalid repository configuration must continue the
// attempt sequence; an existing terminal row would otherwise swallow the
// settlement and leave the status disagreeing with the MR note (#584).
func TestRecoveryInvalidRepoConfigAdvancesPastTerminalAttempt(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := f.seedReview(t, "job-584-invalid-config-attempt")
	id := interruptReview(t, f, job)
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	exhausted := row.Attempt + 3
	written, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: row.Generation,
		Attempt: exhausted, HeadSHA: row.HeadSHA, Profile: row.Profile,
		State: reviewStatusFailed, Detail: reviewExecutionFailedReason,
	})
	if err != nil || !written {
		t.Fatalf("seed exhausted status = %v, %v", written, err)
	}
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [\n"

	if err := f.service.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	drainReviewSideEffects(t, f)

	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if status.State != reviewStatusFailed || status.Detail != invalidRepoConfigDetail || status.Attempt != exhausted+1 {
		t.Fatalf("status = %+v, want failed attempt %d with the invalid-config detail", status, exhausted+1)
	}
	workflow := workflowPublication(t, f, id)
	if workflow.PublicationState != store.WorkflowPublicationSkipped || workflow.PublicationError != invalidRepoConfigDetail {
		t.Fatalf("publication = %q/%q, want skipped invalid config", workflow.PublicationState, workflow.PublicationError)
	}
	var note string
	f.gitlab.mu.Lock()
	for _, candidate := range f.gitlab.notes {
		if strings.Contains(candidate.Body, invalidRepoConfigNote) {
			note = candidate.Body
		}
	}
	f.gitlab.mu.Unlock()
	if note == "" {
		t.Fatalf("notes = %d, want the invalid-config explanation", len(f.gitlab.notes))
	}
}
