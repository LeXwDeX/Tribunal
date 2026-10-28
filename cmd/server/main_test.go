package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

func TestParseOptions(t *testing.T) {
	opts, err := parseOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.configs) != 1 || opts.configs[0] != "config/server.yaml" {
		t.Fatalf("default configs = %v", opts.configs)
	}
	opts, err = parseOptions([]string{"--config", "a.yaml", "--config", "b.yaml", "--check-config"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(opts.configs, ",") != "a.yaml,b.yaml" || !opts.checkConfig {
		t.Fatalf("opts = %+v", opts)
	}
	if _, err := parseOptions([]string{"--nope"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if _, err := parseOptions([]string{"extra"}, &bytes.Buffer{}); err == nil {
		t.Fatal("positional argument accepted")
	}
}

func TestSQLiteFilePath(t *testing.T) {
	tests := []struct {
		dsn    string
		path   string
		memory bool
		err    bool
	}{
		{dsn: ":memory:", memory: true},
		{dsn: "file::memory:", memory: true},
		{dsn: "file:data/review.db", path: "data/review.db"},
		{dsn: "file:data/review.db?cache=shared", path: "data/review.db"},
		{dsn: "file:data/review.db?mode=memory&cache=shared", memory: true},
		{dsn: "file:///tmp/review.db", path: "/tmp/review.db"},
		{dsn: "file:/tmp/review.db", path: "/tmp/review.db"},
		{dsn: "/tmp/review.db", path: "/tmp/review.db"},
		{dsn: "relative/review.db", path: "relative/review.db"},
		{dsn: "file://remote/path.db", err: true},
		{dsn: "", err: true},
		{dsn: "file:data/x.db?bad=%zz", err: true},
	}
	for _, tt := range tests {
		path, memory, err := sqliteFilePath(tt.dsn)
		if (err != nil) != tt.err {
			t.Errorf("sqliteFilePath(%q) error = %v, want err=%v", tt.dsn, err, tt.err)
			continue
		}
		if err != nil {
			continue
		}
		if path != tt.path || memory != tt.memory {
			t.Errorf("sqliteFilePath(%q) = %q, %v", tt.dsn, path, memory)
		}
	}
}

func TestPrepareSQLiteDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "data")
	dsn := "file:" + filepath.Join(root, "review.db") + "?cache=shared"
	if err := prepareSQLiteDirectory(dsn); err != nil {
		t.Fatalf("prepareSQLiteDirectory: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("database directory missing: %v", err)
	}
	if err := prepareSQLiteDirectory(":memory:"); err != nil {
		t.Fatalf("memory dsn: %v", err)
	}
}

func TestDecodeMasterKey(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	t.Setenv("TEST_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	got, err := decodeMasterKey("TEST_MASTER_KEY")
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("decode = %v, %v", got, err)
	}
	t.Setenv("TEST_MASTER_KEY", base64.RawStdEncoding.EncodeToString(key))
	if _, err := decodeMasterKey("TEST_MASTER_KEY"); err != nil {
		t.Fatalf("raw base64 rejected: %v", err)
	}
	t.Setenv("TEST_MASTER_KEY", "short")
	if _, err := decodeMasterKey("TEST_MASTER_KEY"); err == nil {
		t.Fatal("short key accepted")
	}
	t.Setenv("TEST_MASTER_KEY", "")
	if _, err := decodeMasterKey("TEST_MASTER_KEY"); err == nil {
		t.Fatal("empty key accepted")
	}
}

func TestNewLoggerEnvironment(t *testing.T) {
	t.Setenv("REVIEW_LOG_LEVEL", "debug")
	t.Setenv("REVIEW_LOG_FORMAT", "text")
	if _, err := newLogger(&bytes.Buffer{}); err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	t.Setenv("REVIEW_LOG_LEVEL", "verbose")
	if _, err := newLogger(&bytes.Buffer{}); err == nil {
		t.Fatal("invalid level accepted")
	}
	t.Setenv("REVIEW_LOG_LEVEL", "info")
	t.Setenv("REVIEW_LOG_FORMAT", "yaml")
	if _, err := newLogger(&bytes.Buffer{}); err == nil {
		t.Fatal("invalid format accepted")
	}
}

type fakeRuntimeCloser struct {
	err   error
	calls int
}

func (c *fakeRuntimeCloser) Close() error {
	c.calls++
	return c.err
}

func TestCloseRuntime(t *testing.T) {
	const (
		fakeToken     = "fake-secret-token-do-not-log"
		fakePath      = "/fake/private/runtime-path"
		fakeAddress   = "https://fake-mcp.invalid"
		fakeMCPOutput = "fake-mcp-output-do-not-log"
	)
	detail := fakeToken + " " + fakePath + " " + fakeAddress + " " + fakeMCPOutput
	runErr := errors.New("existing run error")
	tests := []struct {
		name      string
		closeErr  error
		runErr    error
		wantClass string
	}{
		{name: "success"},
		{name: "close error on normal stop", closeErr: errors.New(detail), wantClass: "internal"},
		{name: "close timeout on failed run", closeErr: errors.Join(errors.New(detail), context.DeadlineExceeded), runErr: runErr, wantClass: "timeout"},
		{name: "canceled close", closeErr: fmt.Errorf("%s: %w", detail, context.Canceled), wantClass: "canceled"},
	}
	for _, format := range []string{"json", "text"} {
		for _, tt := range tests {
			t.Run(format+"/"+tt.name, func(t *testing.T) {
				t.Setenv("REVIEW_LOG_LEVEL", "error")
				t.Setenv("REVIEW_LOG_FORMAT", format)
				var logs bytes.Buffer
				logger, err := newLogger(&logs)
				if err != nil {
					t.Fatal(err)
				}
				closer := &fakeRuntimeCloser{err: tt.closeErr}
				got := func() error {
					defer closeRuntime(closer, logger)
					return tt.runErr
				}()
				if got != tt.runErr {
					t.Fatalf("run error changed: got %v, want %v", got, tt.runErr)
				}
				if closer.calls != 1 {
					t.Fatalf("Close called %d times, want once", closer.calls)
				}
				out := logs.String()
				if tt.wantClass == "" {
					if out != "" {
						t.Fatalf("successful Close logged %q", out)
					}
					return
				}
				for _, value := range []string{fakeToken, fakePath, fakeAddress, fakeMCPOutput} {
					if strings.Contains(out, value) {
						t.Fatalf("Close log leaked %q: %q", value, out)
					}
				}
				if format == "json" {
					var record map[string]any
					if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
						t.Fatalf("expected one JSON log record: %v; got %q", err, out)
					}
					if len(record) != 5 || record["level"] != "ERROR" || record["msg"] != "runtime close failed" || record["error_class"] != tt.wantClass || record["service"] != "review" || record["time"] == nil {
						t.Fatalf("unexpected Close log record: %v", record)
					}
				} else {
					for _, want := range []string{"level=ERROR", `msg="runtime close failed"`, "error_class=" + tt.wantClass, "service=review"} {
						if !strings.Contains(out, want) {
							t.Fatalf("Close log %q missing %q", out, want)
						}
					}
					if strings.Count(out, "\n") != 1 {
						t.Fatalf("expected one text log record, got %q", out)
					}
				}
			})
		}
	}
}

func TestCheckConfig(t *testing.T) {
	source, err := os.ReadFile("../../config/server.example.yaml")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("REVIEW_MASTER_KEY", key)
	t.Setenv("REVIEW_ADMIN_TOKEN", "abcdefghijklmnopqrstuvwxyz012345")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := run(context.Background(), []string{"--config", path, "--check-config"}, stdout, stderr); err != nil {
		t.Fatalf("run: %v, stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout.String(), "configuration OK") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestCheckConfigRequiresSecrets(t *testing.T) {
	source, err := os.ReadFile("../../config/server.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEW_MASTER_KEY", "")
	t.Setenv("REVIEW_ADMIN_TOKEN", "")
	if err := run(context.Background(), []string{"--config", path, "--check-config"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("check-config accepted missing secrets")
	}
}

func TestReadHeaderTimeout(t *testing.T) {
	if got := readHeaderTimeout(0); got != 10*time.Second {
		t.Errorf("zero timeout = %v", got)
	}
	if got := readHeaderTimeout(3 * time.Second); got != 3*time.Second {
		t.Errorf("short timeout = %v", got)
	}
	if got := readHeaderTimeout(time.Minute); got != 10*time.Second {
		t.Errorf("long timeout = %v", got)
	}
}

func TestBootstrapToken(t *testing.T) {
	t.Setenv("TEST_ADMIN_TOKEN", strings.Repeat("a", 32))
	if _, err := bootstrapToken("TEST_ADMIN_TOKEN"); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	t.Setenv("TEST_ADMIN_TOKEN", "short")
	if _, err := bootstrapToken("TEST_ADMIN_TOKEN"); err == nil {
		t.Fatal("short token accepted")
	}
}

// writeRunConfig writes a runnable server config whose database and
// workspaces live under dir, and sets the required environment.
func writeRunConfig(t *testing.T, dir string, port int) string {
	t.Helper()
	source, err := os.ReadFile("../../config/server.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	config := strings.ReplaceAll(string(source), ":8080", fmt.Sprintf("127.0.0.1:%d", port))
	config = strings.ReplaceAll(config, "http://localhost:8080", fmt.Sprintf("http://127.0.0.1:%d", port))
	config = strings.ReplaceAll(config, "file:data/review.db", "file:"+filepath.Join(dir, "review.db"))
	config = strings.ReplaceAll(config, `root: "data/workspaces"`, fmt.Sprintf("root: %q", filepath.Join(dir, "workspaces")))
	config = strings.ReplaceAll(config, "https://gitlab.example.internal", "http://127.0.0.1:1")
	path := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("REVIEW_MASTER_KEY", key)
	t.Setenv("REVIEW_ADMIN_TOKEN", strings.Repeat("a", 32))
	t.Setenv("REVIEW_LOG_FORMAT", "text")
	t.Setenv("REVIEW_LOG_LEVEL", "info")
	return path
}

func TestRunRefusesLockedDataDirectory(t *testing.T) {
	dir := t.TempDir()
	path := writeRunConfig(t, dir, 1)
	held, err := sqlite.LockDataDir(dir)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer held.Release()
	stderr := &bytes.Buffer{}
	err = run(context.Background(), []string{"--config", path}, &bytes.Buffer{}, stderr)
	if err == nil || !strings.Contains(err.Error(), "locked by another Tribunal instance") {
		t.Fatalf("run error = %v, want data directory lock failure", err)
	}
	if !strings.Contains(stderr.String(), filepath.Join(dir, sqlite.LockFileName)) {
		t.Fatalf("log does not name the lock file: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "review.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database opened despite lock: %v", err)
	}
}

func TestLockSQLiteDirectory(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "review.db") + "?cache=shared"
	first, err := lockSQLiteDirectory(dsn)
	if err != nil || first == nil {
		t.Fatalf("first lock = %v, %v", first, err)
	}
	if _, err := lockSQLiteDirectory(dsn); !errors.Is(err, sqlite.ErrDataDirLocked) {
		t.Fatalf("second lock error = %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if lock, err := lockSQLiteDirectory(":memory:"); err != nil || lock != nil {
		t.Fatalf("memory dsn lock = %v, %v", lock, err)
	}
}

func TestRunStartsAndStopsCleanly(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	path := writeRunConfig(t, t.TempDir(), port)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--config", path}, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		cancel()
		t.Fatalf("server did not become ready: %v", <-done)
	}
	resp, err := http.Get(base + "/api/v1/projects")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", resp.StatusCode)
	}
	http.DefaultClient.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestSessionKeyFromEnv(t *testing.T) {
	legacy := []byte("0123456789abcdef0123456789abcdef")
	tribunal := []byte("abcdef0123456789abcdef0123456789")
	encoded := func(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

	t.Run("generated when both are empty", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "")
		t.Setenv("CRC_SESSION_KEY", "")
		generated, err := sessionKeyFromEnv()
		if err != nil || len(generated) != 32 {
			t.Fatalf("generated key = %d bytes, err=%v", len(generated), err)
		}
	})

	t.Run("tribunal key", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", encoded(tribunal))
		t.Setenv("CRC_SESSION_KEY", "")
		key, err := sessionKeyFromEnv()
		if err != nil || !bytes.Equal(key, tribunal) {
			t.Fatalf("decoded key = %v, err=%v", key, err)
		}
	})

	t.Run("legacy key", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "")
		t.Setenv("CRC_SESSION_KEY", encoded(legacy))
		key, err := sessionKeyFromEnv()
		if err != nil || !bytes.Equal(key, legacy) {
			t.Fatalf("decoded key = %v, err=%v", key, err)
		}
	})

	t.Run("tribunal key wins", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", encoded(tribunal))
		t.Setenv("CRC_SESSION_KEY", encoded(legacy))
		key, err := sessionKeyFromEnv()
		if err != nil || !bytes.Equal(key, tribunal) {
			t.Fatalf("decoded key = %v, err=%v", key, err)
		}
	})

	t.Run("invalid tribunal key does not fall back", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "not-base64!!!")
		t.Setenv("CRC_SESSION_KEY", encoded(legacy))
		_, err := sessionKeyFromEnv()
		if err == nil || !strings.Contains(err.Error(), "TRIBUNAL_SESSION_KEY") {
			t.Fatalf("error = %v, want TRIBUNAL_SESSION_KEY", err)
		}
	})

	t.Run("short tribunal key does not fall back", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", encoded([]byte("too-short")))
		t.Setenv("CRC_SESSION_KEY", encoded(legacy))
		if _, err := sessionKeyFromEnv(); err == nil {
			t.Fatal("short session key accepted")
		}
	})

	t.Run("unset tribunal key falls back", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "")
		if err := os.Unsetenv("TRIBUNAL_SESSION_KEY"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CRC_SESSION_KEY", encoded(legacy))
		key, err := sessionKeyFromEnv()
		if err != nil || !bytes.Equal(key, legacy) {
			t.Fatalf("decoded key = %v, err=%v", key, err)
		}
	})

	t.Run("whitespace tribunal key does not fall back", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "  ")
		t.Setenv("CRC_SESSION_KEY", encoded(legacy))
		key, err := sessionKeyFromEnv()
		if key != nil || err == nil || !strings.Contains(err.Error(), "TRIBUNAL_SESSION_KEY") {
			t.Fatalf("key = %v, error = %v, want TRIBUNAL_SESSION_KEY error", key, err)
		}
	})

	t.Run("whitespace tribunal key does not generate", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "  ")
		t.Setenv("CRC_SESSION_KEY", "")
		key, err := sessionKeyFromEnv()
		if key != nil || err == nil || !strings.Contains(err.Error(), "TRIBUNAL_SESSION_KEY") {
			t.Fatalf("key = %v, error = %v, want TRIBUNAL_SESSION_KEY error", key, err)
		}
	})

	t.Run("legacy whitespace only still generates", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "")
		t.Setenv("CRC_SESSION_KEY", "\t")
		generated, err := sessionKeyFromEnv()
		if err != nil || len(generated) != 32 {
			t.Fatalf("generated key = %d bytes, err=%v", len(generated), err)
		}
	})

	t.Run("padded tribunal key", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "  "+encoded(tribunal)+"  ")
		t.Setenv("CRC_SESSION_KEY", encoded(legacy))
		key, err := sessionKeyFromEnv()
		if err != nil || !bytes.Equal(key, tribunal) {
			t.Fatalf("decoded key = %v, err=%v", key, err)
		}
	})

	t.Run("raw base64 tribunal key", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", base64.RawStdEncoding.EncodeToString(tribunal))
		t.Setenv("CRC_SESSION_KEY", "")
		if _, err := sessionKeyFromEnv(); err != nil {
			t.Fatalf("raw base64 key: %v", err)
		}
	})

	t.Run("invalid legacy key", func(t *testing.T) {
		t.Setenv("TRIBUNAL_SESSION_KEY", "")
		t.Setenv("CRC_SESSION_KEY", "not-base64!!!")
		_, err := sessionKeyFromEnv()
		if err == nil || !strings.Contains(err.Error(), "CRC_SESSION_KEY") {
			t.Fatalf("error = %v, want CRC_SESSION_KEY", err)
		}
	})
}

