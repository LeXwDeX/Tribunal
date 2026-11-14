package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func createBearerToken(t *testing.T, env *authEnv, client *http.Client) (string, string) {
	t.Helper()
	env.loginAs(client, "code-admin")
	me := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	csrf := decodeMap(t, me.body)["csrf_token"].(string)
	response := env.request(client, http.MethodPost, "/api/v1/api-tokens",
		[]byte(`{"name":"ci","expires_in_days":1}`),
		map[string]string{"Content-Type": "application/json", csrfHeaderName: csrf})
	if response.status != http.StatusCreated {
		t.Fatalf("create token = %d, body=%s", response.status, response.body)
	}
	body := decodeMap(t, response.body)
	return body["id"].(string), body["token"].(string)
}

func TestExpiredAPITokenReturnsTokenExpired(t *testing.T) {
	env := newAuthEnv(t)
	_, token := createBearerToken(t, env, env.client())

	env.clock.Advance(26 * time.Hour)
	response := env.request(env.client(), http.MethodGet, "/api/v1/projects", nil,
		map[string]string{"Authorization": "Bearer " + token})
	if response.status != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, body=%s", response.status, response.body)
	}
	code := decodeMap(t, response.body)["error"].(map[string]any)["code"]
	if code != "token_expired" {
		t.Fatalf("expired token code = %v, want token_expired", code)
	}

	unknown := env.request(env.client(), http.MethodGet, "/api/v1/projects", nil,
		map[string]string{"Authorization": "Bearer crc_not-a-real-token"})
	if unknown.status != http.StatusUnauthorized {
		t.Fatalf("unknown token status = %d", unknown.status)
	}
	if code := decodeMap(t, unknown.body)["error"].(map[string]any)["code"]; code != "unauthenticated" {
		t.Fatalf("unknown token code = %v, want unauthenticated", code)
	}
}

func TestEvictedSessionReturnsSessionEvicted(t *testing.T) {
	env := newAuthEnv(t)
	first := env.loginAs(env.client(), "code-admin")
	for i := 0; i < store.DefaultSessionsPerUser; i++ {
		env.clock.Advance(time.Second)
		env.loginAs(env.client(), "code-admin")
	}
	response := env.request(env.clientWithCookie(first), http.MethodGet, "/api/v1/me", nil, nil)
	if response.status != http.StatusUnauthorized {
		t.Fatalf("evicted session status = %d, body=%s", response.status, response.body)
	}
	code := decodeMap(t, response.body)["error"].(map[string]any)["code"]
	if code != "session_evicted" {
		t.Fatalf("evicted session code = %v, want session_evicted", code)
	}
}

func TestRenewAPITokenRotatesTheSecret(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	id, token := createBearerToken(t, env, client)
	me := env.request(client, http.MethodGet, "/api/v1/me", nil, nil)
	csrf := decodeMap(t, me.body)["csrf_token"].(string)

	response := env.request(client, http.MethodPost, "/api/v1/api-tokens/"+id+"/renew",
		[]byte(`{"expires_in_days":30}`),
		map[string]string{"Content-Type": "application/json", csrfHeaderName: csrf})
	if response.status != http.StatusCreated {
		t.Fatalf("renew status = %d, body=%s", response.status, response.body)
	}
	body := decodeMap(t, response.body)
	renewed, _ := body["token"].(string)
	if renewed == "" || renewed == token {
		t.Fatalf("renewed token did not rotate: %q", renewed)
	}

	old := env.request(env.client(), http.MethodGet, "/api/v1/projects", nil,
		map[string]string{"Authorization": "Bearer " + token})
	if old.status != http.StatusUnauthorized {
		t.Fatalf("old secret still accepted: %d %s", old.status, old.body)
	}
	fresh := env.request(env.client(), http.MethodGet, "/api/v1/projects", nil,
		map[string]string{"Authorization": "Bearer " + renewed})
	if fresh.status != http.StatusOK {
		t.Fatalf("renewed secret rejected: %d %s", fresh.status, fresh.body)
	}

	missing := env.request(client, http.MethodPost, "/api/v1/api-tokens/user-other/renew",
		[]byte(`{"expires_in_days":30}`),
		map[string]string{"Content-Type": "application/json", csrfHeaderName: csrf})
	if missing.status != http.StatusNotFound {
		t.Fatalf("missing token renew = %d, body=%s", missing.status, missing.body)
	}
}
