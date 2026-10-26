package scripts_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCodebaseMemoryMCPWrapperConfinesHome proves the wrapper replaces
// inherited shared cache/runtime values with per-HOME directories.
func TestCodebaseMemoryMCPWrapperConfinesHome(t *testing.T) {
	bin := fakeCodebaseMemoryMCPBinary(t)
	root := t.TempDir()
	mcpArgs := []string{"--tool-profile=analysis", "--ui=false"}

	for _, name := range []string{"home-a", "home-b"} {
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(root, name)
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(home, "calls.log")
			stdout, stderr, exit := runCodebaseMemoryMCPWrapper(t,
				codebaseMemoryMCPEnv(bin, logPath, home, "FAKE_STDOUT=mcp-ok"), mcpArgs...)
			if exit != 0 {
				t.Fatalf("exit = %d, want 0\nstderr:\n%s", exit, stderr)
			}
			if stdout != "mcp-ok\n" {
				t.Fatalf("stdout = %q, want only the MCP payload (config output must be suppressed)", stdout)
			}

			wantCache := filepath.Join(home, ".cache", "codebase-memory-mcp")
			wantRuntime := filepath.Join(home, "run")
			invocations := readFakeCBMInvocations(t, logPath)
			if len(invocations) != 5 {
				t.Fatalf("invocations = %d, want 4 config sets and 1 MCP call", len(invocations))
			}
			for _, invocation := range invocations {
				if invocation.env[0] != home || invocation.env[1] != wantCache || invocation.env[2] != wantRuntime {
					t.Fatalf("child env = %v, want HOME %q cache %q runtime %q", invocation.env, home, wantCache, wantRuntime)
				}
			}

			configs := map[string]string{}
			for _, invocation := range invocations[:4] {
				if len(invocation.args) != 4 || invocation.args[0] != "config" || invocation.args[1] != "set" {
					t.Fatalf("unexpected setup invocation: %v", invocation.args)
				}
				configs[invocation.args[2]] = invocation.args[3]
			}
			wantConfigs := map[string]string{
				"auto_index":      "false",
				"auto_watch":      "false",
				"watcher_enabled": "false",
				"ui_enabled":      "false",
			}
			if len(configs) != len(wantConfigs) {
				t.Fatalf("config keys = %v, want %v", configs, wantConfigs)
			}
			for key, want := range wantConfigs {
				if configs[key] != want {
					t.Fatalf("config %s = %q, want %q", key, configs[key], want)
				}
			}
			if !slices.Equal(invocations[4].args, mcpArgs) {
				t.Fatalf("MCP args = %v, want %v", invocations[4].args, mcpArgs)
			}

			for _, dir := range []string{wantCache, wantRuntime} {
				info, err := os.Stat(dir)
				if err != nil {
					t.Fatalf("private directory %s: %v", dir, err)
				}
				if !info.IsDir() || info.Mode().Perm() != 0o700 {
					t.Fatalf("%s mode = %v, want private directory 0700", dir, info.Mode())
				}
			}
		})
	}
}

// TestCodebaseMemoryMCPWrapperPassesIndexArgs proves one wrapper serves both
// the MCP server and the index_command CLI without rewriting arguments.
func TestCodebaseMemoryMCPWrapperPassesIndexArgs(t *testing.T) {
	bin := fakeCodebaseMemoryMCPBinary(t)
	home := t.TempDir()
	logPath := filepath.Join(home, "calls.log")
	args := []string{"cli", "--quiet", "index_repository", `{"repo_path":"/tmp/a b","query":"x y"}`}

	stdout, stderr, exit := runCodebaseMemoryMCPWrapper(t, codebaseMemoryMCPEnv(bin, logPath, home), args...)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", exit, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	invocations := readFakeCBMInvocations(t, logPath)
	if len(invocations) != 5 {
		t.Fatalf("invocations = %d, want 4 config sets and 1 CLI call", len(invocations))
	}
	if !slices.Equal(invocations[4].args, args) {
		t.Fatalf("CLI args = %v, want %v", invocations[4].args, args)
	}
}

