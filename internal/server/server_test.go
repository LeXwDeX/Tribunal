package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

const (
	testAdminToken = "admin-token-0123456789abcdef0123456789"
	testProjectTok = "glpat-project-token"
	testSHA        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type stubGitLab struct {
	mu sync.Mutex

	project      *gitlab.Project
	projectErr   error
	tokenInfo    *gitlab.TokenInfo
	hookID       int64
	listHookErr  error
	hooks        []gitlab.Hook
	deletedHooks []int64
	calls        []string
}

func (s *stubGitLab) record(call string) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
}

func (s *stubGitLab) resetCalls() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

func (s *stubGitLab) callCount(call string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, c := range s.calls {
		if c == call {
			count++
		}
	}
	return count
}

func (s *stubGitLab) GetProject(context.Context, string, string) (*gitlab.Project, error) {
	s.record("GetProject")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projectErr != nil {
		return nil, s.projectErr
	}
	p := *s.project
	return &p, nil
}

func (s *stubGitLab) GetTokenInfo(context.Context, string) (*gitlab.TokenInfo, error) {
	s.record("GetTokenInfo")
	s.mu.Lock()
	defer s.mu.Unlock()
	info := *s.tokenInfo
	return &info, nil
}

func (s *stubGitLab) CurrentUser(context.Context, string) (*gitlab.User, error) {
	s.record("CurrentUser")
	return &gitlab.User{ID: 9, Username: "review-bot"}, nil
}

func (s *stubGitLab) GetMergeRequest(context.Context, string, int64, int64) (*gitlab.MergeRequest, error) {
	s.record("GetMergeRequest")
	return &gitlab.MergeRequest{IID: 7, State: "opened", TargetProjectID: 42, SHA: testSHA}, nil
}

func (s *stubGitLab) GetMemberAccess(context.Context, string, int64, int64) (int, error) {
	s.record("GetMemberAccess")
	return 30, nil
}

func (s *stubGitLab) ListProjectHooks(context.Context, string, int64) ([]gitlab.Hook, error) {
	s.record("ListProjectHooks")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listHookErr != nil {
		return nil, s.listHookErr
	}
	return append([]gitlab.Hook(nil), s.hooks...), nil
}

func (s *stubGitLab) CreateProjectHook(_ context.Context, _ string, _ int64, spec gitlab.HookSpec) (int64, error) {
	s.record("CreateProjectHook")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, gitlab.Hook{
		ID:                    s.hookID,
		URL:                   spec.URL,
		PushEvents:            spec.PushEvents,
		MergeRequestsEvents:   spec.MergeRequestsEvents,
		NoteEvents:            spec.NoteEvents,
		EnableSSLVerification: spec.EnableSSLVerification,
	})
	return s.hookID, nil
}

func (s *stubGitLab) UpdateProjectHook(_ context.Context, _ string, _, hookID int64, spec gitlab.HookSpec) error {
	s.record("UpdateProjectHook")
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.hooks {
		if s.hooks[i].ID != hookID {
			continue
		}
		s.hooks[i].URL = spec.URL
		s.hooks[i].PushEvents = spec.PushEvents
		s.hooks[i].MergeRequestsEvents = spec.MergeRequestsEvents
		s.hooks[i].NoteEvents = spec.NoteEvents
		s.hooks[i].EnableSSLVerification = spec.EnableSSLVerification
		return nil
	}
	return &gitlab.Error{Op: "update_hook", Status: 404}
}

func (s *stubGitLab) DeleteProjectHook(_ context.Context, _ string, _ int64, hookID int64) error {
	s.record("DeleteProjectHook")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedHooks = append(s.deletedHooks, hookID)
	kept := s.hooks[:0]
	for _, hook := range s.hooks {
		if hook.ID != hookID {
			kept = append(kept, hook)
		}
	}
	s.hooks = kept
	return nil
}

