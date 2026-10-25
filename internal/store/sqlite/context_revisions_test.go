package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func listAllContextDocs(t *testing.T, s *Store, projectID int64) []store.ContextDoc {
	t.Helper()
	docs, _, err := s.ListContextDocs(context.Background(), store.ContextDocFilter{ProjectID: projectID, Limit: 200})
	if err != nil {
		t.Fatalf("ListContextDocs: %v", err)
	}
	return docs
}

func findContextDoc(t *testing.T, docs []store.ContextDoc, id string) *store.ContextDoc {
	t.Helper()
	for i := range docs {
		if docs[i].ID == id {
			return &docs[i]
		}
	}
	t.Fatalf("context document %s not found in %d versions", id, len(docs))
	return nil
}

func activeContextDocs(docs []store.ContextDoc) []store.ContextDoc {
	var active []store.ContextDoc
	for _, doc := range docs {
		if doc.Active {
			active = append(active, doc)
		}
	}
	return active
}

func TestManualContextSaveCreatesNewVersion(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	if _, err := s.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: "base-1"}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}

	first, err := s.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "first body",
	})
	if err != nil {
		t.Fatalf("first manual save: %v", err)
	}
	second, err := s.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "second body",
	})
	if err != nil {
		t.Fatalf("second manual save: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("second manual save reused id %s", first.ID)
	}
	if !second.Active || second.Source != "manual" {
		t.Fatalf("second version = %+v, want an active manual version", second)
	}

	docs := listAllContextDocs(t, s, 42)
	if len(docs) != 2 {
		t.Fatalf("versions = %d, want 2", len(docs))
	}
	old := findContextDoc(t, docs, first.ID)
	if old.Content != "first body" || old.Active {
		t.Fatalf("old version = %+v, want unchanged inactive content", old)
	}
	if newDoc := findContextDoc(t, docs, second.ID); newDoc.Content != "second body" || !newDoc.Active {
		t.Fatalf("new version = %+v", newDoc)
	}
	if active := activeContextDocs(docs); len(active) != 1 || active[0].ID != second.ID {
		t.Fatalf("active versions = %+v, want only %s", active, second.ID)
	}

	current, err := s.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || current.ID != second.ID {
		t.Fatalf("current = %+v, %v; want %s", current, err, second.ID)
	}
	byBase, err := s.GetContextDoc(ctx, 42, "base-1", "agents")
	if err != nil || byBase.ID != second.ID {
		t.Fatalf("by base = %+v, %v; want newest %s", byBase, err, second.ID)
	}
	workspace, err := s.GetWorkspace(ctx, 42)
	if err != nil || workspace.ContextSHA != "base-1" {
		t.Fatalf("workspace context sha = %q, %v; want base-1", workspace.ContextSHA, err)
	}
}

func TestContextRevisionEqualTimestampsStayDeterministic(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 7)

	var versions []*store.ContextDoc
	for i := 0; i < 3; i++ {
		doc, err := s.SaveManualContextDoc(ctx, store.ContextDoc{
			ProjectID: 7, BaseSHA: "same-base", Kind: "agents",
			Content: fmt.Sprintf("version-%d", i),
		})
		if err != nil {
			t.Fatalf("save version %d: %v", i, err)
		}
		versions = append(versions, doc)
	}
	if versions[0].CreatedAt != versions[2].CreatedAt {
		t.Fatalf("test clock did not keep timestamps equal: %v vs %v", versions[0].CreatedAt, versions[2].CreatedAt)
	}
	current, err := s.GetCurrentContextDoc(ctx, 7, "agents")
	if err != nil || current.ID != versions[2].ID {
		t.Fatalf("current = %+v, %v; want last inserted %s", current, err, versions[2].ID)
	}
	byBase, err := s.GetContextDoc(ctx, 7, "same-base", "agents")
	if err != nil || byBase.ID != versions[2].ID {
		t.Fatalf("by base = %+v, %v; want last inserted %s", byBase, err, versions[2].ID)
	}
	docs := listAllContextDocs(t, s, 7)
	if active := activeContextDocs(docs); len(active) != 1 || active[0].ID != versions[2].ID {
		t.Fatalf("active versions = %+v, want only %s", active, versions[2].ID)
	}
}

