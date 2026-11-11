package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func writeConfigFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

const configEndpointBase = `
gitlab:
  url: "https://gitlab.example.com"
server:
  listen: ":9090"
storage:
  driver: sqlite
  dsn: "file:/srv/private/review.db?_pragma=password(hunter2)"
secrets:
  master_key_env: REVIEW_MASTER_KEY
auth:
  bootstrap_token_env: REVIEW_ADMIN_TOKEN
  oauth:
    client_id_env: GITLAB_CLIENT_ID
    client_secret_env: GITLAB_CLIENT_SECRET
workspace:
  root: "/srv/private/workspaces"
  git_bin: "/usr/bin/git"
mcp:
  config: "/etc/tribunal/mcp.json"
skills:
  dirs: ["/srv/private/skills"]
prompts_dir: "/srv/private/prompts"
providers:
  claude:
    protocol: anthropic
    model: claude-sonnet
    api_key_env: ANTHROPIC_API_KEY
    max_tokens: 16000
    price:
      input_per_mtok: 1
      output_per_mtok: 2
jobs:
  workers: 2
`

func loadConfigSnapshot(t *testing.T, overlayBody string) *config.Snapshot {
	t.Helper()
	base := writeConfigFile(t, "server.yaml", configEndpointBase)
	overlay := ""
	if overlayBody != "" {
		overlay = writeConfigFile(t, "server.overlay.yaml", overlayBody)
	}
	snapshot, err := config.LoadSnapshot([]string{base}, overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot() error: %v", err)
	}
	return snapshot
}

func TestConfigEndpointsRequirePlatformAdmin(t *testing.T) {
	env := newM6Env(t, m6Options{})
	anonymous := env.anonymousClient()
	assertStatus(t, anonymous.request(http.MethodGet, "/api/v1/config", nil, nil), http.StatusUnauthorized)
	viewer := env.loginAs("code-viewer")
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/config", nil, nil), http.StatusForbidden)
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/config/status", nil, nil), http.StatusForbidden)

	// Without a wired display snapshot the admin route fails closed.
	admin := env.loginAs("code-admin")
	assertStatus(t, admin.request(http.MethodGet, "/api/v1/config", nil, nil), http.StatusServiceUnavailable)
	assertStatus(t, admin.request(http.MethodGet, "/api/v1/config/status", nil, nil), http.StatusServiceUnavailable)
}

func TestConfigViewShowsFileEffectiveValuesAndOrigins(t *testing.T) {
	snapshot := loadConfigSnapshot(t, "server:\n  write_timeout: 30s\njobs:\n  workers: 3\n")
	env := newM6Env(t, m6Options{platform: snapshot.Config})
	env.api.SetConfigState(&config.State{Snapshot: snapshot, UpdatedAt: time.Now()})
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet, "/api/v1/config", nil, nil)
	assertStatus(t, response, http.StatusOK)
	text := string(response.body)
	for _, forbidden := range []string{
		"REVIEW_MASTER_KEY", "REVIEW_ADMIN_TOKEN", "ANTHROPIC_API_KEY",
		"GITLAB_CLIENT_ID", "GITLAB_CLIENT_SECRET", "hunter2",
		"/srv/private", "/etc/tribunal", "file:", "api_key_env", "master_key_env",
		"bootstrap_token_env", "client_secret_env", "prompts_dir",
		`"price"`, "input_per_mtok", "output_per_mtok",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("config response leaked %q: %s", forbidden, text)
		}
	}

	body := env.decode(t, response)
	effective := body["config"].(map[string]any)
	serverView := effective["server"].(map[string]any)
	if serverView["listen"] != ":9090" || serverView["write_timeout"] != float64(30*time.Second) {
		t.Fatalf("server view = %v", serverView)
	}
	jobsView := effective["jobs"].(map[string]any)
	if jobsView["workers"] != float64(3) {
		t.Fatalf("jobs view = %v", jobsView)
	}
	storageView := effective["storage"].(map[string]any)
	if _, ok := storageView["dsn"]; ok {
		t.Fatalf("storage view leaked the DSN field: %v", storageView)
	}
	providers := effective["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("providers = %v", providers)
	}
	provider := providers[0].(map[string]any)
	if provider["name"] != "claude" || provider["api_key_configured"] != true {
		t.Fatalf("provider view = %v", provider)
	}

	origins := body["origins"].(map[string]any)
	for path, want := range map[string]string{
		"server.listen":                    "base",
		"server.read_timeout":              "default",
		"server.write_timeout":             "overlay",
		"jobs.workers":                     "overlay",
		"providers.claude.model":           "base",
		"providers.claude.max_concurrency": "default",
	} {
		if origins[path] != want {
			t.Errorf("origin[%s] = %v, want %s", path, origins[path], want)
		}
	}
	for _, hidden := range []string{"storage.dsn", "workspace.root", "workspace.git_bin", "secrets.master_key_env",
		"auth.bootstrap_token_env", "auth.oauth.client_id_env", "auth.oauth.client_secret_env",
		"mcp.config", "skills.dirs", "skills.dirs[0]", "prompts_dir", "providers.claude.api_key_env",
		"providers.claude.price", "providers.claude.price.input_per_mtok"} {
		if _, ok := origins[hidden]; ok {
			t.Errorf("origins exposed %s", hidden)
		}
	}

	if body["applied"] != true || body["restart_required"] != false {
		t.Fatalf("applied/restart_required = %v/%v", body["applied"], body["restart_required"])
	}
	if body["overlay_present"] != true {
		t.Fatalf("overlay_present = %v", body["overlay_present"])
	}
	if body["base_revision"] == "" || body["overlay_revision"] == "" {
		t.Fatalf("revisions = %v/%v", body["base_revision"], body["overlay_revision"])
	}
	if warnings, ok := body["warnings"].([]any); !ok || len(warnings) != 0 {
		t.Fatalf("warnings = %v, want []", body["warnings"])
	}
}

