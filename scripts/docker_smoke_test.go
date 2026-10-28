package scripts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	smokeSessionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm"
	smokePublicURL  = "http://127.0.0.1:8080"
	callerSecret    = "caller-secret-must-not-leak"
	runLabel        = "io.tribunal.smoke.run"
	projectLabel    = "com.docker.compose.project"
)

func TestDockerArtifactsUseTribunalIdentity(t *testing.T) {
	files := map[string][]string{
		"../Dockerfile": {
			"-o /out/tribunalctl ./cmd/tribunalctl",
			"COPY --from=build /out/tribunalctl /app/tribunalctl",
			"ln -s tribunalctl /app/reviewctl",
			"groupadd --gid 10001 tribunal",
			"useradd --uid 10001 --gid tribunal --create-home --home-dir /home/tribunal",
			"USER 10001:10001",
			"ENV HOME=/home/tribunal",
		},
		"../compose.yaml": {
			"name: crc-review",
			"image: tribunal:local",
			`TRIBUNAL_SESSION_KEY: "${TRIBUNAL_SESSION_KEY:-${CRC_SESSION_KEY:?`,
			`CRC_SESSION_KEY: "${CRC_SESSION_KEY:-}"`,
			`TRIBUNAL_PUBLIC_URL: "${TRIBUNAL_PUBLIC_URL:-${CRC_PUBLIC_URL:-`,
			`CRC_PUBLIC_URL: "${CRC_PUBLIC_URL:-}"`,
			`127.0.0.1:${TRIBUNAL_HTTP_PORT:-${CRC_HTTP_PORT:-8080}}:8080`,
			`CRC_HTTP_PORT: "${CRC_HTTP_PORT:-}"`,
		},
		"../config/server.docker.yaml": {`public_url: "${TRIBUNAL_PUBLIC_URL}"`},
	}
	for path, wants := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s does not contain %q", path, want)
			}
		}
	}
}

type fakeObject struct {
	ID, Name, Run, Project, Created, Volume string
	Tags                                    []string
}

type fakeState struct {
	Next       int
	Volumes    map[string]fakeObject
	Images     map[string]fakeObject
	Containers map[string]fakeObject
	Networks   map[string]fakeObject
	Operations []string
	Calls      []string
	CLI        []string
	MCP        bool
	EnvOK      bool
	Inject     bool
}

func newFakeState() fakeState {
	return fakeState{
		Next: 1, Volumes: map[string]fakeObject{}, Images: map[string]fakeObject{},
		Containers: map[string]fakeObject{}, Networks: map[string]fakeObject{},
	}
}

type smokeHarness struct {
	root, bin, tmp, state, script, testExe string
}

