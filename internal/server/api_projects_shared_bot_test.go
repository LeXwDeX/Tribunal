package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	workspacepkg "github.com/LeXwDeX/tribunal/internal/workspace"
)

const testProvisioningToken = "glpat-test-provisioning"

func testSharedProjectService(t *testing.T, st *sqlite.Store, api gitlab.API, workspace, sharedToken string) *app.ProjectService {
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
		ProvisioningToken: testProvisioningToken,
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
	api.memberAccess = 20 // shared bot Reporter; the stub ignores user ids
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
	t.Setenv("TRIBUNAL_TEST_PROVISIONING_TOKEN", "glpat-provisioning-secret")
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{
		platform: &config.Config{GitLab: config.GitLabConfig{ProvisioningTokenEnv: "TRIBUNAL_TEST_PROVISIONING_TOKEN"}},
		adminFactory: func(st *sqlite.Store) AdminService {
			return testSharedProjectService(t, st, api, t.TempDir(), sharedToken)
		},
	})
	admin := env.loginAs("code-admin")

	response := admin.request(http.MethodGet, "/api/v1/settings", nil, nil)
	if response.status != http.StatusOK {
		t.Fatalf("settings = %d %s", response.status, response.body)
	}
	gitlabView, _ := env.decode(t, response)["gitlab"].(map[string]any)
	if gitlabView["shared_bot_credential"] != true || gitlabView["shared_bot_credential_state"] != "ok" {
		t.Fatalf("gitlab settings = %v", gitlabView)
	}
	if gitlabView["provisioning_credential"] != true {
		t.Fatalf("provisioning_credential = %v", gitlabView["provisioning_credential"])
	}
	if gitlabView["shared_bot_username"] != "review-bot" {
		t.Fatalf("shared bot username = %v", gitlabView["shared_bot_username"])
	}
	if _, ok := gitlabView["shared_bot_credential_expires_at"]; ok {
		t.Fatalf("settings exposed an expiry the stub did not set: %v", gitlabView)
	}
	for _, secret := range []string{sharedToken, "glpat-provisioning-secret"} {
		if bytes.Contains(response.body, []byte(secret)) {
			t.Fatalf("settings response contains a credential value %q", secret)
		}
	}
	for _, forbidden := range []string{"bot_token_env", "TRIBUNAL_BOT_TOKEN", "provisioning_token_env", "TRIBUNAL_TEST_PROVISIONING_TOKEN"} {
		if bytes.Contains(response.body, []byte(forbidden)) {
			t.Fatalf("settings response contains the credential variable name %q", forbidden)
		}
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

// matrixGitLab gives the shared bot and the registrant different project
// roles, which the single access-level stub cannot express.
type matrixGitLab struct {
	*stubGitLab
	mu     sync.Mutex
	access map[int64]int
}

func (s *matrixGitLab) CurrentUser(ctx context.Context, token string) (*gitlab.User, error) {
	s.record("CurrentUser")
	return &gitlab.User{ID: 9, Username: "review-bot"}, nil
}

func (s *matrixGitLab) GetMemberAccess(ctx context.Context, token string, projectID, userID int64) (int, error) {
	s.record("GetMemberAccess")
	s.mu.Lock()
	defer s.mu.Unlock()
	if level, ok := s.access[userID]; ok {
		return level, nil
	}
	return 0, &gitlab.Error{Op: "member_access", Status: 404}
}

func TestRegisterSharedCredentialNonTrustedDenialsAreIndistinguishable(t *testing.T) {
	// GitLab user 9 is the shared bot; code-viewer is user 8.
	variants := []struct {
		name   string
		access map[int64]int
	}{
		{name: "bot below reporter", access: map[int64]int{9: 10, 8: 20}},
		{name: "registrant not reporter", access: map[int64]int{9: 20, 8: 10}},
	}
	type result struct {
		status  int
		code    string
		msg     string
		details string
	}
	var first *result
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			api := &matrixGitLab{stubGitLab: defaultStubGitLab(), access: variant.access}
			env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
				return testSharedProjectService(t, st, api, t.TempDir(), "glpat-shared-bot")
			}})
			client := env.loginAs("code-viewer")
			response := client.json(http.MethodPost, "/api/v1/projects", map[string]any{"project": "42"})
			body := env.decode(t, response)
			errorBody, _ := body["error"].(map[string]any)
			code, _ := errorBody["code"].(string)
			message, _ := errorBody["message"].(string)
			got := &result{status: response.status, code: code, msg: message, details: fmt.Sprint(errorBody["details"])}
			if first == nil {
				first = got
				if got.status != http.StatusBadRequest || got.code != "validation_failed" {
					t.Fatalf("unexpected unified denial: %+v %s", got, response.body)
				}
			}
			if *got != *first {
				t.Fatalf("denial differs between causes: %+v vs %+v", got, first)
			}
			if _, err := env.sqlite.GetProject(context.Background(), 42); err == nil {
				t.Fatal("denied registration persisted a project")
			}
		})
	}
}

