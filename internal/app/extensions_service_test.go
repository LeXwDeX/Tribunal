package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/mcp"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

func newExtensionFixture(t *testing.T) (*ExtensionService, *sqlite.Store) {
	t.Helper()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	return service, st
}

func TestNewExtensionServiceResolvesRelativeWorkspace(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	absolute := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	relative, err := filepath.Rel(cwd, absolute)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if filepath.IsAbs(relative) {
		t.Fatalf("relative workspace is absolute: %q", relative)
	}

	service, err := NewExtensionService(ExtensionServiceConfig{Store: st, Workspace: relative})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	want, err := filepath.Abs(relative)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if service.workspace != want {
		t.Fatalf("workspace = %q, want %q", service.workspace, want)
	}
	uploaded, err := service.UploadSkill(ctx, SkillUpload{
		Scope: "platform", Data: testSkillArchive(t, testSkillBody),
	})
	if err != nil {
		t.Fatalf("UploadSkill: %v", err)
	}
	if !filepath.IsAbs(uploaded.Dir) || !strings.HasPrefix(uploaded.Dir, filepath.Join(want, "skills")) {
		t.Fatalf("uploaded dir = %q, want below %q", uploaded.Dir, filepath.Join(want, "skills"))
	}
}

func TestNewExtensionServiceRejectsRootWorkspace(t *testing.T) {
	_, err := NewExtensionService(ExtensionServiceConfig{
		Store:     openTestStore(t),
		Workspace: string(filepath.Separator),
	})
	if err == nil {
		t.Fatal("filesystem root accepted as extension workspace")
	}
}

func TestExtensionServiceMCPCRUDAndMaskGuard(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)

	created, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope:   "platform",
		Name:    "github",
		Config:  json.RawMessage(`{"command":"npx","env":{"API_KEY":"super-secret"}}`),
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}
	if created.ID == "" || created.ProjectID != nil {
		t.Fatalf("created = %+v", created)
	}
	if !strings.Contains(string(created.Config), "super-secret") {
		t.Fatalf("config was rewritten: %s", created.Config)
	}

	got, err := service.GetMCPServer(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if got.Name != "github" || !got.Enabled {
		t.Fatalf("got = %+v", got)
	}

	project, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope:     "project",
		ProjectID: &projectID,
		Name:      "gitlab",
		Config:    json.RawMessage(`{"url":"https://mcp.example.com"}`),
	})
	if err != nil {
		t.Fatalf("CreateMCPServer project: %v", err)
	}
	list, err := service.ListMCPServers(ctx, "project", &projectID)
	if err != nil || len(list) != 1 || list[0].ID != project.ID {
		t.Fatalf("ListMCPServers = %+v, %v", list, err)
	}

	enabled := false
	updated, err := service.UpdateMCPServer(ctx, project.ID, MCPServerUpdate{
		Enabled: &enabled,
		Config:  json.RawMessage(`{"url":"https://mcp2.example.com"}`),
	})
	if err != nil {
		t.Fatalf("UpdateMCPServer: %v", err)
	}
	if updated.Enabled || !strings.Contains(string(updated.Config), "mcp2") {
		t.Fatalf("updated = %+v", updated)
	}

	for name, config := range map[string]json.RawMessage{
		"env":     json.RawMessage(`{"command":"npx","env":{"API_KEY":"****"}}`),
		"header":  json.RawMessage(`{"url":"https://x","headers":{"Authorization":"****"}}`),
		"unknown": json.RawMessage(`{"url":"https://x","headers":{"X-Trace":"****"}}`),
	} {
		_, err := service.CreateMCPServer(ctx, MCPServerInput{
			Scope:   "platform",
			Name:    "masked-" + name,
			Config:  config,
			Enabled: true,
		})
		masked := strings.Contains(string(config), maskedExtensionSecret) && name != "unknown"
		if masked && err == nil {
			t.Fatalf("%s: masked placeholder was persisted", name)
		}
		if !masked && err != nil {
			t.Fatalf("%s: non-sensitive placeholder rejected: %v", name, err)
		}
	}

	if err := service.DeleteMCPServer(ctx, project.ID); err != nil {
		t.Fatalf("DeleteMCPServer: %v", err)
	}
	if _, err := service.GetMCPServer(ctx, project.ID); err == nil {
		t.Fatal("deleted server still readable")
	}
}

