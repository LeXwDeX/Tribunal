package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestPlatformRoleAdministration(t *testing.T) {
	env := newM6Env(t, m6Options{})
	var logs bytes.Buffer
	env.api.logger = newTestLogger(&logs)
	alice := env.loginAs("code-admin")
	bob := env.loginAs("code-viewer")
	env.grant(8, 42, "read")
	path := "/api/v1/admin/users/8"
	users := env.decode(t, alice.request(http.MethodGet, "/api/v1/admin/users", nil, nil))["users"].([]any)
	var bobUser map[string]any
	for _, item := range users {
		u := item.(map[string]any)
		if u["id"] == float64(8) {
			bobUser = u
		}
		if u["id"] == float64(7) && (u["role"] != "admin" || u["role_source"] != "config") {
			t.Fatalf("configured admin = %#v", u)
		}
	}
	if bobUser == nil || bobUser["role"] != "viewer" || bobUser["role_source"] != "none" || bobUser["global_read"] != false || bobUser["grant_count"] != float64(1) || bobUser["revision"] == nil {
		t.Fatalf("unexpected user: %#v", bobUser)
	}
	revision := bobUser["revision"]
	assertStatus(t, bob.request(http.MethodGet, "/api/v1/admin/users", nil, nil), http.StatusForbidden)
	assertStatus(t, alice.json(http.MethodPatch, path, map[string]any{"disabled": true}), http.StatusBadRequest)
	assertStatus(t, alice.withoutCSRF().json(http.MethodPatch, path, map[string]any{"role": "admin", "revision": revision}), http.StatusForbidden)
	assertStatus(t, alice.json(http.MethodPatch, path, map[string]any{"role": "admin", "revision": revision}), http.StatusOK)
	assertStatus(t, bob.request(http.MethodGet, "/api/v1/admin/users", nil, nil), http.StatusOK)
	// Promotion invalidates the old CSRF token. The session obtains a new one.
	assertStatus(t, bob.json(http.MethodPatch, path, map[string]any{"disabled": true, "revision": 2}), http.StatusForbidden)
	bob.csrf = env.decode(t, bob.request(http.MethodGet, "/api/v1/me", nil, nil))["csrf_token"].(string)
	assertStatus(t, bob.json(http.MethodPatch, path, map[string]any{"disabled": true, "revision": 2}), http.StatusForbidden)
	assertStatus(t, bob.json(http.MethodPatch, path, map[string]any{"role": "viewer", "revision": 2}), http.StatusForbidden)
	created := bob.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "role-test"})
	assertStatus(t, created, http.StatusCreated)
	token := env.decode(t, created)["token"].(string)
	tokenClient := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: token}
	assertStatus(t, tokenClient.request(http.MethodGet, "/api/v1/admin/users", nil, nil), http.StatusOK)
	otherSession := env.loginAs("code-viewer")
	assertStatus(t, otherSession.request(http.MethodGet, "/api/v1/admin/users", nil, nil), http.StatusOK)
	assertStatus(t, alice.json(http.MethodPatch, "/api/v1/admin/users/7", map[string]any{"disabled": true, "revision": 1}), http.StatusForbidden)
	assertStatus(t, alice.json(http.MethodPatch, "/api/v1/admin/users/7", map[string]any{"role": "viewer", "revision": 1}), http.StatusForbidden)
	assertStatus(t, env.bearerClient().json(http.MethodPatch, "/api/v1/admin/users/7", map[string]any{"disabled": true, "revision": 1}), http.StatusForbidden)
	assertStatus(t, env.bearerClient().json(http.MethodPatch, "/api/v1/admin/users/7", map[string]any{"role": "viewer", "revision": 1}), http.StatusForbidden)
	stale := alice.json(http.MethodPatch, path, map[string]any{"role": "viewer", "revision": revision})
	assertStatus(t, stale, http.StatusConflict)
	if !strings.Contains(string(stale.body), `"code":"stale_revision"`) {
		t.Fatal("missing stale revision error code")
	}
	assertStatus(t, alice.json(http.MethodPatch, path, map[string]any{"disabled": true, "revision": revision}), http.StatusConflict)
	list := alice.request(http.MethodGet, "/api/v1/admin/users/8/grants", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if grants := env.decode(t, list)["grants"].([]any); len(grants) != 1 {
		t.Fatalf("grants = %v", grants)
	}
	assertStatus(t, alice.request(http.MethodGet, "/api/v1/admin/users/999/grants", nil, nil), http.StatusNotFound)
	users = env.decode(t, alice.request(http.MethodGet, "/api/v1/admin/users", nil, nil))["users"].([]any)
	for _, item := range users {
		u := item.(map[string]any)
		if u["id"] == float64(8) {
			if u["role"] != "admin" || u["role_source"] != "granted" {
				t.Fatalf("granted admin = %#v", u)
			}
			revision = u["revision"]
		}
	}
	assertStatus(t, alice.json(http.MethodPatch, path, map[string]any{"role": "viewer", "revision": revision}), http.StatusOK)
	assertStatus(t, bob.request(http.MethodGet, "/api/v1/admin/users", nil, nil), http.StatusForbidden)
	assertStatus(t, otherSession.request(http.MethodGet, "/api/v1/admin/users", nil, nil), http.StatusForbidden)
	assertStatus(t, tokenClient.request(http.MethodGet, "/api/v1/admin/users", nil, nil), http.StatusForbidden)
	newCSRF := env.decode(t, bob.request(http.MethodGet, "/api/v1/me", nil, nil))["csrf_token"].(string)
	if newCSRF == bob.csrf {
		t.Fatal("demotion did not invalidate the previous CSRF token")
	}
	if text := logs.String(); !strings.Contains(text, `"event":"platform_audit"`) || !strings.Contains(text, `"actor_id":7`) || !strings.Contains(text, `"target_id":8`) || !strings.Contains(text, `"old_role":"viewer","new_role":"admin"`) || !strings.Contains(text, `"old_role":"admin","new_role":"viewer"`) || strings.Contains(text, testAdminToken) || strings.Contains(text, token) {
		t.Fatal("audit event missing fields or contains a credential")
	}
}

func TestPlatformRoleRejectsLastAdminAndUnknownUser(t *testing.T) {
	env := newM6Env(t, m6Options{})
	_ = env.loginAs("code-viewer")
	// The bootstrap bearer is not a user, so it can exercise the last-admin
	// protection independently of the self-protection rule.
	env.api.auth.admins = map[string]struct{}{}
	admin := env.bearerClient()
	missing := admin.json(http.MethodPatch, "/api/v1/admin/users/999", map[string]any{"role": "admin", "revision": 1})
	assertStatus(t, missing, http.StatusNotFound)
	if !strings.Contains(string(missing.body), `"code":"not_found"`) {
		t.Fatal("missing unknown user error code")
	}
	assertStatus(t, admin.json(http.MethodPatch, "/api/v1/admin/users/8", map[string]any{"role": "admin", "revision": 1}), http.StatusOK)
	protected := admin.json(http.MethodPatch, "/api/v1/admin/users/8", map[string]any{"role": "viewer", "revision": 2})
	assertStatus(t, protected, http.StatusForbidden)
	if !strings.Contains(string(protected.body), `"code":"protected_user"`) {
		t.Fatal("missing last admin error code")
	}
	assertStatus(t, admin.json(http.MethodPatch, "/api/v1/admin/users/8", map[string]any{"disabled": true, "revision": 2}), http.StatusForbidden)
}
