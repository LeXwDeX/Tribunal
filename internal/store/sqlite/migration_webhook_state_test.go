package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/migrations"
)

func TestWebhookStateMigrationPreservesLegacyProjects(t *testing.T) {
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
		 webhook_secret_enc, hook_id, created_at, updated_at)
		VALUES
		(1, 'group/linked', 'https://gitlab.example.com/group/linked', 'main', x'00', x'01', 77, ?, ?),
		(2, 'group/plain', 'https://gitlab.example.com/group/plain', 'main', x'00', x'01', NULL, ?, ?)`,
		stamp, stamp, stamp, stamp); err != nil {
		t.Fatalf("insert projects: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("Open migrated db: %v", err)
	}
	defer s.Close()

	linked, err := s.GetProject(ctx, 1)
	if err != nil {
		t.Fatalf("GetProject linked: %v", err)
	}
	if linked.WebhookState != store.WebhookStateUnknown || linked.HookID == nil || *linked.HookID != 77 {
		t.Fatalf("linked project = %+v", linked)
	}
	if linked.WebhookManagedBy != store.WebhookManagedByService ||
		linked.WebhookCreatedAt == nil || linked.WebhookUpdatedAt == nil {
		t.Fatalf("legacy managed hook ownership = %+v", linked)
	}
	plain, err := s.GetProject(ctx, 2)
	if err != nil {
		t.Fatalf("GetProject plain: %v", err)
	}
	if plain.WebhookState != store.WebhookStateDisabled || plain.HookID != nil {
		t.Fatalf("plain project = %+v", plain)
	}
	if plain.WebhookManagedBy != "" || plain.WebhookCreatedAt != nil || plain.WebhookUpdatedAt != nil {
		t.Fatalf("legacy plain hook ownership = %+v", plain)
	}
	for _, project := range []*store.Project{linked, plain} {
		if project.WebhookEvents == nil || len(project.WebhookEvents) != 0 || project.WebhookError != "" {
			t.Fatalf("migrated webhook fields = %+v", project)
		}
	}
}
