package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRunStartsAndStopsCleanly(t *testing.T) {
	source, err := os.ReadFile("../../config/server.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	dir := t.TempDir()
	config := strings.ReplaceAll(string(source), ":8080", fmt.Sprintf("127.0.0.1:%d", port))
	config = strings.ReplaceAll(config, "http://localhost:8080", fmt.Sprintf("http://127.0.0.1:%d", port))
	config = strings.ReplaceAll(config, "file:data/review.db", "file:"+filepath.Join(dir, "review.db"))
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
