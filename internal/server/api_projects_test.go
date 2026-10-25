package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

func testProjectService(t *testing.T, st *sqlite.Store, api *stubGitLab, workspace string) *app.ProjectService {
	t.Helper()
	cipher, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	service, err := app.NewProjectService(app.ProjectServiceConfig{
		Store:     st,
		GitLab:    api,
		Cipher:    cipher,
		Profiles:  map[string]config.ProfileConfig{"default": {}, "deep": {}},
		PublicURL: "https://review.example.com",
		Workspace: workspace,
		GitLabURL: "https://gitlab.example.com",
		ProbeRemote: func(context.Context, string, string) (string, error) {
			return testSHA, nil
		},
		ValidationTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func registerProject(t *testing.T, client *m6Client) {
	t.Helper()
	response := client.json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42",
		"token":   "glpat-project-token",
	})
	if response.status != http.StatusCreated {
		t.Fatalf("register = %d %s", response.status, response.body)
	}
}

func TestProjectWriteEndpoints(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, workspace)
	}})
	client := env.loginAs("code-admin")
	registerProject(t, client)
	env.grant(7, 42, store.ProjectAccessManage)
	env.grant(8, 42, store.ProjectAccessRead)

	updated := client.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{
		"profile": "deep", "settings": map[string]any{"skip_draft": true}, "enabled": false,
	})
	if updated.status != http.StatusOK {
		t.Fatalf("patch = %d %s", updated.status, updated.body)
	}
	project := env.decode(t, updated)
	if project["profile"] != "deep" || project["enabled"] != false {
		t.Fatalf("patched project = %v", project)
	}

	list := client.request(http.MethodGet, "/api/v1/projects?q=group&enabled=false", nil, nil)
	if list.status != http.StatusOK {
		t.Fatalf("filtered list = %d %s", list.status, list.body)
	}
	if projects := env.decode(t, list)["projects"].([]any); len(projects) != 1 {
		t.Fatalf("filtered projects = %v", projects)
	}
	mismatch := client.request(http.MethodGet, "/api/v1/projects?q=absent&enabled=true", nil, nil)
	if mismatch.status != http.StatusOK || len(env.decode(t, mismatch)["projects"].([]any)) != 0 {
		t.Fatalf("mismatched filter = %d %s", mismatch.status, mismatch.body)
	}
	badFilter := client.request(http.MethodGet, "/api/v1/projects?enabled=maybe", nil, nil)
	if badFilter.status != http.StatusBadRequest {
		t.Fatalf("bad filter = %d %s", badFilter.status, badFilter.body)
	}

	for _, body := range []map[string]any{
		{"profile": "missing"},
		{"bogus": true},
		{},
		{"settings": "nope"},
	} {
		response := client.json(http.MethodPatch, "/api/v1/projects/42", body)
		if response.status != http.StatusBadRequest {
			t.Fatalf("invalid body %v = %d %s", body, response.status, response.body)
		}
	}
	if response := client.json(http.MethodPatch, "/api/v1/projects/999", map[string]any{"enabled": true}); response.status != http.StatusNotFound {
		t.Fatalf("patch missing = %d %s", response.status, response.body)
	}
	if response := client.request(http.MethodGet, "/api/v1/projects/abc", nil, nil); response.status != http.StatusBadRequest {
		t.Fatalf("invalid id = %d %s", response.status, response.body)
	}

	rotated := client.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": "glpat-rotated"})
	if rotated.status != http.StatusOK {
		t.Fatalf("rotate = %d %s", rotated.status, rotated.body)
	}
	saved, err := env.sqlite.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	cipher, _ := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	plain, err := cipher.Decrypt(saved.TokenEnc)
	if err != nil || string(plain) != "glpat-rotated" {
		t.Fatalf("rotated token = %q, %v", plain, err)
	}
	if response := client.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": ""}); response.status != http.StatusBadRequest {
		t.Fatalf("empty token = %d %s", response.status, response.body)
	}
	if response := client.json(http.MethodPut, "/api/v1/projects/999/token", map[string]any{"token": "x"}); response.status != http.StatusNotFound {
		t.Fatalf("rotate missing = %d %s", response.status, response.body)
	}

	hook := client.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil)
	if hook.status != http.StatusOK {
		t.Fatalf("webhook = %d %s", hook.status, hook.body)
	}
	hookBody := env.decode(t, hook)
	if hookBody["hook_id"] != float64(99) || hookBody["state"] != "enabled" {
		t.Fatalf("hook response = %v", hookBody)
	}
	if events, _ := hookBody["events"].([]any); len(events) != 3 {
		t.Fatalf("hook events = %v", hookBody["events"])
	}
	if strings.Contains(string(hook.body), "webhook_secret") || strings.Contains(string(hook.body), "secret") {
		t.Fatalf("webhook response leaked a secret: %s", hook.body)
	}

	// A repeated enable reuses the recorded hook instead of creating another.
	again := client.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil)
	if again.status != http.StatusOK {
		t.Fatalf("re-enable = %d %s", again.status, again.body)
	}
	againBody := env.decode(t, again)
	if againBody["hook_id"] != float64(99) || againBody["state"] != "enabled" {
		t.Fatalf("re-enable response = %v", againBody)
	}
	if api.callCount("CreateProjectHook") != 1 {
		t.Fatalf("duplicate create calls = %d", api.callCount("CreateProjectHook"))
	}
	if api.callCount("UpdateProjectHook") != 1 {
		t.Fatalf("reuse update calls = %d", api.callCount("UpdateProjectHook"))
	}
	if api.callCount("DeleteProjectHook") != 0 {
		t.Fatalf("reuse deleted a hook: %d", api.callCount("DeleteProjectHook"))
	}

	// When the hook was deleted outside the service, an explicit rebuild
	// creates one and adopts the new ID.
	api.mu.Lock()
	api.hooks = nil
	api.hookID = 100
	api.mu.Unlock()
	rebuilt := client.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil)
	if rebuilt.status != http.StatusOK || env.decode(t, rebuilt)["hook_id"] != float64(100) {
		t.Fatalf("rebuilt hook = %d %s", rebuilt.status, rebuilt.body)
	}
	if api.callCount("CreateProjectHook") != 2 {
		t.Fatalf("rebuild create calls = %d", api.callCount("CreateProjectHook"))
	}
	if api.callCount("DeleteProjectHook") != 0 {
		t.Fatalf("rebuild deleted a missing hook: %d", api.callCount("DeleteProjectHook"))
	}
	if response := client.request(http.MethodPost, "/api/v1/projects/999/webhook", nil, nil); response.status != http.StatusNotFound {
		t.Fatalf("webhook missing = %d %s", response.status, response.body)
	}

	projectID := int64(42)
	if _, err := env.sqlite.CreateMCPServer(ctx, store.MCPServer{
		Scope: "project", ProjectID: &projectID, Name: "srv",
		Config: json.RawMessage(`{"command":"x"}`),
	}); err != nil {
		t.Fatalf("seed MCP: %v", err)
	}
	if _, err := env.sqlite.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope: "project", ProjectID: &projectID, Block: "code-review",
		MCPTools: []string{"tool"}, Skills: []string{"skill"},
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	skill, err := env.sqlite.CreateSkill(ctx, store.Skill{
		Scope: "project", ProjectID: &projectID, Name: "dead", Description: "d",
	})
	if err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	managed := filepath.Join(workspace, "skills", "project-42", skill.ID, "skill")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := env.sqlite.SetSkillDir(ctx, skill.ID, managed); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "42", "base"), 0o700); err != nil {
		t.Fatal(err)
	}

	removed := client.request(http.MethodDelete, "/api/v1/projects/42", nil, nil)
	if removed.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", removed.status, removed.body)
	}
	if _, err := env.sqlite.GetProject(ctx, 42); err == nil {
		t.Fatal("project row still exists")
	}
	if servers, err := env.sqlite.ListMCPServers(ctx, "project", &projectID); err != nil || len(servers) != 0 {
		t.Fatalf("project servers = %+v, %v", servers, err)
	}
	if skills, err := env.sqlite.ListSkills(ctx, "project", &projectID); err != nil || len(skills) != 0 {
		t.Fatalf("project skills = %+v, %v", skills, err)
	}
	if bindings, err := env.sqlite.ListBlockBindings(ctx, "project", &projectID); err != nil || len(bindings) != 0 {
		t.Fatalf("project bindings = %+v, %v", bindings, err)
	}
	for _, path := range []string{filepath.Join(workspace, "42"), filepath.Join(workspace, "skills", "project-42")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s was not cleaned up: %v", path, err)
		}
	}
	if response := client.request(http.MethodDelete, "/api/v1/projects/42", nil, nil); response.status != http.StatusNotFound {
		t.Fatalf("second delete = %d %s", response.status, response.body)
	}
}

