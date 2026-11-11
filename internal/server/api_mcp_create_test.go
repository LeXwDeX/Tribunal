package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestMCPServerCreateAuthorizationAndResponses(t *testing.T) {
	env := newWiringEnv(t)
	body := map[string]any{
		"scope": "platform", "name": "custom",
		"config": map[string]any{"command": "npx", "env": map[string]any{"API_KEY": "secret-canary"}},
	}
	anonymous := &m6Client{t: t, base: env.server.URL, client: &http.Client{}}
	assertStatus(t, anonymous.json(http.MethodPost, "/api/v1/mcp-servers", body), http.StatusUnauthorized)
	assertStatus(t, env.loginAs("code-viewer").json(http.MethodPost, "/api/v1/mcp-servers", body), http.StatusForbidden)

	admin := env.bearer()
	created := admin.json(http.MethodPost, "/api/v1/mcp-servers", body)
	assertStatus(t, created, http.StatusCreated)
	row := decodeMap(t, created.body)
	if row["name"] != "custom" || row["scope"] != "platform" || row["enabled"] != true || row["builtin"] != false {
		t.Fatalf("created server = %v", row)
	}
	if config, ok := row["config"].(map[string]any); !ok || len(config) != 0 {
		t.Fatalf("create response config = %v", row["config"])
	}
	if strings.Contains(string(created.body), "secret-canary") {
		t.Fatalf("create response leaked config: %s", created.body)
	}
	id := row["id"].(string)
	if id == "" {
		t.Fatal("create response has no id")
	}

	assertStatus(t, admin.json(http.MethodPost, "/api/v1/mcp-servers", body), http.StatusConflict)

	httpCreated := admin.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "platform", "name": "remote", "config": map[string]any{"url": "https://mcp.example.com"},
	})
	assertStatus(t, httpCreated, http.StatusCreated)

	unknownTop := admin.request(http.MethodPost, "/api/v1/mcp-servers",
		[]byte(`{"scope":"platform","name":"extra","config":{"command":"npx"},"bogus":true}`),
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, unknownTop, http.StatusBadRequest)

	invalidName := admin.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "platform", "name": "bad__name", "config": map[string]any{"command": "npx"},
	})
	assertStatus(t, invalidName, http.StatusBadRequest)

	projectMissing := admin.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "project", "name": "project-server", "config": map[string]any{"command": "npx"},
	})
	assertStatus(t, projectMissing, http.StatusBadRequest)

	// A project manage grant alone never grants host execution configuration.
	if err := env.db.SetProjectGrant(context.Background(), 42, 8, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	viewer := env.loginAs("code-viewer")
	assertStatus(t, viewer.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "project", "project_id": 42, "name": "viewer-server", "config": map[string]any{"command": "npx"},
	}), http.StatusForbidden)

	// The session administrator has implicit manage on the registered project.
	projectCreated := env.business().json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "project", "project_id": 42, "name": "project-server", "config": map[string]any{"command": "npx"},
	})
	assertStatus(t, projectCreated, http.StatusCreated)
	projectRow := decodeMap(t, projectCreated.body)
	if projectRow["scope"] != "project" || projectRow["project_id"] != float64(42) {
		t.Fatalf("project server = %v", projectRow)
	}
	if config, ok := projectRow["config"].(map[string]any); !ok || len(config) != 0 {
		t.Fatalf("project response config = %v", projectRow["config"])
	}
}

func TestMCPServerBuiltinNameConflictOnCreate(t *testing.T) {
	env := newWiringEnv(t)
	if _, err := env.db.CreateMCPServer(context.Background(), store.MCPServer{
		Scope: "platform", Name: "builtin", Config: json.RawMessage(`{"command":"npx"}`),
		Enabled: true, Builtin: "default:1",
	}); err != nil {
		t.Fatal(err)
	}
	response := env.bearer().json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "platform", "name": "builtin", "config": map[string]any{"command": "/bin/true"},
	})
	assertStatus(t, response, http.StatusConflict)
	rows, err := env.db.ListMCPServers(context.Background(), "platform", nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
}