func TestConfigStatusReportsPendingRestartAndLastError(t *testing.T) {
	snapshot := loadConfigSnapshot(t, "")
	env := newM6Env(t, m6Options{platform: snapshot.Config})
	lastErrorAt := time.Now().Add(-time.Minute)
	updatedAt := time.Now().Add(-time.Hour)
	env.api.SetConfigState(&config.State{
		Snapshot:    snapshot,
		LastError:   "重新加载失败：配置已回退到上一份可用快照",
		LastErrorAt: lastErrorAt,
		UpdatedAt:   updatedAt,
	})
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet, "/api/v1/config/status", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if body["applied"] != true || body["restart_required"] != false {
		t.Fatalf("applied/restart_required = %v/%v", body["applied"], body["restart_required"])
	}
	if body["last_error"] != "重新加载失败：配置已回退到上一份可用快照" {
		t.Fatalf("last_error = %v", body["last_error"])
	}
	if body["last_error_at"] == nil || body["updated_at"] == nil {
		t.Fatalf("timestamps = %v/%v", body["last_error_at"], body["updated_at"])
	}
	if warnings, ok := body["warnings"].([]any); !ok || len(warnings) != 0 {
		t.Fatalf("warnings = %v, want []", body["warnings"])
	}
	fileResponse := client.request(http.MethodGet, "/api/v1/config", nil, nil)
	assertStatus(t, fileResponse, http.StatusOK)
	fileBody := env.decode(t, fileResponse)
	if fileBody["last_error"] != body["last_error"] || fileBody["last_error_at"] == nil {
		t.Fatalf("config last_error = %v/%v, want %v/not-null", fileBody["last_error"], fileBody["last_error_at"], body["last_error"])
	}

	// A reloaded display snapshot that differs from the runtime config needs a
	// restart; the runtime config itself is not replaced.
	changed := loadConfigSnapshot(t, "server:\n  listen: \":9191\"\n")
	env.api.SetConfigState(&config.State{Snapshot: changed, UpdatedAt: time.Now()})
	response = client.request(http.MethodGet, "/api/v1/config/status", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body = env.decode(t, response)
	if body["applied"] != false || body["restart_required"] != true {
		t.Fatalf("pending restart = applied %v, restart_required %v", body["applied"], body["restart_required"])
	}
	if body["last_error"] != "" {
		t.Fatalf("last_error was not cleared: %v", body["last_error"])
	}
	if body["last_error_at"] != nil {
		t.Fatalf("last_error_at = %v, want null", body["last_error_at"])
	}

	response = client.request(http.MethodGet, "/api/v1/config", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body = env.decode(t, response)
	if body["restart_required"] != true {
		t.Fatalf("config restart_required = %v", body["restart_required"])
	}
	serverView := body["config"].(map[string]any)["server"].(map[string]any)
	if serverView["listen"] != ":9191" {
		t.Fatalf("config did not show the reloaded value: %v", serverView)
	}
}
