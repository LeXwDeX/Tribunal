package server

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
)

// Built-in skills are listed with a marker, can be disabled and enabled, and
// cannot be deleted.
func TestBuiltinSkillToggleAndDeleteGuard(t *testing.T) {
	env := newWiringEnv(t)
	defaults, err := env.backend.service.SkillDefaults([]config.DefaultSkill{{Name: "code-review", Blocks: []string{"review"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ApplyExtensionDefaults(context.Background(), env.db, nil, defaults, nil); err != nil {
		t.Fatal(err)
	}
	client := env.bearer()

	list := client.request(http.MethodGet, "/api/v1/skills?scope=platform", nil, nil)
	assertStatus(t, list, http.StatusOK)
	skills := decodeMap(t, list.body)["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("skills = %v", skills)
	}
	listed := skills[0].(map[string]any)
	if listed["name"] != "code-review" || listed["builtin"] != true || listed["enabled"] != true {
		t.Fatalf("listed skill = %v", listed)
	}
	id := listed["id"].(string)

	disabled := client.json(http.MethodPatch, "/api/v1/skills/"+id, map[string]any{"enabled": false})
	assertStatus(t, disabled, http.StatusOK)
	if body := decodeMap(t, disabled.body); body["enabled"] != false || body["builtin"] != true {
		t.Fatalf("disabled skill = %v", body)
	}
	enabled := client.json(http.MethodPatch, "/api/v1/skills/"+id, map[string]any{"enabled": true})
	assertStatus(t, enabled, http.StatusOK)
	if body := decodeMap(t, enabled.body); body["enabled"] != true {
		t.Fatalf("enabled skill = %v", body)
	}

	assertStatus(t, client.json(http.MethodPatch, "/api/v1/skills/"+id, map[string]any{}), http.StatusBadRequest)
	assertStatus(t, client.json(http.MethodPatch, "/api/v1/skills/missing", map[string]any{"enabled": true}), http.StatusNotFound)

	deleted := client.request(http.MethodDelete, "/api/v1/skills/"+id, nil, nil)
	assertStatus(t, deleted, http.StatusConflict)
	after := client.request(http.MethodGet, "/api/v1/skills?scope=platform", nil, nil)
	if got := decodeMap(t, after.body)["skills"].([]any); len(got) != 1 {
		t.Fatalf("builtin skill deleted: %v", got)
	}
}

// A caller without write permission gets the same answer for an existing and
// a missing skill, so PATCH cannot be used to probe which skills exist.
func TestUpdateSkillDoesNotRevealExistenceToViewer(t *testing.T) {
	env := newWiringEnv(t)
	defaults, err := env.backend.service.SkillDefaults([]config.DefaultSkill{{Name: "code-review", Blocks: []string{"review"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ApplyExtensionDefaults(context.Background(), env.db, nil, defaults, nil); err != nil {
		t.Fatal(err)
	}
	list := env.bearer().request(http.MethodGet, "/api/v1/skills?scope=platform", nil, nil)
	assertStatus(t, list, http.StatusOK)
	id := decodeMap(t, list.body)["skills"].([]any)[0].(map[string]any)["id"].(string)

	viewer := env.loginAs("code-viewer")
	existing := viewer.json(http.MethodPatch, "/api/v1/skills/"+id, map[string]any{"enabled": false})
	missing := viewer.json(http.MethodPatch, "/api/v1/skills/missing", map[string]any{"enabled": false})
	if existing.status != missing.status || existing.status != http.StatusForbidden {
		t.Fatalf("viewer PATCH existing = %d %s, missing = %d %s; want identical 403",
			existing.status, existing.body, missing.status, missing.body)
	}
	got := decodeMap(t, existing.body)["error"].(map[string]any)
	want := decodeMap(t, missing.body)["error"].(map[string]any)
	delete(got, "request_id")
	delete(want, "request_id")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("viewer PATCH errors differ: existing %v, missing %v", got, want)
	}
}
