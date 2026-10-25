package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

const (
	sessionCookieName = "crc_session"
	csrfHeaderName    = "X-CSRF-Token"
	stateCookieName   = "crc_oauth_state"

	oauthStateTTL = 10 * time.Minute
	oauthBodyMax  = 1 << 20

	defaultSessionTTL = 168 * time.Hour
	sessionLimit      = 4096

	roleAdmin  = "admin"
	roleViewer = "viewer"
)

// AuthConfig configures GitLab OAuth login, browser sessions and CSRF.
// OAuth is disabled unless ClientID, ClientSecret and GitLabURL are all set.
type AuthConfig struct {
	GitLabURL       string
	ClientID        string
	ClientSecret    string
	RedirectURL     string
	SessionKey      []byte
	SessionTTL      time.Duration
	SecureCookies   bool
	BootstrapAdmins []string
	HTTPClient      *http.Client
	Now             func() time.Time
}

type authService struct {
	baseURL      string
	clientID     string
	clientSecret string
	redirectURL  string
	key          []byte
	ttl          time.Duration
	secure       bool
	admins       map[string]struct{}
	client       *http.Client
	now          func() time.Time
}

func newAuthService(cfg AuthConfig) (*authService, error) {
	key := cfg.SessionKey
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, errors.New("server: cannot generate a session key")
		}
	}
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.GitLabURL), "/")
	if cfg.ClientID != "" && cfg.ClientSecret != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, errors.New("server: auth gitlab URL must be an absolute URL when OAuth is configured")
		}
	}
	admins := make(map[string]struct{}, len(cfg.BootstrapAdmins))
	for _, name := range cfg.BootstrapAdmins {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			admins[name] = struct{}{}
		}
	}
	return &authService{
		baseURL:      baseURL,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  strings.TrimSpace(cfg.RedirectURL),
		key:          key,
		ttl:          ttl,
		secure:       cfg.SecureCookies,
		admins:       admins,
		client:       client,
		now:          now,
	}, nil
}

func (a *authService) enabled() bool {
	return a.clientID != "" && a.clientSecret != "" && a.baseURL != ""
}

func (a *authService) redirectURI(r *http.Request) string {
	if a.redirectURL != "" {
		return a.redirectURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/auth/callback"
}

func (a *authService) signature(purpose, value string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(purpose))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *authService) issueState() (string, error) {
	nonce, err := randomToken(18)
	if err != nil {
		return "", err
	}
	payload := strconv.FormatInt(a.now().Add(oauthStateTTL).Unix(), 10) + "." + nonce
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return encoded + "." + a.signature("state", encoded), nil
}

func (a *authService) validState(state string) bool {
	if len(state) > 512 {
		return false
	}
	encoded, signature, ok := strings.Cut(state, ".")
	if !ok || encoded == "" || signature == "" {
		return false
	}
	if !hmac.Equal([]byte(signature), []byte(a.signature("state", encoded))) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	expiryText, nonce, ok := strings.Cut(string(raw), ".")
	if !ok || nonce == "" {
		return false
	}
	expiry, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return false
	}
	return a.now().Unix() <= expiry
}

func (a *authService) sealSession(id string) string {
	return id + "." + a.signature("session", id)
}

// csrfFor derives a stable per-session CSRF token from the signing key, so no
// second secret needs a database column.
func (a *authService) csrfFor(id string) string {
	return a.signature("csrf", id)
}

func (a *authService) openSession(value string) (string, bool) {
	if len(value) > 256 {
		return "", false
	}
	id, signature, ok := strings.Cut(value, ".")
	if !ok || id == "" || signature == "" {
		return "", false
	}
	if !hmac.Equal([]byte(signature), []byte(a.signature("session", id))) {
		return "", false
	}
	return id, true
}

type sessionUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}

type session struct {
	id   string
	user sessionUser
	csrf string
}

type principal struct {
	user      sessionUser
	csrf      string
	sessionID string
	bearer    bool
}

const principalKey contextKey = requestIDKey + 1

