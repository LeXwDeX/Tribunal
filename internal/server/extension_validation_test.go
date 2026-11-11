package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// mcpValidationDetail reads the error code and first detail field of a
// validation response.
func mcpValidationDetail(t *testing.T, response apiResponse) (string, string) {
	t.Helper()
	body := decodeMap(t, response.body)
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error envelope: %s", response.body)
	}
	code, _ := errorBody["code"].(string)
	field := ""
	if details, ok := errorBody["details"].([]any); ok && len(details) > 0 {
		if detail, ok := details[0].(map[string]any); ok {
			field, _ = detail["field"].(string)
		}
	}
	return code, field
}

// TestMCPValidationFieldsAreStableAcrossEntryPoints pins the unified config and
// name validation contract for create, import and draft: the same invalid
// value returns validation_failed with the same detail field everywhere.
func TestMCPValidationFieldsAreStableAcrossEntryPoints(t *testing.T) {
	env := newWiringEnv(t)
	client := env.bearer()

	configs := map[string]any{
		"missing":       nil,
		"empty object":  map[string]any{},
		"array":         []any{},
		"unknown field": map[string]any{"command": "npx", "bogus": "super-secret"},
		"bad url":       map[string]any{"url": "ftp://mcp.example.invalid"},
		"masked secret": map[string]any{"command": "npx", "env": map[string]any{"API_KEY": "****"}},
	}
	for name, config := range configs {
		t.Run("config "+name, func(t *testing.T) {
			create := client.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
				"scope": "platform", "name": "srv", "config": config,
			})
			assertStatus(t, create, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, create); code != "validation_failed" || field != "config" {
				t.Fatalf("create code=%q field=%q body=%s", code, field, create.body)
			}
			imported := client.json(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform", map[string]any{
				"mcpServers": map[string]any{"srv": config},
			})
			assertStatus(t, imported, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, imported); code != "validation_failed" || field != "config" {
				t.Fatalf("import code=%q field=%q body=%s", code, field, imported.body)
			}
			draft := client.json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{
				"scope": "platform", "name": "srv", "config": config,
			})
			assertStatus(t, draft, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, draft); code != "validation_failed" || field != "config" {
				t.Fatalf("draft code=%q field=%q body=%s", code, field, draft.body)
			}
		})
	}

	for _, name := range []string{"", "bad name", "bad/name", "bad__name", strings.Repeat("a", 65)} {
		t.Run("name "+name, func(t *testing.T) {
			config := map[string]any{"command": "npx"}
			create := client.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
				"scope": "platform", "name": name, "config": config,
			})
			assertStatus(t, create, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, create); code != "validation_failed" || field != "name" {
				t.Fatalf("create code=%q field=%q body=%s", code, field, create.body)
			}
			imported := client.json(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform", map[string]any{
				"mcpServers": map[string]any{name: config},
			})
			assertStatus(t, imported, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, imported); code != "validation_failed" || field != "name" {
				t.Fatalf("import code=%q field=%q body=%s", code, field, imported.body)
			}
			draft := client.json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{
				"scope": "platform", "name": name, "config": config,
			})
			assertStatus(t, draft, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, draft); code != "validation_failed" || field != "name" {
				t.Fatalf("draft code=%q field=%q body=%s", code, field, draft.body)
			}
		})
	}
}

// Invalid create and import bodies are rejected before persistence and never
// echo a configuration value.
func TestMCPServerCreationRejectsInvalidConfigWithoutLeaking(t *testing.T) {
	env := newWiringEnv(t)
	client := env.bearer()

	created := client.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "platform",
		"name":  "invalid",
		"config": map[string]any{
			"command": "npx",
			"bogus":   "super-secret",
		},
	})
	assertStatus(t, created, http.StatusBadRequest)
	if strings.Contains(string(created.body), "super-secret") {
		t.Fatalf("response leaked a config value: %s", created.body)
	}
	rows, err := env.db.ListMCPServers(context.Background(), "platform", nil)
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("invalid create persisted rows: %+v", rows)
	}

	imported := client.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform",
		[]byte(`{"mcpServers":{"good":{"command":"npx"},"bad":{"command":"npx","bogus":"super-secret"}}}`),
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, imported, http.StatusBadRequest)
	if strings.Contains(string(imported.body), "super-secret") {
		t.Fatalf("response leaked a config value: %s", imported.body)
	}
	rows, err = env.db.ListMCPServers(context.Background(), "platform", nil)
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("invalid import persisted rows: %+v", rows)
	}
}
