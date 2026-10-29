package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestAuthStatusReportsOAuthState(t *testing.T) {
	env := newAuthEnv(t)
	resp := env.request(env.client(), http.MethodGet, "/auth/status", nil, nil)
	if resp.status != http.StatusOK || !strings.Contains(string(resp.body), `"oauth_enabled":true`) {
		t.Fatalf("status = %d body=%s", resp.status, resp.body)
	}

	env.disabledOAuth = true
	env.restart()
	resp = env.request(env.client(), http.MethodGet, "/auth/status", nil, nil)
	if resp.status != http.StatusOK || !strings.Contains(string(resp.body), `"oauth_enabled":false`) {
		t.Fatalf("disabled status = %d body=%s", resp.status, resp.body)
	}
}

func TestBrowserLoginRedirectsWhenOAuthDisabled(t *testing.T) {
	env := newAuthEnv(t)
	env.disabledOAuth = true
	env.restart()
	client := env.client()

	resp := env.request(client, http.MethodGet, "/auth/login", nil, map[string]string{"Accept": "text/html"})
	if resp.status != http.StatusSeeOther || resp.header.Get("Location") != "/?auth_error=oauth_disabled" {
		t.Fatalf("browser login = %d %q, want redirect", resp.status, resp.header.Get("Location"))
	}
	resp = env.request(client, http.MethodGet, "/auth/login", nil, nil)
	if resp.status != http.StatusServiceUnavailable {
		t.Fatalf("JSON login = %d, want 503", resp.status)
	}
}

func TestBrowserCallbackErrorsRedirect(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/auth/callback?error=access_denied", "/?auth_error=oauth_denied"},
		{"/auth/callback?state=bad&code=x", "/?auth_error=oauth_state"},
	} {
		resp := env.request(client, http.MethodGet, tc.path, nil, map[string]string{"Accept": "text/html"})
		if resp.status != http.StatusSeeOther || resp.header.Get("Location") != tc.want {
			t.Fatalf("%s = %d %q, want %q", tc.path, resp.status, resp.header.Get("Location"), tc.want)
		}
	}
}

func TestLoginKeepsJSONWhenHTMLIsExplicitlyRejected(t *testing.T) {
	env := newAuthEnv(t)
	env.disabledOAuth = true
	env.restart()
	resp := env.request(env.client(), http.MethodGet, "/auth/login", nil, map[string]string{
		"Accept": "application/json, text/html;q=0",
	})
	if resp.status != http.StatusServiceUnavailable ||
		!strings.Contains(resp.header.Get("Content-Type"), "application/json") ||
		!strings.Contains(string(resp.body), "OAuth login is not configured") {
		t.Fatalf("status=%d content-type=%q body=%s", resp.status, resp.header.Get("Content-Type"), resp.body)
	}
}

func TestAuthEnabledTreatsBlankCredentialsAsDisabled(t *testing.T) {
	svc := &authService{baseURL: "https://gitlab.example", clientID: "  ", clientSecret: "secret"}
	if svc.enabled() {
		t.Fatal("blank client ID must disable OAuth")
	}
}
