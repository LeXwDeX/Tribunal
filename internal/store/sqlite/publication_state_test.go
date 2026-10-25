package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func createPublicationWorkflow(t *testing.T, s *Store, projectID int64) string {
	t.Helper()
	ctx := context.Background()
	head := reviewHead("a")
	queued, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: projectID, MRIID: 7, HeadSHA: head,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	wfID, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(queued.JobID, 1, projectID, 7, head))
	if err != nil {
		t.Fatalf("CreateReviewWorkflow: %v", err)
	}
	return wfID
}

func TestWorkflowPublicationProjectionAndFilter(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	wfID := createPublicationWorkflow(t, s, 42)

	wf, err := s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.PublicationState != store.WorkflowPublicationPending || wf.PublicationError != "" {
		t.Fatalf("fresh publication = %q/%q, want pending with no error", wf.PublicationState, wf.PublicationError)
	}

	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationFailed, "publication is not possible"); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	wf, err = s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow after failure: %v", err)
	}
	if wf.PublicationState != store.WorkflowPublicationFailed || wf.PublicationError != "publication is not possible" {
		t.Fatalf("failed publication = %q/%q", wf.PublicationState, wf.PublicationError)
	}

	failed, _, err := s.ListWorkflows(ctx, store.WorkflowFilter{Kind: "review", PublicationState: store.WorkflowPublicationFailed}, "", 10)
	if err != nil {
		t.Fatalf("ListWorkflows failed: %v", err)
	}
	if len(failed) != 1 || failed[0].ID != wfID {
		t.Fatalf("failed filter = %+v, want %s", failed, wfID)
	}
	pending, _, err := s.ListWorkflows(ctx, store.WorkflowFilter{Kind: "review", PublicationState: store.WorkflowPublicationPending}, "", 10)
	if err != nil {
		t.Fatalf("ListWorkflows pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending filter returned %d rows", len(pending))
	}
	if _, err := s.db.Exec(`UPDATE workflows SET status = 'completed' WHERE id = ?`, wfID); err != nil {
		t.Fatalf("mark workflow completed: %v", err)
	}
	recover, err := s.ListPendingPublications(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingPublications: %v", err)
	}
	if len(recover) != 0 {
		t.Fatalf("non-pending failure was offered for automatic retry: %+v", recover)
	}
	if _, err := s.db.Exec(`UPDATE workflows SET status = 'failed' WHERE id = ?`, wfID); err != nil {
		t.Fatalf("mark workflow failed: %v", err)
	}
	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationPending, "retry pending"); err != nil {
		t.Fatalf("SetWorkflowPublicationState pending: %v", err)
	}
	recover, err = s.ListPendingPublications(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingPublications failed pending: %v", err)
	}
	if len(recover) != 1 || recover[0].WorkflowID != wfID ||
		recover[0].Status != string(dag.WorkflowFailed) {
		t.Fatalf("failed pending recovery = %+v, want failed workflow %s", recover, wfID)
	}
	if _, err := s.db.Exec(`UPDATE workflows SET status = 'completed' WHERE id = ?`, wfID); err != nil {
		t.Fatalf("mark workflow completed: %v", err)
	}
	recover, err = s.ListPendingPublications(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingPublications completed pending: %v", err)
	}
	if len(recover) != 1 || recover[0].WorkflowID != wfID ||
		recover[0].Status != string(dag.WorkflowCompleted) {
		t.Fatalf("completed pending recovery = %+v, want completed workflow %s", recover, wfID)
	}

	// A successful publication clears the reason.
	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationPublished, ""); err != nil {
		t.Fatalf("SetWorkflowPublicationState published: %v", err)
	}
	wf, err = s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow after publish: %v", err)
	}
	if wf.PublicationState != store.WorkflowPublicationPublished || wf.PublicationError != "" {
		t.Fatalf("published publication = %q/%q", wf.PublicationState, wf.PublicationError)
	}
}