func TestMCPServerImportIsAtomicAndPreservesExistingRows(t *testing.T) {
	env := newWiringEnv(t)
	admin := env.bearer()
	assertStatus(t, env.loginAs("code-viewer").request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform",
		[]byte(`{"mcpServers":{"alpha":{"command":"npx"}}}`), map[string]string{"Content-Type": "application/json"}),
		http.StatusForbidden)

	body := []byte(`{"mcpServers":{"alpha":{"command":"npx","env":{"API_KEY":"import-canary"}},"beta":{"url":"https://mcp.example.com"}}}`)
	imported := admin.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform", body,
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, imported, http.StatusCreated)
	if strings.Contains(string(imported.body), "import-canary") {
		t.Fatalf("import response leaked config: %s", imported.body)
	}
	servers := decodeMap(t, imported.body)["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("servers = %v", servers)
	}
	for _, value := range servers {
		row := value.(map[string]any)
		if config, ok := row["config"].(map[string]any); !ok || len(config) != 0 {
			t.Fatalf("import response config = %v", row["config"])
		}
	}
	var alphaID string
	for _, value := range servers {
		if row := value.(map[string]any); row["name"] == "alpha" {
			alphaID = row["id"].(string)
		}
	}
	assertStatus(t, admin.json(http.MethodPut, "/api/v1/mcp-servers/"+alphaID, map[string]any{"enabled": false}), http.StatusOK)

	again := admin.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform",
		[]byte(`{"mcpServers":{"alpha":{"command":"npx","env":{"API_KEY":"next-canary"}}}}`),
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, again, http.StatusCreated)
	reimported := decodeMap(t, again.body)["servers"].([]any)[0].(map[string]any)
	if reimported["id"] != alphaID || reimported["enabled"] != false {
		t.Fatalf("reimport changed identity/state: %v", reimported)
	}
	stored, err := env.db.GetMCPServer(context.Background(), alphaID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enabled || !strings.Contains(string(stored.Config), "next-canary") {
		t.Fatalf("stored row = %+v", stored)
	}

	before, err := env.db.ListMCPServers(context.Background(), "platform", nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := admin.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform",
		[]byte(`{"mcpServers":{"good":{"command":"npx"},"bad":{"command":"npx","bogus":"super-secret"}}}`),
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, invalid, http.StatusBadRequest)
	after, err := env.db.ListMCPServers(context.Background(), "platform", nil)
	if err != nil || len(after) != len(before) {
		t.Fatalf("invalid import changed rows: before=%d after=%d err=%v", len(before), len(after), err)
	}

	if _, err := env.db.CreateMCPServer(context.Background(), store.MCPServer{
		Scope: "platform", Name: "builtin", Config: json.RawMessage(`{"command":"npx"}`),
		Enabled: true, Builtin: "default:1",
	}); err != nil {
		t.Fatal(err)
	}
	conflict := admin.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform",
		[]byte(`{"mcpServers":{"builtin":{"command":"npx"},"fresh":{"command":"npx"}}}`),
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, conflict, http.StatusConflict)
	rows, err := env.db.ListMCPServers(context.Background(), "platform", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name == "fresh" {
			t.Fatalf("conflicting import wrote a partial row: %+v", row)
		}
	}

	missingScope := admin.request(http.MethodPost, "/api/v1/mcp-servers/import",
		[]byte(`{"mcpServers":{"alpha":{"command":"npx"}}}`), map[string]string{"Content-Type": "application/json"})
	assertStatus(t, missingScope, http.StatusBadRequest)

	wrapper := admin.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform",
		[]byte(`{"servers":{"alpha":{"command":"npx"}}}`), map[string]string{"Content-Type": "application/json"})
	assertStatus(t, wrapper, http.StatusBadRequest)
}

func TestMCPTestEndpointsReturnToolsAndCategories(t *testing.T) {
	env := newM6Env(t, m6Options{})
	seedMCPServer(env)
	env.mcp.result = MCPTestResult{
		LatencyMS: 12, ToolCount: 2,
		Tools: []MCPTool{
			{Name: "read", ReadOnly: true, Bindable: true, Annotated: true},
			{Name: "write", Destructive: true, Bindable: false, Annotated: true},
		},
	}
	client := env.loginAs("code-admin")

	tested := client.request(http.MethodPost, "/api/v1/mcp-servers/mcp-1/test", nil, nil)
	assertStatus(t, tested, http.StatusOK)
	result := env.decode(t, tested)
	if result["ok"] != true || result["latency_ms"] != float64(12) || result["tool_count"] != float64(2) || result["truncated"] != false {
		t.Fatalf("test response = %v", result)
	}
	tools := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}
	read := tools[0].(map[string]any)
	if read["read_only"] != true || read["bindable"] != true || read["annotated"] != true || read["destructive"] != false {
		t.Fatalf("read tool = %v", read)
	}
	write := tools[1].(map[string]any)
	if write["destructive"] != true || write["bindable"] != false {
		t.Fatalf("write tool = %v", write)
	}
	if len(env.mcp.servers) != 1 || !strings.Contains(string(env.mcp.servers[0].Config), "super-secret") {
		t.Fatalf("tester did not receive the stored config: %+v", env.mcp.servers)
	}

	draft := client.json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{
		"scope": "platform", "name": "draft",
		"config": map[string]any{"command": "npx", "env": map[string]any{"API_KEY": "draft-canary"}},
	})
	assertStatus(t, draft, http.StatusOK)
	if !strings.Contains(string(env.mcp.drafts[0].Config), "draft-canary") || env.mcp.drafts[0].ID != "" {
		t.Fatalf("draft input = %+v", env.mcp.drafts[0])
	}

	byID := client.json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{
		"id": "mcp-1", "config": map[string]any{"command": "npx", "env": map[string]any{"API_KEY": "****"}},
	})
	assertStatus(t, byID, http.StatusOK)
	if env.mcp.drafts[1].ID != "mcp-1" || env.mcp.drafts[1].Scope != "platform" || env.mcp.drafts[1].Name != "" {
		t.Fatalf("draft reference = %+v", env.mcp.drafts[1])
	}

	for _, tc := range []struct {
		category string
		status   int
		code     string
	}{
		{mcp.TestCategoryInvalidConfig, http.StatusBadRequest, "invalid_config"},
		{mcp.TestCategoryConnectTimeout, http.StatusGatewayTimeout, "connect_timeout"},
		{mcp.TestCategoryConnectFailed, http.StatusBadGateway, "connect_failed"},
		{mcp.TestCategoryHandshakeFailed, http.StatusBadGateway, "handshake_failed"},
		{mcp.TestCategoryListToolsTimeout, http.StatusGatewayTimeout, "list_tools_timeout"},
		{mcp.TestCategoryListToolsFailed, http.StatusBadGateway, "list_tools_failed"},
		{mcp.TestCategoryTooManyTools, http.StatusUnprocessableEntity, "too_many_tools"},
		{mcp.TestCategoryCleanupFailed, http.StatusInternalServerError, "cleanup_failed"},
	} {
		env.mcp.err = mcp.NewTestError(tc.category, mcp.ErrConnectFailed)
		response := client.request(http.MethodPost, "/api/v1/mcp-servers/mcp-1/test", nil, nil)
		assertStatus(t, response, tc.status)
		body := env.decode(t, response)["error"].(map[string]any)
		if body["code"] != tc.code {
			t.Fatalf("%s error = %v", tc.category, body)
		}
		if strings.Contains(string(response.body), "super-secret") {
			t.Fatalf("%s response leaked config: %s", tc.category, response.body)
		}
	}
	env.mcp.err = nil

	anonymous := env.anonymousClient().json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{
		"scope": "platform", "name": "draft", "config": map[string]any{"command": "npx"},
	})
	assertStatus(t, anonymous, http.StatusUnauthorized)
	viewer := env.loginAs("code-viewer").json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{
		"scope": "platform", "name": "draft", "config": map[string]any{"command": "npx"},
	})
	assertStatus(t, viewer, http.StatusForbidden)
}

