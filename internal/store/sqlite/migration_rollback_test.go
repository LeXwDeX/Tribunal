package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func TestMigrationRollbackAfterLateFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rollback.db")

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
		VALUES (1, 'group/rollback', 'https://gitlab.example.com/group/rollback',
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
		stamp, stamp, stamp, stamp); err != nil {
		t.Fatalf("insert context docs: %v", err)
	}
	// Leave a rowid hole so a rollback that reinserts rows would renumber them.
	if _, err := legacy.Exec(`DELETE FROM context_docs WHERE id = 'discard'`); err != nil {
		t.Fatalf("delete discard row: %v", err)
	}
	var legacyRowID int64
	if err := legacy.QueryRow(`SELECT rowid FROM context_docs WHERE id = 'legacy-base-1'`).Scan(&legacyRowID); err != nil {
		t.Fatalf("read legacy rowid: %v", err)
	}
	// Fail after 0004's table rebuild and before its schema_migrations insert.
	if _, err := legacy.Exec(`CREATE TRIGGER reject_migration_4
		BEFORE INSERT ON schema_migrations
		WHEN NEW.version = 4
		BEGIN
			SELECT RAISE(ABORT, 'injected migration 4 failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	if _, err := Open(ctx, path, WithClock(newFakeClock().Now)); err == nil {
		t.Fatal("Open succeeded despite injected migration failure")
	} else if !strings.Contains(err.Error(), "record migration 0004_context_revisions.sql") {
		t.Fatalf("Open error = %v, want late 0004 record failure", err)
	}

	checked, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatalf("reopen rolled-back db: %v", err)
	}
	t.Cleanup(func() { _ = checked.Close() })
	if got := count(t, checked, `SELECT COUNT(*) FROM schema_migrations`); got != 2 {
		t.Fatalf("schema_migrations = %d, want only versions 1 and 3", got)
	}
	if got := count(t, checked, `SELECT COUNT(*) FROM schema_migrations WHERE version >= 4`); got != 0 {
		t.Fatalf("recorded %d migrations at or above version 4 after rollback", got)
	}
	if got := count(t, checked, `SELECT COUNT(*) FROM context_docs`); got != 3 {
		t.Fatalf("context_docs = %d, want 3 preserved rows", got)
	}
	var rowID int64
	if err := checked.QueryRow(`SELECT rowid FROM context_docs WHERE id = 'legacy-base-1'`).Scan(&rowID); err != nil {
		t.Fatalf("read rolled-back rowid: %v", err)
	}
	if rowID != legacyRowID {
		t.Fatalf("rowid = %d, want %d preserved", rowID, legacyRowID)
	}
	var content string
	var locked int
	if err := checked.QueryRow(`SELECT content, locked FROM context_docs WHERE id = 'legacy-base-2'`).Scan(&content, &locked); err != nil {
		t.Fatalf("read preserved row: %v", err)
	}
	if content != "agents base 2" || locked != 1 {
		t.Fatalf("preserved row = (%q, %d), want (%q, 1)", content, locked, "agents base 2")
	}
	if got := count(t, checked, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('context_docs_new')`); got != 0 {
		t.Fatalf("context_docs_new still exists after rollback")
	}
	if got := count(t, checked, `SELECT COUNT(*) FROM pragma_table_info('context_docs') WHERE name = 'active'`); got != 0 {
		t.Fatalf("0004 column survived rollback")
	}
	if got := count(t, checked, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_context_docs_active'`); got != 0 {
		t.Fatalf("0004 index survived rollback")
	}

	// The old UNIQUE (project_id, base_sha, kind) constraint must still reject
	// the duplicate that the rebuilt 0004 schema would allow.
	dupTx, err := checked.Begin()
	if err != nil {
		t.Fatalf("begin duplicate check: %v", err)
	}
	if _, err := dupTx.Exec(`INSERT INTO context_docs
		(id, project_id, base_sha, kind, source, content, locked, created_at)
		VALUES ('duplicate', 1, 'base-1', 'agents', 'generated', 'duplicate', 0, ?)`, stamp); err == nil {
		_ = dupTx.Rollback()
		t.Fatal("old unique constraint was lost: duplicate insert succeeded")
	}
	if err := dupTx.Rollback(); err != nil {
		t.Fatalf("rollback duplicate check: %v", err)
	}
	if got := count(t, checked, `SELECT COUNT(*) FROM context_docs`); got != 3 {
		t.Fatalf("context_docs after duplicate check = %d, want 3", got)
	}

	if _, err := checked.Exec(`DROP TRIGGER reject_migration_4`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if err := checked.Close(); err != nil {
		t.Fatalf("close checked db: %v", err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("Open after removing trigger: %v", err)
	}
	defer s.Close()
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != 11 {
		t.Fatalf("schema_migrations after retry = %d, want 10", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations WHERE version IN (4, 5, 6)`); got != 3 {
		t.Fatalf("migrations 4-6 recorded = %d, want 3", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs`); got != 3 {
		t.Fatalf("context_docs after retry = %d, want 3", got)
	}
	if err := s.db.QueryRow(`SELECT rowid FROM context_docs WHERE id = 'legacy-base-1'`).Scan(&rowID); err != nil {
		t.Fatalf("read retried rowid: %v", err)
	}
	if rowID != legacyRowID {
		t.Fatalf("retried rowid = %d, want %d preserved", rowID, legacyRowID)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM pragma_table_info('context_docs') WHERE name = 'active'`); got != 1 {
		t.Fatalf("context_docs.active missing after successful retry")
	}
}

func count(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}
