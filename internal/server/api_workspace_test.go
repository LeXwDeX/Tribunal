package server

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
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

func TestWorkspaceLastErrorIsSafeAtEveryReadEndpoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workspace.db")
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterProject(ctx, store.Project{
		ID: 42, PathWithNamespace: "group/repo", WebURL: "https://gitlab.example.com/group/repo",
		DefaultBranch: "main", TokenEnc: []byte("token"), TokenScopes: []string{"api"},
	}); err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	project := store.Project{ID: 42, PathWithNamespace: "group/repo"}
	env := newM6Env(t, m6Options{store: st, admin: &adminFieldsAdmin{project: &project, projects: []store.Project{project}}})
	env.grant(8, 42, store.ProjectAccessRead)
	client := env.loginAs("code-viewer")
	const sensitive = "/private/work tree/config\nglpat-synthetic-secret other sensitive details"
	cases := []struct {
		name     string
		state    string
		stored   sql.NullString
		expected string
	}{
		{"null", "ready", sql.NullString{}, ""},
		{"empty string", "ready", sql.NullString{Valid: true}, ""},
		{"unknown error", "error", sql.NullString{String: sensitive, Valid: true}, "workspace operation failed"},
		{"unknown ready error", "ready", sql.NullString{String: sensitive, Valid: true}, "workspace operation failed"},
		{"whitespace is nonempty", "error", sql.NullString{String: " ", Valid: true}, "workspace operation failed"},
		{"legacy index", "ready", sql.NullString{String: "index: " + sensitive, Valid: true}, "index: code graph indexing failed"},
		{"forged ready prefix", "ready", sql.NullString{String: "index: attacker " + sensitive, Valid: true}, "index: code graph indexing failed"},
		{"prefix in error state", "error", sql.NullString{String: "index: " + sensitive, Valid: true}, "workspace operation failed"},
		{"prefix in refreshing state", "refreshing", sql.NullString{String: "index: " + sensitive, Valid: true}, "workspace operation failed"},
		{"wrong case prefix", "ready", sql.NullString{String: "Index: " + sensitive, Valid: true}, "workspace operation failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.SaveWorkspace(ctx, store.Workspace{
				ProjectID: 42, State: tc.state, BaseSHA: "base", IndexSHA: "base", ContextSHA: "context",
			}); err != nil {
				t.Fatalf("SaveWorkspace: %v", err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE workspaces SET last_error = ? WHERE project_id = ?`, tc.stored, 42); err != nil {
				t.Fatalf("write historical value: %v", err)
			}
			for _, endpoint := range []string{"/api/v1/projects/42/workspace", "/api/v1/projects/42", "/api/v1/projects"} {
				response := client.request(http.MethodGet, endpoint, nil, nil)
				assertStatus(t, response, http.StatusOK)
				body := env.decode(t, response)
				var workspace map[string]any
				switch endpoint {
				case "/api/v1/projects/42/workspace":
					workspace = body
				case "/api/v1/projects/42":
					workspace = body["workspace"].(map[string]any)
				default:
					projects := body["projects"].([]any)
					if len(projects) != 1 {
						t.Fatalf("project list = %v, want one readable project", projects)
					}
					workspace = projects[0].(map[string]any)["workspace"].(map[string]any)
				}
				lastError, present := workspace["last_error"]
				if tc.expected == "" && present || tc.expected != "" && (!present || lastError != tc.expected) {
					t.Errorf("%s last_error = %v (present %v), want %q", endpoint, lastError, present, tc.expected)
				}
				if workspace["state"] != tc.state || workspace["base_sha"] != "base" || workspace["index_status"] != "ready" {
					t.Errorf("%s changed workspace state or index semantics: %v", endpoint, workspace)
				}
				if endpoint == "/api/v1/projects/42/workspace" && (workspace["index_sha"] != "base" || workspace["context_sha"] != "context") {
					t.Errorf("%s changed workspace checkpoints: %v", endpoint, workspace)
				}
				if strings.Contains(string(response.body), "/private/") || strings.Contains(string(response.body), "glpat-synthetic-secret") || strings.Contains(string(response.body), "other sensitive details") {
					t.Errorf("%s leaked historical data: %s", endpoint, response.body)
				}
				var after sql.NullString
				if err := db.QueryRowContext(ctx, `SELECT last_error FROM workspaces WHERE project_id = ?`, 42).Scan(&after); err != nil {
					t.Fatalf("read historical value: %v", err)
				}
				if after != tc.stored {
					t.Errorf("%s changed stored value from %#v to %#v", endpoint, tc.stored, after)
				}
			}
		})
	}
}

func TestWorkspaceReadsDoNotModifySharedStoreRecord(t *testing.T) {
	workspace := &store.Workspace{ProjectID: 42, State: "ready", BaseSHA: "base", IndexSHA: "base", LastError: "index: /private/work tree glpat-synthetic-secret"}
	original := *workspace
	project := store.Project{ID: 42, PathWithNamespace: "group/repo"}
	env := newM6Env(t, m6Options{
		store: &adminFieldsStore{fakeReviewStore: newFakeReviewStore(), workspace: workspace},
		admin: &adminFieldsAdmin{project: &project, projects: []store.Project{project}},
	})
	env.grant(8, 42, store.ProjectAccessRead)
	client := env.loginAs("code-viewer")
	for _, endpoint := range []string{"/api/v1/projects/42/workspace", "/api/v1/projects/42", "/api/v1/projects"} {
		response := client.request(http.MethodGet, endpoint, nil, nil)
		assertStatus(t, response, http.StatusOK)
		if strings.Contains(string(response.body), "/private/") || strings.Contains(string(response.body), "glpat-synthetic-secret") {
			t.Errorf("%s leaked shared record: %s", endpoint, response.body)
		}
		if !reflect.DeepEqual(*workspace, original) {
			t.Errorf("%s modified shared record: %+v", endpoint, workspace)
		}
	}
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