func TestCodebaseMemoryMCPWrapperFailsClosed(t *testing.T) {
	t.Run("config failure", func(t *testing.T) {
		bin := fakeCodebaseMemoryMCPBinary(t)
		home := t.TempDir()
		logPath := filepath.Join(home, "calls.log")
		_, stderr, exit := runCodebaseMemoryMCPWrapper(t,
			codebaseMemoryMCPEnv(bin, logPath, home, "FAKE_CONFIG_FAIL=1"), "--ui=false")
		if exit != 9 {
			t.Fatalf("exit = %d, want the config failure status 9\nstderr:\n%s", exit, stderr)
		}
		invocations := readFakeCBMInvocations(t, logPath)
		if len(invocations) != 1 || invocations[0].args[0] != "config" {
			t.Fatalf("invocations = %v, want only the failed config call", invocations)
		}
	})

	for _, tc := range []struct {
		name string
		home string
	}{
		{name: "missing HOME"},
		{name: "relative HOME", home: "relative/home"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeCodebaseMemoryMCPBinary(t)
			logPath := filepath.Join(t.TempDir(), "calls.log")
			_, stderr, exit := runCodebaseMemoryMCPWrapper(t,
				codebaseMemoryMCPEnv(bin, logPath, tc.home), "--ui=false")
			if exit == 0 {
				t.Fatalf("exit = 0, want failure\nstderr:\n%s", stderr)
			}
			if invocations := readFakeCBMInvocations(t, logPath); len(invocations) != 0 {
				t.Fatalf("invocations = %v, want none before HOME validation", invocations)
			}
		})
	}

	t.Run("child exit status", func(t *testing.T) {
		bin := fakeCodebaseMemoryMCPBinary(t)
		home := t.TempDir()
		logPath := filepath.Join(home, "calls.log")
		stdout, stderr, exit := runCodebaseMemoryMCPWrapper(t,
			codebaseMemoryMCPEnv(bin, logPath, home, "FAKE_STDOUT=mcp-ok", "FAKE_EXIT=7"), "--ui=false")
		if exit != 7 {
			t.Fatalf("exit = %d, want the child status 7\nstderr:\n%s", exit, stderr)
		}
		if stdout != "mcp-ok\n" {
			t.Fatalf("stdout = %q, want only the MCP payload", stdout)
		}
	})
}

type fakeCBMInvocation struct {
	env  [3]string
	args []string
}

func fakeCodebaseMemoryMCPBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "codebase-memory-mcp"), fakeCodebaseMemoryMCP)
	return bin
}

func codebaseMemoryMCPEnv(bin, logPath, home string, extra ...string) []string {
	env := []string{
		"PATH=" + bin + ":/bin:/usr/bin",
		"FAKE_LOG=" + logPath,
		"CBM_CACHE_DIR=/inherited/cache",
		"CBM_RUNTIME_DIR=/inherited/run",
	}
	if home != "" {
		env = append(env, "HOME="+home)
	}
	return append(env, extra...)
}

func runCodebaseMemoryMCPWrapper(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	script, err := filepath.Abs("codebase-memory-mcp.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("wrapper script: %v", err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = filepath.Dir(script)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("run wrapper: %v\nstderr:\n%s", runErr, stderr.String())
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

func readFakeCBMInvocations(t *testing.T, path string) []fakeCBMInvocation {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var invocations []fakeCBMInvocation
	var env [3]string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "ENV":
			if len(fields) != 4 {
				t.Fatalf("bad log line: %q", line)
			}
			copy(env[:], fields[1:])
		case "CALL":
			invocations = append(invocations, fakeCBMInvocation{env: env, args: fields[1:]})
		default:
			t.Fatalf("bad log line: %q", line)
		}
	}
	return invocations
}

