package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestMCPReadGrantDoesNotExposeConfigLiterals(t *testing.T) {
	env := newM6Env(t, m6Options{})
	project := int64(42)
	env.grant(8, project, store.ProjectAccessRead)
	env.extensions.servers = []MCPServer{{
		ID: "mcp-security", Scope: "project", ProjectID: &project, Name: "synthetic", Enabled: true,
		Config: json.RawMessage(`{"url":"https://example.invalid/path-canary?key=query-canary","headers":{"X-Auth":"header-canary"},"args":["arg-canary"],"x-review":{"index_command":"index-canary"}}`),
	}}
	response := env.loginAs("code-viewer").request(http.MethodGet, "/api/v1/mcp-servers?scope=project&project_id=42", nil, nil)
	assertStatus(t, response, http.StatusOK)
	for _, value := range []string{"path-canary", "query-canary", "header-canary", "arg-canary", "index-canary"} {
		if strings.Contains(string(response.body), value) {
			t.Errorf("response contains synthetic %s", value)
		}
	}
}

func TestMCPRealStoreHandlerLifecycleHidesAndPreservesConfig(t *testing.T) {
	st, srv := newExtensionPaginationServer(t)
	ctx := context.Background()
	// Direct handler calls bypass route guards; TestMCPReadGrantDoesNotExposeConfigLiterals covers the read grant.
	call := func(handler http.HandlerFunc, method, path, id string, payload any, want int) map[string]any {
		t.Helper()
		var data []byte
		if payload != nil {
			var err error
			data, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.SetPathValue("id", id)
		req = req.WithContext(context.WithValue(req.Context(), principalKey, principal{user: sessionUser{ID: 7, Username: "alice", Role: roleAdmin}}))
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != want || strings.Contains(rec.Body.String(), "canary") {
			t.Fatalf("%s %s: status %d body %s", method, path, rec.Code, rec.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	masked := func(row map[string]any) {
		t.Helper()
		config, ok := row["config"].(map[string]any)
		if !ok || len(config) != 0 {
			t.Fatalf("response config is not empty: %v", row)
		}
	}
	stored := func(id, name string, enabled bool, want map[string]any) {
		t.Helper()
		row, err := st.GetMCPServer(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := json.Unmarshal(row.Config, &config); err != nil {
			t.Fatal(err)
		}
		if row.ID != id || row.Name != name || row.Enabled != enabled || !reflect.DeepEqual(config, want) {
			t.Fatalf("stored server differs: id=%s name=%s enabled=%t config=%v", row.ID, row.Name, row.Enabled, config)
		}
	}
	remote := map[string]any{"url": "https://example.invalid/url-canary", "headers": map[string]any{"X-Trace": "header-canary", "Authorization": "auth-canary"}}
	local := map[string]any{"command": "synthetic-command", "args": []any{"arg-canary"}, "env": map[string]any{"API_KEY": "env-canary"}}
	create := func(name string, config map[string]any) string {
		row := call(srv.handleCreateMCPServer, http.MethodPost, "/api/v1/mcp-servers", "", map[string]any{"scope": "platform", "name": name, "config": config}, http.StatusCreated)
		masked(row)
		id, ok := row["id"].(string)
		if !ok || id == "" {
			t.Fatal("created server has no id")
		}
		return id
	}
	remoteID := create("remote", remote)
	localID := create("local", local)
	update := func(id string, payload map[string]any) {
		row := call(srv.handleUpdateMCPServer, http.MethodPut, "/api/v1/mcp-servers/"+id, id, payload, http.StatusOK)
		masked(row)
		if row["id"] != id {
			t.Fatal("update changed server ID")
		}
	}
	update(remoteID, map[string]any{"name": "renamed", "enabled": false})
	stored(remoteID, "renamed", false, remote)
	update(remoteID, map[string]any{"enabled": true})
	stored(remoteID, "renamed", true, remote)
	update(remoteID, map[string]any{"enabled": false, "config": map[string]any{"url": remote["url"], "headers": map[string]any{"X-Trace": "header-canary", "Authorization": "****"}}})
	stored(remoteID, "renamed", false, remote)
	update(localID, map[string]any{"config": map[string]any{"command": "synthetic-command", "args": []any{"arg-canary"}, "env": map[string]any{"API_KEY": "****"}}})
	stored(localID, "local", true, local)
	importRemote := map[string]any{"url": "https://example.invalid/import-canary", "headers": map[string]any{"X-Trace": "import-header-canary"}}
	importNew := map[string]any{"command": "another-command", "args": []any{"new-arg-canary"}, "env": map[string]any{"API_KEY": "new-env-canary"}}
	result := call(srv.handleImportMCPServers, http.MethodPost, "/api/v1/mcp-servers/import?scope=platform", "", map[string]any{"mcpServers": map[string]any{"renamed": importRemote, "new": importNew}}, http.StatusCreated)
	rows, ok := result["servers"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("import rows: %v", result)
	}
	var newID string
	seenExisting := false
	for _, value := range rows {
		row := value.(map[string]any)
		masked(row)
		switch row["name"] {
		case "renamed":
			if row["id"] != remoteID || row["enabled"] != false {
				t.Fatalf("import changed existing identity/state: %v", row)
			}
			seenExisting = true
		case "new":
			newID, _ = row["id"].(string)
			if newID == "" || newID == remoteID || newID == localID || row["enabled"] != true {
				t.Fatalf("new import identity/state: %v", row)
			}
		default:
			t.Fatalf("unexpected imported server: %v", row)
		}
	}
	if newID == "" || !seenExisting {
		t.Fatal("new or existing import missing")
	}
	stored(remoteID, "renamed", false, importRemote)
	stored(localID, "local", true, local)
	stored(newID, "new", true, importNew)
	result = call(srv.handleListMCPServers, http.MethodGet, "/api/v1/mcp-servers?scope=platform", "", nil, http.StatusOK)
	rows, ok = result["servers"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("list rows: %v", result)
	}
	seen := map[string]bool{}
	for _, value := range rows {
		row := value.(map[string]any)
		masked(row)
		seen[row["id"].(string)] = true
	}
	if !seen[remoteID] || !seen[localID] || !seen[newID] {
		t.Fatalf("list missing server IDs: %v", seen)
	}
}

func TestMCPMalformedConfigIsNotReturned(t *testing.T) {
	server := maskMCPServer(MCPServer{Config: json.RawMessage(`malformed-canary`)})
	if strings.Contains(string(server.Config), "malformed-canary") || !json.Valid(server.Config) {
		t.Fatal("malformed config was returned")
	}
}

func TestMCPWriteResponsesHideConfigAndPreserveStoredValues(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")
	config := map[string]any{"url": "https://example.invalid/private-canary", "headers": map[string]any{"X-Auth": "header-canary"}}
	create := client.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{"scope": "platform", "name": "synthetic", "config": config})
	assertStatus(t, create, http.StatusCreated)
	nameOnly := client.json(http.MethodPut, "/api/v1/mcp-servers/mcp-new", map[string]any{"name": "renamed"})
	assertStatus(t, nameOnly, http.StatusOK)
	update := client.json(http.MethodPut, "/api/v1/mcp-servers/mcp-new", map[string]any{"config": config})
	assertStatus(t, update, http.StatusOK)
	imported := client.json(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform", map[string]any{"mcpServers": map[string]any{"imported": config}})
	assertStatus(t, imported, http.StatusCreated)
	for _, response := range []apiResponse{create, nameOnly, update, imported} {
		for _, value := range []string{"private-canary", "header-canary"} {
			if strings.Contains(string(response.body), value) {
				t.Errorf("write response contains synthetic %s", value)
			}
		}
	}
	if !strings.Contains(string(env.extensions.servers[0].Config), "header-canary") {
		t.Fatal("stored config lost the synthetic secret")
	}
}