func TestWorkflowPublicationErrorIsBoundedAndValidated(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	wfID := createPublicationWorkflow(t, s, 42)

	if err := s.SetWorkflowPublicationState(ctx, wfID, "bogus", "reason"); err == nil {
		t.Fatal("invalid publication state was accepted")
	}
	if err := s.SetWorkflowPublicationState(ctx, "missing", store.WorkflowPublicationFailed, "reason"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing workflow error = %v, want ErrNotFound", err)
	}

	long := strings.Repeat("x", 800)
	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationSkipped, "  "+long+"  "); err != nil {
		t.Fatalf("SetWorkflowPublicationState skipped: %v", err)
	}
	wf, err := s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("state = %q, want skipped", wf.PublicationState)
	}
	if len([]rune(wf.PublicationError)) != maxPublicationErrorRunes {
		t.Fatalf("publication error length = %d, want %d", len([]rune(wf.PublicationError)), maxPublicationErrorRunes)
	}

	// Published and pending rows never keep a stale reason.
	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationPending, "retry pending"); err != nil {
		t.Fatalf("SetWorkflowPublicationState pending: %v", err)
	}
	wf, err = s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.PublicationError != "retry pending" {
		t.Fatalf("pending reason = %q, want retry pending", wf.PublicationError)
	}
	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationPublished, "leftover"); err != nil {
		t.Fatalf("SetWorkflowPublicationState published: %v", err)
	}
	wf, err = s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.PublicationError != "" {
		t.Fatalf("published reason = %q, want empty", wf.PublicationError)
	}
}

func TestWorkflowPublicationErrorSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "review.db")
	s, err := Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustProject(t, s, 42)
	wfID := createPublicationWorkflow(t, s, 42)
	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationFailed, "publication is not possible"); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	wf, err := reopened.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow after reopen: %v", err)
	}
	if wf.PublicationState != store.WorkflowPublicationFailed ||
		wf.PublicationError != "publication is not possible" {
		t.Fatalf("reopened publication = %q/%q", wf.PublicationState, wf.PublicationError)
	}
}

func TestSetWorkflowVerdictAcceptsIncomplete(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	wfID := createPublicationWorkflow(t, s, 42)

	if err := s.SetWorkflowVerdict(ctx, wfID, "INCOMPLETE"); err != nil {
		t.Fatalf("SetWorkflowVerdict INCOMPLETE: %v", err)
	}
	wf, err := s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.Verdict != "INCOMPLETE" {
		t.Fatalf("verdict = %q, want INCOMPLETE", wf.Verdict)
	}
	if err := s.SetWorkflowVerdict(ctx, wfID, "MAYBE"); err == nil {
		t.Fatal("unknown verdict was accepted")
	}
}

func TestSetWorkflowPublicationStateIfPendingDoesNotOverwriteSettledState(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	wfID := createPublicationWorkflow(t, s, 42)

	if err := s.SetWorkflowPublicationStateIfPending(ctx, wfID, "review cancelled"); err != nil {
		t.Fatalf("SetWorkflowPublicationStateIfPending: %v", err)
	}
	wf, err := s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.PublicationState != store.WorkflowPublicationSkipped || wf.PublicationError != "review cancelled" {
		t.Fatalf("pending skip = %q/%q", wf.PublicationState, wf.PublicationError)
	}

	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationPublished, ""); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	if err := s.SetWorkflowPublicationStateIfPending(ctx, wfID, "review cancelled"); err != nil {
		t.Fatalf("SetWorkflowPublicationStateIfPending after publish: %v", err)
	}
	wf, err = s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow after publish: %v", err)
	}
	if wf.PublicationState != store.WorkflowPublicationPublished || wf.PublicationError != "" {
		t.Fatalf("published state was overwritten: %q/%q", wf.PublicationState, wf.PublicationError)
	}
	if err := s.SetWorkflowPublicationStateIfPending(ctx, "missing", "reason"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing workflow error = %v, want ErrNotFound", err)
	}
}