func newSmokeHarness(t *testing.T) smokeHarness {
	t.Helper()
	root := t.TempDir()
	h := smokeHarness{root: root, bin: filepath.Join(root, "bin"), tmp: filepath.Join(root, "tmp"), state: filepath.Join(root, "daemon.json")}
	for _, dir := range []string{h.bin, h.tmp} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	h.script, err = filepath.Abs("docker-smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	h.testExe, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(h.bin, "docker"), `#!/bin/sh
exec "$SMOKE_FAKE_TEST_EXE" -test.run '^TestDockerFakeCLI$' -- "$@"
`)
	writeExecutable(t, filepath.Join(h.bin, "rm"), fakeRM)
	h.save(t, newFakeState())
	return h
}

func (h smokeHarness) save(t *testing.T, state fakeState) {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.state, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h smokeHarness) load(t *testing.T) fakeState {
	t.Helper()
	data, err := os.ReadFile(h.state)
	if err != nil {
		t.Fatal(err)
	}
	var state fakeState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (h smokeHarness) command(t *testing.T, script string, extra ...string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = append([]string{
		"PATH=" + h.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMPDIR=" + h.tmp, "SMOKE_FAKE_TMP_ROOT=" + h.tmp,
		"SMOKE_FAKE_STATE=" + h.state, "SMOKE_FAKE_TEST_EXE=" + h.testExe,
		"SMOKE_FAKE_CHILD=1", "GORACE=atexit_sleep_ms=0",
		"TRIBUNAL_SESSION_KEY=" + callerSecret, "CRC_SESSION_KEY=" + callerSecret,
		"TRIBUNAL_PUBLIC_URL=https://caller.invalid", "CRC_PUBLIC_URL=https://caller.invalid",
		"TRIBUNAL_HTTP_PORT=12345", "CRC_HTTP_PORT=12345",
	}, extra...)
	return cmd, cancel
}

func (h smokeHarness) run(t *testing.T, extra ...string) (int, string, fakeState) {
	t.Helper()
	cmd, cancel := h.command(t, h.script, extra...)
	defer cancel()
	output, err := cmd.CombinedOutput()
	code := exitCode(t, err)
	if strings.Contains(string(output), callerSecret) {
		t.Fatal("caller secret appeared in smoke output")
	}
	return code, string(output), h.load(t)
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run script: %v", err)
	}
	return exitErr.ExitCode()
}

func TestDockerSmokeEarlyCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, mode, preflight         string
		wantExit                      int
		removeFails, keep, unreported bool
	}{
		{name: "log creation fails", mode: "log-fail", wantExit: 1},
		{name: "log creation and cleanup fail", mode: "log-fail", wantExit: 1, removeFails: true, keep: true},
		{name: "mktemp fails", mode: "fail", wantExit: 23},
		{name: "mktemp fails without calling cleanup", mode: "fail", wantExit: 23, removeFails: true},
		{name: "mktemp returns an empty path", mode: "empty", wantExit: 1, keep: true, unreported: true},
		{name: "mktemp reports a path then fails", mode: "fail-path", wantExit: 23},
		{name: "mktemp failure and cleanup fail", mode: "fail-path", wantExit: 23, removeFails: true, keep: true},
		{name: "INT during allocation", mode: "int", wantExit: 130},
		{name: "INT and cleanup fail", mode: "int", wantExit: 130, removeFails: true, keep: true},
		{name: "INT before allocation reports its path", mode: "int-silent", wantExit: 130, keep: true, unreported: true},
		{name: "TERM during allocation", mode: "term", wantExit: 143},
		{name: "TERM and cleanup fail", mode: "term", wantExit: 143, removeFails: true, keep: true},
		{name: "TERM before allocation reports its path", mode: "term-silent", wantExit: 143, keep: true, unreported: true},
		{name: "INT during preflight", mode: "success", preflight: "int", wantExit: 130},
		{name: "TERM during preflight", mode: "success", preflight: "term", wantExit: 143},
		{name: "preflight and cleanup fail", mode: "success", preflight: "fail", wantExit: 23, removeFails: true, keep: true},
		{name: "success", mode: "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSmokeHarness(t)
			writeExecutable(t, filepath.Join(h.bin, "mktemp"), fakeMktemp)
			unrelated := filepath.Join(h.tmp, "unrelated")
			if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			rmLog := filepath.Join(h.root, "rm.log")
			extra := []string{"SMOKE_FAKE_MKTEMP=" + tc.mode, "SMOKE_FAKE_RM_LOG=" + rmLog}
			if tc.removeFails {
				extra = append(extra, "SMOKE_FAKE_RM_FAIL=1")
			}
			if tc.preflight != "" {
				extra = append(extra, "SMOKE_FAKE_PREFLIGHT="+tc.preflight)
			}
			code, output, state := h.run(t, extra...)
			if code != tc.wantExit {
				t.Errorf("exit = %d, want %d\n%s\n%v", code, tc.wantExit, output, state.Operations)
			}
			if tc.preflight != "" && (len(state.Calls) != 1 || state.Calls[0] != "compose version") {
				t.Errorf("unexpected Docker preflight calls: %v", state.Calls)
			} else if tc.wantExit != 0 && tc.preflight == "" && len(state.Calls) != 0 {
				t.Errorf("Docker called during initialization failure: %v", state.Calls)
			}
			if tc.wantExit == 0 && len(state.Calls) == 0 {
				t.Error("successful smoke did not call the fake Docker CLI")
			}
			assertEmptyResources(t, state)
			tmpDir := filepath.Join(h.tmp, "tribunal-smoke.fake")
			_, err := os.Stat(tmpDir)
			if tc.keep != (err == nil) || (err != nil && !os.IsNotExist(err)) {
				t.Errorf("temporary directory retained = %v, want %v: %v", err == nil, tc.keep, err)
			}
			data, err := os.ReadFile(unrelated)
			if err != nil || string(data) != "keep" {
				t.Fatalf("unrelated file changed: %q, %v", data, err)
			}
			data, err = os.ReadFile(rmLog)
			if tc.mode == "fail" || tc.unreported {
				if !os.IsNotExist(err) {
					t.Fatalf("rm called without an allocated directory: %q, %v", data, err)
				}
			} else if err != nil || string(data) != "-rf -- "+tmpDir+"\n" {
				t.Errorf("unexpected rm target: %q, %v", data, err)
			}
			if tc.removeFails && tc.mode != "fail" && !strings.Contains(output, "warning: could not remove temporary directory "+tmpDir) {
				t.Fatalf("missing cleanup warning: %s", output)
			}
			if (tc.unreported || tc.mode == "fail") && !strings.Contains(output, "warning: temporary directory creation outcome is unknown") {
				t.Fatalf("missing warning about unreported directory: %s", output)
			}
		})
	}
}

func TestDockerSmokeCleanup(t *testing.T) {
	tests := []struct {
		name, setting, wantOutput, retained string
		wantExit                            int
	}{
		{name: "success"},
		{name: "build fails before image", setting: "SMOKE_FAKE_BUILD_FAIL=before", wantExit: 1},
		{name: "build fails after image", setting: "SMOKE_FAKE_BUILD_FAIL=after", wantExit: 1},
		{name: "build fails after untagged image", setting: "SMOKE_FAKE_BUILD_FAIL=untagged", wantExit: 1},
		{name: "build status survives cleanup failure", setting: "SMOKE_FAKE_BUILD_FAIL=after", wantExit: 23, retained: "image"},
		{name: "run creates resources then fails", setting: "SMOKE_FAKE_RUN_FAIL=1", wantExit: 1},
		{name: "exec status survives cleanup failure", setting: "SMOKE_FAKE_EXEC_FAIL=1", wantExit: 23, retained: "volume"},
		{name: "MCP CLI status survives cleanup failure", setting: "SMOKE_FAKE_MCP_EXEC_FAIL=1", wantExit: 23, retained: "volume"},
		{name: "MCP exposes a write tool", setting: "SMOKE_FAKE_MCP_WRITE_TOOL=1", wantExit: 1, wantOutput: "codebase-memory MCP exposes write tool delete_project"},
		{name: "MCP misses a default tool", setting: "SMOKE_FAKE_MCP_MISSING_TOOL=1", wantExit: 1, wantOutput: "codebase-memory MCP does not list default tool check_index_coverage"},
		{name: "container removal fails", setting: "SMOKE_FAKE_REMOVE_FAIL=container", wantExit: 1, retained: "container"},
		{name: "network removal fails", setting: "SMOKE_FAKE_REMOVE_FAIL=network", wantExit: 1, retained: "network"},
		{name: "volume removal fails", setting: "SMOKE_FAKE_REMOVE_FAIL=volume", wantExit: 1, retained: "volume"},
		{name: "image removal fails", setting: "SMOKE_FAKE_REMOVE_FAIL=image", wantExit: 1, retained: "image"},
		{name: "temporary directory removal fails", setting: "SMOKE_FAKE_RM_FAIL=1", wantExit: 1, wantOutput: "could not remove temporary directory"},
		{name: "TERM status survives cleanup failure", setting: "SMOKE_FAKE_TERM=1", wantExit: 143},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newSmokeHarness(t)
			extra := []string{}
			if tc.setting != "" {
				extra = append(extra, tc.setting)
			}
			if tc.name == "TERM status survives cleanup failure" {
				extra = append(extra, "SMOKE_FAKE_REMOVE_FAIL=volume")
			}
			if tc.name == "build status survives cleanup failure" {
				extra = append(extra, "SMOKE_FAKE_BUILD_STATUS=23", "SMOKE_FAKE_REMOVE_FAIL=image")
			}
			if strings.HasSuffix(tc.name, "exec status survives cleanup failure") || tc.name == "MCP CLI status survives cleanup failure" {
				extra = append(extra, "SMOKE_FAKE_REMOVE_FAIL=volume")
			}
			code, output, state := h.run(t, extra...)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d\n%s\n%v", code, tc.wantExit, output, state.Operations)
			}
			if tc.wantOutput != "" && !strings.Contains(output, tc.wantOutput) {
				t.Fatalf("missing %q\n%s", tc.wantOutput, output)
			}
			if !state.EnvOK {
				t.Fatal("Compose did not receive all fake environment values")
			}
			if tc.name == "success" {
				if !contains(state.CLI, "/app/tribunalctl") || !contains(state.CLI, "/app/reviewctl") || !state.MCP {
					t.Fatalf("CLI or MCP checks missing: %+v", state)
				}
				if countPrefix(state.Operations, "compose.up:") != 2 || countPrefix(state.Operations, "compose.run:") != 1 {
					t.Fatalf("run/recreate checks missing: %v", state.Operations)
				}
			}
			if countPrefix(state.Operations, "compose.down:") != 0 {
				t.Fatalf("unverified compose down: %v", state.Operations)
			}
			if tc.retained != "" && resourceCount(state, tc.retained) == 0 {
				t.Fatalf("failed %s removal did not retain resource", tc.retained)
			}
			if tc.retained == "" && (tc.wantExit == 0 || strings.HasPrefix(tc.name, "build fails") || tc.name == "run creates resources then fails" || strings.HasPrefix(tc.name, "MCP ")) {
				assertEmptyResources(t, state)
			}
			entries, err := os.ReadDir(h.tmp)
			if err != nil {
				t.Fatal(err)
			}
			if (tc.name == "temporary directory removal fails" && len(entries) != 1) || (tc.name != "temporary directory removal fails" && len(entries) != 0) {
				t.Fatalf("unexpected temporary directories: %v", entries)
			}
		})
	}
}

func TestDockerSmokeVolumeCreationFailure(t *testing.T) {
	for _, tc := range []struct {
		name, setting     string
		wantExit, volumes int
	}{
		{"before creation", "SMOKE_FAKE_VOLUME_CREATE_FAIL=before", 23, 0},
		{"after creation", "SMOKE_FAKE_VOLUME_CREATE_FAIL=after", 23, 1},
		{"after creation without a response", "SMOKE_FAKE_VOLUME_CREATE_FAIL=after-silent", 23, 1},
		{"interrupted creation without a response", "SMOKE_FAKE_VOLUME_CREATE_FAIL=term", 143, 1},
		{"initial inspection fails", "SMOKE_FAKE_INITIAL_INSPECT_FAIL=1", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSmokeHarness(t)
			code, output, state := h.run(t, tc.setting)
			if code != tc.wantExit || len(state.Volumes) != tc.volumes || countPrefix(state.Operations, "volume.rm:") != 0 || countPrefix(state.Operations, "compose.build:") != 0 {
				t.Fatalf("unexpected create failure handling: exit %d, %s, %+v", code, output, state)
			}
			if tc.volumes != 0 && !strings.Contains(output, "retained") {
				t.Fatalf("missing warning about unconfirmed volume: %s", output)
			}
		})
	}
}

func TestDockerSmokeIncompleteListingStillCleansVerifiedResources(t *testing.T) {
	for _, kind := range []string{"container", "network"} {
		for _, filter := range []string{"list-name", "list-label"} {
			t.Run(kind+"/"+filter, func(t *testing.T) {
				h := newSmokeHarness(t)
				code, output, state := h.run(t, "SMOKE_FAKE_INJECT="+kind+":"+filter)
				if code != 1 || !strings.Contains(output, "incomplete "+kind+" list") || countPrefix(state.Operations, kind+".rm:") == 0 {
					t.Fatalf("incomplete listing handling: exit %d, %s, %+v", code, output, state)
				}
				assertEmptyResources(t, state)
			})
		}
	}
	for _, filter := range []string{"list-reference", "list-label"} {
		t.Run("image/"+filter, func(t *testing.T) {
			h := newSmokeHarness(t)
			code, output, state := h.run(t, "SMOKE_FAKE_INJECT=image:"+filter)
			if code != 1 || !strings.Contains(output, "could not list image") || countPrefix(state.Operations, "image.rm:") != 1 {
				t.Fatalf("incomplete image listing handling: exit %d, %s, %+v", code, output, state)
			}
			assertEmptyResources(t, state)
		})
	}
}

func TestDockerSmokeMixedOwnership(t *testing.T) {
	h := newSmokeHarness(t)
	code, output, state := h.run(t, "SMOKE_FAKE_INJECT=container:one-label")
	if code != 1 || !strings.Contains(output, "unknown ownership") || len(state.Containers) != 1 || countPrefix(state.Operations, "container.rm:") != 1 {
		t.Fatalf("verified container was not cleaned independently: exit %d, %s, %+v", code, output, state)
	}
}

func TestDockerSmokeAmbiguousImageIsRetained(t *testing.T) {
	for _, problem := range []string{"alias", "list"} {
		t.Run(problem, func(t *testing.T) {
			h := newSmokeHarness(t)
			code, output, state := h.run(t, "SMOKE_FAKE_INJECT=image:"+problem)
			if code != 1 || !strings.Contains(output, "warning:") || len(state.Images) != 1 || countPrefix(state.Operations, "image.rm:") != 0 {
				t.Fatalf("ambiguous image was not retained: exit %d, %s, %+v", code, output, state)
			}
			if len(state.Containers)+len(state.Networks)+len(state.Volumes) != 0 {
				t.Fatalf("confirmed resources were not cleaned: %+v", state)
			}
		})
	}
}

func TestDockerSmokePreexistingResources(t *testing.T) {
	for _, kind := range []string{"volume", "container", "network", "image"} {
		t.Run(kind, func(t *testing.T) {
			h := newSmokeHarness(t)
			state := h.load(t)
			volume := fakeID(1)
			project := "tribunal-smoke-" + volume
			foreign := fakeObject{ID: fakeID(900), Run: "someone-else", Project: project, Created: "before"}
			switch kind {
			case "volume":
				foreign.ID, foreign.Name = volume, volume
				state.Volumes[volume] = foreign
			case "container":
				foreign.Name = project + "-review-1"
				state.Containers[foreign.ID] = foreign
			case "network":
				foreign.Name = project + "_default"
				state.Networks[foreign.ID] = foreign
			case "image":
				foreign.ID = "sha256:" + foreign.ID
				foreign.Name = "tribunal:smoke-" + project
				foreign.Tags = []string{foreign.Name}
				state.Images[foreign.Name] = foreign
			}
			h.save(t, state)
			code, output, state := h.run(t)
			if code == 0 || countPrefix(state.Operations, "compose.build:") != 0 || resourceCount(state, kind) != 1 {
				t.Fatalf("did not fail closed on existing %s: exit %d, %s, %+v", kind, code, output, state)
			}
			if kind == "volume" && (state.Volumes[volume].ID != foreign.ID || state.Volumes[volume].Run != foreign.Run) {
				t.Fatalf("existing volume changed: %+v", state.Volumes)
			}
			if countPrefix(state.Operations, kind+".rm:"+foreign.ID) != 0 {
				t.Fatalf("removed foreign %s: %v", kind, state.Operations)
			}
		})
	}
}

func TestDockerSmokeUncertainOwnershipIsRetained(t *testing.T) {
	for _, kind := range []string{"container", "network", "volume", "image"} {
		for _, problem := range []string{"inspect", "label", "replace"} {
			t.Run(kind+"/"+problem, func(t *testing.T) {
				h := newSmokeHarness(t)
				code, output, state := h.run(t, "SMOKE_FAKE_INJECT="+kind+":"+problem)
				if code == 0 || resourceCount(state, kind) == 0 {
					t.Fatalf("uncertain %s was not retained: exit %d, %s, %+v", kind, code, output, state)
				}
				if !strings.Contains(output, "warning:") {
					t.Fatalf("missing ownership warning: %s", output)
				}
				if countPrefix(state.Operations, kind+".rm:") != 0 {
					t.Fatalf("removed uncertain %s: %v", kind, state.Operations)
				}
				if kind == "network" && (resourceCount(state, "container") != 0 || resourceCount(state, "image") != 0 || resourceCount(state, "volume") != 0) {
					t.Fatalf("cleanup did not continue for confirmed resources: %+v", state)
				}
			})
		}
	}
}

// Pin $$ and date in a copied script to model clients in separate PID namespaces.
// The two clients share one stateful fake daemon and run concurrently.
func TestDockerSmokeProjectIdentityDoesNotDependOnClockAndPID(t *testing.T) {
	h := newSmokeHarness(t)
	repo := filepath.Join(h.root, "repo")
	for _, dir := range []string{"scripts", "config"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"compose.yaml", "config/server.docker.yaml"} {
		source, err := filepath.Abs(filepath.Join("..", path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(source, filepath.Join(repo, path)); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile(h.script)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repo, "scripts", "docker-smoke.sh")
	writeExecutable(t, script, strings.ReplaceAll(string(source), "$$", "4242"))
	writeExecutable(t, filepath.Join(h.bin, "date"), "#!/bin/sh\nprintf '1234567890\\n'\n")
	cmd1, cancel1 := h.command(t, script)
	defer cancel1()
	cmd2, cancel2 := h.command(t, script)
	defer cancel2()
	var out1, out2 bytes.Buffer
	cmd1.Stdout, cmd1.Stderr = &out1, &out1
	cmd2.Stdout, cmd2.Stderr = &out2, &out2
	if err := cmd1.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd2.Start(); err != nil {
		t.Fatal(err)
	}
	err1, err2 := cmd1.Wait(), cmd2.Wait()
	if strings.Contains(out1.String(), callerSecret) || strings.Contains(out2.String(), callerSecret) {
		t.Fatal("caller secret appeared in concurrent smoke output")
	}
	state := h.load(t)
	var projects []string
	for _, operation := range state.Operations {
		if strings.HasPrefix(operation, "compose.config:") {
			projects = append(projects, strings.TrimPrefix(operation, "compose.config:"))
		}
	}
	if err1 != nil || err2 != nil || len(projects) != 2 || projects[0] == projects[1] {
		t.Fatalf("clients did not use distinct daemon identities: projects %v, errors %v/%v\n%s\n%s", projects, err1, err2, out1.String(), out2.String())
	}
	for _, project := range projects {
		if !regexp.MustCompile(`^tribunal-smoke-[a-f0-9]{64}$`).MatchString(project) {
			t.Fatalf("project %q is not derived from the daemon volume", project)
		}
	}
	assertEmptyResources(t, state)
}

func resourceCount(s fakeState, kind string) int {
	switch kind {
	case "volume":
		return len(s.Volumes)
	case "image":
		return len(s.Images)
	case "container":
		return len(s.Containers)
	case "network":
		return len(s.Networks)
	}
	return -1
}

func assertEmptyResources(t *testing.T, s fakeState) {
	t.Helper()
	if len(s.Volumes)+len(s.Images)+len(s.Containers)+len(s.Networks) != 0 {
		t.Fatalf("resources left: %+v", s)
	}
}

func countPrefix(values []string, prefix string) int {
	n := 0
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			n++
		}
	}
	return n
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fakeID(n int) string { return fmt.Sprintf("%064x", n) }

// Each invocation is a separate fake Docker CLI process. A directory lock makes
// daemon state atomic across concurrent smoke scripts; no real Docker runs.
func TestDockerFakeCLI(t *testing.T) {
	if os.Getenv("SMOKE_FAKE_CHILD") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(90)
	}
	args = args[1:]
	path := os.Getenv("SMOKE_FAKE_STATE")
	lock := path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := os.Mkdir(lock, 0o700); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(91)
		}
		time.Sleep(time.Millisecond)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(92)
	}
	var state fakeState
	if json.Unmarshal(data, &state) != nil {
		os.Exit(93)
	}
	code := fakeDockerCommand(&state, args)
	data, err = json.Marshal(state)
	if err != nil || os.WriteFile(path, data, 0o600) != nil || os.Remove(lock) != nil {
		os.Exit(94)
	}
	os.Exit(code)
}

func fakeDockerCommand(s *fakeState, args []string) int {
	s.Calls = append(s.Calls, strings.Join(args, " "))
	if len(args) == 0 {
		return 95
	}
	if args[0] == "info" || (len(args) == 2 && args[0] == "compose" && args[1] == "version") {
		if args[0] == "compose" {
			switch os.Getenv("SMOKE_FAKE_PREFLIGHT") {
			case "int":
				_ = syscall.Kill(os.Getppid(), syscall.SIGINT)
			case "term":
				_ = syscall.Kill(os.Getppid(), syscall.SIGTERM)
			case "fail":
				return 23
			}
		}
		return 0
	}
	if args[0] == "compose" {
		return fakeCompose(s, args[1:])
	}
	if len(args) < 2 {
		return 95
	}
	kind, action := args[0], args[1]
	args = args[2:]
	if kind == "volume" && action == "create" {
		label := optionValue(args, "--label")
		if !strings.HasPrefix(label, runLabel+"=") || len(args) != 2 {
			return 96
		}
		if os.Getenv("SMOKE_FAKE_VOLUME_CREATE_FAIL") == "before" {
			return 23
		}
		id := fakeID(s.Next)
		s.Next++
		if _, exists := s.Volumes[id]; !exists {
			s.Volumes[id] = fakeObject{ID: id, Name: id, Run: strings.TrimPrefix(label, runLabel+"="), Created: "2026-01-01T00:00:00Z"}
		}
		s.Operations = append(s.Operations, "volume.create:"+id)
		mode := os.Getenv("SMOKE_FAKE_VOLUME_CREATE_FAIL")
		if mode != "after-silent" && mode != "term" {
			fmt.Println(id)
		}
		if mode == "term" {
			_ = syscall.Kill(os.Getppid(), syscall.SIGTERM)
		}
		if mode != "" {
			return 23
		}
		return 0
	}
	if action == "ls" {
		filter := optionValue(args, "--filter")
		injection := os.Getenv("SMOKE_FAKE_INJECT")
		if s.Inject && (injection == kind+":list" || (injection == kind+":list-name" && strings.HasPrefix(filter, "name=")) ||
			(injection == kind+":list-reference" && strings.HasPrefix(filter, "reference=")) ||
			(injection == kind+":list-label" && strings.HasPrefix(filter, "label="))) {
			return 1
		}
		var ids []string
		for _, object := range fakeObjects(s, kind) {
			if fakeMatches(object, filter) {
				ids = append(ids, object.ID)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Println(id)
		}
		return 0
	}
	if action == "inspect" {
		if len(args) == 0 {
			return 95
		}
		target := args[len(args)-1]
		if (s.Inject && os.Getenv("SMOKE_FAKE_INJECT") == kind+":inspect") || (kind == "volume" && os.Getenv("SMOKE_FAKE_INITIAL_INSPECT_FAIL") == "1") {
			return 1
		}
		object, ok := fakeLookup(s, kind, target)
		if !ok {
			return 1
		}
		return fakeInspect(kind, object, optionValue(args, "--format"))
	}
	if action == "rm" {
		if len(args) == 0 || (kind == "image" && !contains(args, "--no-prune")) {
			return 95
		}
		target := args[len(args)-1]
		s.Operations = append(s.Operations, kind+".rm:"+target)
		if os.Getenv("SMOKE_FAKE_REMOVE_FAIL") == kind {
			return 1
		}
		object, ok := fakeLookup(s, kind, target)
		if !ok || (kind != "volume" && target != object.ID) {
			return 1
		}
		if kind != "container" && len(s.Containers) > 0 {
			for _, container := range s.Containers {
				if container.Project == object.Project || (kind == "volume" && container.Volume == object.Name) {
					return 1
				}
			}
		}
		switch kind {
		case "container":
			delete(s.Containers, object.ID)
		case "network":
			delete(s.Networks, object.ID)
		case "volume":
			delete(s.Volumes, object.Name)
		case "image":
			delete(s.Images, object.Name)
		default:
			return 95
		}
		return 0
	}
	return 95
}

func fakeObjects(s *fakeState, kind string) []fakeObject {
	var objects []fakeObject
	var source map[string]fakeObject
	switch kind {
	case "container":
		source = s.Containers
	case "network":
		source = s.Networks
	case "image":
		source = s.Images
	case "volume":
		source = s.Volumes
	}
	for _, object := range source {
		objects = append(objects, object)
	}
	return objects
}

func fakeMatches(object fakeObject, filter string) bool {
	switch {
	case strings.HasPrefix(filter, "name="):
		return strings.Contains(object.Name, strings.TrimPrefix(filter, "name="))
	case strings.HasPrefix(filter, "label="+projectLabel+"="):
		return object.Project == strings.TrimPrefix(filter, "label="+projectLabel+"=")
	case strings.HasPrefix(filter, "label="+runLabel+"="):
		return object.Run == strings.TrimPrefix(filter, "label="+runLabel+"=")
	case strings.HasPrefix(filter, "reference="):
		return object.Name == strings.TrimPrefix(filter, "reference=")
	}
	return false
}

func fakeLookup(s *fakeState, kind, target string) (fakeObject, bool) {
	for _, object := range fakeObjects(s, kind) {
		if object.ID == target || object.Name == target {
			return object, true
		}
	}
	return fakeObject{}, false
}

func fakeInspect(kind string, o fakeObject, format string) int {
	labels := map[string]string{runLabel: o.Run, projectLabel: o.Project}
	data := struct {
		ID, Name, CreatedAt string
		Labels              map[string]string
		Config              struct{ Labels map[string]string }
		RepoTags            []string
	}{ID: o.ID, Name: o.Name, CreatedAt: o.Created, Labels: labels, RepoTags: o.Tags}
	data.Config.Labels = labels
	if kind == "container" {
		data.Name = "/" + data.Name
	}
	tmpl, err := template.New("inspect").Funcs(template.FuncMap{"json": func(v any) string {
		data, _ := json.Marshal(v)
		return string(data)
	}}).Parse(format)
	if err != nil || tmpl.Execute(os.Stdout, data) != nil {
		return 95
	}
	fmt.Println()
	return 0
}

type fakeOverride struct {
	Services map[string]struct {
		Image  string
		Labels map[string]string
		Build  struct{ Labels map[string]string }
	}
	Volumes map[string]struct {
		Name     string
		External bool
	}
	Networks map[string]struct{ Labels map[string]string }
}

func fakeCompose(s *fakeState, args []string) int {
	project := optionValue(args, "--project-name")
	envFile := optionValue(args, "--env-file")
	if env, err := os.ReadFile(envFile); err != nil || len(env) != 0 || optionValue(args, "--project-directory") == "" {
		return 96
	}
	var overridePath string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--file" {
			overridePath = args[i+1]
		}
	}
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		if len(args) < 2 {
			return 95
		}
		args = args[2:]
	}
	if len(args) == 0 || project == "" {
		return 95
	}
	action := args[0]
	args = args[1:]
	s.Operations = append(s.Operations, "compose."+action+":"+project)
	if action == "down" {
		return 1
	}
	data, err := os.ReadFile(overridePath)
	if err != nil {
		return 95
	}
	var override fakeOverride
	if yaml.Unmarshal(data, &override) != nil {
		return 95
	}
	service := override.Services["review"]
	volume := override.Volumes["data"]
	owned, exists := s.Volumes[volume.Name]
	if !exists || !volume.External || project != "tribunal-smoke-"+volume.Name || service.Image != "tribunal:smoke-"+project ||
		owned.Run == "" || service.Labels[runLabel] != owned.Run || service.Build.Labels[runLabel] != owned.Run || override.Networks["default"].Labels[runLabel] != owned.Run {
		return 96
	}
	if action == "config" {
		s.EnvOK = os.Getenv("TRIBUNAL_SESSION_KEY") == smokeSessionKey && os.Getenv("CRC_SESSION_KEY") == smokeSessionKey &&
			os.Getenv("TRIBUNAL_PUBLIC_URL") == smokePublicURL && os.Getenv("CRC_PUBLIC_URL") == smokePublicURL &&
			os.Getenv("TRIBUNAL_HTTP_PORT") == "0" && os.Getenv("CRC_HTTP_PORT") == "0" &&
			os.Getenv("GITLAB_URL") == "https://gitlab.invalid" && os.Getenv("LLM_BASE_URL") == "https://llm.invalid/v1" &&
			os.Getenv("LLM_API_KEY") == "smoke-not-a-real-key" && os.Getenv("GITLAB_OAUTH_CLIENT_SECRET") == ""
		return 0
	}
	number, err := strconv.ParseInt(volume.Name[len(volume.Name)-8:], 16, 32)
	if err != nil {
		return 95
	}
	switch action {
	case "build":
		if os.Getenv("SMOKE_FAKE_BUILD_FAIL") == "before" {
			return fakeBuildFailureCode()
		}
		id := "sha256:" + fakeID(3000+int(number))
		image := fakeObject{ID: id, Name: service.Image, Run: owned.Run, Project: project, Tags: []string{service.Image}}
		if os.Getenv("SMOKE_FAKE_BUILD_FAIL") == "untagged" {
			image.Name, image.Tags = "<none>:<none>", nil
		}
		s.Images[image.Name] = image
		if os.Getenv("SMOKE_FAKE_BUILD_FAIL") == "untagged" {
			return fakeBuildFailureCode()
		}
		if os.Getenv("SMOKE_FAKE_BUILD_FAIL") == "after" {
			return fakeBuildFailureCode()
		}
		return 0
	case "run", "up":
		if action == "run" && (contains(args, "--rm") || !contains(args, "--no-deps") || !contains(args, "--check-config")) {
			return 96
		}
		if action == "up" && (!contains(args, "--wait") || optionValue(args, "--wait-timeout") != "180") {
			return 96
		}
		networkID := fakeID(4000 + int(number))
		s.Networks[networkID] = fakeObject{ID: networkID, Name: project + "_default", Run: owned.Run, Project: project}
		containerID := fakeID(5000 + int(number))
		containerName := project + "-review-1"
		if action == "run" {
			containerID = fakeID(6000 + int(number))
			containerName = project + "-review-run-fake"
		} else if contains(args, "--force-recreate") {
			delete(s.Containers, containerID)
			containerID = fakeID(7000 + int(number))
		}
		s.Containers[containerID] = fakeObject{ID: containerID, Name: containerName, Run: owned.Run, Project: project, Volume: volume.Name}
		if action == "run" && os.Getenv("SMOKE_FAKE_RUN_FAIL") == "1" {
			return 1
		}
		if action == "up" && os.Getenv("SMOKE_FAKE_TERM") == "1" {
			_ = syscall.Kill(os.Getppid(), syscall.SIGTERM)
		}
		return 0
	case "exec":
		return fakeComposeExec(s, project, args)
	}
	return 95
}

func fakeBuildFailureCode() int {
	if status, err := strconv.Atoi(os.Getenv("SMOKE_FAKE_BUILD_STATUS")); err == nil && status > 0 && status < 256 {
		return status
	}
	return 1
}

func fakeComposeExec(s *fakeState, project string, args []string) int {
	if len(args) > 0 && args[0] == "-T" {
		args = args[1:]
	}
	if len(args) == 0 || args[0] != "review" {
		return 95
	}
	args = args[1:]
	if len(args) == 0 {
		return 95
	}
	switch args[0] {
	case "sh":
		if (os.Getenv("SMOKE_FAKE_EXEC_FAIL") == "1" && len(args) > 2) || (os.Getenv("SMOKE_FAKE_MCP_EXEC_FAIL") == "1" && len(args) == 2) {
			return 23
		}
		return 0
	case "cat":
		fakeInject(s, project)
		fmt.Print("tribunal-smoke-marker\n")
		return 0
	case "/app/tribunalctl", "/app/reviewctl":
		s.CLI = append(s.CLI, args[0])
		fmt.Print(`{"projects":[]}`)
		return 0
	case "curl":
		url := args[len(args)-1]
		if contains(args, "-w") {
			switch {
			case strings.HasSuffix(url, "/healthz"), strings.HasSuffix(url, "/readyz"):
				fmt.Print("200")
			case strings.HasSuffix(url, "/api/v1/projects"):
				fmt.Print("401")
			case strings.HasSuffix(url, "/auth/login"):
				fmt.Print("503")
			default:
				return 95
			}
		} else {
			switch {
			case strings.HasSuffix(url, "/"):
				fmt.Print(`<div id="root"></div>`)
			case strings.HasSuffix(url, "/api/v1/mcp-servers?scope=platform"):
				fmt.Print(`{"servers":[{"id":"srv-codebase-memory","name":"codebase-memory"}]}`)
			case strings.HasSuffix(url, "/api/v1/mcp-servers/srv-codebase-memory/test"):
				s.MCP = true
				fmt.Print(`{"tools":[{"name":"list_projects"},{"name":"index_status"},{"name":"search_graph"},{"name":"trace_path"},{"name":"get_code_snippet"},{"name":"query_graph"},{"name":"get_graph_schema"},{"name":"get_file_outline"},{"name":"get_architecture"},{"name":"search_code"}`)
				if os.Getenv("SMOKE_FAKE_MCP_MISSING_TOOL") != "1" {
					fmt.Print(`,{"name":"check_index_coverage"}`)
				}
				if os.Getenv("SMOKE_FAKE_MCP_WRITE_TOOL") == "1" {
					fmt.Print(`,{"name":"delete_project"}`)
				}
				fmt.Print(`]}`)
			default:
				return 95
			}
		}
		return 0
	}
	return 95
}

func fakeInject(s *fakeState, project string) {
	setting := os.Getenv("SMOKE_FAKE_INJECT")
	kind, problem, ok := strings.Cut(setting, ":")
	if !ok {
		return
	}
	s.Inject = true
	if problem != "label" && problem != "one-label" && problem != "replace" && problem != "alias" {
		return
	}
	var objects map[string]fakeObject
	switch kind {
	case "container":
		objects = s.Containers
	case "network":
		objects = s.Networks
	case "volume":
		objects = s.Volumes
	case "image":
		objects = s.Images
	}
	for key, object := range objects {
		if object.Project != project && kind != "volume" {
			continue
		}
		if kind == "volume" && "tribunal-smoke-"+object.Name != project {
			continue
		}
		if problem == "label" || problem == "one-label" {
			object.Run = "someone-else"
		} else if problem == "alias" {
			object.Tags = append(object.Tags, "someone-else:tag")
		} else if kind == "volume" {
			object.Created = "2026-01-01T01:00:00Z"
		} else {
			if kind != "image" {
				delete(objects, key)
				object.ID = fakeID(9999)
				key = object.ID
			} else {
				object.ID = "sha256:" + fakeID(9999)
			}
		}
		objects[key] = object
		if kind != "container" || problem == "one-label" {
			break
		}
	}
}

func optionValue(args []string, option string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == option {
			return args[i+1]
		}
	}
	return ""
}

const fakeMktemp = `#!/usr/bin/env bash
set -eu
mode=${SMOKE_FAKE_MKTEMP:-success}
[ "$mode" != fail ] || exit 23
dir="$SMOKE_FAKE_TMP_ROOT/tribunal-smoke.fake"
/bin/mkdir "$dir"
case "$mode" in
  int-silent) kill -INT "$PPID"; exit 23 ;;
  term-silent) kill -TERM "$PPID"; exit 23 ;;
esac
[ "$mode" != empty ] || exit 0
if [ "$mode" = log-fail ]; then
  /bin/mkdir "$dir/smoke.log"
fi
printf '%s\n' "$dir"
[ "$mode" != fail-path ] || exit 23
case "$mode" in
  int) kill -INT "$PPID" ;;
  term) kill -TERM "$PPID" ;;
esac
`

const fakeRM = `#!/usr/bin/env bash
set -u
if [ -n "${SMOKE_FAKE_RM_LOG:-}" ]; then
  printf '%s\n' "$*" >>"$SMOKE_FAKE_RM_LOG"
fi
path=""
for arg in "$@"; do
  case "$arg" in
    -*) ;;
    "$SMOKE_FAKE_TMP_ROOT"/*) path=$arg ;;
    *) exit 99 ;;
  esac
done
[ -n "$path" ] || exit 99
[ "${SMOKE_FAKE_RM_FAIL:-0}" != 1 ] || exit 1
exec /bin/rm "$@"
`
