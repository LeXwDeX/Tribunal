package scripts_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	registrationToken  = "synthetic-registration-token-never-print"
	registrationSecret = "synthetic-webhook-secret-never-print"
)

// registrationBlock reads the commands that an operator actually copies.
func registrationBlock(t *testing.T, path, section string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(data), section, 2)
	if len(parts) != 2 {
		t.Fatalf("section not found in %s: %s", path, section)
	}
	parts = strings.SplitN(parts[1], "```bash\n", 2)
	if len(parts) != 2 {
		t.Fatalf("bash block not found after %s", section)
	}
	var lines []string
	for _, line := range strings.Split(parts[1], "\n") {
		if strings.TrimSpace(line) == "```" {
			return strings.Join(lines, "\n")
		}
		lines = append(lines, line)
	}
	t.Fatalf("bash block not closed after %s", section)
	return ""
}

type registrationHarness struct {
	root, bin, dir, calls, result string
	env                           []string
}

func newRegistrationHarness(t *testing.T) registrationHarness {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	h := registrationHarness{root: root, bin: filepath.Join(root, "bin"), dir: filepath.Join(root, "private"), calls: filepath.Join(root, "calls"), result: filepath.Join(root, "result")}
	for _, dir := range []string{h.bin, h.dir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required to test the documented registration blocks")
	}
	mktemp, err := exec.LookPath("mktemp")
	if err != nil {
		t.Fatal("mktemp is required to test the documented registration blocks")
	}
	h.env = []string{
		"PATH=" + h.bin,
		"HOME=" + root,
		"GITLAB_PROJECT_TOKEN=" + registrationToken,
		"GITLAB_PROJECT=test/project",
		"REGISTRATION_DIR=" + h.dir,
		"TEST_CALLS=" + h.calls,
		"TEST_RESULT=" + h.result,
		"TEST_TOKEN=" + registrationToken,
		"TEST_RESPONSE=" + fmt.Sprintf(`{"project":{"id":17},"webhook_secret":%q}`, registrationSecret),
		"REAL_MKTEMP=" + mktemp,
		"REAL_PYTHON=" + python,
	}
	h.executable(t, "mktemp", `#!/bin/sh
if [ "${GITLAB_PROJECT_TOKEN+x}" = x ]; then printf 'mktemp exported\n' >> "$TEST_CALLS"; fi
case "${TEST_MKTEMP_FAIL:-}" in
  yes) exit 1 ;;
  after) "$REAL_MKTEMP" "$@" || exit 1; exit 1 ;;
esac
exec "$REAL_MKTEMP" "$@"
`)
	h.executable(t, "python3", `#!/bin/sh
if [ "${GITLAB_PROJECT_TOKEN+x}" = x ]; then printf 'python exported\n' >> "$TEST_CALLS"; fi
printf 'python\n' >> "$TEST_CALLS"
exec "$REAL_PYTHON" "$@"
`)
	cli := `#!/bin/sh
printf 'cli\n' >> "$TEST_CALLS"
[ "${GITLAB_PROJECT_TOKEN:-}" = "$TEST_TOKEN" ] || exit 8
case "${0##*/}" in
  go)
    [ "$#" -ge 6 ] && [ "$1" = run ] && [ "$2" = ./cmd/tribunalctl ] &&
    [ "$3" = --url ] && [ "$4" = http://localhost:8080 ] &&
    [ "$5" = projects ] && [ "$6" = register ] || exit 8
    shift 6 ;;
  docker)
    [ "$#" -ge 9 ] && [ "$1" = compose ] && [ "$2" = exec ] &&
    [ "$3" = -T ] && [ "$4" = -e ] && [ "$5" = GITLAB_PROJECT_TOKEN ] &&
    [ "$6" = review ] && [ "$7" = /app/tribunalctl ] &&
    [ "$8" = projects ] && [ "$9" = register ] || exit 8
    shift 9 ;;
  *) exit 8 ;;
esac
[ "$#" -ge 6 ] && [ "$1" = --project ] && [ "$2" = "$GITLAB_PROJECT" ] &&
[ "$3" = --token-env ] && [ "$4" = GITLAB_PROJECT_TOKEN ] &&
[ "$5" = --profile ] && [ "$6" = default ] || exit 8
shift 6
if [ "${1:-}" = --auto-webhook ]; then printf 'auto\n' >> "$TEST_CALLS"; shift; fi
[ "$#" = 0 ] || exit 8
printf '%s' "$TEST_RESPONSE"
[ "${TEST_CLI_FAIL:-}" != yes ] || exit 9
`
	h.executable(t, "go", cli)
	h.executable(t, "docker", cli)
	h.executable(t, "rm", `#!/bin/sh
printf 'rm\n' >> "$TEST_CALLS"
[ "$#" = 2 ] && [ "$1" = -- ] && [ "$2" = "$TEST_ALLOWED_PATH" ] || exit 8
[ "${TEST_RM_FAIL:-}" != yes ] || exit 9
case "${TEST_RM_FAIL:-}" in
  noop) exit 0 ;;
  move) exec /bin/mv -- "$2" "$TEST_PRIVATE/saved-original" ;;
  symlink) /bin/rm -- "$2" && /bin/ln -s missing-target "$2"; exit $? ;;
  dir-mode) /bin/rm -- "$2" && /bin/chmod 0755 "$TEST_PRIVATE"; exit $? ;;
  dir-replace) /bin/rm -- "$2" && /bin/mv "$TEST_PRIVATE" "$TEST_PRIVATE.old" && /bin/mkdir -m 700 "$TEST_PRIVATE"; exit $? ;;
esac
exec /bin/rm -- "$2"
`)
	return h
}