func TestValidateProjectEndpoint(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	admin := env.loginAs("code-admin")

	response := admin.json(http.MethodPost, "/api/v1/projects/validate", map[string]any{
		"project": "https://gitlab.example.com/group/repo.git",
		"token":   testProjectTok,
	})
	if response.status != http.StatusOK {
		t.Fatalf("validate = %d %s", response.status, response.body)
	}
	body := env.decode(t, response)
	for key, want := range map[string]any{
		"project_id":          float64(42),
		"path_with_namespace": "group/repo",
		"web_url":             "https://gitlab.example.com/group/repo",
		"default_branch":      "main",
		"username":            "review-bot",
		"access_level":        float64(30),
		"git_head":            testSHA,
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %v", key, body[key], want)
		}
	}
	scopes, _ := body["token_scopes"].([]any)
	if len(scopes) != 2 || scopes[0] != "api" || scopes[1] != "read_repository" {
		t.Errorf("token_scopes = %v", body["token_scopes"])
	}
	if _, err := env.sqlite.GetProject(context.Background(), 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("validation persisted a project: %v", err)
	}
	for _, call := range []string{"ListProjectHooks", "CreateProjectHook", "UpdateProjectHook", "DeleteProjectHook"} {
		if api.callCount(call) != 0 {
			t.Fatalf("validation called %s", call)
		}
	}

	bearer := env.bearerClient()
	if got := bearer.json(http.MethodPost, "/api/v1/projects/validate", map[string]any{
		"project": "42", "token": testProjectTok,
	}); got.status != http.StatusOK {
		t.Fatalf("bearer validate = %d %s", got.status, got.body)
	}
	for name, client := range map[string]*m6Client{
		"viewer":  env.loginAs("code-viewer"),
		"no csrf": admin.withoutCSRF(),
	} {
		got := client.json(http.MethodPost, "/api/v1/projects/validate", map[string]any{
			"project": "42", "token": testProjectTok,
		})
		if got.status != http.StatusForbidden {
			t.Fatalf("%s validate = %d %s", name, got.status, got.body)
		}
	}
	if got := admin.json(http.MethodPost, "/api/v1/projects/validate", map[string]any{
		"project": "42", "token": testProjectTok, "extra": true,
	}); got.status != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s", got.status, got.body)
	}
}

