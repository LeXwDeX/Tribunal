package scripts_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runtimeDirMaxBytes mirrors the wrapper's threshold: "$HOME/run" is kept up to
// this many bytes; a longer one moves to a short private directory under /tmp.
const runtimeDirMaxBytes = 48

// TestCodebaseMemoryMCPWrapperConfinesHome proves the wrapper replaces
// inherited shared cache/runtime values with per-HOME directories.
func TestCodebaseMemoryMCPWrapperConfinesHome(t *testing.T) {
	bin := fakeCodebaseMemoryMCPBinary(t)
	root := shortHome(t)
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
	home := shortHome(t)
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
		home := shortHome(t)
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
		home := shortHome(t)
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

// TestCodebaseMemoryMCPWrapperKeepsShortRuntimeDirUnderHome proves a HOME whose
// "$HOME/run" fits the byte limit keeps it as the runtime directory.
func TestCodebaseMemoryMCPWrapperKeepsShortRuntimeDirUnderHome(t *testing.T) {
	bin := fakeCodebaseMemoryMCPBinary(t)
	for _, tc := range []struct {
		name    string
		homeLen int
	}{
		{name: "short HOME", homeLen: 30},
		{name: "runtime dir at the byte limit", homeLen: runtimeDirMaxBytes - len("/run")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := homeOfLength(t, tc.homeLen, "h")
			env := wrapperChildEnv(t, bin, home)
			if want := home + "/run"; env[2] != want {
				t.Fatalf("runtime dir = %q, want %q", env[2], want)
			}
			assertPrivateDir(t, env[2])
		})
	}
}

// TestCodebaseMemoryMCPWrapperUsesShortRuntimeDirForLongHome proves a HOME whose
// "$HOME/run" is over the limit gets a short private directory under /tmp,
// keyed by uid and a digest of HOME, while the graph cache stays under HOME.
func TestCodebaseMemoryMCPWrapperUsesShortRuntimeDirForLongHome(t *testing.T) {
	bin := fakeCodebaseMemoryMCPBinary(t)
	parent := "/tmp/tcbm-" + strconv.Itoa(os.Geteuid())
	if _, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
		// The wrapper creates it; remove it again once the leaves are gone
		// (os.Remove fails on a directory that still holds anything).
		t.Cleanup(func() { _ = os.Remove(parent) })
	}
	// leafFor names the directory the wrapper must pick for home and removes
	// that one leaf afterwards, leaving any other leaf under parent alone.
	leafFor := func(t *testing.T, home string) string {
		t.Helper()
		leaf := filepath.Join(parent, sha256Prefix16(home))
		t.Cleanup(func() { _ = os.Remove(leaf) })
		return leaf
	}

	for _, tc := range []struct {
		name    string
		homeLen int
		unit    string
		extra   []string
	}{
		{name: "one byte over the limit", homeLen: runtimeDirMaxBytes - len("/run") + 1, unit: "h"},
		{name: "deep HOME", homeLen: 120, unit: "h"},
		// Few characters but many bytes: counting characters in a UTF-8 locale
		// would wrongly keep "$HOME/run".
		{name: "multibyte HOME", homeLen: 60, unit: "孙", extra: []string{"LC_ALL=en_US.UTF-8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := homeOfLength(t, tc.homeLen, tc.unit)
			leaf := leafFor(t, home)
			env := wrapperChildEnv(t, bin, home, tc.extra...)
			wantCache := filepath.Join(home, ".cache", "codebase-memory-mcp")
			if env[0] != home || env[1] != wantCache || env[2] != leaf {
				t.Fatalf("child env = %v, want HOME %q cache %q runtime %q", env, home, wantCache, leaf)
			}
			if len(leaf) > runtimeDirMaxBytes {
				t.Fatalf("runtime dir %q is %d bytes, want at most %d", leaf, len(leaf), runtimeDirMaxBytes)
			}
			assertPrivateDir(t, leaf)
			assertPrivateDir(t, wantCache)
			if _, err := os.Lstat(filepath.Join(home, "run")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s/run exists (err %v); only the runtime directory may leave HOME", home, err)
			}
		})
	}

	t.Run("stable per HOME and distinct between HOMEs", func(t *testing.T) {
		homeA := homeOfLength(t, 90, "h")
		homeB := homeOfLength(t, 90, "h")
		leafA, leafB := leafFor(t, homeA), leafFor(t, homeB)
		first := wrapperChildEnv(t, bin, homeA)[2]
		second := wrapperChildEnv(t, bin, homeA)[2]
		other := wrapperChildEnv(t, bin, homeB)[2]
		if first != leafA || second != leafA {
			t.Fatalf("runtime dirs for the same HOME = %q, %q, want both %q", first, second, leafA)
		}
		if other != leafB || other == first {
			t.Fatalf("runtime dir for another HOME = %q, want %q, different from %q", other, leafB, first)
		}
	})
}

