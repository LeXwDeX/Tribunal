package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	smokeSessionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm"
	smokePublicURL  = "http://127.0.0.1:8080"
	callerSecret    = "caller-secret-must-not-leak"
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
		"../config/server.docker.yaml": {
			`public_url: "${TRIBUNAL_PUBLIC_URL}"`,
		},
	}
	for path, wants := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(data)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not contain %q", path, want)
			}
		}
	}
}

// TestDockerSmokeCleanup runs the real smoke script against a fake Docker CLI.
// It proves cleanup status handling, not that Compose or Docker work.
func TestDockerSmokeCleanup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to run docker-smoke.sh")
	}
	script, err := filepath.Abs("docker-smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name            string
		buildFail       bool
		downFail        bool
		imageFail       bool
		rmFail          bool
		mcpWriteTool    bool
		mcpMissingTool  bool
		wantExit        int
		wantImageRemove bool
		wantWarnings    []string
		wantOutput      string
	}{
		{name: "success", wantExit: 0, wantImageRemove: true},
		{name: "build failure", buildFail: true, wantExit: 1},
		{name: "project removal failure", downFail: true, wantExit: 1, wantImageRemove: true,
			wantWarnings: []string{"could not remove project"}},
		{name: "image removal failure", imageFail: true, wantExit: 1, wantImageRemove: true,
			wantWarnings: []string{"could not remove image"}},
		{name: "temporary directory removal failure", rmFail: true, wantExit: 1, wantImageRemove: true,
			wantWarnings: []string{"could not remove temporary directory"}},
		{name: "MCP exposes a write tool", mcpWriteTool: true, wantExit: 1, wantImageRemove: true,
			wantOutput: "codebase-memory MCP exposes write tool delete_project"},
		{name: "MCP misses a default tool", mcpMissingTool: true, wantExit: 1, wantImageRemove: true,
			wantOutput: "codebase-memory MCP does not list default tool check_index_coverage"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tmpRoot := filepath.Join(root, "tmp")
			bin := filepath.Join(root, "bin")
			for _, dir := range []string{tmpRoot, bin} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			callLog := filepath.Join(root, "calls.log")
			envLog := filepath.Join(root, "env.log")
			writeExecutable(t, filepath.Join(bin, "docker"), fakeDocker)
			writeExecutable(t, filepath.Join(bin, "rm"), fakeRM)

			cmd := exec.Command("bash", script)
			cmd.Dir = filepath.Dir(script)
			cmd.Env = []string{
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"TMPDIR=" + tmpRoot,
				"SMOKE_FAKE_CALL_LOG=" + callLog,
				"SMOKE_FAKE_ENV_LOG=" + envLog,
				"SMOKE_FAKE_TMP_ROOT=" + tmpRoot,
				"SMOKE_FAKE_BUILD_FAIL=" + boolEnv(tc.buildFail),
				"SMOKE_FAKE_DOWN_FAIL=" + boolEnv(tc.downFail),
				"SMOKE_FAKE_IMAGE_RM_FAIL=" + boolEnv(tc.imageFail),
				"SMOKE_FAKE_RM_FAIL=" + boolEnv(tc.rmFail),
				"SMOKE_FAKE_MCP_WRITE_TOOL=" + boolEnv(tc.mcpWriteTool),
				"SMOKE_FAKE_MCP_MISSING_TOOL=" + boolEnv(tc.mcpMissingTool),
				"TRIBUNAL_SESSION_KEY=" + callerSecret,
				"CRC_SESSION_KEY=" + callerSecret,
				"TRIBUNAL_PUBLIC_URL=https://caller.invalid",
				"CRC_PUBLIC_URL=https://caller.invalid",
				"TRIBUNAL_HTTP_PORT=12345",
				"CRC_HTTP_PORT=12345",
			}
			output, runErr := cmd.CombinedOutput()
			exitCode := 0
			if runErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) {
					t.Fatalf("run script: %v\n%s", runErr, output)
				}
				exitCode = exitErr.ExitCode()
			}
			if exitCode != tc.wantExit {
				t.Fatalf("exit code = %d, want %d\n%s", exitCode, tc.wantExit, output)
			}
			if tc.wantOutput != "" && !strings.Contains(string(output), tc.wantOutput) {
				t.Fatalf("missing failure %q\n%s", tc.wantOutput, output)
			}
			for _, warning := range tc.wantWarnings {
				if !strings.Contains(string(output), "warning: "+warning) {
					t.Fatalf("missing cleanup warning %q\n%s", warning, output)
				}
			}

			env := readEnv(t, envLog)
			for name, want := range map[string]string{
				"TRIBUNAL_SESSION_KEY": smokeSessionKey,
				"CRC_SESSION_KEY":      smokeSessionKey,
				"TRIBUNAL_PUBLIC_URL":  smokePublicURL,
				"CRC_PUBLIC_URL":       smokePublicURL,
				"TRIBUNAL_HTTP_PORT":   "0",
				"CRC_HTTP_PORT":        "0",
			} {
				if env[name] != want {
					t.Errorf("%s override = %q, want fake value", name, env[name])
				}
				if env[name] == callerSecret || env[name] == "https://caller.invalid" || env[name] == "12345" {
					t.Errorf("%s kept a caller value", name)
				}
			}

			calls := readCalls(t, callLog)
			project := ""
			downCalls := 0
			var imageRemoves []string
			var rmCalls [][]string
			var cliCalls []string
			mcpTested := false
			for _, args := range calls {
				if len(args) == 0 {
					continue
				}
				for _, arg := range args {
					if arg == "/app/tribunalctl" || arg == "/app/reviewctl" {
						cliCalls = append(cliCalls, arg)
					}
					if strings.HasSuffix(arg, "/api/v1/mcp-servers/srv-codebase-memory/test") {
						mcpTested = true
					}
				}
				switch args[0] {
				case "compose":
					if len(args) == 2 && args[1] == "version" {
						continue
					}
					name := optionValue(args, "--project-name")
					if name == "" {
						t.Fatalf("compose call without --project-name: %v", args)
					}
					if project == "" {
						project = name
					} else if name != project {
						t.Fatalf("compose calls used projects %q and %q", project, name)
					}
					if contains(args, "down") {
						downCalls++
						if !contains(args, "--volumes") || !contains(args, "--remove-orphans") {
							t.Fatalf("down call did not remove its volume and orphans: %v", args)
						}
					}
				case "image":
					if len(args) != 3 || args[1] != "rm" {
						t.Fatalf("unexpected image call: %v", args)
					}
					imageRemoves = append(imageRemoves, args[2])
				case "info":
				default:
					rmCalls = append(rmCalls, args)
				}
			}
			if !regexp.MustCompile(`^tribunal-smoke-[0-9]+-[0-9]+$`).MatchString(project) {
				t.Fatalf("project %q is not a generated tribunal-smoke project", project)
			}
			if !tc.buildFail {
				if !contains(cliCalls, "/app/tribunalctl") || !contains(cliCalls, "/app/reviewctl") {
					t.Fatalf("smoke did not exercise both CLI paths: %v", cliCalls)
				}
				if !mcpTested {
					t.Fatal("smoke did not test the default codebase-memory MCP server")
				}
			}
			if downCalls != 1 {
				t.Fatalf("compose down calls = %d, want 1", downCalls)
			}
			if tc.wantImageRemove {
				if len(imageRemoves) != 1 {
					t.Fatalf("image removals = %v, want one", imageRemoves)
				}
				if want := "tribunal:smoke-" + project; imageRemoves[0] != want {
					t.Fatalf("image removal = %q, want %q", imageRemoves[0], want)
				}
			} else if len(imageRemoves) != 0 {
				t.Fatalf("image removals = %v, want none after a failed build", imageRemoves)
			}
			if len(rmCalls) != 1 {
				t.Fatalf("temporary directory removals = %v, want one", rmCalls)
			}
			rmArgs := rmCalls[0]
			if len(rmArgs) != 2 || rmArgs[0] != "-rf" {
				t.Fatalf("unexpected rm invocation: %v", rmArgs)
			}
			if filepath.Dir(rmArgs[1]) != tmpRoot || !strings.HasPrefix(filepath.Base(rmArgs[1]), "tribunal-smoke.") {
				t.Fatalf("rm target %q is not the generated temporary directory", rmArgs[1])
			}
			entries, err := os.ReadDir(tmpRoot)
			if err != nil {
				t.Fatal(err)
			}
			if tc.rmFail {
				if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "tribunal-smoke.") {
					t.Fatalf("temporary directory after failed removal = %v, want the smoke directory", entries)
				}
			} else if len(entries) != 0 {
				t.Fatalf("temporary files left after cleanup: %v", entries)
			}
		})
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func boolEnv(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func readCalls(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		args := strings.Split(line, "\t")
		if len(args) > 0 && args[len(args)-1] == "" {
			args = args[:len(args)-1]
		}
		calls = append(calls, args)
	}
	return calls
}

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}

