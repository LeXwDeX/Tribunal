package app

import (
	"context"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

func newContextTestService(t *testing.T) (*ContextService, *store.ContextDoc, *workspace.Workspace, string) {
	t.Helper()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	base := strings.Repeat("a", 40)
	if _, err := st.SaveWorkspace(context.Background(), store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: base,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	manager := workspace.NewManager(t.TempDir(), &repo.Git{})
	svc, err := NewContextService(ContextServiceConfig{Store: st, Workspace: manager})
	if err != nil {
		t.Fatalf("NewContextService: %v", err)
	}
	doc, err := svc.SaveContext(context.Background(), 42, "agents", "first body", nil, nil)
	if err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	return svc, doc, manager.Get(42), base
}

func TestContextServiceManualSaveProjectsToDisk(t *testing.T) {
	svc, first, ws, base := newContextTestService(t)
	ctx := context.Background()

	data, err := ws.ReadContext(base, "AGENTS.md")
	if err != nil || string(data) != "first body" {
		t.Fatalf("projected document = %q, %v", data, err)
	}
	meta, err := ws.ContextMeta(base)
	if err != nil {
		t.Fatalf("ContextMeta: %v", err)
	}
	if meta.SourceSHA != base || meta.GeneratedAt != first.CreatedAt || meta.Locked {
		t.Fatalf("metadata = %+v, want base %s generated at %v", meta, base, first.CreatedAt)
	}

	user := int64(7)
	second, err := svc.SaveContext(ctx, 42, "agents", "second body", nil, &user)
	if err != nil {
		t.Fatalf("second SaveContext: %v", err)
	}
	if second.ID == first.ID || !second.Active {
		t.Fatalf("second version = %+v", second)
	}
	current, err := svc.store.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || current.ID != second.ID {
		t.Fatalf("current = %+v, %v; want %s", current, err, second.ID)
	}
	data, err = ws.ReadContext(base, "AGENTS.md")
	if err != nil || string(data) != "second body" {
		t.Fatalf("projected document after edit = %q, %v", data, err)
	}
	meta, err = ws.ContextMeta(base)
	if err != nil || meta.GeneratedAt != second.CreatedAt || meta.SourceSHA != base {
		t.Fatalf("metadata after edit = %+v, %v", meta, err)
	}
}

func TestContextServiceLockUpdatesProjection(t *testing.T) {
	svc, doc, ws, base := newContextTestService(t)
	ctx := context.Background()

	if err := svc.LockContext(ctx, 42, "agents", true, nil); err != nil {
		t.Fatalf("LockContext: %v", err)
	}
	current, err := svc.store.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || !current.Locked || current.ID != doc.ID {
		t.Fatalf("current after lock = %+v, %v", current, err)
	}
	meta, err := ws.ContextMeta(base)
	if err != nil || !meta.Locked {
		t.Fatalf("metadata after lock = %+v, %v", meta, err)
	}
	data, err := ws.ReadContext(base, "AGENTS.md")
	if err != nil || string(data) != "first body" {
		t.Fatalf("locked content = %q, %v", data, err)
	}

	if err := svc.LockContext(ctx, 42, "agents", false, nil); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	meta, err = ws.ContextMeta(base)
	if err != nil || meta.Locked {
		t.Fatalf("metadata after unlock = %+v, %v", meta, err)
	}
}

func TestProjectContextSkipsInactiveRevision(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	base := strings.Repeat("a", 40)
	if _, err := st.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	manager := workspace.NewManager(t.TempDir(), &repo.Git{})
	ws := manager.Get(42)
	active, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: base, Kind: "agents", Content: "active body",
	})
	if err != nil || !active.Active {
		t.Fatalf("active save = %+v, %v", active, err)
	}
	if err := projectContextDoc(ws, active, false); err != nil {
		t.Fatalf("project active: %v", err)
	}
	if err := st.LockContextDoc(ctx, 42, base, "agents", true, nil); err != nil {
		t.Fatalf("LockContextDoc: %v", err)
	}
	inactive, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: base, Kind: "agents", Content: "inactive body",
	})
	if err != nil || inactive.Active {
		t.Fatalf("inactive save = %+v, %v", inactive, err)
	}
	if err := projectContextDoc(ws, inactive, false); err != nil {
		t.Fatalf("project inactive: %v", err)
	}
	data, err := ws.ReadContext(base, "AGENTS.md")
	if err != nil || string(data) != "active body" {
		t.Fatalf("projection = %q, %v; want active content", data, err)
	}
}

func TestProjectContextUsesStoredContentOnWorkflowRetry(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	base := strings.Repeat("a", 40)
	if _, err := st.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	manager := workspace.NewManager(t.TempDir(), &repo.Git{})
	ws := manager.Get(42)
	original, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: base, Kind: "agents", Content: "stored body",
		WorkflowID: "wf-retry",
	})
	if err != nil {
		t.Fatalf("first generated save: %v", err)
	}
	retry, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: base, Kind: "agents", Content: "retry body",
		WorkflowID: "wf-retry",
	})
	if err != nil || retry.ID != original.ID || retry.Content != "stored body" {
		t.Fatalf("retry = %+v, %v; want original %s", retry, err, original.ID)
	}
	if err := projectContextDoc(ws, retry, false); err != nil {
		t.Fatalf("project retry: %v", err)
	}
	data, err := ws.ReadContext(base, "AGENTS.md")
	if err != nil || string(data) != "stored body" {
		t.Fatalf("projection = %q, %v; want stored content", data, err)
	}
}