func TestRegisterPerProjectTokenReportsNotShared(t *testing.T) {
	api := defaultStubGitLab()
	api.memberAccess = 30
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	client := env.loginAs("code-viewer")

	created := client.json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42", "token": "glpat-project-token", "auto_webhook": false,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("register = %d %s", created.status, created.body)
	}
	if env.decode(t, created)["shared_credential"] != false {
		t.Fatalf("project-token registration marked shared: %s", created.body)
	}
	detail := client.request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	if detail.status != http.StatusOK || env.decode(t, detail)["shared_credential"] != false {
		t.Fatalf("detail = %d %s", detail.status, detail.body)
	}
	list := client.request(http.MethodGet, "/api/v1/projects", nil, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list = %d %s", list.status, list.body)
	}
	projects, _ := env.decode(t, list)["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("projects = %v", projects)
	}
	if projects[0].(map[string]any)["shared_credential"] != false {
		t.Fatalf("list marked a project token as shared: %v", projects[0])
	}
}

func TestValidateSharedCredentialHTTP(t *testing.T) {
	api := defaultStubGitLab()
	api.memberAccess = 20
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testSharedProjectService(t, st, api, t.TempDir(), "glpat-shared-bot")
	}})
	client := env.loginAs("code-viewer")

	response := client.json(http.MethodPost, "/api/v1/projects/validate", map[string]any{"project": "42"})
	if response.status != http.StatusOK {
		t.Fatalf("validate = %d %s", response.status, response.body)
	}
	body := env.decode(t, response)
	if body["shared_credential"] != true || body["username"] != "review-bot" || body["access_level"] != float64(20) {
		t.Fatalf("validation = %v", body)
	}
	if _, err := env.sqlite.GetProject(context.Background(), 42); err == nil {
		t.Fatal("validation persisted a project")
	}
}

