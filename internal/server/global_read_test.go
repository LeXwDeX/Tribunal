package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestGlobalReadAdministration(t *testing.T) {
	env, fake := isolationEnv(t)
	seedIsolationWorkflow(env, fake, "wf-global", 42)
	var logs bytes.Buffer
	env.api.logger = newTestLogger(&logs)
	admin := env.loginAs("code-admin")
	viewer := env.loginAs("code-viewer")
	path := "/api/v1/admin/users/8"
	payload := map[string]any{"global_read": true, "revision": 1}
	assertStatus(t, viewer.json(http.MethodPatch, path, payload), http.StatusForbidden)
	assertStatus(t, admin.withoutCSRF().json(http.MethodPatch, path, payload), http.StatusForbidden)
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/projects/42", nil, nil), http.StatusNotFound)
	assertStatus(t, admin.json(http.MethodPatch, path, payload), http.StatusOK)
	if got := env.decode(t, viewer.request(http.MethodGet, "/api/v1/me", nil, nil))["global_read"]; got != true {
		t.Fatalf("global_read = %v, want true", got)
	}
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/projects/42", nil, nil), http.StatusOK)
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/reviews/wf-global", nil, nil), http.StatusOK)
	registerProjectRow(t, env, 44, "group/new")
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/projects/44", nil, nil), http.StatusOK)
	viewer.csrf = env.decode(t, viewer.request(http.MethodGet, "/api/v1/me", nil, nil))["csrf_token"].(string)
	assertStatus(t, viewer.json(http.MethodPost, "/api/v1/projects/42/merge_requests/7/reviews", nil), http.StatusForbidden)
	assertStatus(t, admin.json(http.MethodPatch, path, payload), http.StatusConflict)
	assertStatus(t, admin.json(http.MethodPatch, path, map[string]any{"disabled": true, "revision": 2}), http.StatusOK)
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/projects/42", nil, nil), http.StatusUnauthorized)
	assertStatus(t, admin.json(http.MethodPatch, path, map[string]any{"disabled": false, "revision": 3}), http.StatusOK)
	viewer = env.loginAs("code-viewer")
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/projects/42", nil, nil), http.StatusOK)
	assertStatus(t, admin.json(http.MethodPatch, path, map[string]any{"global_read": false, "revision": 4}), http.StatusOK)
	assertStatus(t, viewer.request(http.MethodGet, "/api/v1/projects/42", nil, nil), http.StatusNotFound)
	for _, part := range []string{`"event":"platform_audit"`, `"actor_id":7`, `"target_id":8`, `"global_read":true`, `"global_read":false`} {
		if !strings.Contains(logs.String(), part) {
			t.Fatalf("audit missing %s", part)
		}
	}
}

func TestAdministratorCanEnableOwnGlobalRead(t *testing.T) {
	env, _ := isolationEnv(t)
	admin := env.loginAs("code-admin")
	assertStatus(t, admin.request(http.MethodGet, "/api/v1/projects/42", nil, nil), http.StatusNotFound)
	assertStatus(t, admin.json(http.MethodPatch, "/api/v1/admin/users/7", map[string]any{"global_read": true, "revision": 1}), http.StatusOK)
	assertStatus(t, admin.request(http.MethodGet, "/api/v1/projects/42", nil, nil), http.StatusOK)
	if got := env.decode(t, admin.request(http.MethodGet, "/api/v1/me", nil, nil))["global_read"]; got != true {
		t.Fatalf("admin global_read = %v, want true", got)
	}
}