func TestExtensionServiceImportUpsertsByName(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)

	first, err := service.ImportMCPServers(ctx, MCPImportInput{
		Scope: "platform",
		Servers: map[string]json.RawMessage{
			"zeta":  json.RawMessage(`{"command":"z"}`),
			"alpha": json.RawMessage(`{"command":"a"}`),
		},
	})
	if err != nil {
		t.Fatalf("ImportMCPServers: %v", err)
	}
	if len(first) != 2 || first[0].Name != "alpha" || first[1].Name != "zeta" {
		t.Fatalf("import order = %+v", first)
	}

	second, err := service.ImportMCPServers(ctx, MCPImportInput{
		Scope: "platform",
		Servers: map[string]json.RawMessage{
			"alpha": json.RawMessage(`{"command":"a2"}`),
		},
	})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if len(second) != 1 || !strings.Contains(string(second[0].Config), "a2") {
		t.Fatalf("upsert = %+v", second)
	}
	list, err := service.ListMCPServers(ctx, "platform", nil)
	if err != nil || len(list) != 2 {
		t.Fatalf("list after upsert = %+v, %v", list, err)
	}

	if _, err := service.ImportMCPServers(ctx, MCPImportInput{Scope: "platform", Servers: map[string]json.RawMessage{
		"bad": json.RawMessage(`[]`),
	}}); err == nil {
		t.Fatal("invalid import config accepted")
	}
}

func TestExtensionServiceSkillsAndBindings(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)

	uploaded, err := service.UploadSkill(ctx, SkillUpload{
		Scope: "platform",
		Data:  testSkillArchive(t, generateSkillBody("go-review", "Review Go changes")),
	})
	if err != nil {
		t.Fatalf("UploadSkill: %v", err)
	}
	if uploaded.Dir == "" || !uploaded.Enabled || uploaded.CreatedAt.IsZero() {
		t.Fatalf("uploaded = %+v", uploaded)
	}
	if uploaded.Name != "go-review" || uploaded.Description != "Review Go changes" {
		t.Fatalf("uploaded manifest = %+v", uploaded)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: security\ndescription: Security review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registered, err := service.RegisterSkillDir(ctx, SkillDirInput{
		Scope:     "project",
		ProjectID: &projectID,
		Dir:       dir,
	})
	if err != nil {
		t.Fatalf("RegisterSkillDir: %v", err)
	}
	if registered.Name != "security" || registered.Description != "Security review" {
		t.Fatalf("registered = %+v", registered)
	}
	if registered.Dir == dir || !strings.HasPrefix(registered.Dir, service.workspace) {
		t.Fatalf("registered dir = %q, want a managed copy", registered.Dir)
	}

	binding, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope:    "platform",
		Block:    "code-review",
		MCPTools: []string{"mcp__github__search"},
		Skills:   []string{"go-review"},
	})
	if err != nil {
		t.Fatalf("UpsertBlockBinding: %v", err)
	}
	if !reflect.DeepEqual(binding.Skills, []string{"go-review"}) {
		t.Fatalf("binding = %+v", binding)
	}
	list, err := service.ListBlockBindings(ctx, "platform", nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListBlockBindings = %+v, %v", list, err)
	}

	if err := service.DeleteSkill(ctx, uploaded.ID); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
	if err := service.DeleteSkill(ctx, uploaded.ID); err == nil {
		t.Fatal("second DeleteSkill succeeded")
	}
	if _, err := service.UploadSkill(ctx, SkillUpload{Scope: "project", Name: "x"}); err == nil {
		t.Fatal("project skill without project_id accepted")
	}
}

func newHTTPMCPTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test-mcp", Version: "1.0.0"}, nil)
	server.AddTool(&sdk.Tool{Name: "echo", Description: "echo text", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
		})
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	httpServer := httptest.NewServer(inner)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestExtensionServiceTestMCPServerBothPaths(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	httpServer := newHTTPMCPTestServer(t)
	config := json.RawMessage(`{"url":"` + httpServer.URL + `"}`)

	tools, err := service.TestMCPServer(ctx, MCPServer{Name: "remote", Config: config})
	if err != nil {
		t.Fatalf("TestMCPServer short-lived: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}

	manager, err := mcp.New(mcp.Options{
		Platform: mcp.Config{MCPServers: map[string]mcp.ServerConfig{
			"remote": {URL: httpServer.URL},
		}},
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	service.mcp = manager
	tools, err = service.TestMCPServer(ctx, MCPServer{Name: "remote", Config: config})
	if err != nil {
		t.Fatalf("TestMCPServer existing: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("existing tools = %+v", tools)
	}

	other, err := service.TestMCPServer(ctx, MCPServer{Name: "other", Config: config})
	if err != nil || len(other) != 1 {
		t.Fatalf("not-yet-loaded test = %+v, %v", other, err)
	}

	if _, err := service.TestMCPServer(ctx, MCPServer{Name: "bad", Config: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("invalid config accepted")
	}
	unreachable := json.RawMessage(`{"url":"http://127.0.0.1:1/mcp","headers":{"Authorization":"Bearer super-secret"}}`)
	_, err = service.TestMCPServer(ctx, MCPServer{Name: "unreachable", Config: unreachable})
	if err == nil {
		t.Fatal("unreachable server test succeeded")
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error leaked config details: %v", err)
	}
}

func TestExtensionServiceTestMCPServerUsesSubmittedConfig(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	stale, err := mcp.New(mcp.Options{
		Platform: mcp.Config{MCPServers: map[string]mcp.ServerConfig{
			"same": {URL: "http://127.0.0.1:1/mcp"},
		}},
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("stale manager: %v", err)
	}
	t.Cleanup(func() { _ = stale.Close() })
	service.mcp = stale

	httpServer := newHTTPMCPTestServer(t)
	tools, err := service.TestMCPServer(ctx, MCPServer{
		Name: "same", Config: json.RawMessage(`{"url":"` + httpServer.URL + `"}`),
	})
	if err != nil {
		t.Fatalf("TestMCPServer: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestExtensionServiceTestMCPServerParsesConfig(t *testing.T) {
	ctx := context.Background()
	httpServer := newHTTPMCPTestServer(t)

	t.Run("expands environment", func(t *testing.T) {
		service, _ := newExtensionFixture(t)
		t.Setenv("MCP_TEST_URL", httpServer.URL)
		tools, err := service.TestMCPServer(ctx, MCPServer{
			Name: "expanded", Config: json.RawMessage(`{"url":"${MCP_TEST_URL}"}`),
		})
		if err != nil {
			t.Fatalf("TestMCPServer: %v", err)
		}
		if len(tools) != 1 || tools[0].Name != "echo" {
			t.Fatalf("tools = %+v", tools)
		}
	})

	t.Run("denied environment", func(t *testing.T) {
		service, err := NewExtensionService(ExtensionServiceConfig{
			Store:     openTestStore(t),
			Workspace: t.TempDir(),
			DeniedEnv: []string{"MCP_DENIED_URL"},
		})
		if err != nil {
			t.Fatalf("NewExtensionService: %v", err)
		}
		t.Setenv("MCP_DENIED_URL", httpServer.URL)
		_, err = service.TestMCPServer(ctx, MCPServer{
			Name: "denied", Config: json.RawMessage(`{"url":"${MCP_DENIED_URL}"}`),
		})
		if !errors.Is(err, mcp.ErrSensitiveEnv) {
			t.Fatalf("error = %v, want ErrSensitiveEnv", err)
		}
		if strings.Contains(err.Error(), httpServer.URL) {
			t.Fatalf("error leaked the environment value: %v", err)
		}
	})

	t.Run("missing environment", func(t *testing.T) {
		service, _ := newExtensionFixture(t)
		_, err := service.TestMCPServer(ctx, MCPServer{
			Name: "missing", Config: json.RawMessage(`{"url":"${MCP_TEST_MISSING_NEVER_SET}"}`),
		})
		if err == nil || !strings.Contains(err.Error(), "environment variable MCP_TEST_MISSING_NEVER_SET is not set") {
			t.Fatalf("error = %v, want a missing environment error", err)
		}
	})
}

func TestExtensionServiceTestMCPServerUsesProjectWorkspace(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)
	command := filepath.Join(t.TempDir(), "missing-mcp-server")
	_, err := service.TestMCPServer(ctx, MCPServer{
		Scope:     "project",
		ProjectID: &projectID,
		Name:      "cwd",
		Config:    json.RawMessage(`{"command":"` + command + `","x-review":{"home":"probe","call_timeout":"200ms"}}`),
	})
	if err == nil {
		t.Fatal("missing command did not fail")
	}
	if _, statErr := os.Stat(filepath.Join(service.workspace, "42", "probe")); statErr != nil {
		t.Fatalf("project workspace was not used: %v", statErr)
	}
}

func TestExtensionServiceTestProvider(t *testing.T) {
	ctx := context.Background()
	t.Setenv("EXT_TEST_API_KEY", "test-secret-key")
	var sawMaxTokens bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		sawMaxTokens = request.MaxTokens == 1
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"fake-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)

	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{
		"fake": {
			Protocol:       "openai-chat",
			BaseURL:        server.URL + "/v1",
			Model:          "fake-model",
			APIKeyEnv:      "EXT_TEST_API_KEY",
			MaxConcurrency: 1,
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	st := openTestStore(t)
	service, err := NewExtensionService(ExtensionServiceConfig{Store: st, Clients: registry, Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	result, err := service.TestProvider(ctx, "fake")
	if err != nil {
		t.Fatalf("TestProvider: %v", err)
	}
	if result.Model != "fake-model" || result.LatencyMS < 0 {
		t.Fatalf("result = %+v", result)
	}
	if !sawMaxTokens {
		t.Fatal("provider test did not send max_tokens=1")
	}

	if _, err := service.TestProvider(ctx, "missing"); err == nil {
		t.Fatal("unknown provider accepted")
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	t.Cleanup(failing.Close)
	badRegistry, err := llm.NewRegistry(map[string]config.ProviderConfig{
		"bad": {
			Protocol:       "openai-chat",
			BaseURL:        failing.URL + "/v1",
			Model:          "fake-model",
			APIKeyEnv:      "EXT_TEST_API_KEY",
			MaxConcurrency: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.clients = badRegistry
	_, err = service.TestProvider(ctx, "bad")
	if err == nil {
		t.Fatal("failing provider reported success")
	}
	if strings.Contains(err.Error(), "test-secret-key") || strings.Contains(err.Error(), failing.URL) {
		t.Fatalf("provider error leaked details: %v", err)
	}
	if !strings.Contains(err.Error(), string(llm.ErrAuth)) {
		t.Fatalf("provider error lost classification: %v", err)
	}
}

func TestExtensionServiceValidation(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)

	if _, err := service.CreateMCPServer(ctx, MCPServerInput{Scope: "project", Name: "x", Config: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("project scope without project_id accepted")
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{Scope: "platform", Name: "x", Config: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("empty config accepted")
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{Scope: "platform", Config: json.RawMessage(`{"command":"x"}`)}); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "platform", Dir: "relative/path"}); err == nil {
		t.Fatal("relative skill dir accepted")
	}
	if _, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "platform", Dir: t.TempDir()}); err == nil {
		t.Fatal("dir without SKILL.md accepted")
	}
	if _, err := service.ListMCPServers(ctx, "global", nil); err == nil {
		t.Fatal("invalid scope accepted")
	}
}

func TestExtensionServiceRejectsInvalidMCPNamesAndConfigs(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)

	cases := []struct {
		name   string
		config json.RawMessage
	}{
		{name: "stdio without command", config: json.RawMessage(`{"type":"stdio"}`)},
		{name: "http without url", config: json.RawMessage(`{"type":"http","url":""}`)},
		{name: "unknown field", config: json.RawMessage(`{"command":"x","bogus":"super-secret"}`)},
		{name: "unknown x-review field", config: json.RawMessage(`{"command":"x","x-review":{"bogus":true}}`)},
		{name: "invalid env name", config: json.RawMessage(`{"command":"x","env":{"BAD-NAME":"value"}}`)},
		{name: "HOME override", config: json.RawMessage(`{"command":"x","env":{"HOME":"/tmp"}}`)},
		{name: "sensitive template", config: json.RawMessage(`{"command":"x","args":["${GITLAB_TOKEN}"]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.CreateMCPServer(ctx, MCPServerInput{
				Scope: "platform", Name: "invalid", Config: tc.config,
			})
			if err == nil {
				t.Fatal("invalid config accepted")
			}
			if strings.Contains(err.Error(), "super-secret") {
				t.Fatalf("validation error leaked a config value: %v", err)
			}
		})
	}
	servers, err := service.ListMCPServers(ctx, "platform", nil)
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("invalid creates persisted rows: %+v", servers)
	}

	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "bad/name", Config: json.RawMessage(`{"command":"x"}`),
	}); err == nil {
		t.Fatal("invalid server name accepted")
	}
}

func TestExtensionServiceInvalidUpdatePersistsNothing(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	created, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "original", Config: json.RawMessage(`{"command":"original"}`),
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	badName := "bad/name"
	if _, err := service.UpdateMCPServer(ctx, created.ID, MCPServerUpdate{Name: &badName}); err == nil {
		t.Fatal("invalid update name accepted")
	}
	if _, err := service.UpdateMCPServer(ctx, created.ID, MCPServerUpdate{
		Config: json.RawMessage(`{"command":"x","bogus":"super-secret"}`),
	}); err == nil {
		t.Fatal("invalid update config accepted")
	} else if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("validation error leaked a config value: %v", err)
	}

	stored, err := service.GetMCPServer(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if stored.Name != "original" || string(stored.Config) != `{"command":"original"}` {
		t.Fatalf("invalid update changed the row: %+v", stored)
	}
}

func TestExtensionServiceImportValidatesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)

	if _, err := service.ImportMCPServers(ctx, MCPImportInput{
		Scope: "platform",
		Servers: map[string]json.RawMessage{
			"good": json.RawMessage(`{"command":"good"}`),
			"bad":  json.RawMessage(`{"command":"x","bogus":"super-secret"}`),
		},
	}); err == nil {
		t.Fatal("invalid import accepted")
	} else if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("validation error leaked a config value: %v", err)
	}
	if _, err := service.ImportMCPServers(ctx, MCPImportInput{
		Scope:   "platform",
		Servers: map[string]json.RawMessage{"bad/name": json.RawMessage(`{"command":"x"}`)},
	}); err == nil {
		t.Fatal("invalid import name accepted")
	}
	servers, err := service.ListMCPServers(ctx, "platform", nil)
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("invalid import persisted rows: %+v", servers)
	}
}

func TestExtensionServiceValidatesEnvTemplatesWithoutExpanding(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: t.TempDir(),
		DeniedEnv: []string{"CUSTOM_DENIED_FLAG"},
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}

	created, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "templated",
		Config: json.RawMessage(`{"command":"npx","env":{"SAFE":"${SAFE_VALUE}"}}`),
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}
	if !strings.Contains(string(created.Config), "${SAFE_VALUE}") {
		t.Fatalf("stored config expanded a template: %s", created.Config)
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "denied",
		Config: json.RawMessage(`{"command":"npx","env":{"CUSTOM":"${CUSTOM_DENIED_FLAG}"}}`),
	}); err == nil {
		t.Fatal("denied environment template accepted")
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "denied-key",
		Config: json.RawMessage(`{"command":"npx","env":{"CUSTOM_DENIED_FLAG":"value"}}`),
	}); err == nil {
		t.Fatal("denied environment variable accepted")
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "sensitive",
		Config: json.RawMessage(`{"command":"npx","env":{"CUSTOM":"${OPENAI_API_KEY}"}}`),
	}); err == nil {
		t.Fatal("sensitive environment template accepted")
	}
}

const testSkillBody = "---\nname: go-review\ndescription: Review Go changes\n---\n\n# Body\n"

func testSkillArchive(t *testing.T, body string) []byte {
	return testSkillArchiveFiles(t, map[string]string{"SKILL.md": body})
}

func generateSkillBody(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n"
}

func testSkillArchiveFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	if files == nil {
		files = map[string]string{"SKILL.md": testSkillBody}
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := entry.Write([]byte(files[name])); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buffer.Bytes()
}
