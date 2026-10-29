package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func newRestoreHTTPEnv(t *testing.T, cfg *config.Config) (*m6Env, *sqlite.Store, *app.ExtensionService) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cipher, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfigureMCPSecrets(ctx, cipher); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewExtensionService(app.ExtensionServiceConfig{
		Store:     st,
		Workspace: workspace.NewManager(t.TempDir(), nil),
		Platform:  cfg,
		Logger:    newTestLogger(io.Discard),
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	backend := NewAppExtensions(service)
	gitlab := newFakeGitLab(t)
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   stubAdmin{},
		Webhooks:   stubWebhookAcceptor{},
		Database:   st,
		Worker:     &app.Lifecycle{},
		Logger:     newTestLogger(io.Discard),
		Auth: AuthConfig{
			GitLabURL:       gitlab.URL,
			ClientID:        testOAuthClientID,
			ClientSecret:    testOAuthClientSecret,
			RedirectURL:     testOAuthRedirectURL,
			SessionKey:      []byte("0123456789abcdef0123456789abcdef"),
			SessionTTL:      time.Hour,
			BootstrapAdmins: []string{"alice"},
			HTTPClient:      gitlab.Client(),
			Now:             time.Now,
		},
		Store:      st,
		Sessions:   st,
		Tokens:     st,
		Access:     st,
		Extensions: backend,
		Platform:   cfg,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	server := httptest.NewServer(apiServer.Handler())
	t.Cleanup(func() {
		server.Close()
		gitlab.Close()
	})
	return &m6Env{t: t, server: server, gitlab: gitlab, sqlite: st}, st, service
}

func applyRestoreDefaults(t *testing.T, service *app.ExtensionService, st *sqlite.Store, cfg *config.Config) {
	t.Helper()
	defaults, err := service.SkillDefaults(cfg.DefaultExtensions.Skills)
	if err != nil {
		t.Fatalf("SkillDefaults: %v", err)
	}
	if err := app.ApplyExtensionDefaults(context.Background(), st, cfg.Blocks, defaults, nil); err != nil {
		t.Fatalf("ApplyExtensionDefaults: %v", err)
	}
}

func restoreCfg() *config.Config {
	return &config.Config{DefaultExtensions: config.DefaultExtensionsConfig{
		Skills: []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review", "review"}}},
	}}
}

func TestRestoreDefaultsRequiresPlatformAdminAndCSRF(t *testing.T) {
	env, _, _ := newRestoreHTTPEnv(t, restoreCfg())
	body := map[string]any{"blocks": []string{"review"}, "dry_run": true}

	anonymous := env.anonymousClient().json(http.MethodPost, "/api/v1/block-bindings/restore-defaults", body)
	assertStatus(t, anonymous, http.StatusUnauthorized)

	viewer := env.loginAs("code-viewer")
	assertStatus(t, viewer.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults", body), http.StatusForbidden)

	admin := env.loginAs("code-admin")
	assertStatus(t, admin.withoutCSRF().json(http.MethodPost, "/api/v1/block-bindings/restore-defaults", body), http.StatusForbidden)

	allowed := admin.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults", body)
	assertStatus(t, allowed, http.StatusOK)

	bearer := env.bearerClient()
	assertStatus(t, bearer.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults", body), http.StatusOK)
}

func TestRestoreDefaultsHTTPPreviewAndExecute(t *testing.T) {
	ctx := context.Background()
	cfg := restoreCfg()
	env, st, service := newRestoreHTTPEnv(t, cfg)
	applyRestoreDefaults(t, service, st, cfg)
	if _, err := service.UpsertBlockBinding(ctx, app.BlockBindingInput{
		Scope: "platform", Block: "review",
	}); err != nil {
		t.Fatal(err)
	}
	client := env.bearerClient()

	preview := client.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults", map[string]any{
		"blocks": []string{"review"}, "dry_run": true,
	})
	assertStatus(t, preview, http.StatusOK)
	body := decodeMap(t, preview.body)
	if body["restart_required"] != true {
		t.Fatalf("restart_required = %v", body["restart_required"])
	}
	token, _ := body["preview_token"].(string)
	if token == "" {
		t.Fatal("preview_token is empty")
	}
	changes := body["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("changes = %v", changes)
	}
	change := changes[0].(map[string]any)
	if change["block"] != "review" || change["status"] != "change" {
		t.Fatalf("change = %v", change)
	}
	if got := change["add"].(map[string]any)["skills"]; !reflect.DeepEqual(got, []any{"code-review"}) {
		t.Fatalf("add.skills = %v", got)
	}
	if got := platformBindingSkills(t, st, "review"); len(got) != 0 {
		t.Fatalf("dry-run wrote the binding: %v", got)
	}

	execute := client.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults", map[string]any{
		"blocks": []string{"review"}, "dry_run": false, "preview_token": token,
	})
	assertStatus(t, execute, http.StatusOK)
	if got := platformBindingSkills(t, st, "review"); !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("review skills after execution = %v", got)
	}
}

