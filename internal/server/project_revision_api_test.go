package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// TestProjectUpdateRevisionFence covers the #517 API contract: a conditional
// PATCH must not overwrite a newer settings write, a stale revision answers 409
// stale_revision, and a PATCH without revision keeps the pre-change semantics.
func TestProjectUpdateRevisionFence(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	client := env.loginAs("code-admin")
	registerProject(t, client)
	env.grant(7, 42, store.ProjectAccessManage)

	detail := client.json(http.MethodGet, "/api/v1/projects/42", nil)
	assertStatus(t, detail, http.StatusOK)
	revision, ok := env.decode(t, detail)["revision"].(float64)
	if !ok || revision < 1 {
		t.Fatalf("project revision = %v, want a positive number", env.decode(t, detail)["revision"])
	}

	saved := client.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{
		"settings": map[string]any{"skip_draft": true},
		"revision": revision,
	})
	assertStatus(t, saved, http.StatusOK)
	if got := env.decode(t, saved)["revision"].(float64); got != revision+1 {
		t.Fatalf("saved revision = %v, want %v", got, revision+1)
	}

	stale := client.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{
		"settings": map[string]any{"skip_draft": false},
		"revision": revision,
	})
	assertStatus(t, stale, http.StatusConflict)
	if !strings.Contains(string(stale.body), `"code":"stale_revision"`) {
		t.Fatalf("stale response = %s, want code stale_revision", stale.body)
	}

	current := client.json(http.MethodGet, "/api/v1/projects/42", nil)
	assertStatus(t, current, http.StatusOK)
	settings, ok := env.decode(t, current)["settings"].(map[string]any)
	if !ok || settings["skip_draft"] != true {
		t.Fatalf("settings after stale write = %v, want the previous save", settings)
	}

	legacy := client.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{"enabled": false})
	assertStatus(t, legacy, http.StatusOK)
}