func (h registrationHarness) executable(t *testing.T, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.bin, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

type registrationResult struct {
	status, tokenState, fileState, file, stdout, stderr, calls string
}

func (h registrationHarness) run(t *testing.T, block, input string, extra ...string) registrationResult {
	t.Helper()
	script := block + `
registration_status=$?
if [ "${GITLAB_PROJECT_TOKEN+x}" = x ]; then token_state=set; else token_state=unset; fi
if [ "${REGISTRATION_FILE+x}" = x ]; then file_state=set; else file_state=unset; fi
printf '%s\n%s\n%s\n%s' "$registration_status" "$token_state" "$file_state" "${REGISTRATION_FILE:-}" > "$TEST_RESULT"
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", script)
	cmd.Env = append(h.env, extra...)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run documented block: %v", err)
	}
	if strings.Contains(stdout.String()+stderr.String(), registrationToken) || strings.Contains(stdout.String()+stderr.String(), registrationSecret) {
		t.Fatal("synthetic secret appeared in command output")
	}
	data, err := os.ReadFile(h.result)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.SplitN(string(data), "\n", 4)
	if len(fields) != 4 {
		t.Fatal("missing shell result fields")
	}
	calls, err := os.ReadFile(h.calls)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return registrationResult{fields[0], fields[1], fields[2], fields[3], stdout.String(), stderr.String(), string(calls)}
}

func TestRegistrationDocsClearToken(t *testing.T) {
	docs := []struct{ name, path, section string }{
		{"readme", "../README.md", "## 注册项目"},
		{"operations", "../docs/operations.md", "### 浏览器登录与项目注册"},
	}
	cases := []struct {
		name, input, response string
		extra                 []string
		prior, noPython, auto bool
		wantOK, wantFile      bool
		wantCLI, wantPython   int
	}{
		{name: "success", input: "yes\n", wantOK: true, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "automatic webhook", input: "yes\n", auto: true, wantOK: true, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "maximum id", input: "yes\n", response: `{"project":{"id":9223372036854775807}}`, wantOK: true, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "denied", input: "no\n"},
		{name: "confirmation eof"},
		{name: "missing project", extra: []string{"GITLAB_PROJECT="}},
		{name: "missing token", extra: []string{"GITLAB_PROJECT_TOKEN="}},
		{name: "missing directory", extra: []string{"REGISTRATION_DIR="}},
		{name: "missing python", noPython: true},
		{name: "prior response", prior: true, wantFile: true},
		{name: "mktemp failure", input: "yes\n", extra: []string{"TEST_MKTEMP_FAIL=yes"}, wantPython: 1},
		{name: "mktemp failure after creation", input: "yes\n", extra: []string{"TEST_MKTEMP_FAIL=after"}, wantFile: true, wantPython: 1},
		{name: "registration failure", input: "yes\n", extra: []string{"TEST_CLI_FAIL=yes"}, wantFile: true, wantCLI: 1, wantPython: 2},
		{name: "zero id", input: "yes\n", response: `{"project":{"id":0}}`, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "negative id", input: "yes\n", response: `{"project":{"id":-1}}`, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "string id", input: "yes\n", response: `{"project":{"id":"1"}}`, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "fractional id", input: "yes\n", response: `{"project":{"id":1.5}}`, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "boolean id", input: "yes\n", response: `{"project":{"id":true}}`, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "overflow id", input: "yes\n", response: `{"project":{"id":9223372036854775808}}`, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "missing id", input: "yes\n", response: `{"project":{}}`, wantFile: true, wantCLI: 1, wantPython: 3},
		{name: "invalid json", input: "yes\n", response: `{`, wantFile: true, wantCLI: 1, wantPython: 3},
	}
	for _, doc := range docs {
		t.Run(doc.name, func(t *testing.T) {
			block := registrationBlock(t, doc.path, doc.section)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					h := newRegistrationHarness(t)
					extra := append([]string(nil), tc.extra...)
					if tc.response != "" {
						extra = append(extra, "TEST_RESPONSE="+tc.response)
					}
					if tc.prior {
						prior := filepath.Join(h.dir, "prior")
						if err := os.WriteFile(prior, []byte("keep"), 0o600); err != nil {
							t.Fatal(err)
						}
						extra = append(extra, "REGISTRATION_FILE="+prior)
					}
					if tc.noPython {
						if err := os.Remove(filepath.Join(h.bin, "python3")); err != nil {
							t.Fatal(err)
						}
					}
					testBlock := block
					if tc.auto {
						if !strings.Contains(block, "--profile default >") {
							t.Fatal("documented insertion point for --auto-webhook is missing")
						}
						testBlock = strings.Replace(block, "--profile default >", "--profile default --auto-webhook >", 1)
					}
					r := h.run(t, testBlock, tc.input, extra...)
					if r.tokenState != "unset" {
						t.Error("project token remains in the current shell")
					}
					if (r.status == "0") != tc.wantOK {
						t.Errorf("status = %s, success wanted = %v", r.status, tc.wantOK)
					}
					if strings.Count(r.calls, "cli\n") != tc.wantCLI || strings.Count(r.calls, "python\n") != tc.wantPython {
						t.Errorf("unexpected fake command calls: %q", r.calls)
					}
					if tc.auto != strings.Contains(r.calls, "auto\n") {
						t.Error("automatic webhook flag did not match the selected mode")
					}
					if tc.name == "success" && (!strings.Contains(r.calls, "mktemp exported\n") || !strings.Contains(r.calls, "python exported\n")) {
						t.Error("expected exported token to be inherited by the documented subprocesses")
					}
					if tc.wantOK {
						want := "17\n"
						if tc.name == "maximum id" {
							want = "9223372036854775807\n"
						}
						if r.stdout != want {
							t.Error("stdout is not the expected validated project ID")
						}
					} else if r.stdout != "" {
						t.Error("failure emitted unexpected stdout")
					}
					if tc.wantFile {
						if r.fileState != "set" || filepath.Dir(r.file) != h.dir {
							t.Fatal("expected response path was not preserved in the private directory")
						}
						info, err := os.Stat(r.file)
						if err != nil || info.Mode().Perm() != 0o600 {
							t.Fatal("response file is missing or is not mode 0600")
						}
						if tc.prior {
							data, err := os.ReadFile(r.file)
							if err != nil || string(data) != "keep" {
								t.Error("prior response was modified")
							}
						} else if tc.wantCLI == 1 {
							want := tc.response
							if want == "" {
								want = fmt.Sprintf(`{"project":{"id":17},"webhook_secret":%q}`, registrationSecret)
							}
							data, err := os.ReadFile(r.file)
							if err != nil || string(data) != want {
								t.Error("private file did not preserve the fake CLI response")
							}
						}
					} else if r.file != "" {
						t.Error("unexpected response path")
					}
				})
			}
		})
	}
}

// Start cleanup tests from a response created by the actual registration block.
func registrationForCleanup(t *testing.T, fileState string) string {
	t.Helper()
	return `if {
` + registrationBlock(t, "../docs/operations.md", "### 浏览器登录与项目注册") + `
} >/dev/null; then :; else exit 8; fi
case "$REGISTRATION_FILE" in "$TEST_PRIVATE"/tribunal-registration.*) ;; *) exit 8 ;; esac
[[ "${REGISTRATION_FILE##*/}" =~ ^tribunal-registration\.[A-Za-z0-9]{8}$ ]] || exit 8
printf '%s' "$REGISTRATION_FILE" > "$TEST_CREATED"
export TEST_ALLOWED_PATH="$REGISTRATION_FILE"
GITLAB_PROJECT_TOKEN="$TEST_TOKEN"
` + map[string]string{
		"existing": "",
		"missing":  `/bin/rm -- "$REGISTRATION_FILE"`,
		"empty":    `REGISTRATION_FILE=`,
		"unset":    `unset REGISTRATION_FILE`,
	}[fileState] + "\n"
}

func TestRegistrationDocsCleanup(t *testing.T) {
	block := registrationBlock(t, "../docs/operations.md", "完成手动配置或自动注册后")
	for _, tc := range []struct {
		name                             string
		fileState, rmFail, wantFileState string
		wantOK, wantExists               bool
		wantCalls                        int
	}{
		{name: "deleted", fileState: "existing", wantFileState: "unset", wantOK: true, wantCalls: 1},
		{name: "denied", fileState: "existing", rmFail: "yes", wantFileState: "set", wantExists: true, wantCalls: 1},
		{name: "missing file", fileState: "missing", wantFileState: "set"},
		{name: "unset path", fileState: "unset", wantFileState: "unset", wantExists: true},
		{name: "empty path", fileState: "empty", wantFileState: "set", wantExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			other := filepath.Join(h.dir, "other-file")
			if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			created := filepath.Join(h.root, "created")
			extra := []string{"TEST_PRIVATE=" + h.dir, "TEST_CREATED=" + created, "TEST_RM_FAIL=" + tc.rmFail}
			r := h.run(t, registrationForCleanup(t, tc.fileState)+block, "yes\n", extra...)
			data, err := os.ReadFile(created)
			if err != nil {
				t.Fatal(err)
			}
			path := string(data)
			if r.tokenState != "unset" {
				t.Error("project token remains in the current shell")
			}
			if (r.status == "0") != tc.wantOK || r.fileState != tc.wantFileState {
				t.Errorf("status = %s, file state = %s", r.status, r.fileState)
			}
			if tc.wantFileState == "set" && tc.fileState != "empty" && r.file != path {
				t.Error("failed cleanup did not preserve the response path")
			}
			if strings.Count(r.calls, "rm\n") != tc.wantCalls {
				t.Errorf("unexpected fake rm calls: %q", r.calls)
			}
			_, err = os.Stat(path)
			if tc.wantExists != (err == nil) {
				t.Error("unexpected response file existence")
			}
			data, err = os.ReadFile(other)
			if err != nil || string(data) != "keep" {
				t.Error("another file was changed")
			}
		})
	}
}

func TestRegistrationDocsClearTokenWithErrexit(t *testing.T) {
	for _, tc := range []struct {
		name, path, section, input string
		extra                      []string
	}{
		{"readme denial", "../README.md", "## 注册项目", "no\n", nil},
		{"operations denial", "../docs/operations.md", "### 浏览器登录与项目注册", "no\n", nil},
		{"readme CLI failure", "../README.md", "## 注册项目", "yes\n", []string{"TEST_CLI_FAIL=yes"}},
		{"operations invalid ID", "../docs/operations.md", "### 浏览器登录与项目注册", "yes\n", []string{`TEST_RESPONSE={"project":{"id":0}}`}},
		{"cleanup missing path", "../docs/operations.md", "完成手动配置或自动注册后", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			block := registrationBlock(t, tc.path, tc.section)
			script := `set -e
trap 'if [ "${GITLAB_PROJECT_TOKEN+x}" = x ]; then printf set > "$TEST_RESULT"; else printf unset > "$TEST_RESULT"; fi' EXIT
` + block
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-c", script)
			cmd.Env = append(h.env, tc.extra...)
			cmd.Stdin = strings.NewReader(tc.input)
			output, err := cmd.CombinedOutput()
			if strings.Contains(string(output), registrationToken) || strings.Contains(string(output), registrationSecret) {
				t.Fatal("synthetic secret appeared in command output")
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("expected shell failure with errexit, got %v", err)
			}
			state, err := os.ReadFile(h.result)
			if err != nil || string(state) != "unset" {
				t.Error("project token was not cleared before shell exit")
			}
		})
	}
}

// runStrict captures the block's exit status even when errexit stops the shell.
func (h registrationHarness) runStrict(t *testing.T, block, setup, mode, input string, extra ...string) registrationResult {
	t.Helper()
	script := `trap 'test_status=$?
if [ "${GITLAB_PROJECT_TOKEN+x}" = x ]; then token_state=set; else token_state=unset; fi
if [ "${REGISTRATION_FILE+x}" = x ]; then file_state=set; else file_state=unset; fi
printf "%s\n%s\n%s\n%s" "$test_status" "$token_state" "$file_state" "${REGISTRATION_FILE:-}" > "$TEST_RESULT"' EXIT
` + mode + "\n" + setup + "\n" + block + "\nexit $?\n"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", script)
	cmd.Env = append(h.env, extra...)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run documented block: %v", err)
	}
	if strings.Contains(stdout.String()+stderr.String(), registrationToken) || strings.Contains(stdout.String()+stderr.String(), registrationSecret) {
		t.Fatal("synthetic secret appeared in command output")
	}
	data, err := os.ReadFile(h.result)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.SplitN(string(data), "\n", 4)
	if len(fields) != 4 || fields[0] != fmt.Sprint(cmd.ProcessState.ExitCode()) {
		t.Fatalf("shell status was not captured correctly: %q", data)
	}
	calls, err := os.ReadFile(h.calls)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return registrationResult{fields[0], fields[1], fields[2], fields[3], stdout.String(), stderr.String(), string(calls)}
}

func TestRegistrationDocsReadonlyAndUnsetFailures(t *testing.T) {
	docs := []struct{ name, path, section string }{
		{"readme", "../README.md", "## 注册项目"},
		{"operations", "../docs/operations.md", "### 浏览器登录与项目注册"},
	}
	cases := []struct {
		name, setup, tokenState string
		prior                   bool
	}{
		{"readonly token", "readonly GITLAB_PROJECT_TOKEN", "set", false},
		{"readonly token with prior response", "readonly GITLAB_PROJECT_TOKEN", "set", true},
		{"readonly project id", "PROJECT_ID=17; readonly PROJECT_ID", "unset", false},
		{"readonly response path", "readonly REGISTRATION_FILE", "unset", true},
		{"readonly empty response path", "REGISTRATION_FILE=; readonly REGISTRATION_FILE", "unset", false},
		{"readonly confirmation", "registration_confirm=old; readonly registration_confirm", "unset", false},
		{"readonly directory scratch", "registration_dir=old; readonly registration_dir", "unset", false},
		{"readonly identity scratch", "registration_identity=old; readonly registration_identity", "unset", false},
		{"existing path marker", "REGISTRATION_CREATED_FILE=old", "unset", false},
		{"existing empty path marker", "REGISTRATION_CREATED_FILE=", "unset", false},
		{"existing identity marker", "REGISTRATION_CREATED_ID=old", "unset", false},
		{"readonly path marker", "REGISTRATION_CREATED_FILE=old; readonly REGISTRATION_CREATED_FILE", "unset", false},
		{"readonly identity marker", "REGISTRATION_CREATED_ID=old; readonly REGISTRATION_CREATED_ID", "unset", false},
		{"descriptor already in use", "exec 9< /dev/null", "unset", false},
		{"write descriptor already in use", "exec 9> /dev/null", "unset", false},
		{"unset token fails", `builtin() { if [ "$1" = unset ] && [ "$3" = GITLAB_PROJECT_TOKEN ]; then return 7; fi; command builtin "$@"; }`, "set", false},
		{"unset project id fails", `builtin() { if [ "$1" = unset ] && [ "$3" = PROJECT_ID ]; then return 7; fi; command builtin "$@"; }`, "unset", false},
		{"unset response path fails", `builtin() { if [ "$1" = unset ] && [ "$3" = REGISTRATION_FILE ]; then return 7; fi; command builtin "$@"; }`, "unset", false},
		{"unset token does nothing", `builtin() { if [ "$1" = unset ] && [ "$3" = GITLAB_PROJECT_TOKEN ]; then return 0; fi; command builtin "$@"; }`, "set", false},
		{"unset project id does nothing", `PROJECT_ID=17; builtin() { if [ "$1" = unset ] && [ "$3" = PROJECT_ID ]; then return 0; fi; command builtin "$@"; }`, "unset", false},
		{"unset response path does nothing", `builtin() { if [ "$1" = unset ] && [ "$3" = REGISTRATION_FILE ]; then return 0; fi; command builtin "$@"; }`, "unset", true},
	}
	for _, doc := range docs {
		block := registrationBlock(t, doc.path, doc.section)
		for _, mode := range []struct{ name, setup string }{{"normal", ""}, {"errexit", "set -e"}, {"nounset", "set -u"}, {"both", "set -eu"}} {
			for _, tc := range cases {
				t.Run(doc.name+"/"+mode.name+"/"+tc.name, func(t *testing.T) {
					h := newRegistrationHarness(t)
					path := filepath.Join(h.dir, "prior-response")
					var extra []string
					if tc.prior {
						if err := os.WriteFile(path, []byte(registrationSecret), 0o600); err != nil {
							t.Fatal(err)
						}
						extra = append(extra, "REGISTRATION_FILE="+path)
					}
					r := h.runStrict(t, block, tc.setup, mode.setup, "yes\n", extra...)
					if r.status == "0" || r.stdout != "" || r.tokenState != tc.tokenState {
						t.Errorf("status = %s, stdout = %q, token state = %s", r.status, r.stdout, r.tokenState)
					}
					if r.calls != "" {
						t.Errorf("unsafe registration invoked fake commands: %q", r.calls)
					}
					if tc.tokenState == "set" && !strings.Contains(r.stderr, "shell") {
						t.Error("missing safe disposal guidance for an uncleared token")
					}
					if tc.prior {
						data, err := os.ReadFile(path)
						if r.file != path || err != nil || string(data) != registrationSecret {
							t.Error("prior response path or content was not preserved")
						}
					}
				})
			}
		}
	}
}

func TestRegistrationDocsCleanupReadonlyAndUnsetFailures(t *testing.T) {
	block := registrationBlock(t, "../docs/operations.md", "完成手动配置或自动注册后")
	cases := []struct {
		name, setup, fileState, rmFail, tokenState string
		wantOK, wantExists                         bool
		wantCalls                                  int
	}{
		{"success", "", "existing", "", "unset", true, false, 1},
		{"rm failure", "", "existing", "yes", "unset", false, true, 1},
		{"missing file", "", "missing", "", "unset", false, false, 0},
		{"missing path", "", "unset", "", "unset", false, true, 0},
		{"empty path", "", "empty", "", "unset", false, true, 0},
		{"readonly token", "readonly GITLAB_PROJECT_TOKEN", "existing", "", "set", false, true, 0},
		{"readonly path", "readonly REGISTRATION_FILE", "existing", "", "unset", false, true, 0},
		{"unset token fails", `builtin() { if [ "$1" = unset ] && [ "$3" = GITLAB_PROJECT_TOKEN ]; then return 7; fi; command builtin "$@"; }`, "existing", "", "set", false, true, 0},
		{"unset path fails", `builtin() { if [ "$1" = unset ] && [ "$3" = REGISTRATION_FILE ]; then return 7; fi; command builtin "$@"; }`, "existing", "", "unset", false, true, 0},
		{"unset token does nothing", `builtin() { if [ "$1" = unset ] && [ "$3" = GITLAB_PROJECT_TOKEN ]; then return 0; fi; command builtin "$@"; }`, "existing", "", "set", false, true, 0},
		{"unset path does nothing", `builtin() { if [ "$1" = unset ] && [ "$3" = REGISTRATION_FILE ]; then return 0; fi; command builtin "$@"; }`, "existing", "", "unset", false, true, 0},
		{"unset path fails after deletion", `builtin() { if [ "$BASH_SUBSHELL" = 0 ] && [ "$1" = unset ] && [ "$3" = REGISTRATION_FILE ]; then return 7; fi; command builtin "$@"; }`, "existing", "", "unset", false, false, 1},
	}
	for _, mode := range []struct{ name, setup string }{{"normal", ""}, {"errexit", "set -e"}, {"nounset", "set -u"}, {"both", "set -eu"}} {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				h := newRegistrationHarness(t)
				other := filepath.Join(h.dir, "other-file")
				if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				created := filepath.Join(h.root, "created")
				extra := []string{"TEST_PRIVATE=" + h.dir, "TEST_CREATED=" + created, "TEST_RM_FAIL=" + tc.rmFail}
				r := h.runStrict(t, registrationForCleanup(t, tc.fileState)+tc.setup+"\n"+block, "", mode.setup, "yes\n", extra...)
				createdData, err := os.ReadFile(created)
				if err != nil {
					t.Fatal(err)
				}
				path := string(createdData)
				if (r.status == "0") != tc.wantOK || r.tokenState != tc.tokenState || r.stdout != "" {
					t.Errorf("status = %s, stdout = %q, token state = %s", r.status, r.stdout, r.tokenState)
				}
				if tc.tokenState == "set" && !strings.Contains(r.stderr, "shell") {
					t.Error("missing safe disposal guidance for an uncleared token")
				}
				if tc.wantOK && r.fileState != "unset" || !tc.wantOK && tc.fileState != "unset" && r.fileState != "set" {
					t.Errorf("unexpected response path state: %s", r.fileState)
				}
				if !tc.wantOK && (tc.fileState == "existing" || tc.fileState == "missing") && r.file != path {
					t.Error("failed cleanup did not preserve the response path")
				}
				if strings.Count(r.calls, "rm\n") != tc.wantCalls {
					t.Errorf("unexpected fake rm calls: %q", r.calls)
				}
				data, err := os.ReadFile(path)
				if tc.wantExists != (err == nil) || tc.wantExists && !strings.Contains(string(data), registrationSecret) {
					t.Error("unexpected response file existence or content")
				}
				data, err = os.ReadFile(other)
				if err != nil || string(data) != "keep" {
					t.Error("another file was changed")
				}
			})
		}
	}
}

func TestRegistrationDocsReadmeAuthorization(t *testing.T) {
	block := registrationBlock(t, "../README.md", "CLI 默认读取")
	if !strings.Contains(block, "tribunalctl users list") || strings.Contains(block, "grants set") || strings.Contains(block, "grants delete") || strings.Contains(block, "users disable") || strings.Contains(block, "users enable") || strings.Contains(block, "users revoke") {
		t.Error("README must show only the read-only user query, not authorization mutations")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-n")
	cmd.Stdin = strings.NewReader(block)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("README authorization block does not parse: %v: %s", err, output)
	}
}

func TestRegistrationDocsLateTokenCleanupFailure(t *testing.T) {
	setup := `builtin() { if [ "$BASH_SUBSHELL" = 0 ] && [ "$1" = unset ] && [ "$3" = GITLAB_PROJECT_TOKEN ]; then return 7; fi; command builtin "$@"; }`
	for _, doc := range []struct{ name, path, section string }{
		{"readme", "../README.md", "## 注册项目"},
		{"operations", "../docs/operations.md", "### 浏览器登录与项目注册"},
	} {
		for _, mode := range []struct{ name, setup string }{{"normal", ""}, {"errexit", "set -e"}, {"nounset", "set -u"}, {"both", "set -eu"}} {
			t.Run(doc.name+"/"+mode.name, func(t *testing.T) {
				h := newRegistrationHarness(t)
				r := h.runStrict(t, registrationBlock(t, doc.path, doc.section), setup, mode.setup, "yes\n")
				if r.status == "0" || r.stdout != "" || r.tokenState != "set" || !strings.Contains(r.stderr, "shell") {
					t.Errorf("status = %s, stdout = %q, token state = %s", r.status, r.stdout, r.tokenState)
				}
				if !strings.Contains(r.stderr, "不要直接重试") {
					t.Error("late token cleanup failure did not warn against retrying registration")
				}
				if strings.Count(r.calls, "cli\n") != 1 || strings.Count(r.calls, "python\n") != 3 {
					t.Errorf("unexpected fake command calls: %q", r.calls)
				}
				data, err := os.ReadFile(r.file)
				if r.fileState != "set" || filepath.Dir(r.file) != h.dir || err != nil || !strings.Contains(string(data), registrationSecret) {
					t.Error("late cleanup failure did not preserve the private response")
				}
			})
		}
	}
}

func TestRegistrationDocsStrictSuccess(t *testing.T) {
	for _, doc := range []struct{ name, path, section string }{
		{"readme", "../README.md", "## 注册项目"},
		{"operations", "../docs/operations.md", "### 浏览器登录与项目注册"},
	} {
		for _, mode := range []struct{ name, setup string }{{"errexit", "set -e"}, {"nounset", "set -u"}, {"both", "set -eu"}} {
			t.Run(doc.name+"/"+mode.name, func(t *testing.T) {
				h := newRegistrationHarness(t)
				r := h.runStrict(t, registrationBlock(t, doc.path, doc.section), "", mode.setup, "yes\n")
				info, err := os.Stat(r.file)
				if r.status != "0" || r.stdout != "17\n" || r.tokenState != "unset" || r.fileState != "set" || filepath.Dir(r.file) != h.dir || err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("status = %s, stdout = %q, token state = %s; private response missing or has wrong mode", r.status, r.stdout, r.tokenState)
				}
				if strings.Count(r.calls, "cli\n") != 1 || strings.Count(r.calls, "python\n") != 3 {
					t.Errorf("unexpected fake command calls: %q", r.calls)
				}
			})
		}
	}
}

func TestRegistrationDocsControlledAuthorization(t *testing.T) {
	grant := registrationBlock(t, "../docs/operations.md", "先向用户明确确认实际项目 ID、目标 GitLab 用户 ID")
	user := registrationBlock(t, "../docs/operations.md", "每次执行前，先向用户确认目标 GitLab 用户 ID")
	for _, tc := range []struct {
		name, block, setup, calls string
	}{
		{"grant read", grant, "PROJECT_ID=17; GITLAB_USER_ID=23; GRANT_ACTION=read", "compose exec -T review /app/tribunalctl projects grants set 17 23 read\ncompose exec -T review /app/tribunalctl projects grants list 17\n"},
		{"grant manage", grant, "PROJECT_ID=17; GITLAB_USER_ID=23; GRANT_ACTION=manage", "compose exec -T review /app/tribunalctl projects grants set 17 23 manage\ncompose exec -T review /app/tribunalctl projects grants list 17\n"},
		{"grant delete", grant, "PROJECT_ID=17; GITLAB_USER_ID=23; GRANT_ACTION=delete", "compose exec -T review /app/tribunalctl projects grants delete 17 23\ncompose exec -T review /app/tribunalctl projects grants list 17\n"},
		{"grant missing project", grant, "GITLAB_USER_ID=23; GRANT_ACTION=read", ""},
		{"grant malformed user", grant, "PROJECT_ID=17; GITLAB_USER_ID=023; GRANT_ACTION=read", ""},
		{"grant invalid action", grant, "PROJECT_ID=17; GITLAB_USER_ID=23; GRANT_ACTION=disable", ""},
		{"disable user", user, "GITLAB_USER_ID=23; USER_ACTION=disable", "compose exec -T review /app/tribunalctl users disable 23\n"},
		{"enable user", user, "GITLAB_USER_ID=23; USER_ACTION=enable", "compose exec -T review /app/tribunalctl users enable 23\n"},
		{"revoke user", user, "GITLAB_USER_ID=23; USER_ACTION=revoke", "compose exec -T review /app/tribunalctl users revoke 23\n"},
		{"user missing id", user, "USER_ACTION=disable", ""},
		{"user malformed id", user, "GITLAB_USER_ID=023; USER_ACTION=disable", ""},
		{"user invalid action", user, "GITLAB_USER_ID=23; USER_ACTION=read", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			h.executable(t, "docker", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_CALLS\"\n")
			r := h.runStrict(t, tc.block, tc.setup, "set -eu", "")
			if (r.status == "0") != (tc.calls != "") || r.calls != tc.calls {
				t.Errorf("status = %s, fake calls = %q", r.status, r.calls)
			}
		})
	}
}

// The wrapper restricts real rm to the two directories owned by this test.
func (h registrationHarness) useBoundedRealRM(t *testing.T) {
	t.Helper()
	h.executable(t, "rm", `#!/bin/sh
[ "$#" = 2 ] && [ "$1" = -- ] || exit 8
"$REAL_PYTHON" -I -c 'import os, re, sys
p = os.path.realpath(sys.argv[1])
allowed = {os.path.realpath(sys.argv[2]), os.path.realpath(sys.argv[3])}
sys.exit(0 if os.path.dirname(p) in allowed and re.fullmatch(r"tribunal-registration\.[A-Za-z0-9]{8}", os.path.basename(p)) else 1)' "$2" "$TEST_PRIVATE" "$TEST_OTHER_PRIVATE" || exit 8
printf 'rm\n' >> "$TEST_CALLS"
exec /bin/rm -- "$2"
`)
}

func TestRegistrationDocsPathStability(t *testing.T) {
	cleanup := registrationBlock(t, "../docs/operations.md", "完成手动配置或自动注册后")
	for _, doc := range []struct{ name, path, section string }{
		{"readme", "../README.md", "## 注册项目"},
		{"operations", "../docs/operations.md", "### 浏览器登录与项目注册"},
	} {
		for _, relative := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/relative=%t", doc.name, relative), func(t *testing.T) {
				h := newRegistrationHarness(t)
				h.useBoundedRealRM(t)
				other := filepath.Join(h.root, "other", "private")
				if err := os.MkdirAll(other, 0o700); err != nil {
					t.Fatal(err)
				}
				dir := h.dir
				if relative {
					dir = "private"
				}
				script := `if {
` + registrationBlock(t, doc.path, doc.section) + `
}; then
  original="$REGISTRATION_FILE"
  case "$original" in private/tribunal-registration.*|"$TEST_PRIVATE"/tribunal-registration.*) ;; *) exit 8 ;; esac
  name="${original##*/}"
  [[ "$name" =~ ^tribunal-registration\.[A-Za-z0-9]{8}$ ]] || exit 8
  printf sentinel > "$TEST_OTHER_PRIVATE/$name"
  cd "$TEST_OTHER"
  if {
` + cleanup + `
  }; then cleaned=0; else cleaned=$?; fi
  if [ "$cleaned" = 0 ] && ( : <&9 ) 2>/dev/null; then exit 8; fi
  printf 'registered\n%s\n%s\n' "$original" "$cleaned" > "$TEST_RESULT"
