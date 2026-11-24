package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	workspacepkg "github.com/LeXwDeX/tribunal/internal/workspace"
)

// credentialProjectService builds a project service whose credential health is
// pre-populated, so the settings view can be checked without a GitLab probe.
func credentialProjectService(t *testing.T, st *sqlite.Store, registry *app.CredentialHealthRegistry) AdminService {
	t.Helper()
	cipher, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	service, err := app.NewProjectService(app.ProjectServiceConfig{
		Store:       st,
		GitLab:      defaultStubGitLab(),
		Cipher:      cipher,
		Profiles:    map[string]config.ProfileConfig{"default": {}, "deep": {}},
		PublicURL:   "https://review.example.com",
		Workspace:   workspacepkg.NewManager(t.TempDir(), nil),
		GitLabURL:   "https://gitlab.example.com",
		Credentials: registry,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func TestSettingsExposeCredentialHealth(t *testing.T) {
	registry := app.NewCredentialHealthRegistry(nil)
	registry.RecordRejection(42, false, "project token was rejected by GitLab (HTTP 401); replace the project token")
	expiry := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	registry.RecordOutcome(7, false, app.CredentialStateOK, "", &expiry)

	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return credentialProjectService(t, st, registry)
	}})
	admin := env.loginAs("code-admin")

	response := admin.request(http.MethodGet, "/api/v1/settings", nil, nil)
	assertStatus(t, response, http.StatusOK)
	gitlabView, _ := env.decode(t, response)["gitlab"].(map[string]any)
	health, ok := gitlabView["credential_health"].(map[string]any)
	if !ok {
		t.Fatalf("settings has no credential health: %v", gitlabView)
	}
	shared, _ := health["shared"].(map[string]any)
	if shared["state"] != "unknown" {
		t.Fatalf("shared credential health = %v, want untouched unknown", shared)
	}
	projects, _ := health["projects"].([]any)
	if len(projects) != 2 {
		t.Fatalf("credential health projects = %v", health)
	}
	byID := map[float64]map[string]any{}
	for _, entry := range projects {
		view, _ := entry.(map[string]any)
		byID[view["project_id"].(float64)] = view
	}
	rejected := byID[42]
	if rejected["state"] != "rejected" || rejected["failures"] != float64(1) {
		t.Fatalf("project 42 health = %v", rejected)
	}
	if rejected["last_error"] != "project token was rejected by GitLab (HTTP 401); replace the project token" {
		t.Fatalf("project 42 last error = %v", rejected["last_error"])
	}
	healthy := byID[7]
	if healthy["state"] != "ok" || healthy["source"] != "project" {
		t.Fatalf("project 7 health = %v", healthy)
	}
	if healthy["token_expires_at"] != expiry.Format(time.RFC3339) {
		t.Fatalf("project 7 expiry = %v", healthy["token_expires_at"])
	}
}