func TestEphemeralSessionKeyWarns(t *testing.T) {
	t.Setenv("TRIBUNAL_SESSION_KEY", "")
	t.Setenv("CRC_SESSION_KEY", "")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	warnEphemeralSessionKey(logger)
	if !strings.Contains(logs.String(), "TRIBUNAL_SESSION_KEY") || !strings.Contains(logs.String(), "restart") {
		t.Fatalf("warning = %q", logs.String())
	}

	logs.Reset()
	t.Setenv("TRIBUNAL_SESSION_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	warnEphemeralSessionKey(logger)
	if logs.Len() != 0 {
		t.Fatalf("pinned key warned: %q", logs.String())
	}

	logs.Reset()
	t.Setenv("TRIBUNAL_SESSION_KEY", "")
	t.Setenv("CRC_SESSION_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	warnEphemeralSessionKey(logger)
	if logs.Len() != 0 {
		t.Fatalf("legacy pinned key warned: %q", logs.String())
	}

	logs.Reset()
	t.Setenv("TRIBUNAL_SESSION_KEY", "  ")
	t.Setenv("CRC_SESSION_KEY", "")
	warnEphemeralSessionKey(logger)
	if logs.Len() != 0 {
		t.Fatalf("invalid whitespace-only key warned as ephemeral: %q", logs.String())
	}

	logs.Reset()
	t.Setenv("TRIBUNAL_SESSION_KEY", "  ")
	t.Setenv("CRC_SESSION_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	warnEphemeralSessionKey(logger)
	if logs.Len() != 0 {
		t.Fatalf("invalid whitespace-only key with legacy key warned as ephemeral: %q", logs.String())
	}

	logs.Reset()
	t.Setenv("TRIBUNAL_SESSION_KEY", "")
	t.Setenv("CRC_SESSION_KEY", "\t")
	warnEphemeralSessionKey(logger)
	if !strings.Contains(logs.String(), "TRIBUNAL_SESSION_KEY") || !strings.Contains(logs.String(), "restart") {
		t.Fatalf("warning for generated key = %q", logs.String())
	}
}

func TestDeprecatedEnvironmentWarnings(t *testing.T) {
	const (
		legacyURL    = "https://legacy.invalid/do-not-log"
		tribunalURL  = "https://tribunal.invalid"
		legacyPort   = "45678"
		tribunalPort = "56789"
	)
	source, err := os.ReadFile("../../config/server.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEW_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	t.Setenv("REVIEW_ADMIN_TOKEN", strings.Repeat("a", 32))
	t.Setenv("REVIEW_LOG_FORMAT", "text")
	t.Setenv("REVIEW_LOG_LEVEL", "info")

	runCheck := func(t *testing.T) string {
		t.Helper()
		stderr := &bytes.Buffer{}
		if err := run(context.Background(), []string{"--config", path, "--check-config"}, &bytes.Buffer{}, stderr); err != nil {
			t.Fatalf("run: %v", err)
		}
		return stderr.String()
	}

	t.Run("legacy names warn once without values", func(t *testing.T) {
		t.Setenv("TRIBUNAL_PUBLIC_URL", tribunalURL)
		t.Setenv("CRC_PUBLIC_URL", legacyURL)
		t.Setenv("TRIBUNAL_HTTP_PORT", tribunalPort)
		t.Setenv("CRC_HTTP_PORT", legacyPort)
		t.Setenv("TRIBUNAL_SESSION_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
		t.Setenv("CRC_SESSION_KEY", "do-not-log-session")
		logs := runCheck(t)
		for _, name := range []string{"CRC_PUBLIC_URL", "CRC_HTTP_PORT", "CRC_SESSION_KEY"} {
			if strings.Count(logs, "legacy="+name) != 1 {
				t.Fatalf("warning count for %s in %q, want one", name, logs)
			}
		}
		if !strings.Contains(logs, "configured") {
			t.Fatalf("warning does not say the legacy name is configured: %q", logs)
		}
		for _, value := range []string{legacyURL, legacyPort, "do-not-log-session"} {
			if strings.Contains(logs, value) {
				t.Fatalf("warning leaked configured value %q: %q", value, logs)
			}
		}
	})

	t.Run("legacy session key only warns", func(t *testing.T) {
		const sessionValue = "session-value-must-not-appear-0123456789"
		t.Setenv("TRIBUNAL_PUBLIC_URL", tribunalURL)
		t.Setenv("CRC_PUBLIC_URL", "")
		t.Setenv("TRIBUNAL_HTTP_PORT", tribunalPort)
		t.Setenv("CRC_HTTP_PORT", "")
		t.Setenv("TRIBUNAL_SESSION_KEY", "")
		t.Setenv("CRC_SESSION_KEY", sessionValue)
		logs := runCheck(t)
		if strings.Count(logs, "legacy=CRC_SESSION_KEY") != 1 {
			t.Fatalf("CRC_SESSION_KEY warning count in %q, want one", logs)
		}
		if !strings.Contains(logs, "prefer TRIBUNAL_SESSION_KEY") {
			t.Fatalf("warning does not name TRIBUNAL_SESSION_KEY: %q", logs)
		}
		if strings.Contains(logs, sessionValue) {
			t.Fatalf("warning leaked the session key value: %q", logs)
		}
	})

	t.Run("tribunal names do not warn", func(t *testing.T) {
		t.Setenv("TRIBUNAL_PUBLIC_URL", tribunalURL)
		t.Setenv("CRC_PUBLIC_URL", "")
		t.Setenv("TRIBUNAL_HTTP_PORT", tribunalPort)
		t.Setenv("CRC_HTTP_PORT", "")
		t.Setenv("TRIBUNAL_SESSION_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
		t.Setenv("CRC_SESSION_KEY", "")
		logs := runCheck(t)
		for _, name := range []string{"CRC_PUBLIC_URL", "CRC_HTTP_PORT", "CRC_SESSION_KEY"} {
			if strings.Contains(logs, "legacy="+name) {
				t.Fatalf("unexpected warning for %s: %q", name, logs)
			}
		}
	})
}

func TestSecureCookies(t *testing.T) {
	if !secureCookies("https://review.example.com") {
		t.Fatal("https public URL did not enable secure cookies")
	}
	if secureCookies("http://review.example.com") || secureCookies("") {
		t.Fatal("non-https public URL enabled secure cookies")
	}
}

// serveStream starts srv with a handler that holds one streaming response
// open until release or request cancellation, and returns once the client has
// the response headers.
func serveStream(t *testing.T, srv *http.Server, release <-chan struct{}, ended chan<- struct{}) {
	t.Helper()
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-served
	})
	resp, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
}

func TestShutdownHTTPServerNotifiesStreams(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	srv := newHTTPServer(config.ServerConfig{}, nil, func() { once.Do(func() { close(release) }) })
	ended := make(chan struct{})
	serveStream(t, srv, release, ended)

	start := time.Now()
	if err := shutdownHTTPServer(srv, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown waited %s for an open stream", elapsed)
	}
	<-ended
}

func TestShutdownHTTPServerClosesAfterTimeout(t *testing.T) {
	srv := newHTTPServer(config.ServerConfig{}, nil, nil)
	ended := make(chan struct{})
	serveStream(t, srv, nil, ended)

	var logs bytes.Buffer
	if err := shutdownHTTPServer(srv, 50*time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatalf("timed-out shutdown should still stop cleanly: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end the hanging request")
	}
	if !strings.Contains(logs.String(), "HTTP shutdown timed out") {
		t.Fatalf("missing timeout warning: %q", logs.String())
	}
}

func TestStopRecoveryWaitsForRecoveryToReturn(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})
	stop, _ := startRecovery(context.Background(), func(ctx context.Context) error {
		defer close(done)
		close(started)
		<-ctx.Done()
		// A publication finishing its bookkeeping after cancellation.
		time.Sleep(50 * time.Millisecond)
		return ctx.Err()
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	<-started
	stop(5 * time.Second)
	select {
	case <-done:
	default:
		t.Fatal("stopRecovery returned before startup recovery finished")
	}
}

func TestStopRecoveryIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	stop, _ := startRecovery(context.Background(), func(context.Context) error {
		close(started)
		<-release // ignores cancellation
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	<-started
	begin := time.Now()
	stop(50 * time.Millisecond)
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("stopRecovery waited %v past its timeout", elapsed)
	}
}

type recordedCloser struct {
	name   string
	events chan<- string
}

func (c recordedCloser) Close() error   { c.events <- c.name; return nil }
func (c recordedCloser) Release() error { c.events <- c.name; return nil }

func expectEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	select {
	case got := <-events:
		if got != want {
			t.Fatalf("event = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("missing event %q", want)
	}
}

func expectNoEvent(t *testing.T, events <-chan string) {
	t.Helper()
	select {
	case got := <-events:
		t.Fatalf("unexpected event %q", got)
	default:
	}
}

func finishBounded(t *testing.T, owner *resourceOwner) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); owner.Finish() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Finish did not return while a user remained active")
	}
}

func TestResourceOwnerRecoveryTimeout(t *testing.T) {
	events := make(chan string, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	started := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stop, done := startRecovery(context.Background(), func(context.Context) error {
		close(started)
		<-release
		return nil
	}, logger)
	<-started
	stop(10 * time.Millisecond)
	owner := &resourceOwner{runtime: recordedCloser{"runtime", events}, db: recordedCloser{"db", events}, lock: recordedCloser{"lock", events}, logger: logger, recoveryDone: done}
	finishBounded(t, owner)
	expectEvent(t, events, "runtime")
	expectNoEvent(t, events)
	releaseOnce.Do(func() { close(release) })
	expectEvent(t, events, "db")
	expectEvent(t, events, "lock")
	expectNoEvent(t, events)
}

func TestResourceOwnerWaitsForForcedHTTPHandler(t *testing.T) {
	events := make(chan string, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	entered := make(chan struct{})
	tracker := newHTTPTracker(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	srv := newHTTPServer(config.ServerConfig{}, tracker, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close(); <-served })
	resp, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := shutdownHTTPServer(srv, 10*time.Millisecond, logger); err != nil {
		t.Fatal(err)
	}
	tracker.seal()
	owner := &resourceOwner{runtime: recordedCloser{"runtime", events}, db: recordedCloser{"db", events}, lock: recordedCloser{"lock", events}, logger: logger, tracker: tracker}
	finishBounded(t, owner)
	expectEvent(t, events, "runtime")
	expectNoEvent(t, events)
	releaseOnce.Do(func() { close(release) })
	expectEvent(t, events, "db")
	expectEvent(t, events, "lock")
	expectNoEvent(t, events)
}

func TestResourceOwnerSealsTrackerOnPanicPath(t *testing.T) {
	events := make(chan string, 4)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, enteredOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	tracker := newHTTPTracker(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}))
	go tracker.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered
	owner := &resourceOwner{runtime: recordedCloser{"runtime", events}, db: recordedCloser{"db", events}, lock: recordedCloser{"lock", events}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), tracker: tracker}
	finishBounded(t, owner)
	expectEvent(t, events, "runtime")
	expectNoEvent(t, events)
	response := httptest.NewRecorder()
	rejected := make(chan struct{})
	go func() {
		defer close(rejected)
		tracker.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	select {
	case <-rejected:
	case <-time.After(2 * time.Second):
		t.Fatal("tracker accepted a handler after Finish")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("late handler status = %d", response.Code)
	}
	releaseOnce.Do(func() { close(release) })
	expectEvent(t, events, "db")
	expectEvent(t, events, "lock")
	owner.Finish()
	expectNoEvent(t, events)
}

func TestResourceOwnerWaitsForWorker(t *testing.T) {
	events := make(chan string, 4)
	workerFinished := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(workerFinished) }) })
	owner := &resourceOwner{db: recordedCloser{"db", events}, lock: recordedCloser{"lock", events}, workerFinished: workerFinished}
	finishBounded(t, owner)
	expectNoEvent(t, events)
	releaseOnce.Do(func() { close(workerFinished) })
	expectEvent(t, events, "db")
	expectEvent(t, events, "lock")
	owner.Finish()
	expectNoEvent(t, events)
}

