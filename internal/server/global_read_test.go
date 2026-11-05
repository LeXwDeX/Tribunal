package server

import (
	"bytes"
	"encoding/json"
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
	type auditEvent struct {
		Event      string `json:"event"`
		ActorID    int64  `json:"actor_id"`
		TargetID   int64  `json:"target_id"`
		GlobalRead *bool  `json:"global_read"`
	}
	var events []auditEvent
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var event auditEvent
		if json.Unmarshal([]byte(line), &event) == nil && event.Event == "platform_audit" && event.GlobalRead != nil {
			events = append(events, event)
		}
	}
	if len(events) != 2 {
		t.Fatalf("audit events = %d, want 2", len(events))
	}
	for i, want := range []bool{true, false} {
		if events[i].ActorID != 7 || events[i].TargetID != 8 || *events[i].GlobalRead != want {
			t.Fatalf("audit event %d = %+v, want actor 7 target 8 global_read %v", i, events[i], want)
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