func TestGeneratedContextDoesNotReplaceLockedCurrent(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)

	generated, err := s.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "generated",
		WorkflowID: "wf-1", Model: "model-1",
	})
	if err != nil {
		t.Fatalf("SaveContextDoc: %v", err)
	}
	user := int64(7)
	if err := s.LockContextDoc(ctx, 42, "base-1", "agents", true, &user); err != nil {
		t.Fatalf("LockContextDoc: %v", err)
	}

	refreshed, err := s.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "refresh",
		WorkflowID: "wf-2", Model: "model-1",
	})
	if err != nil {
		t.Fatalf("refresh over locked current: %v", err)
	}
	if refreshed.Active || refreshed.ID == generated.ID {
		t.Fatalf("refresh = %+v, want a new inactive version", refreshed)
	}

	retry, err := s.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "refresh retry",
		WorkflowID: "wf-2", Model: "model-1",
	})
	if err != nil || retry.ID != refreshed.ID {
		t.Fatalf("workflow retry = %+v, %v; want idempotent id %s", retry, err, refreshed.ID)
	}
	sameWorkflow, err := s.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "generated retry",
		WorkflowID: "wf-1", Model: "model-1",
	})
	if err != nil || sameWorkflow.ID != generated.ID {
		t.Fatalf("original workflow retry = %+v, %v; want %s", sameWorkflow, err, generated.ID)
	}

	if docs := listAllContextDocs(t, s, 42); len(docs) != 2 {
		t.Fatalf("versions after retries = %d, want 2", len(docs))
	}
	current, err := s.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil {
		t.Fatalf("GetCurrentContextDoc: %v", err)
	}
	if current.ID != generated.ID || !current.Locked || current.Content != "generated" {
		t.Fatalf("current = %+v, want the locked original", current)
	}
	byBase, err := s.GetContextDoc(ctx, 42, "base-1", "agents")
	if err != nil || byBase.ID != refreshed.ID || byBase.Content != "refresh" {
		t.Fatalf("by base = %+v, %v; want newest refresh %s", byBase, err, refreshed.ID)
	}

	manual, err := s.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "manual replacement",
		Locked: true,
	})
	if err != nil {
		t.Fatalf("manual replacement: %v", err)
	}
	if manual.ID == generated.ID || !manual.Active || !manual.Locked {
		t.Fatalf("manual replacement = %+v, want a new active locked version", manual)
	}
	docs := listAllContextDocs(t, s, 42)
	old := findContextDoc(t, docs, generated.ID)
	if !old.Locked || old.Content != "generated" {
		t.Fatalf("old locked version changed: %+v", old)
	}
	if active := activeContextDocs(docs); len(active) != 1 || active[0].ID != manual.ID {
		t.Fatalf("active versions = %+v, want only %s", active, manual.ID)
	}
}

func TestSetActiveContextActivatesNewestVersionOfThatBase(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	if _, err := s.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: "base-1"}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	older, err := s.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "v1",
	})
	if err != nil {
		t.Fatalf("save v1: %v", err)
	}
	newer, err := s.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-1", Kind: "agents", Content: "v2",
	})
	if err != nil {
		t.Fatalf("save v2: %v", err)
	}
	if err := s.SetActiveContext(ctx, 42, "base-1"); err != nil {
		t.Fatalf("SetActiveContext: %v", err)
	}
	current, err := s.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || current.ID != newer.ID {
		t.Fatalf("current = %+v, %v; want newest %s", current, err, newer.ID)
	}
	if err := s.LockContextDoc(ctx, 42, older.BaseSHA, "agents", true, nil); err != nil {
		t.Fatalf("LockContextDoc: %v", err)
	}
	locked, err := s.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || locked.ID != newer.ID || !locked.Locked {
		t.Fatalf("locked current = %+v, %v; want %s", locked, err, newer.ID)
	}
	docs := listAllContextDocs(t, s, 42)
	if old := findContextDoc(t, docs, older.ID); old.Locked {
		t.Fatalf("locking the current version also locked history: %+v", old)
	}
}

