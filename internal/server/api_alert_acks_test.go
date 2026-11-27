package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func alertAckErrorCode(t *testing.T, response apiResponse) string {
	t.Helper()
	envelope, _ := decodeMap(t, response.body)["error"].(map[string]any)
	code, _ := envelope["code"].(string)
	return code
}

func alertAckList(t *testing.T, client *m6Client) []any {
	t.Helper()
	response := client.request(http.MethodGet, "/api/v1/alert-acks", nil, nil)
	assertStatus(t, response, http.StatusOK)
	acks, ok := decodeMap(t, response.body)["acks"].([]any)
	if !ok {
		t.Fatalf("acks missing from %s", response.body)
	}
	return acks
}

func TestAlertAcksRejectUnauthenticated(t *testing.T) {
	env := newM6Env(t, m6Options{})
	anonymous := env.anonymousClient()
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/alert-acks"},
		{http.MethodPut, "/api/v1/alert-acks/workspace-42"},
		{http.MethodDelete, "/api/v1/alert-acks/workspace-42"},
	} {
		response := anonymous.json(call.method, call.path, map[string]any{"fingerprint": "a"})
		if response.status != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401, body = %s", call.method, call.path, response.status, response.body)
		}
	}
}

func TestAlertAckWritesRequireCSRF(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")
	for _, mode := range []string{"missing", "invalid"} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			sender := client.withoutCSRF()
			headers := map[string]string{"Content-Type": "application/json"}
			if mode == "invalid" {
				headers["X-CSRF-Token"] = "not-the-token"
			}
			response := sender.request(method, "/api/v1/alert-acks/workspace-42", []byte(`{"fingerprint":"a"}`), headers)
			if response.status != http.StatusForbidden || alertAckErrorCode(t, response) != "csrf_invalid" {
				t.Fatalf("%s with %s CSRF: status = %d, body = %s", method, mode, response.status, response.body)
			}
		}
	}
	if acks := alertAckList(t, client); len(acks) != 0 {
		t.Fatalf("CSRF-rejected write persisted: %v", acks)
	}
}

func TestAlertAcksRejectBearerClients(t *testing.T) {
	env := newM6Env(t, m6Options{})
	session := env.loginAs("code-admin")
	created := session.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "cli"})
	assertStatus(t, created, http.StatusCreated)
	plaintext, _ := decodeMap(t, created.body)["token"].(string)
	if plaintext == "" {
		t.Fatalf("token create response = %s", created.body)
	}
	clients := map[string]*m6Client{
		"bootstrap": env.bearerClient(),
		"api token": {t: t, base: env.server.URL, client: &http.Client{}, bearer: plaintext},
	}
	for name, client := range clients {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			response := client.json(method, "/api/v1/alert-acks/workspace-42", map[string]any{"fingerprint": "a"})
			if method == http.MethodGet {
				response = client.request(http.MethodGet, "/api/v1/alert-acks", nil, nil)
			}
			if response.status != http.StatusForbidden || alertAckErrorCode(t, response) != "forbidden" {
				t.Fatalf("%s %s status = %d, body = %s", name, method, response.status, response.body)
			}
		}
	}
	if acks := alertAckList(t, session); len(acks) != 0 {
		t.Fatalf("bearer write persisted: %v", acks)
	}
}

