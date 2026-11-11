package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCustomSkillCreationIsForbidden(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")
	archive, contentType := multipartBody(t, map[string]string{"scope": "platform"}, "file", "skill.zip", skillZip(t, testSkillManifest, true))
	tests := []struct {
		name, path, contentType, body, code, message string
	}{
		{"skill archive", "/api/v1/skills", contentType, string(archive), "custom_skill_creation_disabled", "custom skill creation is disabled"},
		{"skill directory", "/api/v1/skills", "application/json", `{"scope":"platform","dir":"/tmp"}`, "custom_skill_creation_disabled", "custom skill creation is disabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			anonymous := env.anonymousClient().request(http.MethodPost, tc.path, []byte(tc.body), map[string]string{"Content-Type": tc.contentType})
			assertStatus(t, anonymous, http.StatusUnauthorized)
			response := client.request(http.MethodPost, tc.path, []byte(tc.body), map[string]string{"Content-Type": tc.contentType})
			assertStatus(t, response, http.StatusForbidden)
			body := env.decode(t, response)["error"].(map[string]any)
			if body["code"] != tc.code || body["message"] != tc.message {
				t.Fatalf("error = %v", body)
			}
		})
	}
}

func TestLockedCreationHandlerDoesNotRequireService(t *testing.T) {
	server := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not JSON"))
	rec := httptest.NewRecorder()
	server.handleUploadSkillOrRegisterDirectory(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