const fakeCodebaseMemoryMCP = `#!/bin/sh
set -u

{
  printf 'ENV\t%s\t%s\t%s\n' "$HOME" "$CBM_CACHE_DIR" "$CBM_RUNTIME_DIR"
  printf 'CALL'
  for arg in "$@"; do
    printf '\t%s' "$arg"
  done
  printf '\n'
} >>"$FAKE_LOG"

if [ "${1:-}" = "config" ]; then
  if [ "${FAKE_CONFIG_FAIL:-0}" = "1" ]; then
    printf 'fake: config failed\n' >&2
    exit 9
  fi
  printf 'config-noise %s=%s\n' "${3:-}" "${4:-}"
  exit 0
fi

if [ "${FAKE_STDOUT:-}" != "" ]; then
  printf '%s\n' "$FAKE_STDOUT"
fi
exit "${FAKE_EXIT:-0}"
`

// TestDockerfileBundlesVerifiedCodebaseMemoryMCP pins the bundled upstream
// release: exact version, static portable archives per TARGETARCH, SHA-256
// values from the v0.12.1 release assets, the isolating wrapper under both
// names, and no upstream installer run.
func TestDockerfileBundlesVerifiedCodebaseMemoryMCP(t *testing.T) {
	data, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{
		"ARG CODEBASE_MEMORY_MCP_VERSION=0.12.1",
		"ARG TARGETARCH",
		"ARG CODEBASE_MEMORY_MCP_SHA256_AMD64=8c3d0341064301e78ecc84cf7348d4e2c6562a0f57448243ea560aaa34de36c1",
		"ARG CODEBASE_MEMORY_MCP_SHA256_ARM64=ed0f1f548cd79b121c401c2e9f7841c189a001af26d3768368088c24f6a53533",
		"codebase-memory-mcp-linux-${TARGETARCH}-portable.tar.gz",
		"https://github.com/DeusData/codebase-memory-mcp/releases/download/v${CODEBASE_MEMORY_MCP_VERSION}/",
		"sha256sum -c -",
		"cmp /tmp/expected-LICENSE /out/LICENSE",
		"COPY --from=codebase-memory /out/codebase-memory-mcp /usr/local/bin/codebase-memory-mcp",
		"COPY scripts/codebase-memory-mcp.sh /usr/local/bin/tribunal-codebase-memory-mcp",
		"ln -s tribunal-codebase-memory-mcp /usr/local/bin/crc-codebase-memory-mcp",
		"/licenses/codebase-memory-mcp/",
		"USER 10001:10001",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Dockerfile does not contain %q", want)
		}
	}
	for _, forbidden := range []string{"install.sh", "codebase-memory-mcp install", "codebase-memory-mcp update"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Dockerfile runs the upstream installer (%q); it would write agent config and hooks", forbidden)
		}
	}
	if user := strings.LastIndex(body, "USER 10001:10001"); user < strings.LastIndex(body, "/usr/local/bin/tribunal-codebase-memory-mcp") {
		t.Error("the wrapper must be installed by root before the image drops to the non-root user")
	}
}

// TestBundledCodebaseMemoryMCPVersionMatchesLicenseRecord keeps the license
// record in third_party in step with the version the Dockerfile downloads.
func TestBundledCodebaseMemoryMCPVersionMatchesLicenseRecord(t *testing.T) {
	version, err := os.ReadFile("../third_party/codebase-memory-mcp/VERSION")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	want := "ARG CODEBASE_MEMORY_MCP_VERSION=" + strings.TrimSpace(string(version)) + "\n"
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("Dockerfile does not pin %q from third_party/codebase-memory-mcp/VERSION", strings.TrimSpace(want))
	}
	license, err := os.ReadFile("../third_party/codebase-memory-mcp/LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(license), "MIT License") || !strings.Contains(string(license), "Copyright (c) 2025 DeusData") {
		t.Fatal("third_party/codebase-memory-mcp/LICENSE is not the upstream MIT license")
	}
}