// An empty token on the rotate endpoint switches an existing project to the
// shared bot when the instance configures it. The stored sentinel is a
// zero-length value and the previous token failure is cleared. This project has
// no managed hook, so the state settles on disabled instead of pending.
func TestRotateEmptyTokenSwitchesToSharedCredentialHTTP(t *testing.T) {
	const sharedToken = "glpat-shared-bot"
	api := defaultStubGitLab()
	api.memberAccess = 20
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testSharedProjectService(t, st, api, t.TempDir(), sharedToken)
	}})
	client := env.loginAs("code-admin")
	registerProject(t, client)
	if err := env.sqlite.UpdateProjectWebhook(context.Background(), 42, store.ProjectWebhookUpdate{
		State: store.WebhookStateFailed, Error: "project token was rejected by GitLab while checking the webhook; replace the project token",
	}); err != nil {
		t.Fatalf("seed failed webhook state: %v", err)
	}
	hookCalls := api.callCount("ListProjectHooks") + api.callCount("CreateProjectHook") +
		api.callCount("UpdateProjectHook") + api.callCount("DeleteProjectHook")

	rotated := client.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": "   "})
	if rotated.status != http.StatusOK {
		t.Fatalf("switch = %d %s", rotated.status, rotated.body)
	}
	body := env.decode(t, rotated)
	if body["id"] != float64(42) || body["path_with_namespace"] != "group/repo" {
		t.Fatalf("switch response lost project fields: %v", body)
	}
	if body["webhook_state"] != store.WebhookStateDisabled || body["webhook_error"] != "" {
		t.Fatalf("switch response webhook = %v/%v, want disabled", body["webhook_state"], body["webhook_error"])
	}
	if _, ok := body["token_expires_at"]; ok {
		t.Fatalf("switch response kept the expiry: %v", body)
	}
	warnings, _ := body["warnings"].([]any)
	if len(warnings) != 1 {
		t.Fatalf("switch warnings = %v, want the unverified shared credential warning", body["warnings"])
	}
	warning, _ := warnings[0].(map[string]any)
	if warning["code"] != "shared_credential_unverified" || warning["message"] == "" {
		t.Fatalf("switch warning = %v", warning)
	}
	for _, forbidden := range []string{sharedToken, "glpat-project-token"} {
		if bytes.Contains(rotated.body, []byte(forbidden)) {
			t.Fatalf("switch response contains a credential %q", forbidden)
		}
	}
	if got := api.callCount("ListProjectHooks") + api.callCount("CreateProjectHook") +
		api.callCount("UpdateProjectHook") + api.callCount("DeleteProjectHook"); got != hookCalls {
		t.Fatalf("switch made %d hook calls", got-hookCalls)
	}

	saved, err := env.sqlite.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(saved.TokenEnc) != 0 || saved.TokenExpiresAt != nil || len(saved.TokenScopes) != 0 {
		t.Fatalf("stored credential = %+v", saved)
	}
	if saved.WebhookState != store.WebhookStateDisabled || saved.WebhookError != "" {
		t.Fatalf("stored webhook = %q/%q, want disabled", saved.WebhookState, saved.WebhookError)
	}
	detail := client.request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	if detail.status != http.StatusOK || env.decode(t, detail)["shared_credential"] != true {
		t.Fatalf("detail after switch = %d %s", detail.status, detail.body)
	}

	// A non-empty token switches back to per-project mode and keeps the
	// hook-less project disabled.
	restored := client.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": "glpat-restored"})
	if restored.status != http.StatusOK {
		t.Fatalf("rotate back = %d %s", restored.status, restored.body)
	}
	restoredBody := env.decode(t, restored)
	if restoredBody["webhook_state"] != store.WebhookStateDisabled || restoredBody["webhook_error"] != "" {
		t.Fatalf("rotate-back webhook = %v/%v, want disabled", restoredBody["webhook_state"], restoredBody["webhook_error"])
	}
	detail = client.request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	if detail.status != http.StatusOK || env.decode(t, detail)["shared_credential"] != false {
		t.Fatalf("detail after rotate back = %d %s", detail.status, detail.body)
	}
}

// The token field must be present and explicitly empty to switch. An omitted
// field or JSON null keeps the original validation error, so a client cannot
// clear the credential by accident.
func TestRotateTokenFieldPresence(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testSharedProjectService(t, st, api, t.TempDir(), "glpat-shared-bot")
	}})
	client := env.loginAs("code-admin")
	registerProject(t, client)

	for _, body := range []map[string]any{{}, {"token": nil}} {
		response := client.json(http.MethodPut, "/api/v1/projects/42/token", body)
		if response.status != http.StatusBadRequest {
			t.Fatalf("body %v = %d %s", body, response.status, response.body)
		}
		errorBody, _ := env.decode(t, response)["error"].(map[string]any)
		if errorBody["message"] != "access token is required" {
			t.Fatalf("body %v error = %v", body, errorBody)
		}
		saved, err := env.sqlite.GetProject(context.Background(), 42)
		if err != nil || len(saved.TokenEnc) == 0 {
			t.Fatalf("body %v cleared the credential: %+v, %v", body, saved, err)
		}
	}

	response := client.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": ""})
	if response.status != http.StatusOK {
		t.Fatalf("explicit empty token = %d %s", response.status, response.body)
	}
	saved, err := env.sqlite.GetProject(context.Background(), 42)
	if err != nil || len(saved.TokenEnc) != 0 {
		t.Fatalf("explicit empty token did not switch: %+v, %v", saved, err)
	}
}

func TestRotateEmptyTokenWithoutSharedCredentialHTTP(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	client := env.loginAs("code-admin")
	registerProject(t, client)

	response := client.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": "  "})
	if response.status != http.StatusBadRequest {
		t.Fatalf("switch without shared = %d %s", response.status, response.body)
	}
	errorBody, _ := env.decode(t, response)["error"].(map[string]any)
	if errorBody["message"] != "access token is required" {
		t.Fatalf("error = %v", errorBody)
	}
	saved, err := env.sqlite.GetProject(context.Background(), 42)
	if err != nil || len(saved.TokenEnc) == 0 {
		t.Fatalf("rejected switch changed the token: %+v, %v", saved, err)
	}
}