else
  printf 'rejected\n%s\n' "${REGISTRATION_FILE:-}" > "$TEST_RESULT"
fi
`
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/bin/bash", "-ax", "-c", script)
				cmd.Dir = h.root
				cmd.Env = append(h.env, "REGISTRATION_DIR="+dir, "TEST_PRIVATE="+h.dir, "TEST_OTHER="+filepath.Dir(other), "TEST_OTHER_PRIVATE="+other)
				cmd.Stdin = strings.NewReader("yes\n")
				output, err := cmd.CombinedOutput()
				if strings.Contains(string(output), registrationToken) || strings.Contains(string(output), registrationSecret) {
					t.Fatal("synthetic secret appeared in command output")
				}
				if err != nil {
					t.Fatalf("documented commands failed: %v", err)
				}
				data, err := os.ReadFile(h.result)
				if err != nil {
					t.Fatal(err)
				}
				fields := strings.Split(strings.TrimSpace(string(data)), "\n")
				if fields[0] == "rejected" {
					if !relative || len(fields) > 1 && fields[1] != "" {
						t.Fatal("registration was rejected unexpectedly or left a response path")
					}
					calls, _ := os.ReadFile(h.calls)
					if strings.Contains(string(calls), "cli\n") {
						t.Fatal("relative directory was rejected only after registration")
					}
					return
				}
				if len(fields) != 3 {
					t.Fatalf("invalid result: %q", data)
				}
				original := fields[1]
				if !filepath.IsAbs(original) {
					original = filepath.Join(h.root, original)
				}
				sentinel, sentinelErr := os.ReadFile(filepath.Join(other, filepath.Base(original)))
				_, originalErr := os.Stat(original)
				if sentinelErr != nil || string(sentinel) != "sentinel" {
					t.Error("cleanup deleted or changed the other directory's sentinel")
				}
				if !filepath.IsAbs(fields[1]) || fields[2] != "0" || !errors.Is(originalErr, os.ErrNotExist) {
					t.Error("registration did not keep a stable absolute response path and delete the original")
				}
			})
		}
	}
}

func TestRegistrationDocsCleanupRejectsChangedTarget(t *testing.T) {
	registration := registrationBlock(t, "../docs/operations.md", "### 浏览器登录与项目注册")
	cleanup := registrationBlock(t, "../docs/operations.md", "完成手动配置或自动注册后")
	for _, tc := range []struct{ name, change string }{
		{"tampered path", `REGISTRATION_FILE="$TEST_PRIVATE/tribunal-registration.SENTINEL"`},
		{"empty path", `REGISTRATION_FILE=`},
		{"unset path", `unset REGISTRATION_FILE`},
		{"relative path", `REGISTRATION_FILE="private/${REGISTRATION_FILE##*/}"`},
		{"replaced file", `/bin/mv -- "$REGISTRATION_FILE" "$TEST_PRIVATE/saved-original"; printf sentinel > "$REGISTRATION_FILE"`},
		{"directory no longer private", `/bin/chmod 0755 "$TEST_PRIVATE"`},
		{"file no longer private", `/bin/chmod 0644 "$REGISTRATION_FILE"`},
		{"descriptor closed", `exec 9<&-`},
		{"descriptor replaced", `exec 9< "$TEST_PRIVATE/tribunal-registration.SENTINEL"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			h.useBoundedRealRM(t)
			other := filepath.Join(h.root, "other", "private")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(h.dir, "tribunal-registration.SENTINEL")
			if err := os.WriteFile(sentinel, []byte("sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			script := `if {
` + registration + `
}; then :; else exit 8; fi
original="$REGISTRATION_FILE"
` + tc.change + `
if {
` + cleanup + `
}; then cleaned=0; else cleaned=$?; fi
printf '%s\n%s\n' "$original" "$cleaned" > "$TEST_RESULT"
`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-x", "-c", script)
			cmd.Dir = h.root
			cmd.Env = append(h.env, "TEST_PRIVATE="+h.dir, "TEST_OTHER_PRIVATE="+other)
			cmd.Stdin = strings.NewReader("yes\n")
			output, err := cmd.CombinedOutput()
			if strings.Contains(string(output), registrationToken) || strings.Contains(string(output), registrationSecret) {
				t.Fatal("synthetic secret appeared in command output")
			}
			if err != nil {
				t.Fatalf("documented commands failed: %v", err)
			}
			data, err := os.ReadFile(h.result)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(fields) != 2 || fields[1] == "0" {
				t.Error("cleanup accepted a changed response target")
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "sentinel" {
				t.Error("cleanup changed the unrelated sentinel")
			}
			if len(fields) == 2 {
				want := registrationSecret
				if tc.name == "replaced file" {
					want = "sentinel"
				}
				if data, err := os.ReadFile(fields[0]); err != nil || !strings.Contains(string(data), want) {
					t.Error("cleanup deleted or changed the original path")
				}
			}
		})
	}
}

func TestRegistrationDocsDirectoryValidation(t *testing.T) {
	for _, doc := range []struct{ name, path, section string }{
		{"readme", "../README.md", "## 注册项目"},
		{"operations", "../docs/operations.md", "### 浏览器登录与项目注册"},
	} {
		for _, mode := range []os.FileMode{0o710, 0o701, 0o755, 0o777} {
			t.Run(fmt.Sprintf("%s/insecure-%o", doc.name, mode), func(t *testing.T) {
				h := newRegistrationHarness(t)
				if err := os.Chmod(h.dir, mode); err != nil {
					t.Fatal(err)
				}
				r := h.runStrict(t, registrationBlock(t, doc.path, doc.section), "", "set -eu", "yes\n")
				if r.status == "0" || r.file != "" || r.tokenState != "unset" || strings.Contains(r.calls, "cli\n") || strings.Contains(r.calls, "mktemp exported\n") {
					t.Errorf("insecure directory was accepted: status=%s, token=%s, calls=%q", r.status, r.tokenState, r.calls)
				}
			})
		}
		t.Run(doc.name+"/symlink-resolved", func(t *testing.T) {
			h := newRegistrationHarness(t)
			link := filepath.Join(h.root, "private-link")
			if err := os.Symlink(h.dir, link); err != nil {
				t.Fatal(err)
			}
			r := h.runStrict(t, registrationBlock(t, doc.path, doc.section), "", "set -eu", "yes\n", "REGISTRATION_DIR="+link)
			if r.status != "0" || filepath.Dir(r.file) != h.dir || !filepath.IsAbs(r.file) {
				t.Error("directory symlink was not resolved to the actual private directory")
			}
		})
		t.Run(doc.name+"/unsafe-created-file", func(t *testing.T) {
			h := newRegistrationHarness(t)
			h.executable(t, "mktemp", `#!/bin/sh
file=$("$REAL_MKTEMP" "$@") || exit 1
/bin/chmod 0644 "$file" || exit 1
printf '%s\n' "$file"
`)
			r := h.runStrict(t, registrationBlock(t, doc.path, doc.section), "", "set -eu", "yes\n")
			if r.status == "0" || r.tokenState != "unset" || r.fileState != "set" || filepath.Dir(r.file) != h.dir || strings.Contains(r.calls, "cli\n") {
				t.Errorf("unsafe created file was accepted: status=%s, token=%s, calls=%q", r.status, r.tokenState, r.calls)
			}
			if _, err := os.Stat(r.file); err != nil {
				t.Error("unsafe created file was not preserved for manual handling")
			}
		})
	}
}

func TestRegistrationDocsCleanupNeedsMarkers(t *testing.T) {
	block := registrationBlock(t, "../docs/operations.md", "完成手动配置或自动注册后")
	for _, tc := range []struct{ name, setup string }{
		{"absent", ""},
		{"mutable markers", `REGISTRATION_CREATED_FILE="$REGISTRATION_FILE"; REGISTRATION_CREATED_ID=1:1`},
		{"missing identity", `readonly REGISTRATION_CREATED_FILE="$REGISTRATION_FILE"`},
		{"missing path marker", `readonly REGISTRATION_CREATED_ID=1:1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			path := filepath.Join(h.dir, "tribunal-registration.SENTINEL")
			if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			r := h.runStrict(t, block, tc.setup, "set -eu", "", "REGISTRATION_FILE="+path, "TEST_ALLOWED_PATH="+path)
			if r.status == "0" || r.tokenState != "unset" || r.file != path || strings.Contains(r.calls, "rm\n") {
				t.Errorf("cleanup accepted missing or mutable markers: status=%s, calls=%q", r.status, r.calls)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "sentinel" {
				t.Error("cleanup changed an unverified file")
			}
		})
	}
}

