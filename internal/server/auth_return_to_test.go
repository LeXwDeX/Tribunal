package server

import (
	"net/http"
	"net/url"
	"testing"
)

func loginWithReturnTo(t *testing.T, env *authEnv, client *http.Client, returnTo string) *url.URL {
	t.Helper()
	path := "/auth/login"
	if returnTo != "" {
		path += "?return_to=" + url.QueryEscape(returnTo)
	}
	response := env.request(client, http.MethodGet, path, nil, nil)
	if response.status != http.StatusFound {
		t.Fatalf("login status = %d, body=%s", response.status, response.body)
	}
	parsed, err := url.Parse(response.header.Get("Location"))
	if err != nil {
		t.Fatalf("parse login Location: %v", err)
	}
	if parsed.Path != "/oauth/authorize" {
		t.Fatalf("login Location = %q", response.header.Get("Location"))
	}
	return parsed
}

func TestLoginReturnToRedirectsAfterCallback(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	authorize := loginWithReturnTo(t, env, client, "/reviews/wf-1?tab=findings")
	state := authorize.Query().Get("state")
	if state == "" {
		t.Fatal("login redirect has no state")
	}
	callback := env.callback(client, "code-admin", state)
	if callback.status != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
	}
	if location := callback.header.Get("Location"); location != "/reviews/wf-1?tab=findings" {
		t.Fatalf("callback Location = %q, want the original deep link", location)
	}
}

func TestLoginRejectsUnsafeReturnTo(t *testing.T) {
	for _, returnTo := range []string{
		"https://evil.example/steal",
		"//evil.example/steal",
		"/\\evil.example",
		"javascript:alert(1)",
		"auth/callback",
		"/auth/logout",
		"",
	} {
		t.Run(returnTo, func(t *testing.T) {
			env := newAuthEnv(t)
			client := env.client()
			authorize := loginWithReturnTo(t, env, client, returnTo)
			callback := env.callback(client, "code-admin", authorize.Query().Get("state"))
			if callback.status != http.StatusSeeOther {
				t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
			}
			if location := callback.header.Get("Location"); location != "/" {
				t.Fatalf("return_to %q produced Location %q, want /", returnTo, location)
			}
		})
	}
}

func TestCSRFInvalidHasItsOwnErrorCode(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	env.loginAs(client, "code-admin")

	response := env.request(client, http.MethodPatch, "/api/v1/admin/users/1",
		[]byte(`{"role":"viewer","revision":1}`),
		map[string]string{"Content-Type": "application/json", csrfHeaderName: "stale-token"})
	if response.status != http.StatusForbidden {
		t.Fatalf("csrf status = %d, body=%s", response.status, response.body)
	}
	body := decodeMap(t, response.body)
	envelope := body["error"].(map[string]any)
	if envelope["code"] != "csrf_invalid" {
		t.Fatalf("csrf error code = %v, want csrf_invalid", envelope["code"])
	}
}