type testEnv struct {
	t          *testing.T
	server     *httptest.Server
	store      *sqlite.Store
	cipher     *secret.Cipher
	api        *stubGitLab
	lifecycle  *app.Lifecycle
	logs       *lockedBuffer
	adminToken string
}

func defaultStubGitLab() *stubGitLab {
	return &stubGitLab{
		project: &gitlab.Project{
			ID:                42,
			PathWithNamespace: "group/repo",
			WebURL:            "https://gitlab.example.com/group/repo",
			HTTPURLToRepo:     "https://gitlab.example.com/group/repo.git",
			DefaultBranch:     "main",
		},
		tokenInfo: &gitlab.TokenInfo{Scopes: []string{"api", "read_repository"}, Active: true},
		hookID:    99,
	}
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := sqlite.Open(context.Background(), t.TempDir()+"/review.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cipher, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	api := defaultStubGitLab()
	logs := &lockedBuffer{}
	lifecycle := &app.Lifecycle{}
	profiles := map[string]config.ProfileConfig{"default": {}, "deep": {}}
	defaults := config.ProjectSettings{
		Language: "en", SkipDraft: true, Ignore: []string{"**/*.lock"},
		MaxFiles: 200, MaxDiffLines: 8000, PostFailureNote: true, AllowRepoConfig: true,
	}
	projectService, err := app.NewProjectService(app.ProjectServiceConfig{
		Store: st, GitLab: api, Cipher: cipher, Profiles: profiles,
		PublicURL: "https://review.example.com",
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	webhookService, err := app.NewWebhookService(app.WebhookServiceConfig{
		Store: st, Cipher: cipher, Profiles: profiles, ReviewDefaults: defaults,
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   projectService,
		Webhooks:   webhookService,
		Database:   st,
		Worker:     lifecycle,
		Logger:     newTestLogger(logs),
		Auth: AuthConfig{
			SessionKey:      []byte("0123456789abcdef0123456789abcdef"),
			SessionTTL:      time.Hour,
			BootstrapAdmins: []string{"alice"},
		},
		Access:   st,
		Sessions: st,
		Tokens:   st,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return &testEnv{
		t:          t,
		server:     httptest.NewServer(apiServer.Handler()),
		store:      st,
		cipher:     cipher,
		api:        api,
		lifecycle:  lifecycle,
		logs:       logs,
		adminToken: testAdminToken,
	}
}

func (e *testEnv) close() {
	e.server.Close()
}

// sessionCookie returns a signed alice session. It lets tests that predate
// interactive login exercise business routes with an explicit project grant.
func (e *testEnv) sessionCookie(userID int64, username string) string {
	e.t.Helper()
	id, err := randomToken(32)
	if err != nil {
		e.t.Fatalf("randomToken: %v", err)
	}
	sum := sha256.Sum256([]byte(id))
	now := time.Now()
	if err := e.store.CreateSession(context.Background(), store.Session{
		IDHash: sum[:], UserID: userID, Username: username, Name: username,
		Role: roleAdmin, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}, 10); err != nil {
		e.t.Fatalf("CreateSession: %v", err)
	}
	auth, err := newAuthService(AuthConfig{SessionKey: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		e.t.Fatalf("newAuthService: %v", err)
	}
	return sessionCookieName + "=" + auth.sealSession(id)
}

func newTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

func (e *testEnv) request(method, path, token string, body []byte, headers map[string]string) apiResponse {
	e.t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("ReadAll: %v", err)
	}
	return apiResponse{status: resp.StatusCode, header: resp.Header, body: data}
}

func (e *testEnv) json(method, path, token string, body any) apiResponse {
	e.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
	}
	return e.request(method, path, token, raw, map[string]string{"Content-Type": "application/json"})
}

func (e *testEnv) register(project, token string, auto bool, settings json.RawMessage) apiResponse {
	return e.json(http.MethodPost, "/api/v1/projects", e.adminToken, map[string]any{
		"project":      project,
		"token":        token,
		"auto_webhook": auto,
		"settings":     settings,
	})
}

func decodeMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return value
}

func TestHealthAndReadiness(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	health := env.request(http.MethodGet, "/healthz", "", nil, nil)
	if health.status != http.StatusOK {
		t.Fatalf("health status = %d", health.status)
	}
	ready := env.request(http.MethodGet, "/readyz", "", nil, nil)
	if ready.status != http.StatusServiceUnavailable {
		t.Fatalf("ready before worker start = %d", ready.status)
	}
	env.lifecycle.MarkStarted()
	ready = env.request(http.MethodGet, "/readyz", "", nil, nil)
	if ready.status != http.StatusOK {
		t.Fatalf("ready status = %d, body=%s", ready.status, ready.body)
	}
	env.lifecycle.MarkStopped()
	ready = env.request(http.MethodGet, "/readyz", "", nil, nil)
	if ready.status != http.StatusServiceUnavailable {
		t.Fatalf("ready after worker stop = %d", ready.status)
	}
}

func TestAdminAuthentication(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()
	for _, token := range []string{"", "wrong-token"} {
		resp := env.request(http.MethodGet, "/api/v1/projects", token, nil, nil)
		if resp.status != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d", token, resp.status)
		}
		if !strings.Contains(resp.header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("missing WWW-Authenticate")
		}
		body := decodeMap(t, resp.body)
		errorBody := body["error"].(map[string]any)
		if errorBody["code"] != "unauthenticated" {
			t.Errorf("error = %v", errorBody)
		}
		if _, ok := errorBody["request_id"].(string); !ok {
			t.Errorf("missing request_id: %v", errorBody)
		}
	}
	allowed := env.request(http.MethodGet, "/api/v1/projects", env.adminToken, nil, nil)
	if allowed.status != http.StatusOK {
		t.Fatalf("authorized status = %d, body=%s", allowed.status, allowed.body)
	}
}

func TestRegisterInspectAndRedact(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	resp := env.register("group/repo", testProjectTok, false, json.RawMessage(`{"skip_draft":false}`))
	if resp.status != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", resp.status, resp.body)
	}
	body := decodeMap(t, resp.body)
	project := body["project"].(map[string]any)
	jobID, _ := body["job_id"].(string)
	webhookSecret, _ := body["webhook_secret"].(string)
	if project["id"] != float64(42) || jobID == "" || webhookSecret == "" {
		t.Fatalf("response = %v", body)
	}
	if project["webhook_state"] != "disabled" {
		t.Fatalf("registered webhook state = %v", project["webhook_state"])
	}
	if events, _ := project["webhook_events"].([]any); len(events) != 0 {
		t.Fatalf("registered webhook events = %v", project["webhook_events"])
	}
	text := string(resp.body)
	for _, forbidden := range []string{testProjectTok, "token_enc", "webhook_secret_enc"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("register response leaked %q", forbidden)
		}
	}
	if !strings.Contains(text, webhookSecret) {
		t.Error("one-time webhook secret missing from create response")
	}

	requestID := resp.header.Get("X-Request-ID")
	parsed, err := uuid.Parse(requestID)
	if err != nil || parsed.Version() != 7 {
		t.Errorf("request id = %q", requestID)
	}

	stored, err := env.store.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	token, err := env.cipher.Decrypt(stored.TokenEnc)
	if err != nil || string(token) != testProjectTok {
		t.Fatalf("stored token = %q, %v", token, err)
	}
	secretPlain, err := env.cipher.Decrypt(stored.WebhookSecretEnc)
	if err != nil || string(secretPlain) != webhookSecret {
		t.Fatalf("stored webhook secret mismatch: %v", err)
	}
	if _, err := env.store.GetJob(context.Background(), jobID); err != nil {
		t.Fatalf("onboarding job missing: %v", err)
	}

	cookie := env.sessionCookie(7, "alice")
	if err := env.store.SetProjectGrant(context.Background(), 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatalf("SetProjectGrant: %v", err)
	}
	get := env.request(http.MethodGet, "/api/v1/projects/42", "", nil, map[string]string{"Cookie": cookie})
	if get.status != http.StatusOK {
		t.Fatalf("get status = %d", get.status)
	}
	getText := string(get.body)
	for _, forbidden := range []string{webhookSecret, testProjectTok, "webhook_secret", "token_enc"} {
		if strings.Contains(getText, forbidden) {
			t.Errorf("GET leaked %q: %s", forbidden, getText)
		}
	}
	list := env.request(http.MethodGet, "/api/v1/projects", "", nil, map[string]string{"Cookie": cookie})
	if list.status != http.StatusOK || strings.Contains(string(list.body), webhookSecret) {
		t.Fatalf("list = %d %s", list.status, list.body)
	}

	duplicate := env.register("group/repo", testProjectTok, false, nil)
	if duplicate.status != http.StatusConflict {
		t.Fatalf("duplicate status = %d", duplicate.status)
	}

	logText := env.logs.String()
	for _, forbidden := range []string{testProjectTok, webhookSecret, testAdminToken, "Authorization"} {
		if strings.Contains(logText, forbidden) {
			t.Errorf("logs leaked %q: %s", forbidden, logText)
		}
	}
}

