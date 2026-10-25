package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioHelperProcess(t *testing.T) {
	if os.Getenv("MCP_STDIO_HELPER") != "1" {
		t.Skip("helper process")
	}
	server := testServer()
	if err := server.Run(context.Background(), &sdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		// stderr is discarded by the manager; keep this diagnostic local.
		os.Stderr.WriteString("stdio helper: " + err.Error() + "\n")
	}
}

func newStdioManager(t *testing.T, review XReviewConfig, denied []string) (*Manager, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	workspace := t.TempDir()
	cfg := ServerConfig{
		Command: exe,
		Args:    []string{"-test.run=^TestStdioHelperProcess$"},
		Env:     map[string]string{"MCP_STDIO_HELPER": "1"},
		XReview: review,
	}
	m, err := New(Options{
		Workspace: workspace,
		Project:   Config{MCPServers: map[string]ServerConfig{"local": cfg}},
		DeniedEnv: denied,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return m, workspace
}

func TestStdioAllowlistEnvironmentAndLifecycle(t *testing.T) {
	t.Setenv("MCP_STDIO_ALLOWED", "allowed-value")
	t.Setenv("MCP_STDIO_PARENT_SECRET", "parent-secret-value")
	t.Setenv("GITLAB_TOKEN", "gitlab-secret-value")

	m, workspace := newStdioManager(t, XReviewConfig{
		EnvAllowlist: []string{"MCP_STDIO_ALLOWED", "MCP_STDIO_PARENT_SECRET", "GITLAB_TOKEN"},
	}, []string{"GITLAB_TOKEN"})

	set, err := m.Tools(context.Background(), []string{
		"mcp__local__echo",
		"mcp__local__env",
		"mcp__local__destroy",
		"mcp__local__noann",
		"mcp__local__missing",
	})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(set.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(set.Tools))
	}
	warnings := strings.Join(set.Warnings, "\n")
	if !strings.Contains(warnings, "mutation annotations") || !strings.Contains(warnings, "not advertised") {
		t.Fatalf("warnings = %q", warnings)
	}
	for _, tool := range set.Tools {
		if tool.Def().Name == "mcp__local__pid" {
			t.Fatal("unlisted tool was exposed")
		}
	}

	echo := findTool(t, set.Tools, "mcp__local__echo")
	res, err := echo.Call(context.Background(), json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if strings.TrimSpace(res.Content) != "echo:hello" {
		t.Fatalf("echo content = %q", res.Content)
	}

	envTool := findTool(t, set.Tools, "mcp__local__env")
	res, err = envTool.Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(res.Content), &values); err != nil {
		t.Fatalf("env output: %v", err)
	}
	if values["MCP_STDIO_HELPER"] != "1" || values["MCP_STDIO_ALLOWED"] != "allowed-value" {
		t.Fatalf("explicit/allowed env missing: %+v", values)
	}
	for _, name := range []string{"MCP_STDIO_PARENT_SECRET", "GITLAB_TOKEN"} {
		if _, ok := values[name]; ok {
			t.Fatalf("restricted env %s reached the child", name)
		}
	}
	if !strings.HasPrefix(values["HOME"], workspace) {
		t.Fatalf("HOME = %q, want below %q", values["HOME"], workspace)
	}

	infos, err := m.ListTools(context.Background(), "local")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	foundDestroy := false
	for _, info := range infos {
		if info.Name == "destroy" {
			foundDestroy = true
			if !info.Destructive {
				t.Fatal("ListTools lost destructive annotation")
			}
		}
	}
	if !foundDestroy {
		t.Fatal("ListTools did not include the advertised destructive tool")
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.Tools(context.Background(), []string{"mcp__local__echo"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Tools after Close error = %v", err)
	}
	if _, err := echo.Call(context.Background(), json.RawMessage(`{"text":"after"}`)); err == nil {
		t.Fatal("tool call after Close succeeded")
	}
}

func TestStdioPoolReusesProcess(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{}, nil)
	set, err := m.Tools(context.Background(), []string{"mcp__local__pid"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	pid := findTool(t, set.Tools, "mcp__local__pid")
	first := pidOf(t, pid)
	second := pidOf(t, pid)
	if first == "" || first != second {
		t.Fatalf("pid changed without restart: %q then %q", first, second)
	}
}

func TestStdioCallTimeoutKeepsProcess(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{CallTimeout: Duration(300 * time.Millisecond)}, nil)
	set, err := m.Tools(context.Background(), []string{"mcp__local__slow", "mcp__local__echo"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	slow := findTool(t, set.Tools, "mcp__local__slow")
	_, err = slow.Call(context.Background(), json.RawMessage(`{"ms":2000}`))
	if !errors.Is(err, ErrCallFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow call error = %v", err)
	}
	echo := findTool(t, set.Tools, "mcp__local__echo")
	res, err := echo.Call(context.Background(), json.RawMessage(`{"text":"still-alive"}`))
	if err != nil {
		t.Fatalf("echo after timeout: %v", err)
	}
	if strings.TrimSpace(res.Content) != "echo:still-alive" {
		t.Fatalf("echo after timeout = %q", res.Content)
	}
}

func TestStdioCrashRestartsProcess(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{}, nil)
	set, err := m.Tools(context.Background(), []string{"mcp__local__pid", "mcp__local__crash"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	pid := findTool(t, set.Tools, "mcp__local__pid")
	first := pidOf(t, pid)

	crash := findTool(t, set.Tools, "mcp__local__crash")
	if _, err := crash.Call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("crash call succeeded")
	}

	deadline := time.Now().Add(5 * time.Second)
	var second agent.Result
	for time.Now().Before(deadline) {
		second, err = pid.Call(context.Background(), json.RawMessage(`{}`))
		if err == nil && strings.TrimSpace(second.Content) != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("pid after restart: %v", err)
	}
	secondPID := strings.TrimSpace(second.Content)
	if secondPID == "" {
		t.Fatal("pid after restart was empty")
	}
	if first == secondPID {
		t.Fatalf("process was not restarted (pid %q)", first)
	}
}

func TestStdioIdleReapsProcess(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{IdleTimeout: Duration(100 * time.Millisecond)}, nil)
	set, err := m.Tools(context.Background(), []string{"mcp__local__pid"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	pid := findTool(t, set.Tools, "mcp__local__pid")
	first := pidOf(t, pid)
	time.Sleep(400 * time.Millisecond)
	m.ReapIdle()
	second := pidOf(t, pid)
	if first == second {
		t.Fatalf("idle session was not closed (pid %q)", first)
	}
}

func TestStdioEmptyAllowlistDoesNotSpawn(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project: Config{MCPServers: map[string]ServerConfig{
			"local": {Command: exe, Args: []string{"-test.run=^TestStdioHelperProcess$"}},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	set, err := m.Tools(context.Background(), nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(set.Tools) != 0 {
		t.Fatalf("empty allowlist returned %d tools", len(set.Tools))
	}
	m.mu.Lock()
	entries := len(m.entries)
	m.mu.Unlock()
	if entries != 0 {
		t.Fatal("empty allowlist started a process")
	}
}

func TestStdioUnknownServerAndInputValidation(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{}, nil)
	if _, err := m.Tools(context.Background(), []string{"mcp__missing__echo"}); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("unknown server error = %v", err)
	}
	set, err := m.Tools(context.Background(), []string{"mcp__local__echo"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	echo := findTool(t, set.Tools, "mcp__local__echo")
	if _, err := echo.Call(context.Background(), json.RawMessage(`[]`)); err == nil {
		t.Fatal("array input accepted")
	}
	if _, err := echo.Call(context.Background(), json.RawMessage(`null`)); err == nil {
		t.Fatal("null input accepted")
	}
	if _, err := echo.Call(context.Background(), json.RawMessage(`{"text":"`+strings.Repeat("x", maxToolInput)+`"}`)); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestStdioProjectOverridesPlatform(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	m, err := New(Options{
		Workspace: workspace,
		Platform: Config{MCPServers: map[string]ServerConfig{
			"local": {Command: "/definitely/not/a/real/mcp-command"},
		}},
		Project: Config{MCPServers: map[string]ServerConfig{
			"local": {
				Command: exe,
				Args:    []string{"-test.run=^TestStdioHelperProcess$"},
				Env:     map[string]string{"MCP_STDIO_HELPER": "1"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	set, err := m.Tools(context.Background(), []string{"mcp__local__echo"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(set.Tools) != 1 {
		t.Fatalf("tools = %d", len(set.Tools))
	}
	if _, err := findTool(t, set.Tools, "mcp__local__echo").Call(context.Background(), json.RawMessage(`{"text":"merged"}`)); err != nil {
		t.Fatalf("echo after merge: %v", err)
	}
}

func TestWorkspaceHomeIsCreatedForStdio(t *testing.T) {
	m, workspace := newStdioManager(t, XReviewConfig{}, nil)
	if _, err := m.Tools(context.Background(), []string{"mcp__local__echo"}); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	home := filepath.Join(workspace, ".mcp", "local", "home")
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		t.Fatalf("home not created: %v", err)
	}
}
