package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func TestModelDefaultsCRUDRevisionAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "defaults.db")
	clock := newFakeClock()
	s, err := Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	empty, err := s.GetModelDefaults(ctx)
	if err != nil || empty.Revision != 0 || empty.Params != (store.ModelParams{}) {
		t.Fatalf("initial defaults = %+v (%v), want an empty document at revision 0", empty, err)
	}
	temperature := 0.3
	params := store.ModelParams{
		MaxTokens: 8192, ContextWindow: 128000, Temperature: &temperature,
		MaxConcurrency: 4, RPM: 60, StrictRoleAlternation: true,
	}
	saved, err := s.PutModelDefaults(ctx, params, 0, 7)
	if err != nil || saved.Revision != 1 || saved.UpdatedBy != 7 || saved.Params.MaxTokens != 8192 {
		t.Fatalf("create defaults = %+v (%v)", saved, err)
	}
	if _, err := s.PutModelDefaults(ctx, store.ModelParams{MaxTokens: 1}, 0, 7); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale put = %v, want ErrStaleRevision", err)
	}
	if _, err := s.DeleteModelDefaults(ctx, 0, 7); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale delete = %v, want ErrStaleRevision", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A restart must keep the defaults: the row is the only source of truth.
	s, err = Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.GetModelDefaults(ctx)
	if err != nil || reloaded.Revision != 1 || reloaded.Params.MaxTokens != 8192 ||
		reloaded.Params.Temperature == nil || *reloaded.Params.Temperature != 0.3 || !reloaded.Params.StrictRoleAlternation {
		t.Fatalf("reloaded defaults = %+v (%v)", reloaded, err)
	}
	updated, err := s.PutModelDefaults(ctx, store.ModelParams{RPM: 30}, 1, 9)
	if err != nil || updated.Revision != 2 || updated.Params.RPM != 30 || updated.Params.MaxTokens != 0 {
		t.Fatalf("replace defaults = %+v (%v)", updated, err)
	}
	reset, err := s.DeleteModelDefaults(ctx, 2, 9)
	if err != nil || reset.Revision != 3 || reset.Params != (store.ModelParams{}) || reset.UpdatedBy != 9 {
		t.Fatalf("reset defaults = %+v (%v)", reset, err)
	}
}

// A model may rely on the platform defaults for a protocol-required value.
// Validation merges the defaults but the stored record keeps only explicit
// values, so a later defaults change still reaches the model.
func TestModelValidationUsesMergedDefaults(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if err := s.ConfigureModelSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	input := store.Model{DisplayName: "claude", Protocol: "anthropic", BaseURL: "https://example.test", Model: "claude-x", CreatedBy: 1}
	if _, err := s.CreateModel(ctx, input, ""); err == nil {
		t.Fatal("anthropic model without max_tokens and without defaults must fail")
	}
	if _, err := s.PutModelDefaults(ctx, store.ModelParams{MaxTokens: 4096}, 0, 1); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateModel(ctx, input, "")
	if err != nil {
		t.Fatalf("create with defaults: %v", err)
	}
	if created.Params.MaxTokens != 0 {
		t.Fatalf("stored max_tokens = %d, want the explicit zero (unset)", created.Params.MaxTokens)
	}
	// A parameter-level update must validate the merged result too: clearing
	// the explicit value keeps the record valid through the default.
	created.Params.MaxTokens = 0
	if _, err := s.UpdateModel(ctx, *created, created.Revision, nil); err != nil {
		t.Fatalf("update relying on defaults: %v", err)
	}
	if _, err := s.PutModelDefaults(ctx, store.ModelParams{}, 1, 1); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetModel(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateModel(ctx, *stored, stored.Revision, nil); err == nil {
		t.Fatal("removing the default must make the anthropic model invalid")
	}
}

// 0029 must apply to a database written by 0028 and leave existing models,
// token usage columns and rows untouched.
func TestModelDefaultsMigrationFromV28(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-defaults.db")
	legacy, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	stamp := "2026-11-01T10:00:00.000000000Z"
	for _, name := range migrationNamesBefore(t, 29) {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`, migrationVersionForTest(t, name), name, stamp); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc, webhook_secret_enc, created_at, updated_at)
		VALUES (7, 'group/defaults', 'https://gitlab.example.com/group/defaults', 'main', x'00', x'01', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO workflows
		(id, kind, project_id, trigger, status, graph_rev, tokens_in, tokens_out, cost_micros, usage_incomplete, created_at)
		VALUES ('wf-defaults', 'review', 7, 'mr_open', 'completed', 1, 17, 5, 3, 1, ?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO llm_models
		(id, display_name, protocol, base_url, model, params_json, revision, created_by, created_at, updated_by, updated_at)
		VALUES ('user:existing', 'existing', 'openai-chat', 'https://example.test', 'm', '{"max_tokens":1024}', 1, 1, ?, 1, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open after 0029: %v", err)
	}
	defer s.Close()
	defaults, err := s.GetModelDefaults(ctx)
	if err != nil || defaults.Revision != 0 || defaults.Params != (store.ModelParams{}) {
		t.Fatalf("migrated defaults = %+v (%v), want empty", defaults, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations WHERE version=29`); got != 1 {
		t.Fatal("0029 not recorded")
	}
	models, err := s.ListModels(ctx)
	if err != nil || len(models) != 1 || models[0].Params.MaxTokens != 1024 {
		t.Fatalf("existing model lost: %+v (%v)", models, err)
	}
	var tokensIn, tokensOut, usageIncomplete int
	if err := s.db.QueryRow(`SELECT tokens_in, tokens_out, usage_incomplete FROM workflows WHERE id='wf-defaults'`).Scan(&tokensIn, &tokensOut, &usageIncomplete); err != nil {
		t.Fatal(err)
	}
	if tokensIn != 17 || tokensOut != 5 || usageIncomplete != 1 {
		t.Fatalf("token usage changed: in=%d out=%d incomplete=%d", tokensIn, tokensOut, usageIncomplete)
	}
}