func TestRegisterValidationAndUpstreamErrors(t *testing.T) {
	t.Run("insufficient scopes", func(t *testing.T) {
		env := newTestEnv(t)
		defer env.close()
		env.api.tokenInfo = &gitlab.TokenInfo{Scopes: []string{"api"}, Active: true}
		resp := env.register("42", testProjectTok, false, nil)
		if resp.status != http.StatusBadRequest {
			t.Fatalf("status = %d, body=%s", resp.status, resp.body)
		}
		body := decodeMap(t, resp.body)
		errorBody := body["error"].(map[string]any)
		if errorBody["code"] != "validation_failed" {
			t.Errorf("error = %v", errorBody)
		}
		if requestID := resp.header.Get("X-Request-ID"); errorBody["request_id"] != requestID {
			t.Errorf("request id mismatch: %v vs %v", errorBody["request_id"], requestID)
		}
	})
	t.Run("upstream rejected", func(t *testing.T) {
		env := newTestEnv(t)
		defer env.close()
		env.api.projectErr = &gitlab.Error{Op: "get_project", Status: 403}
		resp := env.register("42", testProjectTok, false, nil)
		if resp.status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, body=%s", resp.status, resp.body)
		}
		errorBody := decodeMap(t, resp.body)["error"].(map[string]any)
		if errorBody["code"] != "gitlab_rejected" {
			t.Errorf("error = %v", errorBody)
		}
		if strings.Contains(string(resp.body), "https://") || strings.Contains(string(resp.body), testProjectTok) {
			t.Errorf("upstream details leaked: %s", resp.body)
		}
	})
	t.Run("unknown json field", func(t *testing.T) {
		env := newTestEnv(t)
		defer env.close()
		resp := env.json(http.MethodPost, "/api/v1/projects", env.adminToken, map[string]any{
			"project": "42", "token": testProjectTok, "bogus": true,
		})
		if resp.status != http.StatusBadRequest {
			t.Fatalf("status = %d", resp.status)
		}
	})
	t.Run("unknown project settings", func(t *testing.T) {
		env := newTestEnv(t)
		defer env.close()
		resp := env.register("42", testProjectTok, false, json.RawMessage(`{"bogus":true}`))
		if resp.status != http.StatusBadRequest {
			t.Fatalf("status = %d", resp.status)
		}
	})
	t.Run("unknown get", func(t *testing.T) {
		env := newTestEnv(t)
		defer env.close()
		resp := env.request(http.MethodGet, "/api/v1/projects/999", env.adminToken, nil, nil)
		if resp.status != http.StatusNotFound {
			t.Fatalf("status = %d", resp.status)
		}
	})
}