func TestValidateProjectEndpointRejectsForeignURLBeforeOutbound(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	admin := env.loginAs("code-admin")
	response := admin.json(http.MethodPost, "/api/v1/projects/validate", map[string]any{
		"project": "https://evil.example.com/group/repo.git",
		"token":   "glpat-secret",
	})
	if response.status != http.StatusBadRequest {
		t.Fatalf("foreign URL validate = %d %s", response.status, response.body)
	}
	if api.callCount("GetProject") != 0 {
		t.Fatal("foreign URL reached GitLab")
	}
	if strings.Contains(string(response.body), "glpat-secret") {
		t.Fatal("error body leaked the token")
	}
}

func TestValidateProjectEndpointSanitizesUpstreamFailure(t *testing.T) {
	api := defaultStubGitLab()
	api.projectErr = &gitlab.Error{Op: "get_project", Status: 403}
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	admin := env.loginAs("code-admin")
	response := admin.json(http.MethodPost, "/api/v1/projects/validate", map[string]any{
		"project": "42",
		"token":   "glpat-secret",
	})
	if response.status != http.StatusUnprocessableEntity {
		t.Fatalf("upstream failure = %d %s", response.status, response.body)
	}
	for _, forbidden := range []string{"glpat-secret", "https://", "group/repo"} {
		if strings.Contains(string(response.body), forbidden) {
			t.Fatalf("error body leaked %q: %s", forbidden, response.body)
		}
	}
	if code := env.decode(t, response)["error"].(map[string]any)["code"]; code != "gitlab_rejected" {
		t.Fatalf("error code = %v", code)
	}
}

