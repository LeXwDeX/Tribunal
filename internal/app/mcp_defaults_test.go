package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// graphDefault is a codebase-memory default with the given args and
// code-review tools; verify always gets get_code_snippet.
func graphDefault(args []string, codeReviewTools ...string) []config.DefaultMCPServer {
	argv := make([]any, len(args))
	for i, arg := range args {
		argv[i] = arg
	}
	if len(codeReviewTools) == 0 {
		codeReviewTools = []string{"search_graph"}
	}
	return []config.DefaultMCPServer{{
		Name: "codebase-memory",
		Config: map[string]any{
			"command":  "/usr/local/bin/tribunal-codebase-memory-mcp",
			"args":     argv,
			"x-review": map[string]any{"max_output_chars": 20000},
		},
		Tools: map[string][]string{
			"code-review": codeReviewTools,
			"verify":      {"get_code_snippet"},
		},
	}}
}

var analysisArgs = []string{"--tool-profile=analysis"}

func applyMCPDefaults(t *testing.T, service *ExtensionService, st *sqlite.Store, fileBlocks map[string]config.BlockBinding,
	fileMCP mcp.Config, specs []config.DefaultMCPServer) {
	t.Helper()
	defaults, err := service.MCPDefaults(specs, fileMCP)
	if err != nil {
		t.Fatalf("MCPDefaults: %v", err)
	}
	if err := ApplyExtensionDefaults(context.Background(), st, fileBlocks, defaults, nil); err != nil {
		t.Fatalf("ApplyExtensionDefaults: %v", err)
	}
}

func platformMCPServer(t *testing.T, st *sqlite.Store, name string) *store.MCPServer {
	t.Helper()
	rows, err := st.ListMCPServers(context.Background(), store.ScopePlatform, nil)
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	var found *store.MCPServer
	for i := range rows {
		if rows[i].Name == name {
			if found != nil {
				t.Fatalf("duplicate platform server %q", name)
			}
			found = &rows[i]
		}
	}
	return found
}

func storedArgs(t *testing.T, server *store.MCPServer) []string {
	t.Helper()
	parsed, err := mcp.ParseServer(server.Config)
	if err != nil {
		t.Fatalf("stored default does not parse: %v", err)
	}
	return parsed.Args
}

func TestMCPDefaultsRegisterBuiltinServerAndBindTools(t *testing.T) {
	service, st := newExtensionFixture(t)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))

	server := platformMCPServer(t, st, "codebase-memory")
	if server == nil || !server.Enabled || server.Builtin != "codebase-memory" {
		t.Fatalf("server = %+v, want an enabled built-in platform server", server)
	}
	if got := storedArgs(t, server); !slices.Equal(got, analysisArgs) {
		t.Fatalf("args = %q", got)
	}
	listed, err := service.ListMCPServers(context.Background(), store.ScopePlatform, nil)
	if err != nil || len(listed) != 1 || !listed[0].Builtin {
		t.Fatalf("ListMCPServers = %+v, %v; want the built-in flag", listed, err)
	}
	bindings := platformBindings(t, st)
	if got := bindings["code-review"].MCPTools; !slices.Equal(got, []string{"mcp__codebase-memory__search_graph"}) {
		t.Fatalf("code-review tools = %q", got)
	}
	if got := bindings["verify"].MCPTools; !slices.Equal(got, []string{"mcp__codebase-memory__get_code_snippet"}) {
		t.Fatalf("verify tools = %q", got)
	}

	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))
	again := platformMCPServer(t, st, "codebase-memory")
	if again.ID != server.ID || !again.UpdatedAt.Equal(server.UpdatedAt) {
		t.Fatalf("restart rewrote an unchanged default: before %+v after %+v", server, again)
	}
}