func TestPruneContextDocsProtectsOnlyTheActiveRevision(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 1)
	now := clock.Now()

	insert := func(id, base, kind string, active, locked bool, at time.Time) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO context_docs
			(id, project_id, base_sha, kind, source, content, locked, active, created_at)
			VALUES (?, 1, ?, ?, 'generated', 'content', ?, ?, ?)`,
			id, base, kind, boolInt(locked), boolInt(active), formatTime(at)); err != nil {
			t.Fatalf("insert context doc %s: %v", id, err)
		}
	}
	insert("agents-active", "shared", "agents", true, false, now.Add(-10*time.Hour))
	insert("agents-old", "shared", "agents", false, false, now.Add(-9*time.Hour))
	insert("agents-locked", "locked-base", "agents", false, true, now.Add(-8*time.Hour))
	for i := 1; i <= 5; i++ {
		insert(fmt.Sprintf("agents-%d", i), fmt.Sprintf("a-%d", i), "agents", false, false, now.Add(time.Duration(i-7)*time.Hour))
	}
	insert("claude-active", "claude-base", "claude", true, false, now.Add(-10*time.Hour))
	insert("claude-old", "claude-base", "claude", false, false, now.Add(-9*time.Hour))

	if n, err := s.PruneContextDocs(ctx, 3); err != nil {
		t.Fatalf("PruneContextDocs: %v", err)
	} else if n != 3 {
		t.Fatalf("pruned = %d, want 3", n)
	}
	for _, id := range []string{"agents-active", "agents-locked", "agents-3", "agents-4", "agents-5", "claude-active", "claude-old"} {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE id = ?`, id); got != 1 {
			t.Errorf("%s was deleted", id)
		}
	}
	for _, id := range []string{"agents-old", "agents-1", "agents-2"} {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE id = ?`, id); got != 0 {
			t.Errorf("%s was not pruned", id)
		}
	}
	if n, err := s.PruneContextDocs(ctx, 3); err != nil || n != 0 {
		t.Fatalf("second prune = %d, %v; want 0", n, err)
	}
}

func TestPruneContextDocsEqualTimestampsKeepNewestRows(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 1)
	stamp := formatTime(clock.Now())

	insert := func(id string, active bool) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO context_docs
			(id, project_id, base_sha, kind, source, content, locked, active, created_at)
			VALUES (?, 1, ?, 'agents', 'generated', 'content', 0, ?, ?)`,
			id, id, boolInt(active), stamp); err != nil {
			t.Fatalf("insert context doc %s: %v", id, err)
		}
	}
	insert("agents-active", true)
	// Insertion order and ID order differ: the newest row has the smallest ID.
	insert("agents-z", false)
	insert("agents-m", false)
	insert("agents-a", false)

	if n, err := s.PruneContextDocs(ctx, 1); err != nil || n != 2 {
		t.Fatalf("PruneContextDocs = %d, %v; want 2", n, err)
	}
	for _, id := range []string{"agents-active", "agents-a"} {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE id = ?`, id); got != 1 {
			t.Errorf("%s was deleted", id)
		}
	}
	for _, id := range []string{"agents-z", "agents-m"} {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE id = ?`, id); got != 0 {
			t.Errorf("%s was not pruned", id)
		}
	}
}

func TestClaudeContextDoesNotChangeWorkspaceContextSHA(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	if _, err := s.SaveWorkspace(ctx, store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: "base-b", ContextSHA: "base-a",
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	agents, err := s.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-a", Kind: "agents", Content: "agents",
	})
	if err != nil {
		t.Fatalf("save agents: %v", err)
	}
	if agents.BaseSHA != "base-a" {
		t.Fatalf("agents base = %q", agents.BaseSHA)
	}
	claude, err := s.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-b", Kind: "claude", Content: "claude",
	})
	if err != nil {
		t.Fatalf("save claude: %v", err)
	}
	if !claude.Active || claude.BaseSHA != "base-b" {
		t.Fatalf("claude = %+v, want active base-b", claude)
	}
	state, err := s.GetWorkspace(ctx, 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.ContextSHA != "base-a" {
		t.Fatalf("workspace context sha = %q, want agents base-a", state.ContextSHA)
	}
	manual, err := s.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-b", Kind: "claude", Content: "claude manual",
	})
	if err != nil || !manual.Active {
		t.Fatalf("manual claude = %+v, %v", manual, err)
	}
	state, err = s.GetWorkspace(ctx, 42)
	if err != nil || state.ContextSHA != "base-a" {
		t.Fatalf("workspace after manual claude = %q, %v; want base-a", state.ContextSHA, err)
	}
}