func TestRegistrationDocsLocalStartupSecrets(t *testing.T) {
	block := registrationBlock(t, "../README.md", "## 本地启动")
	const master = "synthetic-master-never-print"
	const admin = "synthetic-admin-never-print"
	const session = "synthetic-session-never-print"
	const legacy = "synthetic-legacy-session-never-print"
	const llm = "synthetic-llm-never-print"
	const oauth = "synthetic-oauth-never-print"
	for _, tc := range []struct {
		name, missing, readonly                    string
		empty, failGo, failNpm, failCi, unexported bool
	}{
		{name: "injected"},
		{name: "injected unexported", unexported: true},
		{name: "npm failure", failNpm: true},
		{name: "npm ci failure", failCi: true},
		{name: "check-config failure", failGo: true},
		{name: "master readonly", readonly: "REVIEW_MASTER_KEY"},
		{name: "admin readonly", readonly: "REVIEW_ADMIN_TOKEN"},
		{name: "session readonly", readonly: "TRIBUNAL_SESSION_KEY"},
		{name: "legacy readonly", readonly: "CRC_SESSION_KEY"},
		{name: "master unset", missing: "REVIEW_MASTER_KEY"},
		{name: "master empty", missing: "REVIEW_MASTER_KEY", empty: true},
		{name: "admin unset", missing: "REVIEW_ADMIN_TOKEN"},
		{name: "admin empty", missing: "REVIEW_ADMIN_TOKEN", empty: true},
		{name: "session unset", missing: "TRIBUNAL_SESSION_KEY"},
		{name: "session empty", missing: "TRIBUNAL_SESSION_KEY", empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			if err := os.Mkdir(filepath.Join(h.root, "web"), 0o700); err != nil {
				t.Fatal(err)
			}
			h.executable(t, "openssl", `#!/bin/sh
printf 'openssl\n' >> "$TEST_CALLS"
printf '%s\n' "$TEST_MASTER"
`)
			h.executable(t, "npm", `#!/bin/sh
if [ "${REVIEW_MASTER_KEY+x}${REVIEW_ADMIN_TOKEN+x}${TRIBUNAL_SESSION_KEY+x}${CRC_SESSION_KEY+x}${LLM_API_KEY+x}${GITLAB_PROJECT_TOKEN+x}${GITLAB_OAUTH_CLIENT_SECRET+x}" != "" ]; then
  printf 'npm inherited keys\n' >> "$TEST_CALLS"
fi
printf 'npm\n' >> "$TEST_CALLS"
if [ "${TEST_NPM_FAIL:-}" = yes ] && [ "${1:-}" = run ]; then exit 1; fi
if [ "${TEST_NPM_CI_FAIL:-}" = yes ] && [ "${1:-}" = ci ]; then exit 1; fi
`)
			h.executable(t, "go", `#!/bin/sh
printf 'go\n' >> "$TEST_CALLS"
[ "$REVIEW_MASTER_KEY" = "$TEST_MASTER" ] && [ "$REVIEW_ADMIN_TOKEN" = "$TEST_ADMIN" ] && [ "$TRIBUNAL_SESSION_KEY" = "$TEST_SESSION" ] &&
[ "$CRC_SESSION_KEY" = "$TEST_LEGACY" ] && [ "$LLM_API_KEY" = "$TEST_LLM" ] && [ "$GITLAB_PROJECT_TOKEN" = "$TEST_TOKEN" ] &&
[ "$GITLAB_OAUTH_CLIENT_SECRET" = "$TEST_OAUTH" ] && [ "${TEST_GO_FAIL:-}" != yes ]
`)
			script := "set +x\n"
			if tc.unexported {
				script += "export -n REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY\n"
			}
			if tc.readonly != "" {
				script += "readonly " + tc.readonly + "\n"
			}
			script += `before_master="${REVIEW_MASTER_KEY+x}:${REVIEW_MASTER_KEY-}"
before_admin="${REVIEW_ADMIN_TOKEN+x}:${REVIEW_ADMIN_TOKEN-}"
before_session="${TRIBUNAL_SESSION_KEY+x}:${TRIBUNAL_SESSION_KEY-}"
before_other="$(declare -p CRC_SESSION_KEY LLM_API_KEY GITLAB_PROJECT_TOKEN GITLAB_OAUTH_CLIENT_SECRET)"
before_dir="$PWD"
trap 'set +x; after_other="$(declare -p CRC_SESSION_KEY LLM_API_KEY GITLAB_PROJECT_TOKEN GITLAB_OAUTH_CLIENT_SECRET)"; if [ "$before_master" = "${REVIEW_MASTER_KEY+x}:${REVIEW_MASTER_KEY-}" ] && [ "$before_admin" = "${REVIEW_ADMIN_TOKEN+x}:${REVIEW_ADMIN_TOKEN-}" ] && [ "$before_session" = "${TRIBUNAL_SESSION_KEY+x}:${TRIBUNAL_SESSION_KEY-}" ] && [ "$before_other" = "$after_other" ] && [ "$before_dir" = "$PWD" ]; then printf yes > "$TEST_STATE"; else printf no > "$TEST_STATE"; fi' EXIT
` + block
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-xu", "-c", script)
			cmd.Dir = h.root
			cmd.Env = append(h.env, "TEST_MASTER="+master, "TEST_ADMIN="+admin, "TEST_SESSION="+session,
				"TEST_LEGACY="+legacy, "TEST_LLM="+llm, "TEST_OAUTH="+oauth,
				"CRC_SESSION_KEY="+legacy, "LLM_API_KEY="+llm, "GITLAB_OAUTH_CLIENT_SECRET="+oauth,
				"TEST_STATE="+filepath.Join(h.root, "state"))
			for name, value := range map[string]string{"REVIEW_MASTER_KEY": master, "REVIEW_ADMIN_TOKEN": admin, "TRIBUNAL_SESSION_KEY": session} {
				if tc.missing != name {
					cmd.Env = append(cmd.Env, name+"="+value)
				} else if tc.empty {
					cmd.Env = append(cmd.Env, name+"=")
				}
			}
			if tc.failGo {
				cmd.Env = append(cmd.Env, "TEST_GO_FAIL=yes")
			}
			if tc.failNpm {
				cmd.Env = append(cmd.Env, "TEST_NPM_FAIL=yes")
			}
			if tc.failCi {
				cmd.Env = append(cmd.Env, "TEST_NPM_CI_FAIL=yes")
			}
			output, err := cmd.CombinedOutput()
			if strings.Contains(string(output), master) || strings.Contains(string(output), admin) || strings.Contains(string(output), session) ||
				strings.Contains(string(output), legacy) || strings.Contains(string(output), llm) || strings.Contains(string(output), oauth) ||
				strings.Contains(string(output), registrationToken) {
				t.Error("synthetic startup secret appeared in command output")
			}
			state, readErr := os.ReadFile(filepath.Join(h.root, "state"))
			if readErr != nil || string(state) != "yes" {
				t.Error("startup changed caller key values, other secret export states, or working directory")
			}
			calls, readErr := os.ReadFile(h.calls)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if strings.Contains(string(calls), "npm inherited keys") {
				t.Error("frontend build inherited deployment keys")
			}
			if tc.readonly != "" {
				if err == nil || len(calls) != 0 {
					t.Errorf("startup did not stop when a key was readonly, calls=%q, error=%v", calls, err)
				}
			} else if tc.failCi {
				if err == nil || string(calls) != "npm\n" {
					t.Errorf("startup continued after failed npm ci, calls=%q, error=%v", calls, err)
				}
			} else if tc.failNpm {
				if err == nil || string(calls) != "npm\nnpm\n" {
					t.Errorf("startup continued after failed frontend build, calls=%q, error=%v", calls, err)
				}
			} else if tc.failGo {
				if err == nil || string(calls) != "npm\nnpm\ngo\n" {
					t.Errorf("startup did not stop after failed config check, calls=%q, error=%v", calls, err)
				}
			} else if tc.missing == "" {
				if err != nil || string(calls) != "npm\nnpm\ngo\ngo\n" {
					t.Errorf("startup did not reuse injected values, calls=%q, error=%v", calls, err)
				}
			} else if err == nil || strings.Contains(string(calls), "go\n") {
				t.Errorf("startup ran without %s, calls=%q", tc.missing, calls)
			}
		})
	}
}

