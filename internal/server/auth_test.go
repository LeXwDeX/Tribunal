package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

const (
	testOAuthClientID     = "test-client-id"
	testOAuthClientSecret = "test-client-secret-value"
	testOAuthRedirectURL  = "http://review.example.test/auth/callback"
)

type fakeClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(d)
	c.mu.Unlock()
}

type stubWebhookAcceptor struct{}

func (stubWebhookAcceptor) Handle(context.Context, app.WebhookRequest) (app.WebhookResult, error) {
	return app.WebhookResult{Ignored: "test"}, nil
}

type authEnv struct {
	t        *testing.T
	server   *httptest.Server
	gitlab   *httptest.Server
	clock    *fakeClock
	logs     *lockedBuffer
	key      []byte
	admins   []string
	dsn      string
	db       *sqlite.Store
	sessions SessionStore
	secure   bool
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	env := &authEnv{
		t:      t,
		gitlab: newFakeGitLab(t),
		clock:  &fakeClock{current: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)},
		logs:   &lockedBuffer{},
		key:    []byte("0123456789abcdef0123456789abcdef"),
		admins: []string{"alice"},
		dsn:    filepath.Join(t.TempDir(), "sessions.db"),
	}
	t.Cleanup(func() {
		env.stop()
		env.gitlab.Close()
	})
	env.start()
	return env
}

func (e *authEnv) start() {
	e.t.Helper()
	if e.db == nil {
		db, err := sqlite.Open(context.Background(), e.dsn)
		if err != nil {
			e.t.Fatalf("sqlite.Open: %v", err)
		}
		e.db = db
	}
	sessions := e.sessions
	if sessions == nil {
		sessions = e.db
	}
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   stubAdmin{},
		Webhooks:   stubWebhookAcceptor{},
		Auth: AuthConfig{
			GitLabURL:       e.gitlab.URL,
			ClientID:        testOAuthClientID,
			ClientSecret:    testOAuthClientSecret,
			RedirectURL:     testOAuthRedirectURL,
			SessionKey:      e.key,
			SessionTTL:      time.Hour,
			SecureCookies:   e.secure,
			BootstrapAdmins: e.admins,
			HTTPClient:      e.gitlab.Client(),
			Now:             e.clock.Now,
		},
		Sessions: sessions,
		Tokens:   e.db,
		Access:   e.db,
		Logger:   newTestLogger(e.logs),
	})
	if err != nil {
		e.t.Fatalf("server.New: %v", err)
	}
	e.server = httptest.NewServer(apiServer.Handler())
}

func (e *authEnv) stop() {
	if e.server != nil {
		e.server.Close()
		e.server = nil
	}
	if e.db != nil {
		_ = e.db.Close()
		e.db = nil
	}
}

// restart rebuilds the HTTP server, keeping the configured session key. A
// persisted session store must make the old cookie keep working.
func (e *authEnv) restart() {
	e.t.Helper()
	e.stop()
	e.start()
}

// clientWithCookie returns a client that sends cookie to the current server
// without a cookie jar, so a restart (new port) does not drop it.
func (e *authEnv) clientWithCookie(cookie *http.Cookie) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &cookieTransport{cookie: cookie, base: http.DefaultTransport},
	}
}

type cookieTransport struct {
	cookie *http.Cookie
	base   http.RoundTripper
}

func (t *cookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.AddCookie(t.cookie)
	return t.base.RoundTrip(clone)
}

