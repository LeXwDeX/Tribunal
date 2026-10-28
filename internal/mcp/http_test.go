package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newHTTPMCP(t *testing.T, token string) *httptest.Server {
	t.Helper()
	server := testServer()
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			PropagateRequestCancellation: true,
		},
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
	serverHTTP := httptest.NewServer(handler)
	t.Cleanup(serverHTTP.Close)
	return serverHTTP
}

func TestHTTPIncompleteToolResponses(t *testing.T) {
	for _, result := range []struct {
		name, raw   string
		unavailable bool
	}{
		// SDK decodes null as an empty result, indistinguishable from empty content.
		{"null", `null`, false},
		{"needs input", `{"resultType":"input_required","inputRequests":{}}`, true},
	} {
		t.Run(result.name, func(t *testing.T) {
			server := testServer()
			inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var req struct {
					Method string          `json:"method"`
					ID     json.RawMessage `json:"id"`
				}
				if json.Unmarshal(body, &req) == nil && req.Method == "tools/call" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result.raw + `}`))
					return
				}
				inner.ServeHTTP(w, r)
			}))
			t.Cleanup(httpServer.Close)
			manager := newHTTPManager(t, httpServer.URL, 2*time.Second)
			set, err := manager.Tools(context.Background(), []string{"mcp__remote__echo"})
			if err != nil || len(set.Tools) != 1 {
				t.Fatalf("binding: %+v %v", set, err)
			}
			out, err := set.Tools[0].Call(context.Background(), json.RawMessage(`{}`))
			var unavailable interface{ ToolUnavailable() bool }
			if result.unavailable && (err == nil || !errors.As(err, &unavailable) || !unavailable.ToolUnavailable()) {
				t.Fatalf("incomplete result error = %v", err)
			}
			if !result.unavailable && (err != nil || out.Content != "(no output)") {
				t.Fatalf("empty result = %+v, %v", out, err)
			}
		})
	}
}

const testMaxOutput = 200

func newHTTPManager(t *testing.T, url string, callTimeout time.Duration) *Manager {
	t.Helper()
	cfg := ServerConfig{
		URL: url,
		Headers: map[string]string{
			"Authorization": "Bearer test-token",
		},
		XReview: XReviewConfig{CallTimeout: Duration(callTimeout), MaxOutputChars: testMaxOutput},
	}
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project:   Config{MCPServers: map[string]ServerConfig{"remote": cfg}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return m
}

func TestHTTPAllowlistHeadersAndOutputBounds(t *testing.T) {
	serverHTTP := newHTTPMCP(t, "test-token")
	m := newHTTPManager(t, serverHTTP.URL+"/mcp", 2*time.Second)
	set, err := m.Tools(context.Background(), []string{
		"mcp__remote__echo",
		"mcp__remote__destroy",
		"mcp__remote__missing",
	})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(set.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(set.Tools))
	}
	if !strings.Contains(strings.Join(set.Warnings, "\n"), "mutation annotations") {
		t.Fatalf("warnings = %v", set.Warnings)
	}
	echo := findTool(t, set.Tools, "mcp__remote__echo")
	res, err := echo.Call(context.Background(), json.RawMessage(`{"text":"http"}`))
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if strings.TrimSpace(res.Content) != "echo:http" {
		t.Fatalf("echo content = %q", res.Content)
	}

	infos, err := m.ListTools(context.Background(), "remote")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	seenDestroy := false
	for _, info := range infos {
		if info.Name == "destroy" {
			seenDestroy = true
			if !info.Destructive {
				t.Fatal("destructive annotation was not reported")
			}
		}
	}
	if !seenDestroy {
		t.Fatal("ListTools omitted a destructive advertised tool")
	}

	bigSet, err := m.Tools(context.Background(), []string{"mcp__remote__big"})
	if err != nil {
		t.Fatalf("Tools big: %v", err)
	}
	big := findTool(t, bigSet.Tools, "mcp__remote__big")
	bigResult, err := big.Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("big: %v", err)
	}
	if !bigResult.Truncated || len(bigResult.Content) > testMaxOutput {
		t.Fatalf("big result truncated=%v len=%d", bigResult.Truncated, len(bigResult.Content))
	}
	if !strings.HasSuffix(bigResult.Content, truncationMarker) {
		t.Fatalf("big result lacks truncation marker: %q", bigResult.Content)
	}

	_, err = m.call(context.Background(), "remote", "not-advertised", json.RawMessage(`{}`), false)
	if err == nil {
		t.Fatal("unknown tool call succeeded")
	}
	if strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), serverHTTP.URL) {
		t.Fatalf("error leaked credentials or URL: %v", err)
	}
}