func TestMCPDefaultsUpgradeConfigAndKeepDisabledState(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))
	server := platformMCPServer(t, st, "codebase-memory")
	disabled := false
	if _, err := service.UpdateMCPServer(ctx, server.ID, MCPServerUpdate{Enabled: &disabled}); err != nil {
		t.Fatalf("disable built-in server: %v", err)
	}

	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))
	if got := platformMCPServer(t, st, "codebase-memory"); got.Enabled {
		t.Fatal("a restart re-enabled the server the administrator disabled")
	}

	upgraded := []string{"--tool-profile=analysis", "--new-flag"}
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(upgraded))
	got := platformMCPServer(t, st, "codebase-memory")
	if args := storedArgs(t, got); !slices.Equal(args, upgraded) {
		t.Fatalf("upgrade kept the old config: args %q", args)
	}
	if got.Enabled {
		t.Fatal("upgrade re-enabled the disabled built-in server")
	}
}

func TestMCPDefaultsUpgradeToolGrantsOnDefaultOwnedBindings(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs, "search_graph", "old_tool"))
	if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "verify", MCPTools: []string{},
	}); err != nil {
		t.Fatal(err)
	}

	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs, "search_graph", "trace_path"))
	bindings := platformBindings(t, st)
	want := []string{"mcp__codebase-memory__search_graph", "mcp__codebase-memory__trace_path"}
	if got := bindings["code-review"].MCPTools; !slices.Equal(got, want) {
		t.Fatalf("code-review tools after upgrade = %q, want %q", got, want)
	}
	if got := bindings["verify"].MCPTools; len(got) != 0 {
		t.Fatalf("upgrade rewrote the binding the administrator emptied: %q", got)
	}

	// The administrator's later change to an upgraded binding also wins.
	if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "code-review", MCPTools: []string{"mcp__codebase-memory__search_graph"},
	}); err != nil {
		t.Fatal(err)
	}
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs, "search_graph", "trace_path", "query_graph"))
	if got := platformBindings(t, st)["code-review"].MCPTools; !slices.Equal(got, []string{"mcp__codebase-memory__search_graph"}) {
		t.Fatalf("upgrade rewrote an administrator-changed binding: %q", got)
	}
}

func TestMCPDefaultsLeaveAdministratorServersAlone(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	if _, err := st.CreateMCPServer(ctx, store.MCPServer{
		Scope: store.ScopePlatform, Name: "codebase-memory", Enabled: false,
		Config: json.RawMessage(`{"command":"/opt/cbm"}`),
	}); err != nil {
		t.Fatal(err)
	}
	specs := append(graphDefault(analysisArgs), config.DefaultMCPServer{
		Name: "file-graph", Config: map[string]any{"command": "/bin/true"},
		Tools: map[string][]string{"review": {"search"}},
	})
	fileMCP := mcp.Config{MCPServers: map[string]mcp.ServerConfig{"file-graph": {Command: "/bin/true"}}}
	applyMCPDefaults(t, service, st, nil, fileMCP, specs)
	applyMCPDefaults(t, service, st, nil, fileMCP, graphDefault([]string{"--tool-profile=scout"}))

	server := platformMCPServer(t, st, "codebase-memory")
	if server.Builtin != "" || server.Enabled || !strings.Contains(string(server.Config), "/opt/cbm") {
		t.Fatalf("administrator-owned server changed: %+v", server)
	}
	if got := platformMCPServer(t, st, "file-graph"); got != nil {
		t.Fatalf("server defined in mcp.config was duplicated in the store: %+v", got)
	}
	if bindings := platformBindings(t, st); len(bindings) != 0 {
		t.Fatalf("defaults of administrator-owned servers were bound: %+v", bindings)
	}
}