func readmeStandaloneNpmCommand(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	section := strings.SplitN(string(data), "全新检出没有 `web/dist` 时", 2)
	if len(section) != 2 {
		t.Fatal("standalone frontend build guidance not found")
	}
	build := strings.SplitN(section[1], "先执行 `", 2)
	if len(build) != 2 || !strings.Contains(build[1], "`") {
		t.Fatal("standalone frontend build command not found")
	}
	return strings.SplitN(build[1], "`", 2)[0]
}

func TestReadmeNpmCommandsDoNotInheritSecrets(t *testing.T) {
	buildCommand := readmeStandaloneNpmCommand(t)
	verifyCommand := registrationBlock(t, "../README.md", "## 验证")
	const keys = "REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY CRC_SESSION_KEY LLM_API_KEY GITLAB_PROJECT_TOKEN GITLAB_OAUTH_CLIENT_SECRET"
	for _, tc := range []struct {
		name, command, fail, readonly, calls string
	}{
		{"build success", buildCommand, "", "", "npm ci\nnpm run build\n"},
		{"build ci failure", buildCommand, "npm ci", "", "npm ci\n"},
		{"build failure", buildCommand, "npm run build", "", "npm ci\nnpm run build\n"},
		{"build readonly", buildCommand, "", "CRC_SESSION_KEY", ""},
		{"verify success", verifyCommand, "", "", "npm test\nnpm run build\ngo build ./...\ngo vet ./...\ngo test ./...\ngo test -race ./...\ngo mod verify\n"},
		{"verify npm test failure", verifyCommand, "npm test", "", "npm test\n"},
		{"verify npm build failure", verifyCommand, "npm run build", "", "npm test\nnpm run build\n"},
		{"verify go build failure", verifyCommand, "go build ./...", "", "npm test\nnpm run build\ngo build ./...\n"},
		{"verify readonly", verifyCommand, "", "REVIEW_MASTER_KEY", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			if err := os.Mkdir(filepath.Join(h.root, "web"), 0o700); err != nil {
				t.Fatal(err)
			}
			h.executable(t, "npm", `#!/bin/sh
for key in `+keys+`; do
  eval "present=\${$key+x}"
  if [ "$present" = x ]; then printf 'inherited %s\n' "$key" >> "$TEST_CALLS"; fi
done
[ "${BUILD_SETTING:-}" = configured ] || printf 'missing build setting\n' >> "$TEST_CALLS"
printf 'npm %s\n' "$*" >> "$TEST_CALLS"
[ "npm $*" != "${TEST_FAIL:-}" ]
`)
			h.executable(t, "go", `#!/bin/sh
printf 'go %s\n' "$*" >> "$TEST_CALLS"
[ "${REVIEW_MASTER_KEY:-}" = synthetic-master ] && [ "${REVIEW_ADMIN_TOKEN:-}" = synthetic-admin ] &&
[ "${TRIBUNAL_SESSION_KEY:-}" = synthetic-session ] && [ "${CRC_SESSION_KEY:-}" = synthetic-legacy ] &&
[ "${LLM_API_KEY:-}" = synthetic-llm ] && [ "${GITLAB_PROJECT_TOKEN:-}" = "$TEST_TOKEN" ] &&
[ "${GITLAB_OAUTH_CLIENT_SECRET:-}" = synthetic-oauth ] && [ "go $*" != "${TEST_FAIL:-}" ]
`)
			setup := ""
			if tc.readonly != "" {
				setup = "readonly " + tc.readonly + "\n"
			}
			script := `set +x
` + setup + `before="$(declare -p ` + keys + `)"
before_dir="$PWD"
if {
` + tc.command + `
}; then status=0; else status=$?; fi
after="$(declare -p ` + keys + `)"
if [ "$before" = "$after" ] && [ "$before_dir" = "$PWD" ]; then preserved=yes; else preserved=no; fi
printf '%s\n%s\n' "$status" "$preserved" > "$TEST_RESULT"
`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-u", "-c", script)
			cmd.Dir = h.root
			cmd.Env = append(h.env, "REVIEW_MASTER_KEY=synthetic-master", "REVIEW_ADMIN_TOKEN=synthetic-admin",
				"TRIBUNAL_SESSION_KEY=synthetic-session", "CRC_SESSION_KEY=synthetic-legacy", "LLM_API_KEY=synthetic-llm",
				"GITLAB_OAUTH_CLIENT_SECRET=synthetic-oauth", "BUILD_SETTING=configured", "TEST_FAIL="+tc.fail)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("documented command did not return control: %v", err)
			}
			for _, secret := range []string{"synthetic-master", "synthetic-admin", "synthetic-session", "synthetic-legacy", "synthetic-llm", "synthetic-oauth", registrationToken} {
				if strings.Contains(string(output), secret) {
					t.Error("synthetic secret appeared in output")
				}
			}
			result, err := os.ReadFile(h.result)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(strings.TrimSpace(string(result)), "\n")
			if len(fields) != 2 || fields[1] != "yes" || (fields[0] == "0") != (tc.fail == "" && tc.readonly == "") {
				t.Errorf("unexpected status or caller state: %q", result)
			}
			calls, err := os.ReadFile(h.calls)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if string(calls) != tc.calls {
				t.Errorf("unexpected subprocess calls or inherited keys: %q", calls)
			}
		})
	}
}

