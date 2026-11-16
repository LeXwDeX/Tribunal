package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	workspacepkg "github.com/LeXwDeX/tribunal/internal/workspace"
)

func testSharedProjectService(t *testing.T, st *sqlite.Store, api *stubGitLab, workspace, sharedToken string) *app.ProjectService {
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
		Workspace: workspacepkg.NewManager(workspace, nil),
		GitLabURL: "https://gitlab.example.com",
		ProbeRemote: func(context.Context, string, string) (string, error) {
			return testSHA, nil
		},
		ValidationTimeout: 5 * time.Second,
		SharedBotToken:    sharedToken,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

// Registration without auto_webhook now creates the webhook by default, and
// an explicit false keeps the manual path.
func TestRegisterAutoWebhookDefaultsOn(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	client := env.loginAs("code-admin")

	created := client.json(http.MethodPost, "/api/v1/projects", map[string]any{"project": "42", "token": "glpat-project-token"})
	if created.status != http.StatusCreated {
		t.Fatalf("register = %d %s", created.status, created.body)
	}
	if api.callCount("CreateProjectHook") != 1 {
		t.Fatalf("CreateProjectHook calls = %d, want 1", api.callCount("CreateProjectHook"))
	}
	saved, err := env.sqlite.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.HookID == nil || *saved.HookID != 99 || saved.WebhookState != store.WebhookStateEnabled {
		t.Fatalf("webhook state = %+v", saved)
	}
}

func TestRegisterExplicitAutoWebhookFalseKeepsManualPath(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	client := env.loginAs("code-admin")

	created := client.json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42", "token": "glpat-project-token", "auto_webhook": false,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("register = %d %s", created.status, created.body)
	}
	if api.callCount("CreateProjectHook") != 0 {
		t.Fatalf("explicit false created a hook: %d", api.callCount("CreateProjectHook"))
	}
	body := env.decode(t, created)
	if secretValue, _ := body["webhook_secret"].(string); secretValue == "" {
		t.Fatalf("manual registration did not return a one-time secret: %v", body)
	}
	saved, err := env.sqlite.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.HookID != nil || saved.WebhookState != store.WebhookStateDisabled {
		t.Fatalf("manual webhook state = %+v", saved)
	}
}

// URL-only registration in shared-credential mode: no token field, an
// auto-created webhook, and the shared-credential capability bit surfaced to
// the console.
func TestRegisterSharedCredentialURLOnly(t *testing.T) {
	const sharedToken = "glpat-shared-bot"
	api := defaultStubGitLab()
	api.memberAccess = 40 // shared bot Maintainer; the stub ignores user ids
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testSharedProjectService(t, st, api, t.TempDir(), sharedToken)
	}})
	client := env.loginAs("code-viewer")

	created := client.json(http.MethodPost, "/api/v1/projects", map[string]any{"project": "42"})
	if created.status != http.StatusCreated {
		t.Fatalf("register = %d %s", created.status, created.body)
	}
	body := env.decode(t, created)
	if body["shared_credential"] != true {
		t.Fatalf("registration did not mark shared credential: %v", body)
	}
	project, _ := body["project"].(map[string]any)
	if project["webhook_state"] != store.WebhookStateEnabled || project["hook_id"] != float64(99) {
		t.Fatalf("registration webhook = %v", project)
	}
	saved, err := env.sqlite.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(saved.TokenEnc) != 0 {
		t.Fatalf("shared project stored a token: %v", saved.TokenEnc)
	}

	me := client.request(http.MethodGet, "/api/v1/me", nil, nil)
	if me.status != http.StatusOK || env.decode(t, me)["shared_bot_credential"] != true {
		t.Fatalf("me = %d %s", me.status, me.body)
	}
	detail := client.request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	if detail.status != http.StatusOK {
		t.Fatalf("detail = %d %s", detail.status, detail.body)
	}
	if env.decode(t, detail)["shared_credential"] != true {
		t.Fatalf("detail did not mark shared credential: %s", detail.body)
	}
}

func TestRegisterEmptyTokenWithoutSharedCredentialReturns400(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	client := env.loginAs("code-viewer")

	response := client.json(http.MethodPost, "/api/v1/projects", map[string]any{"project": "42"})
	if response.status != http.StatusBadRequest {
		t.Fatalf("register = %d %s", response.status, response.body)
	}
	body := env.decode(t, response)
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["message"] != "access token is required" {
		t.Fatalf("error = %v", errorBody)
	}
	if api.callCount("GetProject") != 0 {
		t.Fatalf("GitLab was called without a credential: %d", api.callCount("GetProject"))
	}
}

func TestSettingsExposeSharedBotCredentialState(t *testing.T) {
	const sharedToken = "glpat-shared-bot"
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testSharedProjectService(t, st, api, t.TempDir(), sharedToken)
	}})
	admin := env.loginAs("code-admin")

	response := admin.request(http.MethodGet, "/api/v1/settings", nil, nil)
	if response.status != http.StatusOK {
		t.Fatalf("settings = %d %s", response.status, response.body)
	}
	gitlabView, _ := env.decode(t, response)["gitlab"].(map[string]any)
	if gitlabView["shared_bot_credential"] != true || gitlabView["shared_bot_credential_state"] != "ok" {
		t.Fatalf("gitlab settings = %v", gitlabView)
	}
	if gitlabView["shared_bot_username"] != "review-bot" {
		t.Fatalf("shared bot username = %v", gitlabView["shared_bot_username"])
	}
	if _, ok := gitlabView["shared_bot_credential_expires_at"]; ok {
		t.Fatalf("settings exposed an expiry the stub did not set: %v", gitlabView)
	}
	if bytes.Contains(response.body, []byte(sharedToken)) {
		t.Fatal("settings response contains the shared token")
	}
}

func TestMeWithoutSharedCredentialCapability(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	client := env.loginAs("code-viewer")
	me := client.request(http.MethodGet, "/api/v1/me", nil, nil)
	if me.status != http.StatusOK {
		t.Fatalf("me = %d %s", me.status, me.body)
	}
	if env.decode(t, me)["shared_bot_credential"] != false {
		t.Fatalf("shared capability = %v", env.decode(t, me)["shared_bot_credential"])
	}
}
