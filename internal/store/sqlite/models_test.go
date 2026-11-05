package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func TestModelCRUDAndCredentialIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if err := s.ConfigureModelSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	input := store.Model{DisplayName: "same name", Protocol: "openai-chat", Model: "example", Params: store.ModelParams{MaxTokens: 2, MaxConcurrency: 1}, CreatedBy: 42}
	model, err := s.CreateModel(ctx, input, "test-secret-canary")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(model.ID, "user:") || model.Revision != 1 || !model.HasAPIKey {
		t.Fatalf("invalid model: %+v", model)
	}
	other, err := s.CreateModel(ctx, input, "")
	if err != nil || other.ID == model.ID {
		t.Fatal("duplicate display name must get a distinct id")
	}
	var ciphertext []byte
	if err := s.db.QueryRowContext(ctx, `SELECT api_key_sealed FROM llm_models WHERE id = ?`, model.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListModels(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v", err)
	}
	got, err := s.GetModel(ctx, model.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fmt.Sprintf("%v", model), fmt.Sprintf("%+v", model), fmt.Sprintf("%+v", got), fmt.Sprintf("%+v", list)} {
		if strings.Contains(value, "test-secret-canary") || strings.Contains(value, string(ciphertext)) || strings.Contains(value, fmt.Sprintf("%v", ciphertext)) {
			t.Fatal("credential leaked")
		}
	}
	serialized, err := json.Marshal(list)
	if err != nil || strings.Contains(string(serialized), "test-secret-canary") || strings.Contains(string(serialized), string(ciphertext)) {
		t.Fatal("credential leaked in JSON")
	}
	plain, err := s.OpenModelCredential(ctx, model.ID)
	if err != nil || string(plain) != "test-secret-canary" {
		t.Fatal("credential did not open")
	}
	model.DisplayName = "changed"
	if _, err := s.UpdateModel(ctx, *model, model.Revision+1, nil); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale update: %v", err)
	}
	model, err = s.UpdateModel(ctx, *model, model.Revision, nil)
	if err != nil || model.Revision != 2 {
		t.Fatalf("update: %v", err)
	}
	plain, err = s.OpenModelCredential(ctx, model.ID)
	if err != nil || string(plain) != "test-secret-canary" {
		t.Fatal("update did not preserve credential")
	}
	if err := s.SoftDeleteModel(ctx, model.ID, 1, 42); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := s.SoftDeleteModel(ctx, model.ID, model.Revision, 42); err != nil {
		t.Fatal(err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM llm_models WHERE id = ? AND api_key_sealed IS NULL AND deleted_at IS NOT NULL`, model.ID); got != 1 {
		t.Fatal("deleted row or credential state incorrect")
	}
}

func TestModelsMigrationFromV120(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range migrationNamesBefore(t, 25) {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?, ?, ?)`, migrationVersionForTest(t, name), name, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, name, role) VALUES (123, 'existing', 'Existing', 'viewer')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := rowCount(t, s, `SELECT COUNT(*) FROM users WHERE id=123 AND username='existing'`); got != 1 {
		t.Fatal("existing user lost")
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations WHERE version=26`); got != 1 {
		t.Fatal("migration not recorded")
	}
	if models, err := s.ListModels(ctx); err != nil || len(models) != 0 {
		t.Fatalf("new model table: %v", err)
	}
}

func TestConcurrentModelUpdatesUseRevision(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if err := s.ConfigureModelSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateModel(ctx, store.Model{DisplayName: "model", Protocol: "openai-chat", Model: "m", Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}, CreatedBy: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.UpdateModel(ctx, *m, 1, nil); errs <- err }()
	}
	wg.Wait()
	close(errs)
	success, stale := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrStaleRevision) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success=%d stale=%d", success, stale)
	}
}

func TestModelWrongKeyIsUnavailable(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	path := filepath.Join(t.TempDir(), "models.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			_ = s.Close()
		}
	})
	if err := s.ConfigureModelSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateModel(ctx, store.Model{DisplayName: "model", Protocol: "openai-chat", Model: "m", Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}, CreatedBy: 1}, "credential")
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := s.db.QueryRowContext(ctx, `SELECT api_key_sealed FROM llm_models WHERE id = ?`, m.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureModelSecrets(ctx, sealTestCipher(t, 2)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetModel(ctx, m.ID)
	if err != nil || !got.Unavailable {
		t.Fatal("model with wrong key must be unavailable")
	}
	if strings.Contains(logs.String(), "credential") || strings.Contains(logs.String(), string(sealed)) || strings.Contains(logs.String(), fmt.Sprintf("%v", sealed)) {
		t.Fatal("model credential leaked to logs")
	}
}

func TestCreateModelRollsBackWhenReadBackFails(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if err := s.ConfigureModelSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER break_model AFTER INSERT ON llm_models BEGIN UPDATE llm_models SET params_json='not-json' WHERE id=NEW.id; END`); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateModel(ctx, store.Model{DisplayName: "model", Protocol: "openai-chat", Model: "m", Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}, CreatedBy: 1}, "")
	if err == nil {
		t.Fatal("create with corrupt read-back must fail")
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM llm_models`); got != 0 {
		t.Fatalf("rollback left %d rows", got)
	}
}