func TestRestoreDefaultsHTTPValidationAndConflict(t *testing.T) {
	ctx := context.Background()
	cfg := restoreCfg()
	env, st, service := newRestoreHTTPEnv(t, cfg)
	applyRestoreDefaults(t, service, st, cfg)
	client := env.bearerClient()
	path := "/api/v1/block-bindings/restore-defaults"

	assertStatus(t, client.json(http.MethodPost, path, map[string]any{
		"blocks": []string{"nope"}, "dry_run": true,
	}), http.StatusBadRequest)
	assertStatus(t, client.json(http.MethodPost, path, map[string]any{
		"blocks": []string{"review"},
	}), http.StatusBadRequest)
	assertStatus(t, client.json(http.MethodPost, path, map[string]any{
		"blocks": []string{"review"}, "dry_run": false,
	}), http.StatusBadRequest)
	assertStatus(t, client.json(http.MethodPost, path, map[string]any{
		"blocks": []string{"review"}, "dry_run": true, "grants": map[string]any{"skills": []string{"code-review"}},
	}), http.StatusBadRequest)

	preview := client.json(http.MethodPost, path, map[string]any{"blocks": []string{"review"}, "dry_run": true})
	assertStatus(t, preview, http.StatusOK)
	token := decodeMap(t, preview.body)["preview_token"].(string)
	if _, err := service.UpsertBlockBinding(ctx, app.BlockBindingInput{
		Scope: "platform", Block: "review", Skills: []string{"team-rules"},
	}); err != nil {
		t.Fatal(err)
	}
	stale := client.json(http.MethodPost, path, map[string]any{
		"blocks": []string{"review"}, "dry_run": false, "preview_token": token,
	})
	assertStatus(t, stale, http.StatusConflict)
	if got := platformBindingSkills(t, st, "review"); !reflect.DeepEqual(got, []string{"team-rules"}) {
		t.Fatalf("conflict overwrote the binding: %v", got)
	}
}

func TestRestoreDefaultsHTTPAllBlocksAreSorted(t *testing.T) {
	env, _, _ := newRestoreHTTPEnv(t, restoreCfg())
	client := env.bearerClient()
	response := client.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults", map[string]any{"dry_run": true})
	assertStatus(t, response, http.StatusOK)
	changes := decodeMap(t, response.body)["changes"].([]any)
	if len(changes) != len(config.BindableBlocks) {
		t.Fatalf("changes = %v", changes)
	}
	for i := 1; i < len(changes); i++ {
		before := changes[i-1].(map[string]any)["block"].(string)
		after := changes[i].(map[string]any)["block"].(string)
		if before >= after {
			t.Fatalf("changes are not sorted: %v", changes)
		}
	}
}

func platformBindingSkills(t *testing.T, st *sqlite.Store, block string) []string {
	t.Helper()
	rows, err := st.ListBlockBindings(context.Background(), store.ScopePlatform, nil)
	if err != nil {
		t.Fatalf("ListBlockBindings: %v", err)
	}
	for _, row := range rows {
		if row.Block == block {
			return row.Skills
		}
	}
	return nil
}
