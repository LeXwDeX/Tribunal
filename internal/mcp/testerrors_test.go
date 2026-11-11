package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolInfoAnnotationFlags(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name        string
		annotations *sdk.ToolAnnotations
		readOnly    bool
		destructive bool
		bindable    bool
		annotated   bool
	}{
		{name: "unannotated", bindable: true},
		{name: "read only", annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}, readOnly: true, bindable: true, annotated: true},
		{name: "additive", annotations: &sdk.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no}, bindable: false, annotated: true},
		{name: "contradictory", annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes}, readOnly: true, destructive: true, bindable: false, annotated: true},
		{name: "metadata only", annotations: &sdk.ToolAnnotations{Title: "metadata"}, bindable: false, annotated: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, ok := infoFor(&sdk.Tool{Name: "probe", Annotations: tc.annotations})
			if !ok {
				t.Fatal("infoFor rejected a valid tool")
			}
			if info.ReadOnly != tc.readOnly || info.Destructive != tc.destructive ||
				info.Bindable != tc.bindable || info.Annotated != tc.annotated {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

func TestTestCategoryMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"unknown", errors.New("private upstream detail"), ""},
		{"invalid config", fmt.Errorf("parse: %w", ErrInvalidConfig), TestCategoryInvalidConfig},
		{"connect timeout", fmt.Errorf("dial: %w", ErrConnectTimeout), TestCategoryConnectTimeout},
		{"connect failed", fmt.Errorf("dial: %w", ErrConnectFailed), TestCategoryConnectFailed},
		{"handshake", fmt.Errorf("initialize: %w", ErrHandshakeFailed), TestCategoryHandshakeFailed},
		{"list timeout", fmt.Errorf("list: %w", ErrListToolsTimeout), TestCategoryListToolsTimeout},
		{"list failed", fmt.Errorf("list: %w", ErrListTools), TestCategoryListToolsFailed},
		{"too many", fmt.Errorf("list: %w", ErrTooManyTools), TestCategoryTooManyTools},
		{"cleanup", NewTestError(TestCategoryCleanupFailed, ErrCleanupFailed), TestCategoryCleanupFailed},
		{"deadline fallback", context.DeadlineExceeded, TestCategoryConnectTimeout},
		{"joined keeps first category", errors.Join(
			NewTestError(TestCategoryListToolsFailed, ErrListTools),
			NewTestError(TestCategoryCleanupFailed, ErrCleanupFailed),
		), TestCategoryListToolsFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TestCategory(tc.err); got != tc.want {
				t.Fatalf("category = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTestErrorDoesNotEchoCause(t *testing.T) {
	err := NewTestError(TestCategoryConnectFailed, errors.New("https://example.invalid/token-canary"))
	if text := err.Error(); text != "mcp: test failed: connect_failed" {
		t.Fatalf("error text = %q", text)
	}
}

func TestListToolsMissingCommandIsConnectFailed(t *testing.T) {
	manager, err := New(Options{
		Workspace: t.TempDir(),
		Platform: Config{MCPServers: map[string]ServerConfig{
			"local": {Command: "/definitely/not/a/real/tribunal-mcp-binary"},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	_, err = manager.ListTools(context.Background(), "local")
	if got := TestCategory(err); got != TestCategoryConnectFailed {
		t.Fatalf("category = %q, err = %v", got, err)
	}
}

func TestListToolsHandshakeFailureIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &request)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"error":{"code":-32601,"message":"unsupported"}}`))
	}))
	t.Cleanup(server.Close)
	manager, err := New(Options{
		Workspace: t.TempDir(),
		Platform: Config{MCPServers: map[string]ServerConfig{
			"remote": {URL: server.URL},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	_, err = manager.ListTools(context.Background(), "remote")
	if got := TestCategory(err); got != TestCategoryHandshakeFailed {
		t.Fatalf("category = %q, err = %v", got, err)
	}
}

func TestListToolsFailureIsClassified(t *testing.T) {
	_, manager, state := annotationServer(t)
	state.failList.Store(true)
	_, err := manager.ListTools(context.Background(), "remote")
	if got := TestCategory(err); got != TestCategoryListToolsFailed {
		t.Fatalf("category = %q, err = %v", got, err)
	}
}

func TestListToolsTimeoutIsClassified(t *testing.T) {
	server := testServer()
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		if request.Method == "tools/list" {
			time.Sleep(300 * time.Millisecond)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	manager, err := New(Options{
		Workspace: t.TempDir(),
		Platform: Config{MCPServers: map[string]ServerConfig{
			"remote": {URL: httpServer.URL, XReview: XReviewConfig{CallTimeout: Duration(2 * time.Second)}},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = manager.ListTools(ctx, "remote")
	if got := TestCategory(err); got != TestCategoryListToolsTimeout {
		t.Fatalf("category = %q, err = %v", got, err)
	}
}

func TestListToolsTooManyToolsIsClassified(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "many", Version: "1"}, nil)
	for i := 0; i < maxRemoteTools+1; i++ {
		name := fmt.Sprintf("tool-%03d", i)
		server.AddTool(&sdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
			})
	}
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	httpServer := httptest.NewServer(inner)
	t.Cleanup(httpServer.Close)
	manager, err := New(Options{
		Workspace: t.TempDir(),
		Platform: Config{MCPServers: map[string]ServerConfig{
			"remote": {URL: httpServer.URL},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	_, err = manager.ListTools(context.Background(), "remote")
	if got := TestCategory(err); got != TestCategoryTooManyTools {
		t.Fatalf("category = %q, err = %v", got, err)
	}
}