func TestValidateProjectEndpointUnavailable(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		cipher, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
		if err != nil {
			t.Fatal(err)
		}
		service, err := app.NewProjectService(app.ProjectServiceConfig{
			Store: st, GitLab: api, Cipher: cipher,
			Profiles:  map[string]config.ProfileConfig{"default": {}},
			GitLabURL: "https://gitlab.example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}})
	admin := env.loginAs("code-admin")
	response := admin.json(http.MethodPost, "/api/v1/projects/validate", map[string]any{
		"project": "42", "token": "glpat-secret",
	})
	if response.status != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured validate = %d %s", response.status, response.body)
	}
	if api.callCount("GetProject") != 0 {
		t.Fatal("unconfigured validation reached GitLab")
	}
}

func registerProjectRow(t *testing.T, env *m6Env, id int64, path string) {
	t.Helper()
	_, err := env.sqlite.RegisterProject(context.Background(), store.Project{
		ID:                id,
		PathWithNamespace: path,
		WebURL:            "https://gitlab.example.com/" + path,
		DefaultBranch:     "main",
		TokenEnc:          []byte{1, 2, 3},
		WebhookSecretEnc:  []byte{4, 5, 6},
		TokenScopes:       []string{"api"},
	})
	if err != nil {
		t.Fatalf("RegisterProject %d: %v", id, err)
	}
}

func TestProjectListPagination(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	for _, id := range []int64{42, 43, 44} {
		registerProjectRow(t, env, id, "group/project-"+strconv.FormatInt(id, 10))
	}
	if _, err := env.sqlite.UpdateProject(context.Background(), 43, store.ProjectUpdate{
		Enabled: ptr(false),
	}); err != nil {
		t.Fatalf("disable project: %v", err)
	}
	client := env.loginAs("code-admin")
	for _, id := range []int64{42, 43, 44} {
		env.grant(7, id, store.ProjectAccessManage)
	}

	first := client.request(http.MethodGet, "/api/v1/projects?limit=2", nil, nil)
	assertStatus(t, first, http.StatusOK)
	firstBody := env.decode(t, first)
	projects := firstBody["projects"].([]any)
	if len(projects) != 2 {
		t.Fatalf("first page = %v", projects)
	}
	cursor, _ := firstBody["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("first page is missing next_cursor")
	}

	second := client.request(http.MethodGet, "/api/v1/projects?limit=2&cursor="+cursor, nil, nil)
	assertStatus(t, second, http.StatusOK)
	secondBody := env.decode(t, second)
	secondProjects := secondBody["projects"].([]any)
	if len(secondProjects) != 1 || secondBody["next_cursor"] != "" {
		t.Fatalf("second page = %v", secondBody)
	}
	if secondProjects[0].(map[string]any)["id"] != float64(44) {
		t.Fatalf("second page = %v", secondProjects)
	}

	filtered := client.request(http.MethodGet,
		"/api/v1/projects?q=project-4&enabled=false&limit=1", nil, nil)
	assertStatus(t, filtered, http.StatusOK)
	filteredBody := env.decode(t, filtered)
	if items := filteredBody["projects"].([]any); len(items) != 1 ||
		items[0].(map[string]any)["id"] != float64(43) {
		t.Fatalf("filtered page = %v", filteredBody)
	}

	invalid := client.request(http.MethodGet, "/api/v1/projects?cursor=not-a-cursor", nil, nil)
	assertStatus(t, invalid, http.StatusBadRequest)

	changedQuery := client.request(http.MethodGet,
		"/api/v1/projects?q=project-42&limit=2&cursor="+cursor, nil, nil)
	assertStatus(t, changedQuery, http.StatusBadRequest)
	changedEnabled := client.request(http.MethodGet,
		"/api/v1/projects?enabled=false&limit=2&cursor="+cursor, nil, nil)
	assertStatus(t, changedEnabled, http.StatusBadRequest)

	matchingHead := client.request(http.MethodGet, "/api/v1/projects?q=project-4&limit=1", nil, nil)
	assertStatus(t, matchingHead, http.StatusOK)
	matchingBody := env.decode(t, matchingHead)
	matchingCursor, _ := matchingBody["next_cursor"].(string)
	if matchingCursor == "" {
		t.Fatalf("matching q page has no cursor: %v", matchingBody)
	}
	matchingNext := client.request(http.MethodGet,
		"/api/v1/projects?q=project-4&limit=1&cursor="+matchingCursor, nil, nil)
	assertStatus(t, matchingNext, http.StatusOK)
	matchingNextBody := env.decode(t, matchingNext)
	matchingItems := matchingNextBody["projects"].([]any)
	if len(matchingItems) != 1 || matchingItems[0].(map[string]any)["id"] != float64(43) {
		t.Fatalf("matching q second page = %v", matchingNextBody)
	}

	head := client.request(http.MethodGet, "/api/v1/projects?limit=1", nil, nil)
	assertStatus(t, head, http.StatusOK)
	headBody := env.decode(t, head)
	headCursor, _ := headBody["next_cursor"].(string)
	if headCursor == "" {
		t.Fatal("head page is missing next_cursor")
	}
	deleted := client.request(http.MethodDelete, "/api/v1/projects/42", nil, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	afterDelete := client.request(http.MethodGet, "/api/v1/projects?limit=1&cursor="+headCursor, nil, nil)
	assertStatus(t, afterDelete, http.StatusOK)
	afterBody := env.decode(t, afterDelete)
	afterProjects := afterBody["projects"].([]any)
	if len(afterProjects) != 1 || afterProjects[0].(map[string]any)["id"] != float64(43) {
		t.Fatalf("page after delete = %v", afterBody)
	}
}

func TestProjectWebhookFailureStateVisible(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	client := env.loginAs("code-admin")
	registerProject(t, client)
	env.grant(7, 42, store.ProjectAccessManage)

	api.listHookErr = &gitlab.Error{Op: "list_hooks", Status: 403}
	failed := client.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil)
	if failed.status != http.StatusUnprocessableEntity {
		t.Fatalf("failed webhook = %d %s", failed.status, failed.body)
	}
	for _, forbidden := range []string{testProjectTok, "webhook_secret", "upstream"} {
		if strings.Contains(string(failed.body), forbidden) {
			t.Fatalf("webhook error leaked %q: %s", forbidden, failed.body)
		}
	}
	detail := client.request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	if detail.status != http.StatusOK {
		t.Fatalf("get = %d %s", detail.status, detail.body)
	}
	body := env.decode(t, detail)
	if body["webhook_state"] != "failed" {
		t.Fatalf("state = %v", body["webhook_state"])
	}
	if lastError, _ := body["webhook_error"].(string); !strings.Contains(lastError, "status 403") {
		t.Fatalf("webhook_error = %v", body["webhook_error"])
	}
	if strings.Contains(string(detail.body), "webhook_secret") || strings.Contains(string(detail.body), testProjectTok) {
		t.Fatalf("detail leaked a secret: %s", detail.body)
	}

	api.listHookErr = nil
	recovered := client.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil)
	if recovered.status != http.StatusOK {
		t.Fatalf("recovered webhook = %d %s", recovered.status, recovered.body)
	}
	recoveredBody := env.decode(t, recovered)
	if recoveredBody["state"] != "enabled" {
		t.Fatalf("recovered state = %v", recoveredBody["state"])
	}
	if recoveredBody["last_error"] != "" {
		t.Fatalf("enabled response last_error = %v", recoveredBody["last_error"])
	}
	if events, _ := recoveredBody["events"].([]any); len(events) != 3 {
		t.Fatalf("events = %v", recoveredBody["events"])
	}
	after := client.request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	afterBody := env.decode(t, after)
	if afterBody["webhook_state"] != "enabled" || afterBody["webhook_error"] != "" {
		t.Fatalf("detail after recovery = %v", afterBody)
	}
}

