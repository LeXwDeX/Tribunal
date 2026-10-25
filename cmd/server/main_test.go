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
	t.Setenv("CRC_SESSION_KEY", "")
	generated, err := sessionKeyFromEnv()
	if err != nil || len(generated) != 32 {
		t.Fatalf("generated key = %d bytes, err=%v", len(generated), err)
	}

	raw := []byte("0123456789abcdef0123456789abcdef")
	t.Setenv("CRC_SESSION_KEY", base64.StdEncoding.EncodeToString(raw))
	key, err := sessionKeyFromEnv()
	if err != nil || !bytes.Equal(key, raw) {
		t.Fatalf("decoded key = %v, err=%v", key, err)
	}

	t.Setenv("CRC_SESSION_KEY", base64.RawStdEncoding.EncodeToString(raw))
	if _, err := sessionKeyFromEnv(); err != nil {
		t.Fatalf("raw base64 key: %v", err)
	}

	t.Setenv("CRC_SESSION_KEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := sessionKeyFromEnv(); err == nil {
		t.Fatal("short session key accepted")
	}
	t.Setenv("CRC_SESSION_KEY", "not-base64!!!")
	if _, err := sessionKeyFromEnv(); err == nil {
		t.Fatal("invalid base64 accepted")
	}
}

func TestEphemeralSessionKeyWarns(t *testing.T) {
	t.Setenv("CRC_SESSION_KEY", "")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	warnEphemeralSessionKey(logger)
	if !strings.Contains(logs.String(), "CRC_SESSION_KEY") || !strings.Contains(logs.String(), "restart") {
		t.Fatalf("warning = %q", logs.String())
	}

	logs.Reset()
	t.Setenv("CRC_SESSION_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	warnEphemeralSessionKey(logger)
	if logs.Len() != 0 {
		t.Fatalf("pinned key warned: %q", logs.String())
	}
}

func TestSecureCookies(t *testing.T) {
	if !secureCookies("https://review.example.com") {
		t.Fatal("https public URL did not enable secure cookies")
	}
	if secureCookies("http://review.example.com") || secureCookies("") {
		t.Fatal("non-https public URL enabled secure cookies")
	}
}