func TestAlertAckPutValidatesKeyAndFingerprint(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")
	valid := strings.Repeat("k", 128)
	cases := []struct {
		name, key, body, field string
	}{
		{"key too long", strings.Repeat("k", 129), `{"fingerprint":"a"}`, "key"},
		{"key with space", "bad%20key", `{"fingerprint":"a"}`, "key"},
		{"key with slash", "bad%2Fkey", `{"fingerprint":"a"}`, "key"},
		{"key non-ascii", "%C3%A9", `{"fingerprint":"a"}`, "key"},
		{"fingerprint empty", valid, `{"fingerprint":""}`, "fingerprint"},
		{"fingerprint missing", valid, `{}`, "fingerprint"},
		{"fingerprint too long", valid, `{"fingerprint":"` + strings.Repeat("f", 257) + `"}`, "fingerprint"},
		{"fingerprint control", valid, `{"fingerprint":"a\u0001b"}`, "fingerprint"},
		{"fingerprint non-ascii", valid, `{"fingerprint":"é"}`, "fingerprint"},
		{"unknown field", valid, `{"fingerprint":"a","extra":1}`, ""},
		{"malformed", valid, `{`, ""},
	}
	for _, tc := range cases {
		response := client.request(http.MethodPut, "/api/v1/alert-acks/"+tc.key, []byte(tc.body), map[string]string{"Content-Type": "application/json"})
		if response.status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400, body = %s", tc.name, response.status, response.body)
		}
		if strings.Contains(string(response.body), "bad key") || strings.Contains(string(response.body), strings.Repeat("f", 257)) {
			t.Fatalf("%s: error echoed the input: %s", tc.name, response.body)
		}
		if tc.field != "" && !strings.Contains(string(response.body), `"field":"`+tc.field+`"`) {
			t.Fatalf("%s: error does not name field %s: %s", tc.name, tc.field, response.body)
		}
	}
	response := client.request(http.MethodDelete, "/api/v1/alert-acks/"+strings.Repeat("k", 129), nil, nil)
	if response.status != http.StatusBadRequest {
		t.Fatalf("DELETE with long key: status = %d, body = %s", response.status, response.body)
	}
	if acks := alertAckList(t, client); len(acks) != 0 {
		t.Fatalf("invalid writes persisted: %v", acks)
	}
	// The bounds themselves are accepted.
	edge := client.json(http.MethodPut, "/api/v1/alert-acks/"+valid, map[string]any{"fingerprint": strings.Repeat("~", 256)})
	assertStatus(t, edge, http.StatusOK)
	symbols := client.json(http.MethodPut, "/api/v1/alert-acks/a.b_c:d-E9", map[string]any{"fingerprint": "2026-11-27T00:00:00Z|x y"})
	assertStatus(t, symbols, http.StatusOK)
}

func TestAlertAcksRoundTripPerUser(t *testing.T) {
	fixed := time.Date(2026, 11, 27, 8, 30, 0, 0, time.UTC)
	env := newM6Env(t, m6Options{})
	env.api.now = func() time.Time { return fixed }
	alice := env.loginAs("code-admin")
	bob := env.loginAs("code-viewer")

	put := alice.json(http.MethodPut, "/api/v1/alert-acks/workspace-42", map[string]any{"fingerprint": "2026-01-01T00:00:00Z|abc|"})
	assertStatus(t, put, http.StatusOK)
	row := decodeMap(t, put.body)
	if row["key"] != "workspace-42" || row["fingerprint"] != "2026-01-01T00:00:00Z|abc|" || row["acked_at"] != fixed.Format(time.RFC3339) {
		t.Fatalf("put row = %v", row)
	}
	if len(row) != 3 {
		t.Fatalf("put row has extra fields: %v", row)
	}
	// PUT is idempotent and last write wins.
	again := alice.json(http.MethodPut, "/api/v1/alert-acks/workspace-42", map[string]any{"fingerprint": "next"})
	assertStatus(t, again, http.StatusOK)
	acks := alertAckList(t, alice)
	if len(acks) != 1 || acks[0].(map[string]any)["fingerprint"] != "next" {
		t.Fatalf("alice acks = %v", acks)
	}
	if acks := alertAckList(t, bob); len(acks) != 0 {
		t.Fatalf("bob sees alice's acks: %v", acks)
	}
	// Bob cannot remove alice's ack; the delete still reports success.
	deleted := bob.request(http.MethodDelete, "/api/v1/alert-acks/workspace-42", nil, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	if acks := alertAckList(t, alice); len(acks) != 1 {
		t.Fatalf("bob's delete removed alice's ack: %v", acks)
	}
	own := alice.request(http.MethodDelete, "/api/v1/alert-acks/workspace-42", nil, nil)
	assertStatus(t, own, http.StatusNoContent)
	if acks := alertAckList(t, alice); len(acks) != 0 {
		t.Fatalf("alice acks after delete = %v", acks)
	}
	missing := alice.request(http.MethodDelete, "/api/v1/alert-acks/never-acked", nil, nil)
	assertStatus(t, missing, http.StatusNoContent)
}

func TestAlertAcksUnconfiguredStore(t *testing.T) {
	env := newM6Env(t, m6Options{})
	env.api.alertAcks = nil
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/alert-acks/workspace-42", strings.NewReader(`{"fingerprint":"a"}`))
		req.SetPathValue("key", "workspace-42")
		req = req.WithContext(context.WithValue(req.Context(), principalKey, principal{user: sessionUser{ID: 7, Username: "alice", Role: roleAdmin}, sessionID: "s"}))
		response := httptest.NewRecorder()
		switch method {
		case http.MethodGet:
			env.api.handleListAlertAcks(response, req)
		case http.MethodPut:
			env.api.handlePutAlertAck(response, req)
		default:
			env.api.handleDeleteAlertAck(response, req)
		}
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503, body = %s", method, response.Code, response.Body.String())
		}
	}
}
