package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCustomExtensionCreationIsForbidden(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")
	archive, contentType := multipartBody(t, map[string]string{"scope": "platform"}, "file", "skill.zip", skillZip(t, testSkillManifest, true))
	tests := []struct {
		name, path, contentType, body, code, message string
	}{
		{"skill archive", "/api/v1/skills", contentType, string(archive), "custom_skill_creation_disabled", "custom skill creation is disabled"},
		{"skill directory", "/api/v1/skills", "application/json", `{"scope":"platform","dir":"/tmp"}`, "custom_skill_creation_disabled", "custom skill creation is disabled"},
		{"mcp create", "/api/v1/mcp-servers", "application/json", `{"scope":"platform","name":"custom","config":{"command":"npx"}}`, "custom_mcp_creation_disabled", "custom MCP server creation is disabled"},
		{"mcp import", "/api/v1/mcp-servers/import?scope=platform", "application/json", `{"mcpServers":{"custom":{"command":"npx"}}}`, "custom_mcp_creation_disabled", "custom MCP server creation is disabled"},
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

func TestLockedCreationHandlersDoNotRequireService(t *testing.T) {
	server := &Server{}
	for name, handler := range map[string]http.HandlerFunc{
		"skill":      server.handleUploadSkillOrRegisterDirectory,
		"mcp create": server.handleCreateMCPServer,
		"mcp import": server.handleImportMCPServers,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not JSON"))
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}