func optionValue(args []string, option string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == option {
			return args[i+1]
		}
	}
	return ""
}

func contains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

const fakeDocker = `#!/usr/bin/env bash
set -u

log_call() {
  local arg
  for arg in "$@"; do
    printf '%s\t' "$arg"
  done >>"$SMOKE_FAKE_CALL_LOG"
  printf '\n' >>"$SMOKE_FAKE_CALL_LOG"
}
log_env() {
  printf 'TRIBUNAL_SESSION_KEY=%s\n' "${TRIBUNAL_SESSION_KEY:-}" >>"$SMOKE_FAKE_ENV_LOG"
  printf 'CRC_SESSION_KEY=%s\n' "${CRC_SESSION_KEY:-}" >>"$SMOKE_FAKE_ENV_LOG"
  printf 'TRIBUNAL_PUBLIC_URL=%s\n' "${TRIBUNAL_PUBLIC_URL:-}" >>"$SMOKE_FAKE_ENV_LOG"
  printf 'CRC_PUBLIC_URL=%s\n' "${CRC_PUBLIC_URL:-}" >>"$SMOKE_FAKE_ENV_LOG"
  printf 'TRIBUNAL_HTTP_PORT=%s\n' "${TRIBUNAL_HTTP_PORT:-}" >>"$SMOKE_FAKE_ENV_LOG"
  printf 'CRC_HTTP_PORT=%s\n' "${CRC_HTTP_PORT:-}" >>"$SMOKE_FAKE_ENV_LOG"
}
log_call "$@"
log_env

if [ "$#" -eq 1 ] && [ "$1" = "info" ]; then
  exit 0
fi
if [ "$#" -eq 2 ] && [ "$1" = "compose" ] && [ "$2" = "version" ]; then
  exit 0
fi
if [ "$#" -eq 3 ] && [ "$1" = "image" ] && [ "$2" = "rm" ]; then
  if [ "${SMOKE_FAKE_IMAGE_RM_FAIL:-0}" = "1" ]; then
    printf 'fake docker: image removal failed\n' >&2
    exit 1
  fi
  exit 0
fi
if [ "$#" -ge 1 ] && [ "$1" = "compose" ]; then
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --project-name|--project-directory|--env-file|--file)
        shift 2
        ;;
      *)
        break
        ;;
    esac
  done
  sub=${1:-}
  [ "$#" -gt 0 ] && shift
  case "$sub" in
    config|run|up)
      exit 0
      ;;
    build)
      if [ "${SMOKE_FAKE_BUILD_FAIL:-0}" = "1" ]; then
        printf 'fake docker: build failed\n' >&2
        exit 1
      fi
      exit 0
      ;;
    down)
      if [ "${SMOKE_FAKE_DOWN_FAIL:-0}" = "1" ]; then
        printf 'fake docker: project removal failed\n' >&2
        exit 1
      fi
      exit 0
      ;;
    logs)
      exit 0
      ;;
    exec)
      ;;
    *)
      printf 'fake docker: unexpected compose subcommand %s\n' "$sub" >&2
      exit 9
      ;;
  esac
  if [ "${1:-}" = "-T" ]; then
    shift
  fi
  if [ "${1:-}" = "review" ]; then
    shift
  fi
  command=${1:-}
  [ "$#" -gt 0 ] && shift
  case "$command" in
    sh)
      exit 0
      ;;
    cat)
      printf 'tribunal-smoke-marker\n'
      exit 0
      ;;
    /app/tribunalctl|/app/reviewctl)
      printf '{"projects":[]}\n'
      exit 0
      ;;
    curl)
      url=""
      want_status=0
      for arg in "$@"; do
        case "$arg" in
          -w) want_status=1 ;;
          http://*|https://*) url=$arg ;;
        esac
      done
      if [ "$want_status" = 1 ]; then
        case "$url" in
          *"/healthz"|*"/readyz") printf '200' ;;
          *"/api/v1/projects") printf '401' ;;
          *"/auth/login") printf '503' ;;
          *) printf 'fake docker: unexpected status URL %s\n' "$url" >&2; exit 9 ;;
        esac
      else
        case "$url" in
          */) printf '<div id="root"></div>' ;;
          *"/api/v1/mcp-servers?scope=platform")
            printf '{"servers":[{"id":"srv-codebase-memory","scope":"platform","name":"codebase-memory","enabled":true}],"next_cursor":""}'
            ;;
          *"/api/v1/mcp-servers/srv-codebase-memory/test")
            printf '{"tools":[{"name":"list_projects"},{"name":"index_status"},{"name":"search_graph"},{"name":"trace_path"},{"name":"get_code_snippet"},{"name":"query_graph"},{"name":"get_graph_schema"},{"name":"get_file_outline"},{"name":"get_architecture"},{"name":"search_code"}'
            if [ "${SMOKE_FAKE_MCP_MISSING_TOOL:-0}" != "1" ]; then
              printf ',{"name":"check_index_coverage"}'
            fi
            if [ "${SMOKE_FAKE_MCP_WRITE_TOOL:-0}" = "1" ]; then
              printf ',{"name":"delete_project"}'
            fi
            printf ']}'
            ;;
          *) printf 'fake docker: unexpected fetch URL %s\n' "$url" >&2; exit 9 ;;
        esac
      fi
      exit 0
      ;;
    *)
      printf 'fake docker: unexpected exec command %s\n' "$command" >&2
      exit 9
      ;;
  esac
fi

printf 'fake docker: unexpected invocation\n' >&2
exit 9
`

const fakeRM = `#!/usr/bin/env bash
set -u

log_call() {
  local arg
  for arg in "$@"; do
    printf '%s\t' "$arg"
  done >>"$SMOKE_FAKE_CALL_LOG"
  printf '\n' >>"$SMOKE_FAKE_CALL_LOG"
}
log_call "$@"

path=""
for arg in "$@"; do
  case "$arg" in
    -*) ;;
    *)
      case "$arg" in
        "$SMOKE_FAKE_TMP_ROOT"/*) path=$arg ;;
        *)
          printf 'fake rm: refused path outside test TMPDIR: %s\n' "$arg" >&2
          exit 99
          ;;
      esac
      ;;
  esac
done
if [ -z "$path" ]; then
  printf 'fake rm: no target path\n' >&2
  exit 99
fi
if [ "${SMOKE_FAKE_RM_FAIL:-0}" = "1" ]; then
  printf 'fake rm: removal failed\n' >&2
  exit 1
fi
exec /bin/rm "$@"
`