func TestReadmeNpmUnsetFalseSuccess(t *testing.T) {
	const keys = "REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY CRC_SESSION_KEY LLM_API_KEY GITLAB_PROJECT_TOKEN GITLAB_OAUTH_CLIENT_SECRET"
	commands := []struct{ name, command string }{
		{"startup", registrationBlock(t, "../README.md", "## 本地启动")},
		{"build", readmeStandaloneNpmCommand(t)},
		{"verify", registrationBlock(t, "../README.md", "## 验证")},
	}
	for _, tc := range commands {
		for _, key := range strings.Fields(keys) {
			for _, shadow := range []struct{ name, setup string }{
				{"function", `builtin() { test_builtin "$@"; }`},
				{"alias", "shopt -s expand_aliases\nalias builtin='test_builtin'"},
			} {
				for _, empty := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/empty=%t", tc.name, shadow.name, key, empty), func(t *testing.T) {
						h := newRegistrationHarness(t)
						if err := os.Mkdir(filepath.Join(h.root, "web"), 0o700); err != nil {
							t.Fatal(err)
						}
						for _, name := range []string{"npm", "go"} {
							h.executable(t, name, "#!/bin/sh\nprintf 'called\\n' >> \"$TEST_CALLS\"\n")
						}
						script := `set +x
test_builtin() {
  if [ "$1" = unset ] && [ "$2" = -v ]; then
    shift 2
    for name; do
      if [ "$name" != "$TEST_SKIP" ]; then command builtin unset -v "$name" || return; fi
    done
    return 0
  fi
  command builtin "$@"
}
` + shadow.setup + `
before="$(declare -p ` + keys + `)"
before_dir="$PWD"
if {
` + tc.command + `
}; then status=0; else status=$?; fi
after="$(declare -p ` + keys + `)"
if [ "$before" = "$after" ] && [ "$before_dir" = "$PWD" ]; then preserved=yes; else preserved=no; fi
printf '%s\n%s\n' "$status" "$preserved" > "$TEST_RESULT"
`
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						cmd := exec.CommandContext(ctx, "/bin/bash", "-u", "-c", script)
						cmd.Dir = h.root
						cmd.Env = append(h.env, "TEST_SKIP="+key)
						for _, name := range strings.Fields(keys) {
							value := "synthetic-" + name
							if empty && name == key {
								value = ""
							}
							cmd.Env = append(cmd.Env, name+"="+value)
						}
						output, err := cmd.CombinedOutput()
						if err != nil {
							t.Fatalf("documented command did not return control: %v", err)
						}
						for _, name := range strings.Fields(keys) {
							if strings.Contains(string(output), "synthetic-"+name) {
								t.Error("synthetic secret appeared in output")
							}
						}
						result, err := os.ReadFile(h.result)
						if err != nil {
							t.Fatal(err)
						}
						fields := strings.Split(strings.TrimSpace(string(result)), "\n")
						if len(fields) != 2 || fields[0] == "0" || fields[1] != "yes" {
							t.Errorf("false-success unset did not stop or changed caller state: %q", result)
						}
						calls, err := os.ReadFile(h.calls)
						if err != nil && !errors.Is(err, os.ErrNotExist) {
							t.Fatal(err)
						}
						if len(calls) != 0 {
							t.Errorf("npm or go ran after a false-success unset: %q", calls)
						}
					})
				}
			}
		}
	}
}

