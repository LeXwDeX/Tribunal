package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestMCPServerValidationRejectsUnknownFieldsWithoutLeaking(t *testing.T) {
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