func TestWebhookEndpoint(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()
	created := env.register("42", testProjectTok, false, nil)
	if created.status != http.StatusCreated {
		t.Fatalf("register: %d %s", created.status, created.body)
	}
	webhookSecret := decodeMap(t, created.body)["webhook_secret"].(string)
	env.api.resetCalls()

	body := mrHTTPBody(42, 7, "open", "")
	headers := map[string]string{
		"Content-Type":        "application/json",
		"X-Gitlab-Event":      "Merge Request Hook",
		"X-Gitlab-Token":      webhookSecret,
		"X-Gitlab-Event-UUID": uuid.NewString(),
	}
	resp := env.request(http.MethodPost, "/webhooks/gitlab", "", body, headers)
	if resp.status != http.StatusOK {
		t.Fatalf("webhook status = %d, body=%s", resp.status, resp.body)
	}
	result := decodeMap(t, resp.body)
	if result["accepted"] != true || result["job_id"] == "" {
		t.Fatalf("webhook response = %v", result)
	}
	job, err := env.store.GetJob(context.Background(), result["job_id"].(string))
	if err != nil || job.Kind != "review.candidate" {
		t.Fatalf("job = %+v, %v", job, err)
	}
	if env.api.callCount("GetProject") != 0 || env.api.callCount("GetTokenInfo") != 0 {
		t.Fatal("webhook ingress called GitLab")
	}

	replay := env.request(http.MethodPost, "/webhooks/gitlab", "", body, headers)
	if decodeMap(t, replay.body)["ignored"] != "duplicate" {
		t.Fatalf("replay = %s", replay.body)
	}

	wrong := env.request(http.MethodPost, "/webhooks/gitlab", "", body, map[string]string{
		"Content-Type":        "application/json",
		"X-Gitlab-Event":      "Merge Request Hook",
		"X-Gitlab-Token":      "wrong",
		"X-Gitlab-Event-UUID": uuid.NewString(),
	})
	if wrong.status != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", wrong.status)
	}

	missingUUID := env.request(http.MethodPost, "/webhooks/gitlab", "", body, map[string]string{
		"Content-Type":   "application/json",
		"X-Gitlab-Event": "Merge Request Hook",
		"X-Gitlab-Token": webhookSecret,
	})
	if missingUUID.status != http.StatusOK || decodeMap(t, missingUUID.body)["ignored"] != "missing_event_uuid" {
		t.Fatalf("missing uuid = %s", missingUUID.body)
	}

	oversized := bytes.Repeat([]byte("x"), webhookBodyLimit+1)
	large := env.request(http.MethodPost, "/webhooks/gitlab", "", oversized, map[string]string{
		"Content-Type":        "application/json",
		"X-Gitlab-Event":      "Merge Request Hook",
		"X-Gitlab-Token":      webhookSecret,
		"X-Gitlab-Event-UUID": uuid.NewString(),
	})
	if large.status != http.StatusOK || decodeMap(t, large.body)["ignored"] != "body_too_large" {
		t.Fatalf("oversized = %d %s", large.status, large.body)
	}
}