func TestExampleConfigKeyGuidance(t *testing.T) {
	data, err := os.ReadFile("../config/server.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "openssl rand") {
		t.Error("example config must not suggest printing generated keys")
	}
	if !strings.Contains(string(data), "docs/agent-deployment.md#2-准备配置") {
		t.Error("example config must link to the private key generation steps")
	}
}

func TestRegistrationDocsKeyGuidanceAndSmoke(t *testing.T) {
	envExample, err := os.ReadFile("../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(envExample), "openssl rand") || !strings.Contains(string(envExample), "docs/agent-deployment.md#2-准备配置") {
		t.Error(".env.example must point to private key generation without a printing command")
	}
	block := registrationBlock(t, "../docs/operations.md", "### Docker 可用时的单镜像烟测")
	const master = "synthetic-smoke-master-never-print"
	const admin = "synthetic-smoke-admin-never-print"
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("openssl-fails=%t", fail), func(t *testing.T) {
			h := newRegistrationHarness(t)
			h.executable(t, "openssl", `#!/bin/sh
printf 'openssl\n' >> "$TEST_CALLS"
[ "${TEST_OPENSSL_FAIL:-}" != yes ] || exit 9
case "$2" in -base64) printf '%s\n' "$TEST_MASTER" ;; -hex) printf '%s\n' "$TEST_ADMIN" ;; *) exit 8 ;; esac
`)
			h.executable(t, "docker", `#!/bin/sh
case "$*" in
  *"$TEST_MASTER"*|*"$TEST_ADMIN"*) exit 8 ;;
esac
printf 'docker\n' >> "$TEST_CALLS"
case "$*" in
  *--check-config*) [ "${REVIEW_MASTER_KEY:-}" = "$TEST_MASTER" ] && [ "${REVIEW_ADMIN_TOKEN:-}" = "$TEST_ADMIN" ] ;;
esac
`)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-xu", "-c", block)
			cmd.Env = append(h.env, "TEST_MASTER="+master, "TEST_ADMIN="+admin)
			if fail {
				cmd.Env = append(cmd.Env, "TEST_OPENSSL_FAIL=yes")
			}
			output, err := cmd.CombinedOutput()
			if strings.Contains(string(output), master) || strings.Contains(string(output), admin) {
				t.Error("synthetic smoke key appeared in command output")
			}
			calls, readErr := os.ReadFile(h.calls)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if fail {
				if err == nil || string(calls) != "docker\ndocker\ndocker\nopenssl\n" {
					t.Errorf("smoke continued after key generation failure: calls=%q, error=%v", calls, err)
				}
			} else if err != nil || string(calls) != "docker\ndocker\ndocker\nopenssl\nopenssl\ndocker\n" {
				t.Errorf("smoke did not pass generated keys safely: calls=%q, error=%v", calls, err)
			}
		})
	}
}

// The whole local smoke is copied as one unit; older revisions had separate blocks.
func localSmokeCommands(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	section := strings.SplitN(string(data), "## 9. 无外部凭据的本地烟测", 2)
	if len(section) != 2 {
		t.Fatal("local smoke section not found")
	}
	section = strings.SplitN(section[1], "### Docker 可用时的 Compose 烟测", 2)
	parts := strings.Split(section[0], "```bash\n")
	if len(parts) < 2 {
		t.Fatal("local smoke commands not found")
	}
	var blocks []string
	for _, part := range parts[1:] {
		block := strings.SplitN(part, "\n```", 2)
		if len(block) != 2 {
			t.Fatal("local smoke block not closed")
		}
		blocks = append(blocks, block[0])
	}
	// Redirect the old fixed build target into this test's directory, never /tmp.
	return strings.ReplaceAll(strings.Join(blocks, "\n"), "/tmp/tribunal-server", `"$TEST_SERVER"`)
}

