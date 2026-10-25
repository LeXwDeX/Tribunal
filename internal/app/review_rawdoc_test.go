package app

import (
	"context"
	"errors"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func (f *reviewFixture) setRawError(path string, err error) {
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	if err == nil {
		delete(f.gitlab.rawErr, path)
		return
	}
	f.gitlab.rawErr[path] = err
}

func transientRawError() error {
	return &gitlab.Error{Op: "get_raw_file", Status: 500}
}

// A transient repository-document failure during collection is retryable and
// must not run or publish a review with an empty document snapshot.
func TestRunReviewPropagatesTransientRepoDocError(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.setRawError(transientRawErrorPath, transientRawError())
	job := f.seedRunFor(t, "job-raw-doc-collect", reviewTestHead, "mr_open")

	err := f.service.Handle(ctx, job)
	if err == nil {
		t.Fatal("transient repository document error was swallowed")
	}
	if IsPermanentJob(err) {
		t.Fatalf("transient error = %v, want a retryable error", err)
	}
	if _, err := f.db.GetWorkflowByJob(ctx, job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a workflow was created from a failed snapshot: %v", err)
	}
	if f.gitlab.noteCount() != 0 {
		t.Fatalf("published %d notes from a failed snapshot", f.gitlab.noteCount())
	}

	f.setRawError(transientRawErrorPath, nil)
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	if _, err := f.db.GetWorkflowByJob(ctx, job.ID); err != nil {
		t.Fatalf("retry did not create a workflow: %v", err)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("summary notes = %d, want 1", f.gitlab.noteCount())
	}
}

// Recovery must not interpret a transient document failure as a changed
// repository configuration and cancel the workflow.
func TestRecoverKeepsWorkflowOnTransientRepoDocError(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedRunFor(t, "job-raw-doc-recover", reviewTestHead, "mr_open")
	workflowID := createPendingReview(t, f, job.ID, jobGeneration(t, job))
	f.setRawError("AGENTS.md", transientRawError())

	err := f.service.Recover(ctx)
	if err == nil {
		t.Fatal("recovery swallowed a transient repository document error")
	}
	workflow, err := f.db.GetWorkflow(ctx, workflowID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if workflow.Status == "cancelled" {
		t.Fatal("recovery cancelled a workflow on a transient document error")
	}
	if workflow.Status != "pending" {
		t.Fatalf("workflow status = %q, want pending", workflow.Status)
	}

	f.setRawError("AGENTS.md", nil)
	if err := f.service.Recover(ctx); err != nil {
		t.Fatalf("recovery after network recovery: %v", err)
	}
	workflow, err = f.db.GetWorkflow(ctx, workflowID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if workflow.Status != "completed" {
		t.Fatalf("workflow status = %q, want completed", workflow.Status)
	}
}

// Publication must leave the workflow pending instead of marking it stale when
// the target-branch configuration read is transiently unavailable.
func TestPublishPendingRetriesTransientRepoDocError(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedRunFor(t, "job-raw-doc-publish", reviewTestHead, "mr_open")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if err := f.db.SetWorkflowPublicationState(ctx, workflow.ID, store.WorkflowPublicationPending, ""); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	f.setRawError(transientRawErrorPath, transientRawError())

	err = f.service.Recover(ctx)
	if err == nil {
		t.Fatal("publication swallowed a transient repository document error")
	}
	record, dossierErr := f.db.GetWorkflowDossier(ctx, workflow.ID)
	if dossierErr != nil {
		t.Fatalf("GetWorkflowDossier: %v", dossierErr)
	}
	if record.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("publication state = %q, want pending", record.PublicationState)
	}

	f.setRawError(transientRawErrorPath, nil)
	if err := f.service.Recover(ctx); err != nil {
		t.Fatalf("publication retry: %v", err)
	}
	record, err = f.db.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier: %v", err)
	}
	if record.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("publication state = %q, want published", record.PublicationState)
	}
}

// A missing document is still an empty document, not an error.
func TestRawDocTreatsOnlyNotFoundAsEmpty(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	content, err := f.service.rawDoc(context.Background(), "token", 42, "AGENTS.md", "main")
	if err != nil || content != "" {
		t.Fatalf("missing document = %q, %v; want empty and nil", content, err)
	}
	f.setRawError("AGENTS.md", transientRawError())
	if _, err := f.service.rawDoc(context.Background(), "token", 42, "AGENTS.md", "main"); err == nil {
		t.Fatal("transient error was treated as a missing document")
	}
}

const transientRawErrorPath = ".gitlab-review.yml"
