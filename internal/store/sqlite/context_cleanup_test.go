package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func insertContextCleanupWorkflow(t *testing.T, s *Store, id string, projectID int64, status, baseSHA string) {
	t.Helper()
	now := formatTime(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	var base any
	if baseSHA != "" {
		base = baseSHA
	}
	if _, err := s.db.Exec(`INSERT INTO workflows
		(id, kind, project_id, base_sha, trigger, status, created_at)
		VALUES (?, 'review', ?, ?, 'api', ?, ?)`,
		id, projectID, base, status, now); err != nil {
		t.Fatalf("insert workflow %s: %v", id, err)
	}
}

func insertContextCleanupDoc(t *testing.T, s *Store, id string, projectID int64, baseSHA, kind string, locked bool) {
	t.Helper()
	now := formatTime(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	if _, err := s.db.Exec(`INSERT INTO context_docs
		(id, project_id, base_sha, kind, source, content, locked, created_at)
		VALUES (?, ?, ?, ?, 'generated', 'content', ?, ?)`,
		id, projectID, baseSHA, kind, boolInt(locked), now); err != nil {
		t.Fatalf("insert context doc %s: %v", id, err)
	}
}

func insertContextCleanupWorkspace(t *testing.T, s *Store, projectID int64, baseSHA, contextSHA string) {
	t.Helper()
	if _, err := s.SaveWorkspace(context.Background(), store.Workspace{
		ProjectID: projectID, State: "ready", BaseSHA: baseSHA, ContextSHA: contextSHA,
	}); err != nil {
		t.Fatalf("save workspace %d: %v", projectID, err)
	}
}

func protectedContextKeys(protected map[string]struct{}) []string {
	keys := make([]string, 0, len(protected))
	for key := range protected {
		keys = append(keys, key)
	}
	return keys
}

func TestProtectedContextSHAsKeepsEveryRetainedRevision(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 7)
	mustProject(t, s, 8)
	insertContextCleanupWorkspace(t, s, 7, "workspace-base", "workspace-context")
	insertContextCleanupWorkspace(t, s, 8, "other-workspace-base", "other-workspace-context")

	insertContextCleanupDoc(t, s, "agents-current", 7, "doc-agents", "agents", false)
	insertContextCleanupDoc(t, s, "agents-history", 7, "doc-history", "agents", false)
	insertContextCleanupDoc(t, s, "claude-history", 7, "doc-claude", "claude", false)
	insertContextCleanupDoc(t, s, "agents-locked", 7, "doc-locked", "agents", true)
	insertContextCleanupDoc(t, s, "other-project", 8, "doc-other", "agents", false)

	insertContextCleanupWorkflow(t, s, "wf-pending", 7, "pending", "wf-pending-base")
	insertContextCleanupWorkflow(t, s, "wf-running", 7, "running", "wf-running-base")
	insertContextCleanupWorkflow(t, s, "wf-pending-publish", 7, "failed", "dossier-pending-base")
	insertCleanupDossier(t, s, "wf-pending-publish", nil, store.WorkflowPublicationPending)
	insertContextCleanupWorkflow(t, s, "wf-published", 7, "completed", "dossier-published-base")
	insertCleanupDossier(t, s, "wf-published", nil, store.WorkflowPublicationPublished)
	insertContextCleanupWorkflow(t, s, "wf-skipped", 7, "completed", "dossier-skipped-base")
	insertCleanupDossier(t, s, "wf-skipped", nil, store.WorkflowPublicationSkipped)
	insertContextCleanupWorkflow(t, s, "wf-null-base", 7, "pending", "")
	insertContextCleanupWorkflow(t, s, "wf-other-project", 8, "pending", "other-project-base")

	protected, err := s.ProtectedContextSHAs(ctx, 7)
	if err != nil {
		t.Fatalf("ProtectedContextSHAs: %v", err)
	}
	want := []string{
		"doc-agents", "doc-claude", "doc-history", "doc-locked",
		"dossier-pending-base",
		"wf-pending-base", "wf-running-base",
		"workspace-base", "workspace-context",
	}
	got := protectedContextKeys(protected)
	// Map order is random; compare as sets.
	if len(got) != len(want) {
		t.Fatalf("protected = %v, want %v", got, want)
	}
	for _, key := range want {
		if _, ok := protected[key]; !ok {
			t.Errorf("protected set missing %q: %v", key, got)
		}
	}
	for _, key := range []string{
		"", "dossier-published-base", "dossier-skipped-base",
		"other-project-base", "doc-other", "other-workspace-base",
	} {
		if _, ok := protected[key]; ok {
			t.Errorf("protected set unexpectedly contains %q", key)
		}
	}
}

func TestProtectedContextSHAsRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if _, err := s.ProtectedContextSHAs(ctx, 0); err == nil {
		t.Fatal("zero project id accepted")
	}
	empty, err := s.ProtectedContextSHAs(ctx, 9)
	if err != nil {
		t.Fatalf("empty project ProtectedContextSHAs: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty project protected = %v, want empty", empty)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ProtectedContextSHAs(cancelled, 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ProtectedContextSHAs = %v, want context.Canceled", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.ProtectedContextSHAs(ctx, 7); err == nil {
		t.Fatal("ProtectedContextSHAs on a closed store returned nil error")
	}
}