func TestRegistrationDocsLocalSmokeIsolation(t *testing.T) {
	const master, admin, session = "synthetic-production-master", "synthetic-production-admin", "synthetic-production-session"
	const smokeMaster, smokeAdmin, smokeSession = "synthetic-test-master", "synthetic-test-admin", "synthetic-test-session"
	for _, tc := range []struct{ name, setup, fail string }{
		{"success exported", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER" REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN" TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"`, ""},
		{"success unexported", `REVIEW_MASTER_KEY="$TEST_PROD_MASTER"; REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN"; TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"; export -n REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY`, ""},
		{"success mixed", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER"; REVIEW_ADMIN_TOKEN=; export -n REVIEW_ADMIN_TOKEN; unset TRIBUNAL_SESSION_KEY`, ""},
		{"readonly key", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER" REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN" TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"; readonly REVIEW_MASTER_KEY`, "readonly"},
		{"npm failure", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER" REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN" TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"`, "npm"},
		{"generation failure", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER" REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN" TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"`, "openssl"},
		{"config failure", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER" REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN" TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"`, "config"},
		{"unexpected health status", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER" REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN" TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"`, "bad-health"},
		{"unexpected bearer status", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER" REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN" TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"`, "bad-api"},
		{"interrupted", `export REVIEW_MASTER_KEY="$TEST_PROD_MASTER" REVIEW_ADMIN_TOKEN="$TEST_PROD_ADMIN" TRIBUNAL_SESSION_KEY="$TEST_PROD_SESSION"`, "interrupt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			if err := os.Mkdir(filepath.Join(h.root, "web"), 0o700); err != nil {
				t.Fatal(err)
			}
			npmExit := "exit 0"
			if tc.fail == "npm" {
				npmExit = "exit 9"
			}
			h.executable(t, "npm", fmt.Sprintf(`#!/bin/sh
if [ "${REVIEW_MASTER_KEY+x}${REVIEW_ADMIN_TOKEN+x}${TRIBUNAL_SESSION_KEY+x}${TEST_PROD_MASTER+x}" != "" ]; then printf 'npm inherited keys\n' >> %q; fi
printf 'npm %%s\n' "$*" >> %q
%s
`, h.calls, h.calls, npmExit))
			h.executable(t, "go", `#!/bin/sh
printf 'go\n' >> "$TEST_CALLS"
[ "$1" = build ] && [ "$2" = -o ] || exit 8
exec /bin/cp "$TEST_SERVER_FIXTURE" "$3"
`)
			h.executable(t, "server-fixture", `#!/bin/sh
[ "${REVIEW_MASTER_KEY:-}" = "$TEST_SMOKE_MASTER" ] && [ "${REVIEW_ADMIN_TOKEN:-}" = "$TEST_SMOKE_ADMIN" ] && [ "${TRIBUNAL_SESSION_KEY:-}" = "$TEST_SMOKE_SESSION" ] || exit 8
case "$*" in
  *--check-config*) printf 'config\n' >> "$TEST_CALLS"; [ "${TEST_FAIL:-}" != config ]; exit $? ;;
esac
trap 'printf "server stopped\n" >> "$TEST_CALLS"; exit 0' TERM
printf 'server\n' >> "$TEST_CALLS"
: > "$TEST_SERVER_READY"
while :; do /bin/sleep 0.1; done
`)
			h.executable(t, "openssl", `#!/bin/sh
printf 'openssl\n' >> "$TEST_CALLS"
[ "${TEST_FAIL:-}" != openssl ] || exit 9
case "$2" in
  -hex) printf '%s\n' "$TEST_SMOKE_ADMIN" ;;
  -base64) if [ -e "$TEST_GENERATED" ]; then printf '%s\n' "$TEST_SMOKE_SESSION"; else : > "$TEST_GENERATED"; printf '%s\n' "$TEST_SMOKE_MASTER"; fi ;;
  *) exit 8 ;;
esac
`)
			h.executable(t, "mktemp", `#!/bin/sh
[ "$1" = -d ] || exit 8
exec "$REAL_MKTEMP" -d "$TEST_ROOT/smoke.XXXXXXXX"
`)
			h.executable(t, "cat", "#!/bin/sh\nexec /bin/cat \"$@\"\n")
			h.executable(t, "mkdir", "#!/bin/sh\nexec /bin/mkdir \"$@\"\n")
			h.executable(t, "seq", "#!/bin/sh\nexec /usr/bin/seq \"$@\"\n")
			h.executable(t, "sleep", "#!/bin/sh\nexit 0\n")
			h.executable(t, "curl", `#!/bin/sh
printf 'curl %s\n' "$*" >> "$TEST_CALLS"
while [ ! -e "$TEST_SERVER_READY" ]; do /bin/sleep 0.05; done
if [ "${TEST_FAIL:-}" = interrupt ]; then kill -INT "$PPID"; exit 130; fi
case "$*" in
  *'/api/v1/projects'*) IFS= read -r header; [ "$header" = "Authorization: Bearer $TEST_SMOKE_ADMIN" ] || exit 8; if [ "$TEST_FAIL" = bad-api ]; then printf 302; elif [ "${*#*-w}" != "$*" ]; then printf 200; fi ;;
  *'/healthz'*) if [ "$TEST_FAIL" = bad-health ]; then printf 302; elif [ "${*#*-w}" != "$*" ]; then printf 200; fi ;;
  *'/auth/login'*) printf 503 ;;
  *'127.0.0.1:18080/'*) case "$*" in *'-w'*) printf 200 ;; esac ;;
esac
exit 0
`)
			h.executable(t, "rm", `#!/bin/sh
for arg do path=$arg; done
case "$path" in "$TEST_ROOT"/smoke.*) exec /bin/rm "$@" ;; *) exit 8 ;; esac
`)
			script := `set +x
set +e
` + tc.setup + `
before_master="$(declare -p REVIEW_MASTER_KEY 2>/dev/null)"
before_admin="$(declare -p REVIEW_ADMIN_TOKEN 2>/dev/null)"
before_session="$(declare -p TRIBUNAL_SESSION_KEY 2>/dev/null)"
set -x
` + localSmokeCommands(t) + `
smoke_status=$?
set +x
after_master="$(declare -p REVIEW_MASTER_KEY 2>/dev/null)"
after_admin="$(declare -p REVIEW_ADMIN_TOKEN 2>/dev/null)"
after_session="$(declare -p TRIBUNAL_SESSION_KEY 2>/dev/null)"
if [ "$before_master" = "$after_master" ] && [ "$before_admin" = "$after_admin" ] && [ "$before_session" = "$after_session" ]; then preserved=yes; else preserved=no; fi
printf '%s\n%s\n' "$smoke_status" "$preserved" > "$TEST_RESULT"
`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-c", script)
			cmd.Dir = h.root
			cmd.Env = append(h.env, "TEST_ROOT="+h.root, "TEST_SERVER="+filepath.Join(h.root, "server"), "TEST_SERVER_FIXTURE="+filepath.Join(h.bin, "server-fixture"), "TEST_SERVER_READY="+filepath.Join(h.root, "server-ready"), "TEST_GENERATED="+filepath.Join(h.root, "generated"), "TEST_FAIL="+tc.fail,
				"TEST_PROD_MASTER="+master, "TEST_PROD_ADMIN="+admin, "TEST_PROD_SESSION="+session, "TEST_SMOKE_MASTER="+smokeMaster, "TEST_SMOKE_ADMIN="+smokeAdmin, "TEST_SMOKE_SESSION="+smokeSession)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("documented smoke did not return control: %v", err)
			}
			for _, secret := range []string{master, admin, session, smokeMaster, smokeAdmin, smokeSession} {
				if strings.Contains(string(output), secret) {
					t.Error("synthetic key appeared on stdout, stderr or xtrace")
				}
			}
			result, err := os.ReadFile(h.result)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(strings.TrimSpace(string(result)), "\n")
			if len(fields) != 2 || fields[1] != "yes" {
				t.Errorf("caller key values or export states changed: %q", result)
			}
			if len(fields) != 2 || (fields[0] == "0") != (tc.fail == "") {
				t.Errorf("unexpected smoke status: %q", result)
			}
			calls, _ := os.ReadFile(h.calls)
			if strings.Contains(string(calls), "npm inherited keys") {
				t.Error("npm inherited caller deployment keys")
			}
			if (tc.fail == "" || tc.fail == "interrupt" || tc.fail == "bad-health" || tc.fail == "bad-api") && !strings.Contains(string(calls), "server stopped\n") {
				t.Error("smoke did not stop its own test server")
			}
			if (tc.fail == "npm" && strings.Contains(string(calls), "go\n")) || (tc.fail == "openssl" && strings.Contains(string(calls), "config\n")) || (tc.fail == "config" && strings.Contains(string(calls), "server\n")) {
				t.Errorf("smoke continued after a failure: %q", calls)
			}
			leftovers, err := filepath.Glob(filepath.Join(h.root, "smoke.*"))
			if err != nil || len(leftovers) != 0 {
				t.Error("smoke did not clean its own temporary directory")
			}
			if tc.fail == "" {
				for _, want := range []string{"npm ci\n", "npm run build\n", "go\n", "config\n", "/healthz", "/readyz", "/auth/login", "/api/v1/projects"} {
					if !strings.Contains(string(calls), want) {
						t.Errorf("smoke did not exercise %s", want)
					}
				}
				if strings.Count(string(calls), "config\n") != 2 {
					t.Error("expected default and OAuth config checks")
				}
			}
		})
	}
}

func TestRegistrationDocsCleanupVerifiesUnlink(t *testing.T) {
	block := registrationBlock(t, "../docs/operations.md", "完成手动配置或自动注册后")
	for _, mode := range []struct{ name, setup string }{{"normal", ""}, {"errexit", "set -e"}, {"nounset", "set -u"}, {"both", "set -eu"}} {
		for _, tc := range []struct{ name, rmMode string }{{"noop", "noop"}, {"moved inode", "move"}, {"dangling symlink", "symlink"}, {"directory mode changed", "dir-mode"}, {"directory replaced", "dir-replace"}} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				h := newRegistrationHarness(t)
				created := filepath.Join(h.root, "created")
				fdResult := filepath.Join(h.root, "fd-result")
				script := `trap 'if ( : <&9 ) 2>/dev/null && [ "${REGISTRATION_FILE:-}" = "${TEST_ALLOWED_PATH:-}" ] && [ -n "${REGISTRATION_FILE:-}" ]; then printf preserved > "$TEST_FD"; else printf changed > "$TEST_FD"; fi' EXIT
` + mode.setup + "\n" + registrationForCleanup(t, "existing") + block
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/bin/bash", "-c", script)
				cmd.Env = append(h.env, "TEST_PRIVATE="+h.dir, "TEST_CREATED="+created, "TEST_FD="+fdResult, "TEST_RM_FAIL="+tc.rmMode)
				cmd.Stdin = strings.NewReader("yes\n")
				output, err := cmd.CombinedOutput()
				if strings.Contains(string(output), registrationToken) || strings.Contains(string(output), registrationSecret) {
					t.Error("synthetic secret appeared in output")
				}
				if err == nil {
					t.Error("cleanup falsely reported success")
				}
				fd, readErr := os.ReadFile(fdResult)
				if readErr != nil || string(fd) != "preserved" {
					t.Error("failed verification did not preserve response path and descriptor 9")
				}
				path, readErr := os.ReadFile(created)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if tc.rmMode == "noop" {
					data, readErr := os.ReadFile(string(path))
					if readErr != nil || !strings.Contains(string(data), registrationSecret) {
						t.Error("no-op rm did not preserve response")
					}
				}
			})
		}
	}
}

func TestRegistrationDocsClipboardUsesOpenedResponse(t *testing.T) {
	block := registrationBlock(t, "../docs/operations.md", "手动配置时，在已验证 HTTPS")
	for _, tc := range []struct {
		name, change string
		wantOK       bool
	}{
		{"repeat on same descriptor", "", true},
		{"replaced path", `/bin/mv -- "$REGISTRATION_FILE" "$TEST_PRIVATE/saved-original"; printf sentinel > "$REGISTRATION_FILE"`, false},
		{"changed directory mode", `/bin/chmod 0755 "$TEST_PRIVATE"`, false},
		{"replaced descriptor", `exec 9< /dev/null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegistrationHarness(t)
			clipboard := filepath.Join(h.root, "clipboard")
			created := filepath.Join(h.root, "created")
			h.executable(t, "pbcopy", "#!/bin/sh\nexec /bin/cat > \"$TEST_CLIPBOARD\"\n")
			script := registrationForCleanup(t, "existing") + tc.change + `
set -x
if {
` + block + `
}; then first=0; else first=$?; fi
if [ "$first" = 0 ]; then
  if {
` + block + `
  }; then second=0; else second=$?; fi
else second="$first"; fi
set +x
if ( : <&9 ) 2>/dev/null; then fd=open; else fd=closed; fi
printf '%s\n%s\n%s\n' "$first" "$second" "$fd" > "$TEST_RESULT"
`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-c", script)
			cmd.Env = append(h.env, "TEST_PRIVATE="+h.dir, "TEST_CREATED="+created, "TEST_CLIPBOARD="+clipboard)
			cmd.Stdin = strings.NewReader("yes\n")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("clipboard test failed: %v", err)
			}
			if strings.Contains(string(output), registrationToken) || strings.Contains(string(output), registrationSecret) {
				t.Error("synthetic secret appeared on stdout, stderr or xtrace")
			}
			result, err := os.ReadFile(h.result)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(strings.TrimSpace(string(result)), "\n")
			if len(fields) != 3 || (fields[0] == "0" && fields[1] == "0") != tc.wantOK || fields[2] != "open" {
				t.Errorf("unexpected clipboard status or descriptor state: %q", result)
			}
			data, readErr := os.ReadFile(clipboard)
			if tc.wantOK && (readErr != nil || string(data) != registrationSecret) {
				t.Error("repeat clipboard read did not recover the original one-time secret")
			}
			if !tc.wantOK && strings.Contains(string(data), registrationSecret) {
				t.Error("clipboard received secret from an unverified response")
			}
		})
	}
}

func TestRegistrationDocsExistingSessionKeyGuidance(t *testing.T) {
	data, err := os.ReadFile("../docs/agent-deployment.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "容器反复重启，日志提到 session key") {
			if !strings.Contains(line, "已有部署须恢复原来保存的值") || !strings.Contains(line, "首次部署") || !strings.Contains(line, "用户明确授权") || !strings.Contains(line, "会话失效") || strings.Contains(line, "重新生成") {
				t.Error("existing deployments must restore the saved session key; new keys require first deployment or explicit authorization")
			}
			return
		}
	}
	t.Error("session key troubleshooting row not found")
}
