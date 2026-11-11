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

// TestMCPDocumentLevelErrorsKeepTheirFields documents the boundary of the
// name/config contract: structural and selector errors keep their own fields.
func TestMCPDocumentLevelErrorsKeepTheirFields(t *testing.T) {
	env := newWiringEnv(t)
	client := env.bearer()

	for name, body := range map[string]string{
		"empty document": `{}`,
		"empty servers":  `{"mcpServers":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := client.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform",
				[]byte(body), map[string]string{"Content-Type": "application/json"})
			assertStatus(t, response, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, response); code != "validation_failed" || field != "mcpServers" {
				t.Fatalf("code=%q field=%q body=%s", code, field, response.body)
			}
		})
	}

	cases := []struct {
		name  string
		path  string
		body  string
		field string
	}{
		{"import unknown top field", "/api/v1/mcp-servers/import?scope=platform", `{"servers":{}}`, "body"},
		{"create invalid json", "/api/v1/mcp-servers", `{`, "body"},
		{"create invalid scope", "/api/v1/mcp-servers", `{"scope":"global","name":"srv","config":{"command":"npx"}}`, "scope"},
		{"import missing scope", "/api/v1/mcp-servers/import", `{"mcpServers":{"srv":{"command":"npx"}}}`, "scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := client.request(http.MethodPost, tc.path, []byte(tc.body),
				map[string]string{"Content-Type": "application/json"})
			assertStatus(t, response, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, response); code != "validation_failed" || field != tc.field {
				t.Fatalf("code=%q field=%q body=%s", code, field, response.body)
			}
		})
	}
}

// TestMCPValidationPriorityIsStable checks that a request with both an invalid
// name and an invalid config reports the same field on every entry point.
func TestMCPValidationPriorityIsStable(t *testing.T) {
	env := newWiringEnv(t)
	client := env.bearer()
	badConfig := map[string]any{}

	create := client.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "platform", "name": "bad name", "config": badConfig,
	})
	assertStatus(t, create, http.StatusBadRequest)
	if code, field := mcpValidationDetail(t, create); code != "validation_failed" || field != "name" {
		t.Fatalf("create code=%q field=%q body=%s", code, field, create.body)
	}
	imported := client.json(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform", map[string]any{
		"mcpServers": map[string]any{"good": badConfig, "bad name": map[string]any{"command": "npx"}},
	})
	assertStatus(t, imported, http.StatusBadRequest)
	if code, field := mcpValidationDetail(t, imported); code != "validation_failed" || field != "name" {
		t.Fatalf("import code=%q field=%q body=%s", code, field, imported.body)
	}
	draft := client.json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{
		"scope": "platform", "name": "bad name", "config": badConfig,
	})
	assertStatus(t, draft, http.StatusBadRequest)
	if code, field := mcpValidationDetail(t, draft); code != "validation_failed" || field != "name" {
		t.Fatalf("draft code=%q field=%q body=%s", code, field, draft.body)
	}

	// A scope selector error always wins because it is handled before the body
	// name/config validation on every entry point.
	for _, tc := range []struct {
		name string
		path string
		body []byte
	}{
		{"create", "/api/v1/mcp-servers", []byte(`{"scope":"global","name":"bad name","config":{}}`)},
		{"import", "/api/v1/mcp-servers/import?scope=global", []byte(`{"mcpServers":{"bad name":{}}}`)},
		{"draft", "/api/v1/mcp-servers:test", []byte(`{"scope":"global","name":"bad name","config":{}}`)},
	} {
		t.Run("scope wins "+tc.name, func(t *testing.T) {
			response := client.request(http.MethodPost, tc.path, tc.body,
				map[string]string{"Content-Type": "application/json"})
			assertStatus(t, response, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, response); code != "validation_failed" || field != "scope" {
				t.Fatalf("code=%q field=%q body=%s", code, field, response.body)
			}
		})
	}
}

// TestMCPDraftValidationPrecedesTestSlot pins the order fix: an invalid draft
// answers 400 even when every test slot is busy, while a valid draft still
// gets 429. Missing ids are resolved before the slot as well.
func TestMCPDraftValidationPrecedesTestSlot(t *testing.T) {
	env := newWiringEnv(t)
	client := env.bearer()
	created := client.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "platform", "name": "stored", "config": map[string]any{"command": "/bin/true"},
	})
	assertStatus(t, created, http.StatusCreated)
	storedID, _ := decodeMap(t, created.body)["id"].(string)
	if storedID == "" {
		t.Fatalf("created server has no id: %s", created.body)
	}

	for i := 0; i < mcpTestConcurrency; i++ {
		mcpTestSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < mcpTestConcurrency; i++ {
			<-mcpTestSlots
		}
	}()

	for _, tc := range []struct {
		name  string
		body  map[string]any
		code  string
		field string
	}{
		{"bad name", map[string]any{"scope": "platform", "name": "bad name", "config": map[string]any{"command": "/bin/true"}},
			"validation_failed", "name"},
		{"bad config", map[string]any{"scope": "platform", "name": "draft", "config": map[string]any{"command": "npx", "bogus": true}},
			"validation_failed", "config"},
		{"bad config by id", map[string]any{"id": storedID, "config": map[string]any{"command": "npx", "bogus": true}},
			"validation_failed", "config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := client.json(http.MethodPost, "/api/v1/mcp-servers:test", tc.body)
			assertStatus(t, response, http.StatusBadRequest)
			if code, field := mcpValidationDetail(t, response); code != tc.code || field != tc.field {
				t.Fatalf("code=%q field=%q body=%s", code, field, response.body)
			}
		})
	}

	missing := client.json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{"id": "missing"})
	assertStatus(t, missing, http.StatusNotFound)

	busy := client.json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{
		"scope": "platform", "name": "draft", "config": map[string]any{"command": "/bin/true"},
	})
	assertStatus(t, busy, http.StatusTooManyRequests)
	if body := decodeMap(t, busy.body)["error"].(map[string]any); body["code"] != "test_busy" {
		t.Fatalf("busy error = %v", body)
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