func TestProjectWritePermissions(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	admin := env.loginAs("code-admin")
	registerProject(t, admin)
	env.grant(7, 42, store.ProjectAccessManage)
	env.grant(8, 42, store.ProjectAccessRead)

	noCSRF := admin.withoutCSRF()
	if response := noCSRF.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{"enabled": false}); response.status != http.StatusForbidden {
		t.Fatalf("session write without CSRF = %d %s", response.status, response.body)
	}

	// The static bootstrap bearer is management-plane only and has no project
	// grant, so a project write is indistinguishable from an unknown project.
	bearer := env.bearerClient()
	if response := bearer.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{"enabled": true}); response.status != http.StatusNotFound {
		t.Fatalf("bootstrap bearer write = %d %s", response.status, response.body)
	}

	viewer := env.loginAs("code-viewer")
	if response := viewer.request(http.MethodGet, "/api/v1/projects", nil, nil); response.status != http.StatusOK {
		t.Fatalf("viewer list = %d %s", response.status, response.body)
	}
	for _, response := range []apiResponse{
		viewer.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{"enabled": true}),
		viewer.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": "x"}),
		viewer.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil),
		viewer.request(http.MethodDelete, "/api/v1/projects/42", nil, nil),
	} {
		if response.status != http.StatusForbidden {
			t.Fatalf("viewer write = %d %s", response.status, response.body)
		}
	}
	if _, err := env.sqlite.GetProject(context.Background(), 42); err != nil {
		t.Fatalf("viewer write changed the project: %v", err)
	}
}
