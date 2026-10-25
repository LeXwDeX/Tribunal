package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func TestContextDocsMigrationPreservesLegacyRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, name := range []string{"0001_init.sql", "0003_review_state.sql"} {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES
		(1, '0001_init.sql', '2026-10-22T12:00:00.000000000Z'),
		(3, '0003_review_state.sql', '2026-10-22T12:00:00.000000000Z')`); err != nil {
		t.Fatalf("record legacy migrations: %v", err)
	}
	stamp := "2026-10-22T10:00:00.000000000Z"
	if _, err := legacy.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc,
		 webhook_secret_enc, created_at, updated_at)
		VALUES (1, 'group/legacy', 'https://gitlab.example.com/group/legacy',
		 'main', x'00', x'01', ?, ?)`, stamp, stamp); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO workspaces
		(project_id, state, base_sha, context_sha, updated_at)
		VALUES (1, 'ready', 'base-1', 'base-1', ?)`, stamp); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO context_docs
		(id, project_id, base_sha, kind, source, content, locked, created_at)
		VALUES
		('discard', 1, 'discard', 'agents', 'generated', 'discard', 0, ?),
		('legacy-base-1', 1, 'base-1', 'agents', 'generated', 'agents base 1', 0, ?),
		('legacy-base-2', 1, 'base-2', 'agents', 'generated', 'agents base 2', 1, ?),
		('legacy-claude', 1, 'base-2', 'claude', 'manual', 'claude base 2', 0, ?)`,
		stamp, stamp, "2026-10-22T11:00:00.000000000Z", "2026-10-22T09:00:00.000000000Z"); err != nil {
		t.Fatalf("insert context docs: %v", err)
	}
	// Leave a rowid hole so an implicit reinsert would renumber the rows.
	if _, err := legacy.Exec(`DELETE FROM context_docs WHERE id = 'discard'`); err != nil {
		t.Fatalf("delete discard row: %v", err)
	}
	var legacyRowID int64
	if err := legacy.QueryRow(`SELECT rowid FROM context_docs WHERE id = 'legacy-base-1'`).Scan(&legacyRowID); err != nil {
		t.Fatalf("read legacy rowid: %v", err)
	}
	if legacyRowID == 1 {
		t.Fatalf("test setup did not create a rowid hole: %d", legacyRowID)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("Open migrated db: %v", err)
	}
	defer s.Close()

	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != 11 {
		t.Fatalf("schema_migrations = %d, want 10", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs`); got != 3 {
		t.Fatalf("context_docs = %d, want 3 preserved rows", got)
	}
	var migratedRowID int64
	if err := s.db.QueryRow(`SELECT rowid FROM context_docs WHERE id = 'legacy-base-1'`).Scan(&migratedRowID); err != nil {
		t.Fatalf("read migrated rowid: %v", err)
	}
	if migratedRowID != legacyRowID {
		t.Fatalf("legacy rowid = %d, want %d preserved", migratedRowID, legacyRowID)
	}
	agents1, err := s.GetContextDoc(ctx, 1, "base-1", "agents")
	if err != nil || agents1.Content != "agents base 1" || !agents1.Active {
		t.Fatalf("legacy base-1 = %+v, %v; want preserved active row", agents1, err)
	}
	agents2, err := s.GetContextDoc(ctx, 1, "base-2", "agents")
	if err != nil || agents2.Content != "agents base 2" || !agents2.Locked || agents2.Active {
		t.Fatalf("legacy base-2 = %+v, %v; want preserved locked inactive row", agents2, err)
	}
	claude, err := s.GetCurrentContextDoc(ctx, 1, "claude")
	if err != nil || claude.ID != "legacy-claude" || claude.Content != "claude base 2" {
		t.Fatalf("legacy claude = %+v, %v; want preserved row", claude, err)
	}

	// The old (project_id, base_sha, kind) unique constraint is gone: another
	// manual save of the same base creates a new version.
	saved, err := s.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: 1, BaseSHA: "base-1", Kind: "agents", Content: "new manual version",
	})
	if err != nil {
		t.Fatalf("save on migrated schema: %v", err)
	}
	if saved.ID == agents1.ID || !saved.Active {
		t.Fatalf("migrated save = %+v, want a new active version", saved)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs`); got != 4 {
		t.Fatalf("context_docs after save = %d, want 4", got)
	}
}
