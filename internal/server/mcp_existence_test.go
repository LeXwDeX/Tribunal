package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// A caller without write permission gets the same answer for an existing and
// a missing MCP server on every id route, so they cannot probe which servers
// exist. Platform and project scopes are both covered; the viewer holds the
// manage grant on the project, which still does not allow host configuration.
func TestMCPServerRoutesDoNotRevealExistenceToViewer(t *testing.T) {
	env := newWiringEnv(t)
	admin := env.business()
	ids := map[string]string{}
	projectID := int64(42)
	for scope, row := range map[string]store.MCPServer{
		"platform": {Scope: "platform", Name: "platform-tools", Config: json.RawMessage(`{"command":"npx"}`), Enabled: true},
		"project":  {Scope: "project", ProjectID: &projectID, Name: "project-tools", Config: json.RawMessage(`{"command":"npx"}`), Enabled: true},
	} {
		created, err := env.db.CreateMCPServer(context.Background(), row)
		if err != nil {
			t.Fatal(err)
		}
		ids[scope] = created.ID
	}
	viewer := env.loginAs("code-viewer")
	if err := env.db.SetProjectGrant(context.Background(), 42, 8, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}

	routes := []struct {
		method string
		suffix string
		body   any
	}{
		{http.MethodPut, "", map[string]any{"enabled": false}},
		{http.MethodDelete, "", nil},
		{http.MethodPost, "/test", nil},
	}
	for _, route := range routes {
		missing := viewer.json(route.method, "/api/v1/mcp-servers/missing"+route.suffix, route.body)
		for scope, id := range ids {
			existing := viewer.json(route.method, "/api/v1/mcp-servers/"+id+route.suffix, route.body)
			if existing.status != missing.status {
				t.Fatalf("%s %s server%s: existing = %d %s, missing = %d %s",
					route.method, scope, route.suffix, existing.status, existing.body, missing.status, missing.body)
			}
			got := decodeMap(t, existing.body)["error"].(map[string]any)
			want := decodeMap(t, missing.body)["error"].(map[string]any)
			delete(got, "request_id")
			delete(want, "request_id")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s server%s errors differ: existing %v, missing %v", route.method, scope, route.suffix, got, want)
			}
		}
	}

	// Administrators keep their behaviour: a missing server is 404, an
	// existing one can still be updated and deleted.
	assertStatus(t, admin.json(http.MethodPut, "/api/v1/mcp-servers/missing", map[string]any{"enabled": false}), http.StatusNotFound)
	for _, id := range ids {
		assertStatus(t, admin.json(http.MethodPut, "/api/v1/mcp-servers/"+id, map[string]any{"enabled": false}), http.StatusOK)
		assertStatus(t, admin.request(http.MethodDelete, "/api/v1/mcp-servers/"+id, nil, nil), http.StatusNoContent)
	}
}
