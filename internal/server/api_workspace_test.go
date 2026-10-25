package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func prepareProject(t *testing.T, env *m6Env, projectID int64, baseSHA string) {
	t.Helper()
	ctx := context.Background()
	if _, err := env.sqlite.RegisterProject(ctx, store.Project{
		ID:                projectID,
		PathWithNamespace: "group/repo",
		WebURL:            "https://gitlab.example.com/group/repo",
		DefaultBranch:     "main",
		TokenEnc:          []byte("token"),
		TokenScopes:       []string{"api"},
	}); err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	if _, err := env.sqlite.SaveWorkspace(ctx, store.Workspace{
		ProjectID: projectID,
		State:     "ready",
		BaseSHA:   baseSHA,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
}

func TestWorkspaceStatusAndRefresh(t *testing.T) {
	env := newM6Env(t, m6Options{})
	prepareProject(t, env, 42, "base-1")
	env.grant(7, 42, store.ProjectAccessManage)
	env.grant(8, 42, store.ProjectAccessRead)
	client := env.loginAs("code-admin")

	status := client.request(http.MethodGet, "/api/v1/projects/42/workspace", nil, nil)
	assertStatus(t, status, http.StatusOK)
	body := env.decode(t, status)
	if body["state"] != "ready" || body["base_sha"] != "base-1" {
		t.Fatalf("workspace = %v", body)
	}

	missing := client.request(http.MethodGet, "/api/v1/projects/99/workspace", nil, nil)
	assertStatus(t, missing, http.StatusNotFound)

	refresh := client.json(http.MethodPost, "/api/v1/projects/42/workspace/refresh",
		map[string]any{"regenerate_context": true, "reindex": true})
	assertStatus(t, refresh, http.StatusAccepted)
	if env.decode(t, refresh)["job_id"] != "job-refresh" {
		t.Fatalf("refresh body = %s", refresh.body)
	}
	if len(env.workspace.calls) != 1 {
		t.Fatalf("refresh calls = %v", env.workspace.calls)
	}
	call := env.workspace.calls[0]
	if call.projectID != 42 || !call.regenerateContext || !call.reindex {
		t.Fatalf("refresh call = %+v", call)
	}

	viewer := env.loginAs("code-viewer")
	denied := viewer.json(http.MethodPost, "/api/v1/projects/42/workspace/refresh", map[string]any{})
	assertStatus(t, denied, http.StatusForbidden)
}

func TestContextDocsKindValidationAndManualSave(t *testing.T) {
	env := newM6Env(t, m6Options{})
	base := strings.Repeat("a", 40)
	prepareProject(t, env, 42, base)
	env.grant(7, 42, store.ProjectAccessManage)
	client := env.loginAs("code-admin")

	list := client.request(http.MethodGet, "/api/v1/projects/42/context-docs", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if docs := env.decode(t, list)["documents"].([]any); len(docs) != 0 {
		t.Fatalf("documents = %v", docs)
	}

	badKind := client.request(http.MethodGet, "/api/v1/projects/42/context-docs?kind=bogus", nil, nil)
	assertStatus(t, badKind, http.StatusBadRequest)

	noDoc := client.request(http.MethodGet, "/api/v1/projects/42/context-docs/current?kind=agents", nil, nil)
	assertStatus(t, noDoc, http.StatusNotFound)

	mismatch := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"kind": "claude", "content": "body"})
	assertStatus(t, mismatch, http.StatusBadRequest)

	badQueryKind := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=other",
		map[string]any{"content": "body"})
	assertStatus(t, badQueryKind, http.StatusBadRequest)

	empty := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"content": "  "})
	assertStatus(t, empty, http.StatusBadRequest)

	saved := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"kind": "agents", "content": "first body", "locked": false})
	assertStatus(t, saved, http.StatusOK)
	savedBody := env.decode(t, saved)
	firstID, _ := savedBody["id"].(string)
	if firstID == "" || savedBody["source"] != "manual" || savedBody["base_sha"] != base {
		t.Fatalf("saved doc = %v", savedBody)
	}
	if savedBody["locked"] != false {
		t.Fatalf("locked = %v", savedBody["locked"])
	}
	if savedBody["updated_by"] != float64(7) {
		t.Fatalf("updated_by = %v", savedBody["updated_by"])
	}

	// Every manual save appends a new version; the previous row stays intact.
	again := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"content": "second body"})
	assertStatus(t, again, http.StatusOK)
	againBody := env.decode(t, again)
	secondID, _ := againBody["id"].(string)
	if secondID == "" || secondID == firstID || againBody["content"] != "second body" {
		t.Fatalf("second save = %v", againBody)
	}
	if againBody["active"] != true {
		t.Fatalf("second save is not active: %v", againBody)
	}
	listAgain := client.request(http.MethodGet, "/api/v1/projects/42/context-docs?kind=agents", nil, nil)
	assertStatus(t, listAgain, http.StatusOK)
	docs := env.decode(t, listAgain)["documents"].([]any)
	if len(docs) != 2 {
		t.Fatalf("manual save created %d versions, want 2", len(docs))
	}
	byID := make(map[string]map[string]any, len(docs))
	active := 0
	for _, item := range docs {
		doc := item.(map[string]any)
		byID[doc["id"].(string)] = doc
		if doc["active"] == true {
			active++
		}
	}
	if active != 1 || byID[secondID]["active"] != true {
		t.Fatalf("active versions = %v, want only %s", docs, secondID)
	}
	if byID[firstID]["content"] != "first body" || byID[firstID]["active"] != false {
		t.Fatalf("first version changed: %v", byID[firstID])
	}

	// A locked document may still be replaced through the manual path; the new
	// version inherits the current lock when the request omits it.
	lock := client.json(http.MethodPost, "/api/v1/projects/42/context-docs/current/lock?kind=agents",
		map[string]any{"locked": true})
	assertStatus(t, lock, http.StatusNoContent)
	current := client.request(http.MethodGet, "/api/v1/projects/42/context-docs/current?kind=agents", nil, nil)
	assertStatus(t, current, http.StatusOK)
	if env.decode(t, current)["locked"] != true {
		t.Fatalf("locked document = %s", current.body)
	}
	third := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"content": "third body"})
	assertStatus(t, third, http.StatusOK)
	thirdBody := env.decode(t, third)
	thirdID, _ := thirdBody["id"].(string)
	if thirdID == "" || thirdID == secondID || thirdBody["locked"] != true || thirdBody["active"] != true {
		t.Fatalf("manual save over locked version = %v", thirdBody)
	}
	list = client.request(http.MethodGet, "/api/v1/projects/42/context-docs?kind=agents", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if got := len(env.decode(t, list)["documents"].([]any)); got != 3 {
		t.Fatalf("versions after locked edit = %d, want 3", got)
	}

	// A different kind is a separate document.
	claude := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=claude",
		map[string]any{"content": "claude body"})
	assertStatus(t, claude, http.StatusOK)
	if env.decode(t, claude)["kind"] != "claude" {
		t.Fatalf("claude doc = %s", claude.body)
	}
}

func TestContextDocLockRequiresBody(t *testing.T) {
	env := newM6Env(t, m6Options{})
	prepareProject(t, env, 42, "base-1")
	env.grant(7, 42, store.ProjectAccessManage)
	client := env.loginAs("code-admin")
	response := client.json(http.MethodPost, "/api/v1/projects/42/context-docs/current/lock?kind=agents",
		map[string]any{})
	assertStatus(t, response, http.StatusBadRequest)
}

func TestContextDetailsRequireContextService(t *testing.T) {
	env := newM6Env(t, m6Options{noContexts: true})
	base := strings.Repeat("a", 40)
	prepareProject(t, env, 42, base)
	env.grant(7, 42, store.ProjectAccessManage)
	client := env.loginAs("code-admin")

	save := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"content": "body"})
	assertStatus(t, save, http.StatusServiceUnavailable)

	lock := client.json(http.MethodPost, "/api/v1/projects/42/context-docs/current/lock?kind=agents",
		map[string]any{"locked": true})
	assertStatus(t, lock, http.StatusServiceUnavailable)
}