func TestSetActiveContextDoesNotReplaceLockedRevision(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	if _, err := s.SaveWorkspace(ctx, store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: "base-b", ContextSHA: "base-a",
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	locked, err := s.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-a", Kind: "agents", Content: "locked",
	})
	if err != nil {
		t.Fatalf("save base-a: %v", err)
	}
	if err := s.LockContextDoc(ctx, 42, "base-a", "agents", true, nil); err != nil {
		t.Fatalf("LockContextDoc: %v", err)
	}
	if _, err := s.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "base-b", Kind: "agents", Content: "newer",
	}); err != nil {
		t.Fatalf("save base-b: %v", err)
	}
	if err := s.SetActiveContext(ctx, 42, "base-b"); err != nil {
		t.Fatalf("SetActiveContext: %v", err)
	}
	current, err := s.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || current.ID != locked.ID || !current.Locked {
		t.Fatalf("current = %+v, %v; want locked base-a", current, err)
	}
	state, err := s.GetWorkspace(ctx, 42)
	if err != nil || state.ContextSHA != "base-a" {
		t.Fatalf("workspace context sha = %q, %v; want base-a", state.ContextSHA, err)
	}
}

func TestListContextDocsCursorStableWithEqualTimestamps(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 1)
	stamp := formatTime(clock.Now())
	for _, id := range []string{"agents-z", "agents-m", "agents-a"} {
		if _, err := s.db.Exec(`INSERT INTO context_docs
			(id, project_id, base_sha, kind, source, content, locked, active, created_at)
			VALUES (?, 1, ?, 'agents', 'generated', 'content', 0, 0, ?)`,
			id, id, stamp); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	page, cursor, err := s.ListContextDocs(ctx, store.ContextDocFilter{ProjectID: 1, Limit: 1})
	if err != nil || len(page) != 1 || page[0].ID != "agents-a" || cursor == "" {
		t.Fatalf("first page = %+v, cursor %q, %v; want newest rowid agents-a", page, cursor, err)
	}
	second, cursor, err := s.ListContextDocs(ctx, store.ContextDocFilter{ProjectID: 1, Limit: 1, Cursor: cursor})
	if err != nil || len(second) != 1 || second[0].ID != "agents-m" || cursor == "" {
		t.Fatalf("second page = %+v, cursor %q, %v", second, cursor, err)
	}
	third, cursor, err := s.ListContextDocs(ctx, store.ContextDocFilter{ProjectID: 1, Limit: 1, Cursor: cursor})
	if err != nil || len(third) != 1 || third[0].ID != "agents-z" || cursor != "" {
		t.Fatalf("third page = %+v, cursor %q, %v", third, cursor, err)
	}

	// Previously issued cursors encoded the row id instead of the rowid.
	legacy, _, err := s.ListContextDocs(ctx, store.ContextDocFilter{
		ProjectID: 1, Limit: 2, Cursor: encodeCursor(clock.Now(), "agents-a"),
	})
	if err != nil || len(legacy) != 2 || legacy[0].ID != "agents-m" || legacy[1].ID != "agents-z" {
		t.Fatalf("legacy cursor page = %+v, %v", legacy, err)
	}
	if _, _, err := s.ListContextDocs(ctx, store.ContextDocFilter{
		ProjectID: 1, Cursor: encodeCursor(clock.Now(), "missing"),
	}); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("missing legacy cursor error = %v, want ErrInvalidCursor", err)
	}
}