func principalFrom(ctx context.Context) (principal, bool) {
	value, ok := ctx.Value(principalKey).(principal)
	return value, ok
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.auth.enabled() {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "OAuth login is not configured", nil)
		return
	}
	state, err := s.auth.issueState()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	query := url.Values{
		"client_id":     {s.auth.clientID},
		"redirect_uri":  {s.auth.redirectURI(r)},
		"response_type": {"code"},
		"scope":         {"read_user"},
		"state":         {state},
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/auth",
		HttpOnly: true,
		Secure:   s.auth.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthStateTTL / time.Second),
		Expires:  s.auth.now().Add(oauthStateTTL),
	})
	http.Redirect(w, r, s.auth.baseURL+"/oauth/authorize?"+query.Encode(), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.auth.enabled() {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "OAuth login is not configured", nil)
		return
	}
	query := r.URL.Query()
	if query.Get("error") != "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "GitLab login was not completed", nil)
		return
	}
	state := query.Get("state")
	code := query.Get("code")
	stateCookie, cookieErr := r.Cookie(stateCookieName)
	if state == "" || code == "" || cookieErr != nil ||
		!constantTimeEqualStrings(stateCookie.Value, state) || !s.auth.validState(state) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid or expired login state", nil)
		return
	}
	s.clearStateCookie(w)

	accessToken, err := s.exchangeCode(r.Context(), code, s.auth.redirectURI(r))
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "GitLab authentication failed", nil)
		return
	}
	user, err := s.fetchUser(r.Context(), accessToken)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "GitLab authentication failed", nil)
		return
	}
	user.Role = s.roleFor(user.Username)

	// Rotate any existing session before issuing the new one. A failed delete
	// aborts login: issuing a second cookie would leave a live old session.
	if err := s.deleteSession(r.Context(), r); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	entry, err := s.createSession(r.Context(), user)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A disabled or removed account cannot log in. The response does not
			// distinguish the two cases.
			writeError(w, r, http.StatusForbidden, "forbidden", "account is not allowed to sign in", nil)
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	s.setSessionCookie(w, s.auth.sealSession(entry.id))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteSession(r.Context(), r); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	value, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         value.user.ID,
		"username":   value.user.Username,
		"name":       value.user.Name,
		"role":       value.user.Role,
		"csrf_token": value.csrf,
	})
}

func (s *Server) exchangeCode(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {s.auth.clientID},
		"client_secret": {s.auth.clientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.auth.baseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.auth.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, oauthBodyMax))
		return "", errors.New("server: OAuth token endpoint rejected the request")
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, oauthBodyMax)).Decode(&payload); err != nil {
		return "", err
	}
	if payload.AccessToken == "" {
		return "", errors.New("server: OAuth token response has no access token")
	}
	return payload.AccessToken, nil
}

func (s *Server) fetchUser(ctx context.Context, accessToken string) (sessionUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.auth.baseURL+"/api/v4/user", nil)
	if err != nil {
		return sessionUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := s.auth.client.Do(req)
	if err != nil {
		return sessionUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, oauthBodyMax))
		return sessionUser{}, errors.New("server: GitLab user endpoint rejected the request")
	}
	var payload struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, oauthBodyMax)).Decode(&payload); err != nil {
		return sessionUser{}, err
	}
	if payload.Username == "" {
		return sessionUser{}, errors.New("server: GitLab user response has no username")
	}
	name := payload.Name
	if name == "" {
		name = payload.Username
	}
	return sessionUser{ID: payload.ID, Username: payload.Username, Name: name}, nil
}

func (s *Server) roleFor(username string) string {
	if _, ok := s.auth.admins[strings.ToLower(strings.TrimSpace(username))]; ok {
		return roleAdmin
	}
	return roleViewer
}

// createSession persists the session before the cookie is signed. A storage
// error leaves no usable cookie behind.
func (s *Server) createSession(ctx context.Context, user sessionUser) (session, error) {
	if s.sessions == nil {
		return session{}, errors.New("server: session store is not configured")
	}
	id, err := randomToken(32)
	if err != nil {
		return session{}, err
	}
	now := s.auth.now()
	expires := now.Add(s.auth.ttl)
	sum := sha256.Sum256([]byte(id))
	if err := s.sessions.CreateSession(ctx, store.Session{
		IDHash:    sum[:],
		UserID:    user.ID,
		Username:  user.Username,
		Name:      user.Name,
		Role:      user.Role,
		ExpiresAt: expires,
		CreatedAt: now,
	}, sessionLimit); err != nil {
		return session{}, err
	}
	return session{id: id, user: user, csrf: s.auth.csrfFor(id)}, nil
}