func TestHTTPCallTimeoutKeepsSession(t *testing.T) {
	serverHTTP := newHTTPMCP(t, "test-token")
	m := newHTTPManager(t, serverHTTP.URL+"/mcp", 300*time.Millisecond)
	set, err := m.Tools(context.Background(), []string{"mcp__remote__slow", "mcp__remote__echo"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	slow := findTool(t, set.Tools, "mcp__remote__slow")
	_, err = slow.Call(context.Background(), json.RawMessage(`{"ms":2000}`))
	if !errors.Is(err, ErrCallFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow error = %v", err)
	}
	var unavailable interface{ ToolUnavailable() bool }
	if !errors.As(err, &unavailable) || !unavailable.ToolUnavailable() {
		t.Fatalf("timed out tool not marked unavailable: %v", err)
	}
	echo := findTool(t, set.Tools, "mcp__remote__echo")
	res, err := echo.Call(context.Background(), json.RawMessage(`{"text":"alive"}`))
	if err != nil {
		t.Fatalf("echo after timeout: %v", err)
	}
	if strings.TrimSpace(res.Content) != "echo:alive" {
		t.Fatalf("echo content = %q", res.Content)
	}
}

func TestHTTPDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	cfg := ServerConfig{
		URL:     redirector.URL + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer REDIRECT-SECRET"},
	}
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project:   Config{MCPServers: map[string]ServerConfig{"remote": cfg}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	_, err = m.Tools(context.Background(), []string{"mcp__remote__echo"})
	if err == nil {
		t.Fatal("redirecting server unexpectedly connected")
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target was reached %d times", targetHits.Load())
	}
	if strings.Contains(err.Error(), "REDIRECT-SECRET") {
		t.Fatalf("error leaked a header value: %v", err)
	}
}

func TestHTTPProjectOverridesPlatform(t *testing.T) {
	serverHTTP := newHTTPMCP(t, "test-token")
	m, err := New(Options{
		Workspace: t.TempDir(),
		Platform: Config{MCPServers: map[string]ServerConfig{
			"remote": {URL: "http://127.0.0.1:1/mcp"},
		}},
		Project: Config{MCPServers: map[string]ServerConfig{
			"remote": {
				URL:     serverHTTP.URL + "/mcp",
				Headers: map[string]string{"Authorization": "Bearer test-token"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	set, err := m.Tools(context.Background(), []string{"mcp__remote__echo"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if _, err := findTool(t, set.Tools, "mcp__remote__echo").Call(context.Background(), json.RawMessage(`{"text":"merged"}`)); err != nil {
		t.Fatalf("echo: %v", err)
	}
}

func TestHTTPNilTransportFallsBackToDefault(t *testing.T) {
	serverHTTP := newHTTPMCP(t, "test-token")
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project: Config{MCPServers: map[string]ServerConfig{
			"remote": {
				URL:     serverHTTP.URL + "/mcp",
				Headers: map[string]string{"Authorization": "Bearer test-token"},
			},
		}},
		HTTPClient: &http.Client{Transport: nil},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	set, err := m.Tools(context.Background(), []string{"mcp__remote__echo"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	res, err := findTool(t, set.Tools, "mcp__remote__echo").Call(context.Background(), json.RawMessage(`{"text":"default"}`))
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if strings.TrimSpace(res.Content) != "echo:default" {
		t.Fatalf("echo content = %q", res.Content)
	}
}

func TestHTTPManagerClose(t *testing.T) {
	serverHTTP := newHTTPMCP(t, "test-token")
	m := newHTTPManager(t, serverHTTP.URL+"/mcp", 2*time.Second)
	set, err := m.Tools(context.Background(), []string{"mcp__remote__echo"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.Tools(context.Background(), []string{"mcp__remote__echo"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Tools after Close = %v", err)
	}
	if _, err := findTool(t, set.Tools, "mcp__remote__echo").Call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("call after Close succeeded")
	}
}
