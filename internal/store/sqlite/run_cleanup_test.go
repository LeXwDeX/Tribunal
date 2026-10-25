package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func insertCleanupWorkflow(t *testing.T, s *Store, id string, projectID int64, status string) {
	t.Helper()
	now := formatTime(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	if _, err := s.db.Exec(`INSERT INTO workflows
		(id, kind, project_id, trigger, status, created_at)
		VALUES (?, 'review', ?, 'api', ?, ?)`, id, projectID, status, now); err != nil {
		t.Fatalf("insert workflow %s: %v", id, err)
	}
}

func insertCleanupDossier(t *testing.T, s *Store, workflowID string, jobID *string, publication string) {
	t.Helper()
	now := formatTime(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	var job any
	if jobID != nil {
		job = *jobID
	}
	if _, err := s.db.Exec(`INSERT INTO workflow_dossiers
		(workflow_id, job_id, generation, version, hash, data, publication_state,
		 created_at, updated_at)
		VALUES (?, ?, 0, 1, 'hash', x'00', ?, ?, ?)`,
		workflowID, job, publication, now, now); err != nil {
		t.Fatalf("insert dossier %s: %v", workflowID, err)
	}
}

func TestListWorkspaceProjectIDsUsesRegisteredWorkspaces(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 21)
	mustProject(t, s, 34)
	if _, err := s.db.Exec(`DELETE FROM workspaces WHERE project_id = 34`); err != nil {
		t.Fatal(err)
	}

	ids, err := s.ListWorkspaceProjectIDs(ctx)
	if err != nil {
		t.Fatalf("ListWorkspaceProjectIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != 21 {
		t.Fatalf("ids = %v, want [21]", ids)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ListWorkspaceProjectIDs(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ListWorkspaceProjectIDs = %v, want context.Canceled", err)
	}
}

func TestProtectedRunRefsKeepsActiveAndUnpublishedWork(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 7)

	for _, wf := range []struct {
		id     string
		status string
	}{
		{id: "wf-pending", status: "pending"},
		{id: "wf-running", status: "running"},
		{id: "wf-completed", status: "completed"},
		{id: "wf-failed", status: "failed"},
		{id: "wf-cancelled", status: "cancelled"},
	} {
		insertCleanupWorkflow(t, s, wf.id, 7, wf.status)
	}
	for _, job := range []struct {
		id    string
		state string
	}{
		{id: "job-queued", state: store.JobQueued},
		{id: "job-running", state: store.JobRunning},
		{id: "job-done", state: store.JobDone},
		{id: "job-failed", state: store.JobFailed},
	} {
		insertJobRow(t, s, job.id, job.state, time.Now())
	}

	publishedJob := "job-published"
	insertCleanupDossier(t, s, "wf-completed", &publishedJob, store.WorkflowPublicationPublished)
	failedPublishJob := "job-failed-publish"
	insertCleanupDossier(t, s, "wf-failed-publish", &failedPublishJob, store.WorkflowPublicationFailed)
	pendingJob := "job-pending-publish"
	insertCleanupDossier(t, s, "wf-failed", &pendingJob, store.WorkflowPublicationPending)
	insertCleanupDossier(t, s, "wf-cancelled", nil, store.WorkflowPublicationPending)

	protected, err := s.ProtectedRunRefs(ctx)
	if err != nil {
		t.Fatalf("ProtectedRunRefs: %v", err)
	}
	for _, want := range []string{
		"wf-pending", "wf-running",
		"job-queued", "job-running",
		"wf-failed", "job-pending-publish", "wf-cancelled",
	} {
		if _, ok := protected[want]; !ok {
			t.Errorf("protected refs missing %q: %v", want, protected)
		}
	}
	for _, unwanted := range []string{
		"wf-completed", "wf-completed-no-dossier", "job-done", "job-failed",
		"job-published", "wf-failed-publish", "job-failed-publish",
	} {
		if _, ok := protected[unwanted]; ok {
			t.Errorf("protected refs unexpectedly contains %q", unwanted)
		}
	}

	_ = s.Close()
	if _, err := s.ProtectedRunRefs(ctx); err == nil {
		t.Fatal("ProtectedRunRefs on a closed store returned nil error")
	}
}