func TestBuiltinMCPServerAllowsOnlyEnableChanges(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))
	server := platformMCPServer(t, st, "codebase-memory")

	var appErr *Error
	if err := service.DeleteMCPServer(ctx, server.ID); !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("DeleteMCPServer = %v, want a conflict for a built-in server", err)
	}
	rename := "graph"
	if _, err := service.UpdateMCPServer(ctx, server.ID, MCPServerUpdate{Name: &rename}); !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("rename = %v, want a conflict", err)
	}
	if _, err := service.UpdateMCPServer(ctx, server.ID, MCPServerUpdate{Config: json.RawMessage(`{"command":"/bin/true"}`)}); !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("config edit = %v, want a conflict", err)
	}
	if _, err := service.ImportMCPServers(ctx, MCPImportInput{
		Scope: store.ScopePlatform, Servers: map[string]json.RawMessage{"codebase-memory": json.RawMessage(`{"command":"/bin/true"}`)},
	}); !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("import over the built-in server = %v, want a conflict", err)
	}
	same := "codebase-memory"
	disabled := false
	updated, err := service.UpdateMCPServer(ctx, server.ID, MCPServerUpdate{Name: &same, Enabled: &disabled})
	if err != nil || updated.Enabled || !updated.Builtin {
		t.Fatalf("disable = %+v, %v; want a disabled built-in server", updated, err)
	}
	if got := platformMCPServer(t, st, "codebase-memory"); got == nil || got.Enabled {
		t.Fatalf("stored server = %+v", got)
	}
}

func TestMCPDefaultsRejectInvalidServer(t *testing.T) {
	service, _ := newExtensionFixture(t)
	_, err := service.MCPDefaults([]config.DefaultMCPServer{{
		Name: "bad", Config: map[string]any{"command": "/bin/true", "unknown": true},
	}}, mcp.Config{})
	if err == nil || !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("err = %v, want a validation error naming the server", err)
	}
}

func TestMCPToolProviderSkipsUnconfiguredServers(t *testing.T) {
	// A disabled or deleted server is absent from the manager. Its tools
	// stay in the binding but are not offered; that is the administrator's
	// choice, not a degraded review.
	manager, err := mcp.New(mcp.Options{
		Workspace: t.TempDir(),
		Platform:  mcp.Config{MCPServers: map[string]mcp.ServerConfig{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	provider := NewMCPToolProvider(manager, map[string]config.BlockBinding{
		"code-review": {MCPTools: []string{"mcp__codebase-memory__search_graph"}},
	})
	tools, err := provider.Tools(context.Background(), "code-review")
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools = %d, %v; want disabled servers skipped without error", len(tools), err)
	}
}

func TestRuntimeServesBuiltinMCPServerUntilDisabled(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: t.TempDir(), RunCleanupAfter: time.Hour},
		Storage: config.StorageConfig{
			LLMCallRetention: time.Hour, DeliveryRetention: time.Hour, JobRetention: time.Hour, ContextVersions: 1,
		},
		Engine:            config.EngineConfig{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Minute},
		Profiles:          testProfiles(),
		DefaultExtensions: config.DefaultExtensionsConfig{MCPServers: graphDefault(analysisArgs)},
	}
	start := func() *Runtime {
		t.Helper()
		runtime, err := NewRuntime(ctx, cfg, RuntimeDeps{
			Store: st, Cipher: cipher, GitLab: newRuntimeTestGitLab(), LLM: runtimeTestClients{},
			Git: repoGit{git: &repo.Git{}},
		})
		if err != nil {
			t.Fatalf("NewRuntime: %v", err)
		}
		return runtime
	}

	runtime := start()
	if !runtime.MCPManager().HasServer("codebase-memory") {
		t.Fatal("fresh runtime does not serve the built-in MCP server")
	}
	server := platformMCPServer(t, st, "codebase-memory")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := st.UpdateMCPServer(ctx, server.ID, store.MCPServerUpdate{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	runtime = start()
	t.Cleanup(func() { _ = runtime.Close() })
	if runtime.MCPManager().HasServer("codebase-memory") {
		t.Fatal("restart re-enabled the built-in MCP server the administrator disabled")
	}
}