// deleteSession revokes the session named by a valid signed cookie. An
// absent or unsigned cookie is not an error.
func (s *Server) deleteSession(ctx context.Context, r *http.Request) error {
	if s.sessions == nil {
		return nil
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	id, ok := s.auth.openSession(cookie.Value)
	if !ok {
		return nil
	}
	sum := sha256.Sum256([]byte(id))
	return s.sessions.DeleteSession(ctx, sum[:])
}

func (s *Server) sessionFromRequest(r *http.Request) (session, bool) {
	if s.sessions == nil {
		return session{}, false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return session{}, false
	}
	id, ok := s.auth.openSession(cookie.Value)
	if !ok {
		return session{}, false
	}
	sum := sha256.Sum256([]byte(id))
	stored, err := s.sessions.GetSession(r.Context(), sum[:], s.auth.now())
	if err != nil || stored == nil {
		return session{}, false
	}
	// Roles come from the current configuration, not from login time, so
	// removing a bootstrap admin also downgrades existing sessions.
	user := sessionUser{
		ID:       stored.UserID,
		Username: stored.Username,
		Name:     stored.Name,
		Role:     s.roleFor(stored.Username),
	}
	return session{id: id, user: user, csrf: s.auth.csrfFor(id)}, true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.auth.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.auth.ttl / time.Second),
		Expires:  s.auth.now().Add(s.auth.ttl),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.auth.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func (s *Server) clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/auth",
		HttpOnly: true,
		Secure:   s.auth.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "not_found", "not found", nil)
}

func (s *Server) authenticate(r *http.Request) (principal, bool) {
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		if s.authorized(r) {
			return principal{
				user:   sessionUser{Username: "api", Name: "API client", Role: roleAdmin},
				bearer: true,
			}, true
		}
		raw := bearerValue(r)
		if raw == "" || s.tokens == nil {
			return principal{}, false
		}
		sum := sha256.Sum256([]byte(raw))
		user, ok := s.authenticateAPIToken(r, sum[:])
		if !ok {
			return principal{}, false
		}
		return principal{user: user, bearer: true}, true
	}
	entry, ok := s.sessionFromRequest(r)
	if !ok {
		return principal{}, false
	}
	return principal{user: entry.user, csrf: entry.csrf, sessionID: entry.id}, true
}

// authenticateAPIToken resolves a persisted token digest. Failures are
// indistinguishable to the caller and never log token material.
func (s *Server) authenticateAPIToken(r *http.Request, hash []byte) (sessionUser, bool) {
	_, owner, err := s.tokens.FindAPITokenByHash(r.Context(), hash, s.now())
	if err != nil || owner == nil {
		return sessionUser{}, false
	}
	// The stored users.role reflects login time. Re-evaluate against the
	// current bootstrap_admins so removing an admin also downgrades tokens
	// minted earlier. The configured bootstrap bearer token keeps admin.
	return sessionUser{
		ID:       owner.ID,
		Username: owner.Username,
		Name:     owner.Name,
		Role:     s.roleFor(owner.Username),
	}, true
}

// bearerValue extracts the trimmed bearer value. The bootstrap token check
// reuses it so both authentication paths agree on the header syntax.
func bearerValue(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, value, ok := strings.Cut(raw, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// apiAuth authenticates browser sessions and bearer clients and requires the
// per-session CSRF token for unsafe session requests. Authorization is a
// separate per-route decision: a route must pick an explicit policy, and an
// unknown policy denies. Bearer clients are not session-bound and need no
// CSRF token.
func (s *Server) apiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required", nil)
			return
		}
		if !safeMethod(r.Method) && !value.bearer {
			token := strings.TrimSpace(r.Header.Get(csrfHeaderName))
			if token == "" || !constantTimeEqualStrings(value.csrf, token) {
				writeError(w, r, http.StatusForbidden, "forbidden", "invalid CSRF token", nil)
				return
			}
		}
		ctx := context.WithValue(r.Context(), principalKey, value)
		next(w, r.WithContext(ctx))
	}
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func randomToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
