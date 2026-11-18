package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

// TestProjectRevisionMigrationDefaultsUpgradedRows rehearses the 0035 upgrade
// on a database written before it (#517): existing projects start at revision 1
// so a client can load and then conditionally save them.
func TestProjectRevisionMigrationDefaultsUpgradedRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-projects.db")

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
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
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
			t.Fatalf("migrationVersion(%s): %v", name, err)
		}
		if version >= 35 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, stamp); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc, webhook_secret_enc, created_at, updated_at)
		VALUES (7, 'group/legacy', 'https://gitlab.example.com/group/legacy', 'main', x'00', x'01', ?, ?)`,
		stamp, stamp); err != nil {
		t.Fatalf("insert legacy project: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open upgraded store: %v", err)
	}
	defer st.Close()

	project, err := st.GetProject(ctx, 7)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if project.Revision != 1 {
		t.Fatalf("upgraded project revision = %d, want 1", project.Revision)
	}
	updated, err := st.UpdateProject(ctx, 7, store.ProjectUpdate{
		Enabled:  ptrBool(false),
		Revision: ptrInt64(project.Revision),
	})
	if err != nil {
		t.Fatalf("conditional update after upgrade: %v", err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision after upgrade write = %d, want 2", updated.Revision)
	}
}