func TestResourceOwnerConcurrentFinish(t *testing.T) {
	events := make(chan string, 20)
	done := make(chan struct{})
	var doneOnce sync.Once
	t.Cleanup(func() { doneOnce.Do(func() { close(done) }) })
	owner := &resourceOwner{runtime: recordedCloser{"runtime", events}, db: recordedCloser{"db", events}, lock: recordedCloser{"lock", events}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), backgroundDone: done}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); owner.Finish() }()
	}
	finished := make(chan struct{})
	go func() { defer close(finished); wg.Wait() }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Finish calls did not return")
	}
	expectEvent(t, events, "runtime")
	expectNoEvent(t, events)
	doneOnce.Do(func() { close(done) })
	expectEvent(t, events, "db")
	expectEvent(t, events, "lock")
	owner.Finish()
	expectNoEvent(t, events)
}

func TestHTTPTrackerSealRejectsLateHandlers(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	tracker := newHTTPTracker(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tracker.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-entered
	done := tracker.seal()
	if tracker.seal() != done {
		t.Fatal("seal returned a different completion signal")
	}
	select {
	case <-done:
		t.Fatal("tracker finished while handler was active")
	default:
	}
	response := httptest.NewRecorder()
	tracker.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("late handler status = %d", response.Code)
	}
	releaseOnce.Do(func() { close(release) })
	<-finished
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tracker did not finish")
	}
}