func TestMCPDraftMissingStoredServerIsNotFound(t *testing.T) {
	env := newM6Env(t, m6Options{})
	response := env.loginAs("code-admin").json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{"id": "missing"})
	assertStatus(t, response, http.StatusNotFound)
}

func TestMCPDraftRouteDoesNotRevealExistenceToViewer(t *testing.T) {
	env := newM6Env(t, m6Options{})
	seedMCPServer(env)
	viewer := env.loginAs("code-viewer")
	for _, id := range []string{"mcp-1", "missing"} {
		response := viewer.json(http.MethodPost, "/api/v1/mcp-servers:test", map[string]any{"id": id})
		assertStatus(t, response, http.StatusForbidden)
		if body := env.decode(t, response)["error"].(map[string]any); body["code"] != "forbidden" {
			t.Fatalf("id %s error = %v", id, body)
		}
	}
}

func TestMCPTestConcurrencyLimit(t *testing.T) {
	env := newM6Env(t, m6Options{})
	seedMCPServer(env)
	client := env.loginAs("code-admin")
	for i := 0; i < mcpTestConcurrency; i++ {
		mcpTestSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < mcpTestConcurrency; i++ {
			<-mcpTestSlots
		}
	}()
	response := client.request(http.MethodPost, "/api/v1/mcp-servers/mcp-1/test", nil, nil)
	assertStatus(t, response, http.StatusTooManyRequests)
	if body := env.decode(t, response)["error"].(map[string]any); body["code"] != "test_busy" {
		t.Fatalf("error = %v", body)
	}
}

func TestMCPWriteAuditOmitsConfigValues(t *testing.T) {
	_, srv := newExtensionPaginationServer(t)
	var logs bytes.Buffer
	srv.logger = newTestLogger(&logs)
	if srv.extensions == nil {
		t.Fatal("extension service is not wired")
	}
	call := func(handler http.HandlerFunc, method, path string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), principalKey,
			principal{user: sessionUser{ID: 7, Username: "alice", Role: roleAdmin}}))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	created := call(srv.handleCreateMCPServer, http.MethodPost, "/api/v1/mcp-servers",
		[]byte(`{"scope":"platform","name":"audited","config":{"command":"npx","env":{"API_KEY":"audit-canary"}}}`))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	imported := call(srv.handleImportMCPServers, http.MethodPost, "/api/v1/mcp-servers/import?scope=platform",
		[]byte(`{"mcpServers":{"imported":{"command":"npx","env":{"API_KEY":"import-canary"}}}}`))
	if imported.Code != http.StatusCreated {
		t.Fatalf("import status = %d body=%s", imported.Code, imported.Body.String())
	}
	text := logs.String()
	for _, want := range []string{"platform_audit", `"action":"create"`, `"action":"import"`, `"target_id":"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("audit log missing %q: %s", want, text)
		}
	}
	for _, secret := range []string{"audit-canary", "import-canary"} {
		if strings.Contains(text, secret) {
			t.Fatalf("audit log leaked %q: %s", secret, text)
		}
	}
}