func newFakeGitLab(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			if err := r.ParseForm(); err != nil {
				http.Error(w, "form", http.StatusBadRequest)
				return
			}
			form := r.Form
			if form.Get("client_id") != testOAuthClientID ||
				form.Get("client_secret") != testOAuthClientSecret ||
				form.Get("grant_type") != "authorization_code" ||
				form.Get("redirect_uri") != testOAuthRedirectURL {
				http.Error(w, "invalid client", http.StatusUnauthorized)
				return
			}
			code := form.Get("code")
			if !strings.HasPrefix(code, "code-") {
				http.Error(w, "invalid code", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-token-` + code + `","token_type":"bearer","scope":"read_user"}`))
		case "/api/v4/user":
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !strings.HasPrefix(token, "access-token-code-") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			username := "bob"
			name := "Bob Viewer"
			id := 8
			if token == "access-token-code-admin" {
				username, name, id = "alice", "Alice Admin", 7
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":` + strconv.Itoa(id) + `,"username":"` + username + `","name":"` + name + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func (e *authEnv) client() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (e *authEnv) request(client *http.Client, method, path string, body []byte, headers map[string]string) apiResponse {
	e.t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
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

func (e *authEnv) startLogin(client *http.Client) (string, *url.URL) {
	e.t.Helper()
	resp := e.request(client, http.MethodGet, "/auth/login", nil, nil)
	if resp.status != http.StatusFound {
		e.t.Fatalf("login status = %d, body=%s", resp.status, resp.body)
	}
	location := resp.header.Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		e.t.Fatalf("parse Location %q: %v", location, err)
	}
	if parsed.Scheme+"://"+parsed.Host != e.gitlab.URL || parsed.Path != "/oauth/authorize" {
		e.t.Fatalf("login Location = %q, want %s/oauth/authorize", location, e.gitlab.URL)
	}
	return parsed.Query().Get("state"), parsed
}

func (e *authEnv) callback(client *http.Client, code, state string) apiResponse {
	e.t.Helper()
	query := url.Values{"code": {code}, "state": {state}}
	return e.request(client, http.MethodGet, "/auth/callback?"+query.Encode(), nil, nil)
}

func (e *authEnv) loginAs(client *http.Client, code string) *http.Cookie {
	e.t.Helper()
	state, _ := e.startLogin(client)
	resp := e.callback(client, code, state)
	if resp.status != http.StatusSeeOther {
		e.t.Fatalf("callback status = %d, body=%s", resp.status, resp.body)
	}
	var session *http.Cookie
	for _, cookie := range (&http.Response{Header: resp.header}).Cookies() {
		if cookie.Name == sessionCookieName {
			session = cookie
		}
	}
	if session == nil {
		e.t.Fatal("callback did not set a session cookie")
	}
	return session
}

func TestOAuthLoginRedirect(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()

	state, parsed := env.startLogin(client)
	if state == "" {
		t.Fatal("login redirect has no state")
	}
	query := parsed.Query()
	if query.Get("client_id") != testOAuthClientID {
		t.Errorf("client_id = %q", query.Get("client_id"))
	}
	if query.Get("redirect_uri") != testOAuthRedirectURL {
		t.Errorf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	if query.Get("scope") != "read_user" {
		t.Errorf("scope = %q", query.Get("scope"))
	}
	if query.Get("response_type") != "code" {
		t.Errorf("response_type = %q", query.Get("response_type"))
	}
	if strings.Contains(state, testOAuthClientSecret) {
		t.Error("state contains the client secret")
	}
}

func TestOAuthCallbackIssuesSessionAndMapsRoles(t *testing.T) {
	env := newAuthEnv(t)

	adminClient := env.client()
	adminCookie := env.loginAs(adminClient, "code-admin")
	if !adminCookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if adminCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v", adminCookie.SameSite)
	}
	if adminCookie.Secure {
		t.Error("session cookie unexpectedly Secure when not configured")
	}
	if adminCookie.Path != "/" || adminCookie.MaxAge <= 0 {
		t.Errorf("cookie path/max-age = %q/%d", adminCookie.Path, adminCookie.MaxAge)
	}
	for _, secret := range []string{"access-token", "code-admin", testOAuthClientSecret, testOAuthClientID} {
		if strings.Contains(adminCookie.Value, secret) {
			t.Errorf("cookie value leaks %q", secret)
		}
	}

	me := env.request(adminClient, http.MethodGet, "/api/v1/me", nil, nil)
	if me.status != http.StatusOK {
		t.Fatalf("me status = %d, body=%s", me.status, me.body)
	}
	body := decodeMap(t, me.body)
	if body["id"] != float64(7) || body["username"] != "alice" || body["name"] != "Alice Admin" || body["role"] != roleAdmin {
		t.Fatalf("me = %v", body)
	}
	csrf, _ := body["csrf_token"].(string)
	if csrf == "" {
		t.Fatal("me did not return a CSRF token")
	}

	viewerClient := env.client()
	env.loginAs(viewerClient, "code-viewer")
	viewerMe := env.request(viewerClient, http.MethodGet, "/api/v1/me", nil, nil)
	if viewerMe.status != http.StatusOK {
		t.Fatalf("viewer me status = %d, body=%s", viewerMe.status, viewerMe.body)
	}
	viewerBody := decodeMap(t, viewerMe.body)
	if viewerBody["username"] != "bob" || viewerBody["role"] != roleViewer {
		t.Fatalf("viewer me = %v", viewerBody)
	}

	for _, secret := range []string{"access-token", "code-admin", testOAuthClientSecret, "code-viewer"} {
		if strings.Contains(env.logs.String(), secret) {
			t.Errorf("logs leak %q: %s", secret, env.logs.String())
		}
	}
}

func TestOAuthCallbackRejectsInvalidState(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()

	state, _ := env.startLogin(client)
	resp := env.callback(client, "code-admin", state+"tampered")
	if resp.status != http.StatusBadRequest {
		t.Fatalf("tampered state status = %d, body=%s", resp.status, resp.body)
	}
	if len(resp.header.Values("Set-Cookie")) != 0 {
		t.Error("tampered state set a cookie")
	}

	state, _ = env.startLogin(client)
	env.clock.Advance(oauthStateTTL + time.Second)
	resp = env.callback(client, "code-admin", state)
	if resp.status != http.StatusBadRequest {
		t.Fatalf("expired state status = %d, body=%s", resp.status, resp.body)
	}

	resp = env.request(client, http.MethodGet, "/auth/callback?error=access_denied", nil, nil)
	if resp.status != http.StatusBadRequest {
		t.Fatalf("OAuth error status = %d, body=%s", resp.status, resp.body)
	}

	state, _ = env.startLogin(client)
	resp = env.callback(client, "not-a-code", state)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("bad code status = %d, body=%s", resp.status, resp.body)
	}

	me := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	if me.status != http.StatusUnauthorized {
		t.Fatalf("me after rejected callbacks = %d", me.status)
	}
}

func TestLogoutClearsSession(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	env.loginAs(client, "code-admin")

	resp := env.request(client, http.MethodPost, "/auth/logout", nil, nil)
	if resp.status != http.StatusNoContent {
		t.Fatalf("logout status = %d, body=%s", resp.status, resp.body)
	}
	var cleared bool
	for _, cookie := range (&http.Response{Header: resp.header}).Cookies() {
		if cookie.Name == sessionCookieName && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not clear the session cookie")
	}
	me := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	if me.status != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d", me.status)
	}
	again := env.request(client, http.MethodPost, "/auth/logout", nil, nil)
	if again.status != http.StatusNoContent {
		t.Fatalf("second logout = %d", again.status)
	}
}

func TestSessionCookieFlagsWhenSecure(t *testing.T) {
	env := newAuthEnv(t)
	env.secure = true
	env.restart()
	cookie := env.loginAs(env.client(), "code-admin")
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("secure cookie flags = %+v", cookie)
	}
}

func TestSessionSurvivesRestartWithSameKey(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	cookie := env.loginAs(client, "code-admin")
	before := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	if before.status != http.StatusOK {
		t.Fatalf("me before restart = %d, body=%s", before.status, before.body)
	}
	csrfBefore := decodeMap(t, before.body)["csrf_token"]

	env.restart()

	resumed := env.clientWithCookie(cookie)
	after := env.request(resumed, http.MethodGet, "/api/v1/me", nil, nil)
	if after.status != http.StatusOK {
		t.Fatalf("me after restart = %d, body=%s", after.status, after.body)
	}
	body := decodeMap(t, after.body)
	if body["id"] != float64(7) || body["username"] != "alice" || body["role"] != roleAdmin {
		t.Fatalf("me after restart = %v", body)
	}
	if body["csrf_token"] != csrfBefore {
		t.Fatalf("csrf token changed across restart: %q -> %q", csrfBefore, body["csrf_token"])
	}
}

func TestSessionKeyRotationInvalidatesCookie(t *testing.T) {
	env := newAuthEnv(t)
	cookie := env.loginAs(env.client(), "code-admin")
	env.key = []byte("abcdef0123456789abcdef0123456789")
	env.restart()

	resp := env.request(env.clientWithCookie(cookie), http.MethodGet, "/api/v1/me", nil, nil)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("me with rotated key = %d, body=%s", resp.status, resp.body)
	}
}

func TestLogoutRevokesSessionAcrossRestart(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	cookie := env.loginAs(client, "code-admin")
	resp := env.request(client, http.MethodPost, "/auth/logout", nil, nil)
	if resp.status != http.StatusNoContent {
		t.Fatalf("logout status = %d, body=%s", resp.status, resp.body)
	}

	env.restart()
	after := env.request(env.clientWithCookie(cookie), http.MethodGet, "/api/v1/me", nil, nil)
	if after.status != http.StatusUnauthorized {
		t.Fatalf("me after logout and restart = %d, body=%s", after.status, after.body)
	}
}

func TestAdminRoleRecomputedAfterRestart(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	cookie := env.loginAs(client, "code-admin")
	csrf := decodeMap(t, env.request(client, http.MethodGet, "/api/v1/me", nil, nil).body)["csrf_token"].(string)

	env.admins = nil
	env.restart()

	resumed := env.clientWithCookie(cookie)
	me := env.request(resumed, http.MethodGet, "/api/v1/me", nil, nil)
	if me.status != http.StatusOK {
		t.Fatalf("me after admin removal = %d, body=%s", me.status, me.body)
	}
	if role := decodeMap(t, me.body)["role"]; role != roleViewer {
		t.Fatalf("role after admin removal = %v", role)
	}
	write := env.request(resumed, http.MethodPost, "/api/v1/projects", []byte(`{"project":"group/repo","token":"glpat-token"}`), map[string]string{
		"Content-Type": "application/json",
		csrfHeaderName: csrf,
	})
	if write.status != http.StatusForbidden {
		t.Fatalf("write after admin removal = %d, body=%s", write.status, write.body)
	}
}

func TestSessionExpiryBoundary(t *testing.T) {
	env := newAuthEnv(t)
	cookie := env.loginAs(env.client(), "code-admin")

	env.clock.Advance(time.Hour - time.Second)
	alive := env.request(env.clientWithCookie(cookie), http.MethodGet, "/api/v1/me", nil, nil)
	if alive.status != http.StatusOK {
		t.Fatalf("me before expiry = %d, body=%s", alive.status, alive.body)
	}

	env.clock.Advance(time.Second)
	expired := env.request(env.clientWithCookie(cookie), http.MethodGet, "/api/v1/me", nil, nil)
	if expired.status != http.StatusUnauthorized {
		t.Fatalf("me at expiry = %d, body=%s", expired.status, expired.body)
	}
}

func TestCallbackRotatesExistingSession(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	first := env.loginAs(client, "code-admin")
	env.loginAs(client, "code-admin")

	old := env.request(env.clientWithCookie(first), http.MethodGet, "/api/v1/me", nil, nil)
	if old.status != http.StatusUnauthorized {
		t.Fatalf("old cookie after rotation = %d, body=%s", old.status, old.body)
	}
	current := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	if current.status != http.StatusOK {
		t.Fatalf("new cookie after rotation = %d, body=%s", current.status, current.body)
	}
}

func TestOAuthCallbackRejectsStateReplay(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	state, _ := env.startLogin(client)
	first := env.callback(client, "code-admin", state)
	if first.status != http.StatusSeeOther {
		t.Fatalf("first callback = %d, body=%s", first.status, first.body)
	}
	replay := env.callback(client, "code-admin", state)
	if replay.status != http.StatusBadRequest {
		t.Fatalf("replayed state = %d, body=%s", replay.status, replay.body)
	}
}

type failingSessionStore struct {
	createErr error
	getErr    error
	deleteErr error
}

func (f failingSessionStore) CreateSession(context.Context, store.Session, int) error {
	return f.createErr
}

func (f failingSessionStore) GetSession(context.Context, []byte, time.Time) (*store.Session, error) {
	return nil, f.getErr
}

func (f failingSessionStore) DeleteSession(context.Context, []byte) error {
	return f.deleteErr
}

func TestCallbackFailsClosedWhenSessionCannotPersist(t *testing.T) {
	env := newAuthEnv(t)
	env.sessions = failingSessionStore{createErr: errors.New("disk full")}
	env.restart()

	client := env.client()
	state, _ := env.startLogin(client)
	resp := env.callback(client, "code-admin", state)
	if resp.status != http.StatusInternalServerError {
		t.Fatalf("callback with failing store = %d, body=%s", resp.status, resp.body)
	}
	for _, cookie := range (&http.Response{Header: resp.header}).Cookies() {
		if cookie.Name == sessionCookieName {
			t.Fatalf("callback signed a session cookie without persistence: %+v", cookie)
		}
	}
	if me := env.request(client, http.MethodGet, "/api/v1/me", nil, nil); me.status != http.StatusUnauthorized {
		t.Fatalf("me after failed persist = %d", me.status)
	}
}

func TestSessionLookupFailureIsUnauthenticated(t *testing.T) {
	env := newAuthEnv(t)
	cookie := env.loginAs(env.client(), "code-admin")
	env.sessions = failingSessionStore{getErr: errors.New("database unavailable")}
	env.restart()

	resp := env.request(env.clientWithCookie(cookie), http.MethodGet, "/api/v1/me", nil, nil)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("me with failing lookup = %d, body=%s", resp.status, resp.body)
	}
}

func TestLogoutFailsClosedWhenSessionCannotBeRevoked(t *testing.T) {
	env := newAuthEnv(t)
	cookie := env.loginAs(env.client(), "code-admin")
	env.sessions = failingSessionStore{deleteErr: errors.New("database unavailable")}
	env.restart()

	resp := env.request(env.clientWithCookie(cookie), http.MethodPost, "/auth/logout", nil, nil)
	if resp.status != http.StatusInternalServerError {
		t.Fatalf("logout with failing store = %d, body=%s", resp.status, resp.body)
	}
	for _, cleared := range (&http.Response{Header: resp.header}).Cookies() {
		if cleared.Name == sessionCookieName && cleared.MaxAge < 0 {
			t.Fatal("logout cleared the cookie although revocation failed")
		}
	}
}

func TestMeReturnsCSRFToken(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()

	unauthenticated := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	if unauthenticated.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated me = %d", unauthenticated.status)
	}

	env.loginAs(client, "code-admin")
	first := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	second := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	if first.status != http.StatusOK || second.status != http.StatusOK {
		t.Fatalf("me statuses = %d/%d", first.status, second.status)
	}
	firstBody := decodeMap(t, first.body)
	secondBody := decodeMap(t, second.body)
	token, _ := firstBody["csrf_token"].(string)
	if token == "" || token != secondBody["csrf_token"] {
		t.Fatalf("csrf tokens = %q/%q", token, secondBody["csrf_token"])
	}
}

func TestCSRFRequiredForUnsafeSessionRequests(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	env.loginAs(client, "code-admin")

	me := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	csrf := decodeMap(t, me.body)["csrf_token"].(string)

	payload := []byte(`{"project":"group/repo","token":"glpat-token"}`)
	resp := env.request(client, http.MethodPost, "/api/v1/projects", payload, map[string]string{"Content-Type": "application/json"})
	if resp.status != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, body=%s", resp.status, resp.body)
	}
	if code := decodeMap(t, resp.body)["error"].(map[string]any)["code"]; code != "forbidden" {
		t.Errorf("missing CSRF code = %v", code)
	}

	resp = env.request(client, http.MethodPost, "/api/v1/projects", payload, map[string]string{
		"Content-Type": "application/json",
		csrfHeaderName: "wrong-token",
	})
	if resp.status != http.StatusForbidden {
		t.Fatalf("mismatched CSRF status = %d, body=%s", resp.status, resp.body)
	}

	resp = env.request(client, http.MethodPost, "/api/v1/projects", payload, map[string]string{
		"Content-Type": "application/json",
		csrfHeaderName: csrf,
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("valid CSRF status = %d, body=%s", resp.status, resp.body)
	}
}

func TestViewerCannotUseUnsafeMethods(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	env.loginAs(client, "code-viewer")

	list := env.request(client, http.MethodGet, "/api/v1/projects", nil, nil)
	if list.status != http.StatusOK {
		t.Fatalf("viewer list = %d, body=%s", list.status, list.body)
	}
	if !strings.Contains(string(list.body), `"projects"`) {
		t.Fatalf("viewer list body = %s", list.body)
	}

	me := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	csrf := decodeMap(t, me.body)["csrf_token"].(string)
	resp := env.request(client, http.MethodPost, "/api/v1/projects", []byte(`{"project":"group/repo","token":"glpat-token"}`), map[string]string{
		"Content-Type": "application/json",
		csrfHeaderName: csrf,
	})
	if resp.status != http.StatusForbidden {
		t.Fatalf("viewer write status = %d, body=%s", resp.status, resp.body)
	}
	if code := decodeMap(t, resp.body)["error"].(map[string]any)["code"]; code != "forbidden" {
		t.Errorf("viewer write code = %v", code)
	}
}

func TestUnauthenticatedAPIReturns401(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()

	for _, request := range []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodGet, "/api/v1/me", nil},
		{http.MethodGet, "/api/v1/projects", nil},
		{http.MethodPost, "/api/v1/projects", []byte(`{"project":"group/repo","token":"glpat-token"}`)},
	} {
		resp := env.request(client, request.method, request.path, request.body, map[string]string{"Content-Type": "application/json"})
		if resp.status != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, body=%s", request.method, request.path, resp.status, resp.body)
		}
		if !strings.Contains(resp.header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s %s missing WWW-Authenticate", request.method, request.path)
		}
		body := decodeMap(t, resp.body)
		errorBody := body["error"].(map[string]any)
		if errorBody["code"] != "unauthenticated" || errorBody["request_id"] == "" {
			t.Errorf("%s %s error = %v", request.method, request.path, errorBody)
		}
	}
}

func TestAPITokenRoleFollowsCurrentBootstrapAdmins(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	env.loginAs(client, "code-admin")
	csrf := decodeMap(t, env.request(client, http.MethodGet, "/api/v1/me", nil, nil).body)["csrf_token"].(string)
	created := env.request(client, http.MethodPost, "/api/v1/api-tokens", []byte(`{"name":"ci"}`), map[string]string{
		"Content-Type": "application/json",
		csrfHeaderName: csrf,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create api token = %d, body=%s", created.status, created.body)
	}
	token, _ := decodeMap(t, created.body)["token"].(string)
	if !strings.HasPrefix(token, apiTokenPrefix) {
		t.Fatalf("created token = %q", token)
	}

	// The token outlives the admin configuration that minted it. Its role must
	// follow the current configuration, not the stored users.role.
	env.admins = nil
	env.restart()

	me := env.request(env.client(), http.MethodGet, "/api/v1/me", nil, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if me.status != http.StatusOK {
		t.Fatalf("api token me = %d, body=%s", me.status, me.body)
	}
	if role := decodeMap(t, me.body)["role"]; role != roleViewer {
		t.Fatalf("api token role after admin removal = %v", role)
	}
	write := env.request(env.client(), http.MethodPost, "/api/v1/projects", []byte(`{"project":"group/repo","token":"glpat-token"}`), map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	})
	if write.status != http.StatusForbidden {
		t.Fatalf("api token write after admin removal = %d, body=%s", write.status, write.body)
	}
}

func TestBootstrapBearerStaysAdminWithoutBootstrapAdmins(t *testing.T) {
	env := newAuthEnv(t)
	env.admins = nil
	env.restart()

	me := env.request(env.client(), http.MethodGet, "/api/v1/me", nil, map[string]string{
		"Authorization": "Bearer " + testAdminToken,
	})
	if me.status != http.StatusOK {
		t.Fatalf("bootstrap bearer me = %d, body=%s", me.status, me.body)
	}
	if role := decodeMap(t, me.body)["role"]; role != roleAdmin {
		t.Fatalf("bootstrap bearer role = %v", role)
	}
	write := env.request(env.client(), http.MethodPost, "/api/v1/projects", []byte(`{"project":"group/repo","token":"glpat-token"}`), map[string]string{
		"Authorization": "Bearer " + testAdminToken,
		"Content-Type":  "application/json",
	})
	if write.status != http.StatusCreated {
		t.Fatalf("bootstrap bearer write = %d, body=%s", write.status, write.body)
	}
}

func TestAdminBearerStillWorks(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()

	resp := env.request(client, http.MethodPost, "/api/v1/projects", []byte(`{"project":"group/repo","token":"glpat-token"}`), map[string]string{
		"Authorization": "Bearer " + testAdminToken,
		"Content-Type":  "application/json",
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("bearer register status = %d, body=%s", resp.status, resp.body)
	}

	me := env.request(client, http.MethodGet, "/api/v1/me", nil, map[string]string{
		"Authorization": "Bearer " + testAdminToken,
	})
	if me.status != http.StatusOK {
		t.Fatalf("bearer me status = %d, body=%s", me.status, me.body)
	}
	if role := decodeMap(t, me.body)["role"]; role != roleAdmin {
		t.Fatalf("bearer me role = %v", role)
	}
}
