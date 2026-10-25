package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexHelperProcess(t *testing.T) {
	if os.Getenv("MCP_INDEX_HELPER") != "1" {
		t.Skip("helper process")
	}
	out := os.Getenv("MCP_INDEX_OUT")
	if out == "" {
		t.Fatal("MCP_INDEX_OUT is required")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	data, err := json.Marshal(map[string]any{
		"cwd":  cwd,
		"args": os.Args,
		"env":  values,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunIndexUsesArgvRestrictedEnvironment(t *testing.T) {
	t.Setenv("MCP_INDEX_PARENT_SECRET", "parent-secret-value")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	baseDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "index-result.json")
	cfg := ServerConfig{
		Command: exe,
		Env: map[string]string{
			"MCP_INDEX_HELPER": "1",
			"MCP_INDEX_OUT":    out,
		},
		XReview: XReviewConfig{
			EnvAllowlist: []string{"MCP_INDEX_PARENT_SECRET"},
			IndexCommand: []string{
				exe,
				"-test.run=^TestIndexHelperProcess$",
				"--",
				"{base_dir}",
				"{workspace}",
			},
		},
	}
	m, err := New(Options{
		Workspace: workspace,
		Project:   Config{MCPServers: map[string]ServerConfig{"local": cfg}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	count, err := m.RunIndex(context.Background(), baseDir)
	if err != nil {
		t.Fatalf("RunIndex: %v", err)
	}
	if count != 1 {
		t.Fatalf("RunIndex count = %d, want 1", count)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read helper output: %v", err)
	}
	var result struct {
		Cwd  string   `json:"cwd"`
		Args []string `json:"args"`
		Env  map[string]string
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode helper output: %v", err)
	}
	physicalBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cwd != baseDir && result.Cwd != physicalBase {
		t.Fatalf("cwd = %q, base = %q", result.Cwd, baseDir)
	}
	joined := strings.Join(result.Args, "\n")
	if !strings.Contains(joined, baseDir) {
		t.Fatalf("base_dir was not substituted: %v", result.Args)
	}
	if !strings.Contains(joined, workspace) {
		t.Fatalf("workspace was not substituted: %v", result.Args)
	}
	if result.Env["MCP_INDEX_HELPER"] != "1" || result.Env["MCP_INDEX_OUT"] != out {
		t.Fatalf("explicit index env missing: %+v", result.Env)
	}
	if _, ok := result.Env["MCP_INDEX_PARENT_SECRET"]; ok {
		t.Fatal("parent secret was inherited by the index command")
	}
	if !strings.HasPrefix(result.Env["HOME"], workspace) {
		t.Fatalf("index HOME = %q, want below %q", result.Env["HOME"], workspace)
	}
}

func TestRunIndexWithNoConfiguredCommand(t *testing.T) {
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project: Config{MCPServers: map[string]ServerConfig{
			"local": {Command: "/usr/bin/true"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	count, err := m.RunIndex(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("RunIndex: %v", err)
	}
	if count != 0 {
		t.Fatalf("RunIndex count = %d, want 0", count)
	}
}

func TestRunIndexFailureDoesNotLeakArguments(t *testing.T) {
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project: Config{MCPServers: map[string]ServerConfig{
			"local": {
				Command: "/usr/bin/true",
				XReview: XReviewConfig{
					IndexCommand: []string{"/definitely/not/a/command", "TOP-SECRET-ARG"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	count, err := m.RunIndex(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("RunIndex succeeded with a missing executable")
	}
	if count != 0 {
		t.Fatalf("RunIndex count = %d, want 0", count)
	}
	if strings.Contains(err.Error(), "TOP-SECRET-ARG") {
		t.Fatalf("error leaked a command argument: %v", err)
	}
}

func TestRunIndexRejectsMissingBase(t *testing.T) {
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project: Config{MCPServers: map[string]ServerConfig{
			"local": {Command: "/usr/bin/true"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.RunIndex(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("RunIndex accepted a missing base directory")
	}
}