func TestResourceOwnerInitializationFailureIsSynchronous(t *testing.T) {
	for _, hasDB := range []bool{false, true} {
		events := make(chan string, 3)
		owner := &resourceOwner{lock: recordedCloser{"lock", events}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if hasDB {
			owner.db = recordedCloser{"db", events}
		}
		owner.Finish()
		want := 1
		if hasDB {
			want++
		}
		if len(events) != want {
			t.Fatalf("initialization cleanup was not synchronous: got %d events, want %d", len(events), want)
		}
		if hasDB {
			expectEvent(t, events, "db")
		}
		expectEvent(t, events, "lock")
		expectNoEvent(t, events)
	}
}

func TestWarnUsernameBootstrapAdmins(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	lookup := func(_ context.Context, username string) (int64, bool, error) {
		switch username {
		case "alice":
			return 42, true, nil
		case "carol":
			return 0, false, errors.New("storage unavailable for carol")
		}
		return 0, false, nil
	}
	warnUsernameBootstrapAdmins(context.Background(), logger, []string{"id:7", " Alice ", "bob", "carol"}, lookup)
	out := logs.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want one warning per username entry, got %d: %q", len(lines), out)
	}
	for _, name := range []string{"alice", "Alice", "bob", "carol"} {
		if strings.Contains(out, name) {
			t.Fatalf("warning leaks username %q: %q", name, out)
		}
	}
	for i, want := range []string{"index=1", "index=2", "index=3"} {
		if !strings.Contains(lines[i], "level=WARN") || !strings.Contains(lines[i], want) || !strings.Contains(lines[i], "deprecated") {
			t.Fatalf("line %d = %q, want WARN with %s", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[0], "replacement=id:42") {
		t.Fatalf("bound entry has no replacement: %q", lines[0])
	}
	if strings.Contains(lines[1], "replacement=") || strings.Contains(lines[2], "replacement=") {
		t.Fatalf("unbound entry got a replacement: %q", out)
	}

	logs.Reset()
	warnUsernameBootstrapAdmins(context.Background(), logger, []string{"id:7", "ID: 8"}, lookup)
	if logs.Len() != 0 {
		t.Fatalf("id-only entries warned: %q", logs.String())
	}
}