// TestCodebaseMemoryMCPWrapperRuntimeParent covers the per-uid /tmp parent of
// the short runtime directory: it is created private, an own directory is
// reused, and anything else is refused. A fake id moves the parent to a uid no
// account has, so these tests never touch the real /tmp/tcbm-<uid>.
func TestCodebaseMemoryMCPWrapperRuntimeParent(t *testing.T) {
	t.Run("created private and reused", func(t *testing.T) {
		bin := fakeCodebaseMemoryMCPBinary(t)
		parent := fakeUIDParent(t, bin)
		home := homeOfLength(t, 90, "h")
		want := filepath.Join(parent, sha256Prefix16(home))
		for _, run := range []string{"first run creates the parent", "second run reuses it"} {
			if got := wrapperChildEnv(t, bin, home)[2]; got != want {
				t.Fatalf("%s: runtime dir = %q, want %q", run, got, want)
			}
			assertPrivateDir(t, parent)
			assertPrivateDir(t, want)
		}
	})

	t.Run("prunes stale leaves of other HOMEs only", func(t *testing.T) {
		bin := fakeCodebaseMemoryMCPBinary(t)
		parent := fakeUIDParent(t, bin)
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		home := homeOfLength(t, 90, "h")
		own := filepath.Join(parent, sha256Prefix16(home))
		stale := filepath.Join(parent, "0123456789abcdef")
		fresh := filepath.Join(parent, "fedcba9876543210")
		other := filepath.Join(parent, "not-a-runtime-leaf")
		old := time.Now().Add(-72 * time.Hour)
		for _, dir := range []string{own, stale, fresh, other} {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if dir != fresh {
				if err := os.Chtimes(dir, old, old); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := wrapperChildEnv(t, bin, home)[2]; got != own {
			t.Fatalf("runtime dir = %q, want %q", got, own)
		}
		if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale leaf %s still exists (err %v), want it pruned", stale, err)
		}
		for _, keep := range []string{own, fresh, other} {
			if _, err := os.Lstat(keep); err != nil {
				t.Fatalf("%s was removed (err %v), want it kept", keep, err)
			}
		}
	})

	t.Run("rejects a uid that is not a number", func(t *testing.T) {
		bin := fakeCodebaseMemoryMCPBinary(t)
		writeExecutable(t, filepath.Join(bin, "id"), "#!/bin/sh\necho ../x\n")
		home := homeOfLength(t, 90, "h")
		logPath := filepath.Join(t.TempDir(), "calls.log")

		stdout, stderr, exit := runCodebaseMemoryMCPWrapper(t,
			codebaseMemoryMCPEnv(bin, logPath, home), "--ui=false")
		if exit == 0 {
			t.Fatalf("exit = 0, want failure\nstderr:\n%s", stderr)
		}
		if stdout != "" || !strings.Contains(stderr, "unexpected output from id -u") {
			t.Fatalf("stdout = %q, stderr = %q, want empty stdout and the uid refusal", stdout, stderr)
		}
		if invocations := readFakeCBMInvocations(t, logPath); len(invocations) != 0 {
			t.Fatalf("invocations = %v, want none", invocations)
		}
	})

	for _, tc := range []struct {
		name string
		// plant creates the unusable parent. It returns a directory that must
		// stay empty because the parent leads to it, or "".
		plant func(t *testing.T, parent string) string
	}{
		{name: "symlink to a directory of the same user", plant: func(t *testing.T, parent string) string {
			target := t.TempDir()
			if err := os.Symlink(target, parent); err != nil {
				t.Fatal(err)
			}
			return target
		}},
		{name: "dangling symlink", plant: func(t *testing.T, parent string) string {
			if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), parent); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{name: "regular file", plant: func(t *testing.T, parent string) string {
			if err := os.WriteFile(parent, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			bin := fakeCodebaseMemoryMCPBinary(t)
			parent := fakeUIDParent(t, bin)
			untouched := tc.plant(t, parent)
			home := homeOfLength(t, 90, "h")
			logPath := filepath.Join(t.TempDir(), "calls.log")

			stdout, stderr, exit := runCodebaseMemoryMCPWrapper(t,
				codebaseMemoryMCPEnv(bin, logPath, home), "--ui=false")
			if exit == 0 {
				t.Fatalf("exit = 0, want failure\nstderr:\n%s", stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if want := "wrapper: " + parent + " must be a real directory"; !strings.Contains(stderr, want) {
				t.Fatalf("stderr = %q, want the wrapper's own refusal %q", stderr, want)
			}
			if invocations := readFakeCBMInvocations(t, logPath); len(invocations) != 0 {
				t.Fatalf("invocations = %v, want none before the runtime directory is trusted", invocations)
			}
			if untouched != "" {
				if entries, err := os.ReadDir(untouched); err != nil || len(entries) != 0 {
					t.Fatalf("%s holds %v (err %v), want it left empty", untouched, entries, err)
				}
			}
		})
	}
}

// TestCodebaseMemoryMCPWrapperRuntimeDirHashTool proves the leaf name comes
// from sha256sum when installed and from "shasum -a 256" otherwise, and that a
// missing or unusable hash tool fails closed. PATH holds only fakes plus a few
// system tools, so the result does not depend on what the host ships.
func TestCodebaseMemoryMCPWrapperRuntimeDirHashTool(t *testing.T) {
	digestA := "0123456789abcdef" + strings.Repeat("0", 48)
	digestB := "fedcba9876543210" + strings.Repeat("f", 48)
	for _, tc := range []struct {
		name      string
		sha256sum string // digest the fake sha256sum prints; "" = not installed
		shasum    string // digest the fake "shasum -a 256" prints; "" = not installed
		wantLeaf  string // "" = the wrapper must fail closed
		wantErr   string
	}{
		{name: "prefers sha256sum", sha256sum: digestA, shasum: digestB, wantLeaf: digestA[:16]},
		{name: "falls back to shasum -a 256", shasum: digestB, wantLeaf: digestB[:16]},
		{name: "no hash tool", wantErr: "sha256sum or shasum"},
		{name: "short digest", sha256sum: "abcdef", wantErr: "hash HOME"},
		{name: "non-hex digest", sha256sum: strings.Repeat("g", 64), wantErr: "hash HOME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeCodebaseMemoryMCPBinary(t)
			parent := fakeUIDParent(t, bin)
			if tc.sha256sum != "" {
				writeExecutable(t, filepath.Join(bin, "sha256sum"), fakeHashTool("", tc.sha256sum))
			}
			if tc.shasum != "" {
				writeExecutable(t, filepath.Join(bin, "shasum"), fakeHashTool("-a 256", tc.shasum))
			}
			path := "PATH=" + bin + ":" + realToolsBin(t, "cat", "cut", "mkdir")
			home := homeOfLength(t, 90, "h")

			if tc.wantLeaf != "" {
				want := filepath.Join(parent, tc.wantLeaf)
				if got := wrapperChildEnv(t, bin, home, path)[2]; got != want {
					t.Fatalf("runtime dir = %q, want %q", got, want)
				}
				return
			}
			logPath := filepath.Join(t.TempDir(), "calls.log")
			stdout, stderr, exit := runCodebaseMemoryMCPWrapper(t,
				codebaseMemoryMCPEnv(bin, logPath, home, path), "--ui=false")
			if exit == 0 {
				t.Fatalf("exit = 0, want failure\nstderr:\n%s", stderr)
			}
			if stdout != "" || !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("stdout = %q, stderr = %q, want empty stdout and an error containing %q", stdout, stderr, tc.wantErr)
			}
			if invocations := readFakeCBMInvocations(t, logPath); len(invocations) != 0 {
				t.Fatalf("invocations = %v, want none", invocations)
			}
			if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s exists (err %v), want nothing created after the failure", parent, err)
			}
		})
	}
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

// shortHome returns an existing private directory directly under /tmp to use as
// HOME. Its "$HOME/run" is far below the wrapper's byte limit, unlike
// t.TempDir whose length varies by platform and test name, so the wrapper
// creates nothing outside it.
func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "cbmh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

// homeOfLength returns a HOME of exactly n bytes below a fresh shortHome. Its
// last element is built from unit, so a multibyte unit gives a HOME with fewer
// characters than bytes. The wrapper creates the path.
func homeOfLength(t *testing.T, n int, unit string) string {
	t.Helper()
	base := shortHome(t)
	fill := n - len(base) - 1
	if fill < 1 {
		t.Fatalf("cannot build a %d-byte HOME below %q", n, base)
	}
	return base + "/" + strings.Repeat(unit, fill/len(unit)) + strings.Repeat("h", fill%len(unit))
}

// wrapperChildEnv runs the wrapper once for home and returns the HOME, cache and
// runtime directories its child saw, which every child call must agree on.
func wrapperChildEnv(t *testing.T, bin, home string, extra ...string) [3]string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "calls.log")
	env := codebaseMemoryMCPEnv(bin, logPath, home, append([]string{"FAKE_STDOUT=mcp-ok"}, extra...)...)
	stdout, stderr, exit := runCodebaseMemoryMCPWrapper(t, env, "--ui=false")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 for HOME %q\nstderr:\n%s", exit, home, stderr)
	}
	if stdout != "mcp-ok\n" {
		t.Fatalf("stdout = %q, want only the MCP payload", stdout)
	}
	invocations := readFakeCBMInvocations(t, logPath)
	if len(invocations) != 5 {
		t.Fatalf("invocations = %d, want 4 config sets and 1 MCP call", len(invocations))
	}
	for _, invocation := range invocations {
		if invocation.env != invocations[0].env {
			t.Fatalf("child env = %v, want %v on every call", invocation.env, invocations[0].env)
		}
	}
	return invocations[0].env
}

