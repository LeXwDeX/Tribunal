package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
)

// Built-in MCP servers are listed with a marker, can be disabled, and cannot
// be deleted, renamed or reconfigured through the API.
func TestBuiltinMCPServerToggleAndGuards(t *testing.T) {
	env := newWiringEnv(t)
	defaults, err := env.backend.service.MCPDefaults([]config.DefaultMCPServer{{
		Name:   "codebase-memory",
		Config: map[string]any{"command": "/usr/local/bin/tribunal-codebase-memory-mcp", "args": []any{"--tool-profile=analysis"}},
		Tools:  map[string][]string{"review": {"search_graph"}},
	}}, mcp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ApplyExtensionDefaults(context.Background(), env.db, nil, defaults, nil); err != nil {
		t.Fatal(err)
	}
	client := env.bearer()

	list := client.request(http.MethodGet, "/api/v1/mcp-servers?scope=platform", nil, nil)
	assertStatus(t, list, http.StatusOK)
	servers := decodeMap(t, list.body)["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("servers = %v", servers)
	}
	listed := servers[0].(map[string]any)
	if listed["name"] != "codebase-memory" || listed["builtin"] != true || listed["enabled"] != true {
		t.Fatalf("listed server = %v", listed)
	}
	id := listed["id"].(string)

	disabled := client.json(http.MethodPut, "/api/v1/mcp-servers/"+id, map[string]any{"name": "codebase-memory", "enabled": false})
	assertStatus(t, disabled, http.StatusOK)
	if body := decodeMap(t, disabled.body); body["enabled"] != false || body["builtin"] != true {
		t.Fatalf("disabled server = %v", body)
	}
	assertStatus(t, client.json(http.MethodPut, "/api/v1/mcp-servers/"+id, map[string]any{"name": "graph"}), http.StatusConflict)
	assertStatus(t, client.json(http.MethodPut, "/api/v1/mcp-servers/"+id,
		map[string]any{"config": map[string]any{"command": "/bin/true"}}), http.StatusConflict)
	assertStatus(t, client.request(http.MethodDelete, "/api/v1/mcp-servers/"+id, nil, nil), http.StatusConflict)

	after := client.request(http.MethodGet, "/api/v1/mcp-servers?scope=platform", nil, nil)
	if got := decodeMap(t, after.body)["servers"].([]any); len(got) != 1 {
		t.Fatalf("built-in server deleted: %v", got)
	}
}
