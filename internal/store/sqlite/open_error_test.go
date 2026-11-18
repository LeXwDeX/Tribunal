package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

// TestOpenWrapsMigrationFailure covers a schema migration that cannot be
// recorded: Open must keep the cause and expose the stage, migration file and
// SQLite result code without callers parsing the message (#543).
func TestOpenWrapsMigrationFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	// An extra NOT NULL column makes the migration bookkeeping insert fail
	// after the first migration body has been applied.
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL,
		unexpected TEXT NOT NULL
	)`); err != nil {
		raw.Close()
		t.Fatalf("create schema_migrations: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("Open succeeded despite an incompatible schema_migrations table")
	}
	var openErr *OpenError
	if !errors.As(err, &openErr) {
		t.Fatalf("Open error is not an *OpenError: %v", err)
	}
	if openErr.Stage != StageMigrate {
		t.Fatalf("stage = %q, want %q", openErr.Stage, StageMigrate)
	}
	if !strings.HasPrefix(openErr.Migration, "0001_") {
		t.Fatalf("migration = %q, want the first migration file", openErr.Migration)
	}
	if openErr.Code&0xff != 19 {
		t.Fatalf("sqlite code = %d, want a constraint failure", openErr.Code)
	}
	if openErr.Err == nil {
		t.Fatal("underlying cause was dropped")
	}
}

// TestOpenWrapsBackfillFailure covers the startup command replay backfill
// failing after migrations succeeded.
func TestOpenWrapsBackfillFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := first.db.ExecContext(ctx, `DROP TABLE review_command_replay_backfill`); err != nil {
		first.Close()
		t.Fatalf("drop backfill marker table: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("Open succeeded despite a missing backfill marker table")
	}
	var openErr *OpenError
	if !errors.As(err, &openErr) {
		t.Fatalf("Open error is not an *OpenError: %v", err)
	}
	if openErr.Stage != StageBackfill {
		t.Fatalf("stage = %q, want %q", openErr.Stage, StageBackfill)
	}
	if openErr.Err == nil {
		t.Fatal("underlying cause was dropped")
	}
}

// TestOpenWrapsForeignKeyCheckFailure rehearses a migration that leaves an
// orphan row: the error must name the stage and offending table.
func TestOpenWrapsForeignKeyCheckFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		legacy.Close()
		t.Fatalf("create schema_migrations: %v", err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		legacy.Close()
		t.Fatalf("read migrations: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	stamp := "2026-11-18T10:00:00.000000000Z"
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			legacy.Close()
			t.Fatalf("migrationVersion(%s): %v", name, err)
		}
		if version >= 34 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			legacy.Close()
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			legacy.Close()
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, stamp); err != nil {
			legacy.Close()
			t.Fatalf("record %s: %v", name, err)
		}
	}
	// Foreign keys are off on this plain connection, so the orphan is accepted.
	if _, err := legacy.Exec(`INSERT INTO context_docs
		(id, project_id, base_sha, kind, source, content, created_at)
		VALUES ('orphan', 999, 'sha', 'agents', 'generated', 'body', ?)`, stamp); err != nil {
		legacy.Close()
		t.Fatalf("insert orphan context doc: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("Open succeeded despite an orphan row")
	}
	var openErr *OpenError
	if !errors.As(err, &openErr) {
		t.Fatalf("Open error is not an *OpenError: %v", err)
	}
	if openErr.Stage != StageFKCheck {
		t.Fatalf("stage = %q, want %q", openErr.Stage, StageFKCheck)
	}
	if openErr.Table != "context_docs" {
		t.Fatalf("table = %q, want context_docs", openErr.Table)
	}
}