func TestWebhookStorageFailureResponse(t *testing.T) {
	logs := &lockedBuffer{}
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   stubAdmin{},
		Webhooks:   failingWebhook{err: errors.New("disk full")},
		Logger:     newTestLogger(logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(apiServer.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/gitlab", strings.NewReader(`{}`))
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || decodeMap(t, data)["ignored"] != "storage_error" {
		t.Fatalf("response = %d %s", resp.StatusCode, data)
	}
}

func TestPanicRecoveryReturnsRequestID(t *testing.T) {
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   panicAdmin{},
		Webhooks:   failingWebhook{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(apiServer.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/projects", strings.NewReader(`{"project":"42","token":"token"}`))
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, data)
	}
	body := decodeMap(t, data)["error"].(map[string]any)
	if body["code"] != "internal" || body["request_id"] != resp.Header.Get("X-Request-ID") {
		t.Fatalf("body = %v", body)
	}
}

type failingWebhook struct{ err error }

func (f failingWebhook) Handle(context.Context, app.WebhookRequest) (app.WebhookResult, error) {
	return app.WebhookResult{Ignored: "storage_error"}, f.err
}

type panicAdmin struct{}

func (panicAdmin) Register(context.Context, app.RegisterInput) (*app.Registration, error) {
	panic("boom")
}
func (panicAdmin) Validate(context.Context, app.ValidateProjectInput) (*app.ProjectValidation, error) {
	return &app.ProjectValidation{}, nil
}
func (panicAdmin) List(context.Context) ([]store.Project, error) { return nil, nil }
func (panicAdmin) ListFiltered(context.Context, app.ProjectListFilter) ([]store.Project, error) {
	return nil, nil
}
func (panicAdmin) ListFilteredPage(context.Context, app.ProjectListFilter, string, int) ([]store.Project, string, error) {
	return nil, "", nil
}
func (panicAdmin) Get(context.Context, int64) (*store.Project, error) {
	return &store.Project{}, nil
}
func (panicAdmin) Update(context.Context, int64, app.UpdateProjectInput) (*store.Project, error) {
	return &store.Project{}, nil
}
func (panicAdmin) RotateToken(context.Context, int64, string) (*store.Project, error) {
	return &store.Project{}, nil
}
func (panicAdmin) CreateWebhook(context.Context, int64) (*app.WebhookRegistration, error) {
	return &app.WebhookRegistration{}, nil
}
func (panicAdmin) RebuildWebhook(context.Context, int64) (*app.WebhookRegistration, error) {
	return &app.WebhookRegistration{}, nil
}
func (panicAdmin) DisableWebhook(context.Context, int64) (*store.Project, error) {
	return &store.Project{}, nil
}
func (panicAdmin) Delete(context.Context, int64) error { return nil }

type stubAdmin struct{}

func (stubAdmin) Register(context.Context, app.RegisterInput) (*app.Registration, error) {
	return &app.Registration{}, nil
}
func (stubAdmin) Validate(context.Context, app.ValidateProjectInput) (*app.ProjectValidation, error) {
	return &app.ProjectValidation{}, nil
}
func (stubAdmin) List(context.Context) ([]store.Project, error) { return nil, nil }
func (stubAdmin) ListFiltered(context.Context, app.ProjectListFilter) ([]store.Project, error) {
	return nil, nil
}
func (stubAdmin) ListFilteredPage(context.Context, app.ProjectListFilter, string, int) ([]store.Project, string, error) {
	return nil, "", nil
}
func (stubAdmin) Get(context.Context, int64) (*store.Project, error) {
	return &store.Project{}, nil
}
func (stubAdmin) Update(context.Context, int64, app.UpdateProjectInput) (*store.Project, error) {
	return &store.Project{}, nil
}
func (stubAdmin) RotateToken(context.Context, int64, string) (*store.Project, error) {
	return &store.Project{}, nil
}
func (stubAdmin) CreateWebhook(context.Context, int64) (*app.WebhookRegistration, error) {
	return &app.WebhookRegistration{}, nil
}
func (stubAdmin) RebuildWebhook(context.Context, int64) (*app.WebhookRegistration, error) {
	return &app.WebhookRegistration{}, nil
}
func (stubAdmin) DisableWebhook(context.Context, int64) (*store.Project, error) {
	return &store.Project{}, nil
}
func (stubAdmin) Delete(context.Context, int64) error { return nil }

func mrHTTPBody(projectID, iid int64, action, oldrev string) []byte {
	body := map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"id": projectID},
		"object_attributes": map[string]any{
			"iid":               iid,
			"target_project_id": projectID,
			"action":            action,
			"oldrev":            oldrev,
			"target_branch":     "main",
			"last_commit":       map[string]any{"id": testSHA},
			"labels":            []string{},
		},
		"merge_request": map[string]any{
			"iid":               iid,
			"target_project_id": projectID,
			"target_branch":     "main",
			"state":             "opened",
			"labels":            []string{},
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

func TestServerRequiresDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRequestIDHeader(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()
	resp := env.request(http.MethodGet, "/healthz", "", nil, nil)
	id, err := uuid.Parse(resp.header.Get("X-Request-ID"))
	if err != nil || id.Version() != 7 {
		t.Fatalf("request id = %q", resp.header.Get("X-Request-ID"))
	}
}

func TestWebhookSlowBodyBounded(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()
	addr := strings.TrimPrefix(env.server.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	requestHead := fmt.Sprintf(
		"POST /webhooks/gitlab HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n"+
			"Content-Length: 100000\r\nX-Gitlab-Event: Push Hook\r\nX-Gitlab-Token: slow-token\r\n"+
			"X-Gitlab-Event-UUID: %s\r\n\r\n{",
		addr, uuid.NewString())
	if _, err := io.WriteString(conn, requestHead); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("slow body response exceeded bound: %v", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, data)
	}
	body := decodeMap(t, data)
	if body["ignored"] != "request_timeout" {
		t.Fatalf("body = %s", data)
	}
	if body["accepted"] != nil {
		t.Fatalf("slow body was accepted: %s", data)
	}
}

func TestWebhookPanicReturnsIgnored200(t *testing.T) {
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   stubAdmin{},
		Webhooks:   panicWebhook{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(apiServer.Handler())
	defer ts.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/gitlab", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", "token")
	req.Header.Set("X-Gitlab-Event-UUID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, data)
	}
	body := decodeMap(t, data)
	if body["ignored"] != "internal_error" || body["accepted"] != nil {
		t.Fatalf("body = %s", data)
	}
	if body["request_id"] != resp.Header.Get("X-Request-ID") {
		t.Fatalf("request id mismatch: body=%v header=%q", body["request_id"], resp.Header.Get("X-Request-ID"))
	}
}

type panicWebhook struct{}

func (panicWebhook) Handle(context.Context, app.WebhookRequest) (app.WebhookResult, error) {
	panic("webhook boom")
}

func TestProjectWebhookLifecycleEndpoints(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()
	resp := env.register("group/repo", testProjectTok, false, json.RawMessage(`{}`))
	if resp.status != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", resp.status, resp.body)
	}
	const userID = int64(7)
	cookie := env.sessionCookie(userID, "alice")
	if err := env.store.SetProjectGrant(context.Background(), 42, userID, store.ProjectAccessManage); err != nil {
		t.Fatalf("SetProjectGrant: %v", err)
	}
	me := env.request(http.MethodGet, "/api/v1/me", "", nil, map[string]string{"Cookie": cookie})
	if me.status != http.StatusOK {
		t.Fatalf("me status = %d, body=%s", me.status, me.body)
	}
	headers := map[string]string{
		"Cookie":       cookie,
		csrfHeaderName: decodeMap(t, me.body)["csrf_token"].(string),
	}

	enable := env.request(http.MethodPost, "/api/v1/projects/42/webhook", "", nil, headers)
	if enable.status != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", enable.status, enable.body)
	}
	enabled := decodeMap(t, enable.body)
	if enabled["state"] != store.WebhookStateEnabled || enabled["managed_by"] != store.WebhookManagedByService ||
		enabled["hook_id"] != float64(99) {
		t.Fatalf("enable response = %v", enabled)
	}
	events, _ := enabled["events"].([]any)
	if len(events) != 3 {
		t.Fatalf("events = %v", enabled["events"])
	}
	if created, _ := enabled["created_at"].(string); created == "" {
		t.Fatalf("created_at = %v", enabled["created_at"])
	}
	for _, key := range []string{"webhook_secret", "token", "webhook_secret_enc"} {
		if _, ok := enabled[key]; ok {
			t.Fatalf("enable response leaked %q: %s", key, enable.body)
		}
	}

	again := env.request(http.MethodPost, "/api/v1/projects/42/webhook", "", nil, headers)
	if again.status != http.StatusOK {
		t.Fatalf("second enable status = %d, body=%s", again.status, again.body)
	}
	if env.api.callCount("CreateProjectHook") != 1 {
		t.Fatalf("enable created %d hooks", env.api.callCount("CreateProjectHook"))
	}

	rebuild := env.request(http.MethodPost, "/api/v1/projects/42/webhook/rebuild", "", nil, headers)
	if rebuild.status != http.StatusOK {
		t.Fatalf("rebuild status = %d, body=%s", rebuild.status, rebuild.body)
	}
	rebuilt := decodeMap(t, rebuild.body)
	if rebuilt["state"] != store.WebhookStateEnabled || rebuilt["hook_id"] != float64(99) {
		t.Fatalf("rebuild response = %v", rebuilt)
	}
	if env.api.callCount("CreateProjectHook") != 1 || env.api.callCount("UpdateProjectHook") != 2 {
		t.Fatalf("rebuild creates = %d, updates = %d",
			env.api.callCount("CreateProjectHook"), env.api.callCount("UpdateProjectHook"))
	}

	disable := env.request(http.MethodDelete, "/api/v1/projects/42/webhook", "", nil, headers)
	if disable.status != http.StatusOK {
		t.Fatalf("disable status = %d, body=%s", disable.status, disable.body)
	}
	disabled := decodeMap(t, disable.body)
	if disabled["state"] != store.WebhookStateDisabled || disabled["hook_id"] != float64(0) {
		t.Fatalf("disable response = %v", disabled)
	}
	if len(env.api.deletedHooks) != 1 || env.api.deletedHooks[0] != 99 {
		t.Fatalf("deleted hooks = %v", env.api.deletedHooks)
	}
	stored, err := env.store.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if stored.HookID != nil || stored.WebhookState != store.WebhookStateDisabled ||
		stored.WebhookManagedBy != store.WebhookManagedByService {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestProjectWebhookControlRequiresManageGrant(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()
	if resp := env.register("group/repo", testProjectTok, false, nil); resp.status != http.StatusCreated {
		t.Fatalf("register status = %d", resp.status)
	}
	const userID = int64(7)
	cookie := env.sessionCookie(userID, "alice")
	if err := env.store.SetProjectGrant(context.Background(), 42, userID, store.ProjectAccessRead); err != nil {
		t.Fatal(err)
	}
	me := env.request(http.MethodGet, "/api/v1/me", "", nil, map[string]string{"Cookie": cookie})
	if me.status != http.StatusOK {
		t.Fatalf("me status = %d", me.status)
	}
	headers := map[string]string{
		"Cookie":       cookie,
		csrfHeaderName: decodeMap(t, me.body)["csrf_token"].(string),
	}
	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/projects/42/webhook"},
		{http.MethodPost, "/api/v1/projects/42/webhook/rebuild"},
		{http.MethodDelete, "/api/v1/projects/42/webhook"},
	} {
		resp := env.request(request.method, request.path, "", nil, headers)
		if resp.status != http.StatusForbidden {
			t.Fatalf("%s %s status = %d, body=%s", request.method, request.path, resp.status, resp.body)
		}
	}
	if env.api.callCount("CreateProjectHook") != 0 || len(env.api.deletedHooks) != 0 {
		t.Fatalf("read grant reached GitLab")
	}
}