func assertPrivateDir(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("private directory %s: %v", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s mode = %v, want private directory 0700", dir, info.Mode())
	}
}

// sha256Prefix16 is the first 16 hex characters of the SHA-256 of s, the leaf
// name the wrapper derives from HOME.
func sha256Prefix16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// fakeUIDParent installs a fake id in bin that reports a uid no real account is
// likely to have, so the wrapper's per-uid parent is a /tmp path this test
// controls. It returns that parent, which does not exist yet and is removed on
// cleanup (a symlink there is removed, never followed).
func fakeUIDParent(t *testing.T, bin string) string {
	t.Helper()
	for range 100 {
		uid := strconv.Itoa(900_000_000 + rand.IntN(99_000_000))
		parent := "/tmp/tcbm-" + uid
		if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		writeExecutable(t, filepath.Join(bin, "id"), "#!/bin/sh\necho "+uid+"\n")
		t.Cleanup(func() { _ = os.RemoveAll(parent) })
		return parent
	}
	t.Fatal("no unused fake uid under /tmp")
	return ""
}

// realToolsBin returns a directory of symlinks to the named system tools; a PATH
// of fakes plus this directory hides every other tool on the host.
func realToolsBin(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("system tool %s: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeHashTool stands in for sha256sum (args == "") or "shasum -a 256" (args ==
// "-a 256"): it drains stdin and prints digest in the real tool's format.
func fakeHashTool(args, digest string) string {
	guard := ""
	if args != "" {
		guard = `[ "$*" = "` + args + `" ] || exit 3` + "\n"
	}
	return "#!/bin/sh\n" + guard + "cat >/dev/null\nprintf '%s  -\\n' '" + digest + "'\n"
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
