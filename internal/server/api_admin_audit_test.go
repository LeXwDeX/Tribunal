package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func adminUserRevision(t *testing.T, env *m6Env, client *m6Client, id int64) int64 {
	t.Helper()
	response := client.request(http.MethodGet, "/api/v1/admin/users", nil, nil)
	assertStatus(t, response, http.StatusOK)
	for _, item := range env.decode(t, response)["users"].([]any) {
		user := item.(map[string]any)
		if int64(user["id"].(float64)) == id {
			return int64(user["revision"].(float64))
		}
	}
	t.Fatalf("user %d missing from the admin list", id)
	return 0
}

func requireAuditFields(t *testing.T, logs, action string, want ...string) {
	t.Helper()
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if !strings.Contains(line, `"event":"platform_audit"`) || !strings.Contains(line, `"action":"`+action+`"`) {
			continue
		}
		missing := false
		for _, field := range want {
			if !strings.Contains(line, field) {
				missing = true
				break
			}
		}
		if !missing {
			found = true
		}
	}
	if !found {
		t.Fatalf("no platform_audit line for action %q with %v:\n%s", action, want, logs)
	}
}

// A disabled-only PATCH leaves the role and grants unchanged, so it used to
// skip every audit branch even though it invalidates the user's credentials.
func TestAdminDisabledOnlyChangeAudited(t *testing.T) {
	env := newM6Env(t, m6Options{})
	env.ensureUser(8, "bob", "Bob Viewer", roleViewer)
	var logs bytes.Buffer
	env.api.logger = newTestLogger(&logs)
	admin := env.loginAs("code-admin")
	revision := adminUserRevision(t, env, admin, 8)

	response := admin.json(http.MethodPatch, "/api/v1/admin/users/8", map[string]any{"disabled": true, "revision": revision})
	assertStatus(t, response, http.StatusOK)

	text := logs.String()
	requireAuditFields(t, text, "update_user",
		`"actor_id":7`, `"target_id":8`, `"old_role":"viewer"`, `"new_role":"viewer"`,
		`"disabled":true`, `"result":"ok"`)
	if strings.Contains(text, `"global_read"`) {
		t.Fatalf("disabled-only audit must not claim a global_read change:\n%s", text)
	}
}

func TestAdminGrantAndCredentialWritesAudited(t *testing.T) {
	env := newM6Env(t, m6Options{})
	env.grant(8, 42, store.ProjectAccessRead)
	var logs bytes.Buffer
	env.api.logger = newTestLogger(&logs)
	admin := env.loginAs("code-admin")

	set := admin.json(http.MethodPut, "/api/v1/admin/projects/42/grants/8", map[string]any{"role": "manage"})
	assertStatus(t, set, http.StatusOK)
	deleted := admin.request(http.MethodDelete, "/api/v1/admin/projects/42/grants/8", nil, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	revoked := admin.request(http.MethodDelete, "/api/v1/admin/users/8/credentials", nil, nil)
	assertStatus(t, revoked, http.StatusNoContent)
	missing := admin.json(http.MethodPut, "/api/v1/admin/projects/42/grants/999", map[string]any{"role": "read"})
	assertStatus(t, missing, http.StatusNotFound)

	text := logs.String()
	requireAuditFields(t, text, "grant",
		`"actor_id":7`, `"target_id":8`, `"project_id":42`, `"role":"manage"`, `"result":"ok"`)
	requireAuditFields(t, text, "revoke_grant",
		`"actor_id":7`, `"target_id":8`, `"project_id":42`, `"result":"ok"`)
	requireAuditFields(t, text, "revoke_credentials",
		`"actor_id":7`, `"target_id":8`, `"result":"ok"`)
	// Rejected writes carry a stable code instead of the raw store error.
	requireAuditFields(t, text, "grant",
		`"target_id":999`, `"project_id":42`, `"code":"not_found"`, `"result":"rejected"`)
}
