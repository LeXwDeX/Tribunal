package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type annotationServerState struct {
	stripped           atomic.Bool
	duplicate          atomic.Bool
	crossPageDuplicate atomic.Bool
	invalidDuplicate   atomic.Bool
	failList           atomic.Bool
	cache              atomic.Bool
}

// Strip readOnlyHint from one response to exercise an older SDK's wire format.
func annotationServer(t *testing.T) (*sdk.Server, *Manager, *annotationServerState) {
	return annotationServerWithPageSize(t, 0)
}

func annotationServerWithPageSize(t *testing.T, pageSize int) (*sdk.Server, *Manager, *annotationServerState) {
	t.Helper()
	state := &annotationServerState{}
	server := sdk.NewServer(&sdk.Implementation{Name: "annotations-test", Version: "1"}, &sdk.ServerOptions{
		PageSize: pageSize,
		SetCacheable: func(_ context.Context, _ sdk.Request, cache *sdk.Cacheable) {
			if state.cache.Load() {
				cache.TTLMs = 60_000
			}
		},
	})
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		inner.ServeHTTP(recorder, r)
		body := recorder.Body.Bytes()
		var response map[string]any
		if json.Unmarshal(body, &response) == nil {
			result, _ := response["result"].(map[string]any)
			tools, isList := result["tools"].([]any)
			if isList && state.failList.Load() {
				http.Error(w, "private server error", http.StatusInternalServerError)
				return
			}
			for _, item := range tools {
				tool, _ := item.(map[string]any)
				if tool["name"] == "zeta" && state.crossPageDuplicate.Load() {
					schema := map[string]any{"type": "object"}
					if state.invalidDuplicate.Load() {
						schema["properties"] = map[string]any{"bad": map[string]any{"type": "object", "x-mcp-header": "X-Bad"}}
					}
					result["tools"] = append(tools, map[string]any{
						"name": "change", "inputSchema": schema,
						"annotations": map[string]any{"readOnlyHint": false},
					})
				}
				if tool["name"] == "metadata_only" {
					annotations, _ := tool["annotations"].(map[string]any)
					delete(annotations, "readOnlyHint")
					state.stripped.Store(true)
				}
				if tool["name"] == "change" && state.duplicate.Load() {
					duplicate := make(map[string]any, len(tool))
					for key, value := range tool {
						duplicate[key] = value
					}
					duplicate["annotations"] = map[string]any{"readOnlyHint": false, "destructiveHint": false}
					result["tools"] = append([]any{duplicate}, tools...)
				}
			}
			body, _ = json.Marshal(response)
		}
		for key, values := range recorder.Header() {
			if !strings.EqualFold(key, "Content-Length") {
				w.Header()[key] = values
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(httpServer.Close)
	return server, newHTTPManager(t, httpServer.URL+"/mcp", 2*time.Second), state
}

func TestMCPAnnotationsInvalidDuplicateAcrossPages(t *testing.T) {
	server, manager, state := annotationServerWithPageSize(t, 1)
	state.cache.Store(true)
	var calls atomic.Int32
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	addAnnotatedTool(server, "zeta", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	set, err := manager.Tools(context.Background(), []string{"mcp__remote__change"})
	if err != nil || len(set.Tools) != 1 {
		t.Fatalf("initial binding = %+v, %v", set, err)
	}
	state.crossPageDuplicate.Store(true)
	state.invalidDuplicate.Store(true)
	if _, err := set.Tools[0].Call(context.Background(), json.RawMessage(`{}`)); err == nil || calls.Load() != 0 {
		t.Fatalf("filtered duplicate was called: calls=%d err=%v", calls.Load(), err)
	}
	set, err = manager.Tools(context.Background(), []string{"mcp__remote__change"})
	if err != nil || len(set.Tools) != 0 || len(set.Warnings) != 1 {
		t.Fatalf("duplicate binding = %+v, %v", set, err)
	}
}

func addAnnotatedTool(server *sdk.Server, name string, annotations *sdk.ToolAnnotations, calls *atomic.Int32) {
	server.AddTool(&sdk.Tool{
		Name:        name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: annotations,
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.Add(1)
		return textResult("ok"), nil
	})
}

func TestMCPAnnotationsBindingReportsGaps(t *testing.T) {
	server, manager, state := annotationServer(t)
	var calls atomic.Int32
	yes, no := true, false
	addAnnotatedTool(server, "read", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	addAnnotatedTool(server, "legacy", nil, &calls)
	addAnnotatedTool(server, "unbound", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	addAnnotatedTool(server, "additive", &sdk.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no}, &calls)
	addAnnotatedTool(server, "contradictory", &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes}, &calls)
	addAnnotatedTool(server, "metadata_only", &sdk.ToolAnnotations{Title: "metadata only"}, &calls)

	set, err := manager.Tools(context.Background(), []string{
		"mcp__remote__read", "mcp__remote__legacy", "mcp__remote__additive",
		"mcp__remote__contradictory", "mcp__remote__metadata_only",
	})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if !state.stripped.Load() {
		t.Fatal("test server did not omit readOnlyHint on the wire")
	}
	got := make(map[string]bool)
	for _, tool := range set.Tools {
		got[tool.Def().Name] = true
	}
	if len(got) != 2 || !got["mcp__remote__read"] || !got["mcp__remote__legacy"] {
		t.Errorf("tools = %v, want only explicit read-only and bound legacy tools", got)
	}
	if len(set.Warnings) != 3 {
		t.Errorf("warnings = %v, want one gap per unsafe tool", set.Warnings)
	}
	for _, name := range []string{"additive", "contradictory", "metadata_only"} {
		if !strings.Contains(strings.Join(set.Warnings, "\n"), name) {
			t.Errorf("warnings = %v, missing gap for %q", set.Warnings, name)
		}
	}
	legacy := findTool(t, set.Tools, "mcp__remote__legacy")
	if _, err := legacy.Call(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("bound unannotated tool: %v", err)
	}
	infos, err := manager.ListTools(context.Background(), "remote")
	if err != nil {
		t.Fatal(err)
	}
	seenLegacy := false
	for _, info := range infos {
		if info.Name == "legacy" {
			seenLegacy = true
			if info.ReadOnly {
				t.Fatal("an unannotated tool was reported as read-only")
			}
		}
	}
	if !seenLegacy {
		t.Fatal("ListTools omitted the unannotated tool")
	}
}

func TestMCPAnnotationsRecheckBeforeCalls(t *testing.T) {
	yes, no := true, false
	readOnly := &sdk.ToolAnnotations{ReadOnlyHint: true}
	additive := &sdk.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no}
	contradictory := &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes}
	for _, tc := range []struct {
		name      string
		initial   *sdk.ToolAnnotations
		updated   *sdk.ToolAnnotations
		reconnect bool
		removed   bool
		cache     bool
		allowed   bool
	}{
		{name: "same session becomes additive", initial: readOnly, updated: additive},
		{name: "same session cached becomes additive", initial: readOnly, updated: additive, cache: true},
		{name: "reconnect becomes additive", initial: readOnly, updated: additive, reconnect: true},
		{name: "legacy reconnect becomes additive", updated: additive, reconnect: true},
		{name: "reconnect becomes contradictory", initial: readOnly, updated: contradictory, reconnect: true},
		{name: "reconnect drops read-only claim", initial: readOnly, reconnect: true},
		{name: "removed after binding", initial: readOnly, removed: true},
		{name: "cached removal after binding", initial: readOnly, removed: true, cache: true},
		{name: "legacy reconnect remains unannotated", reconnect: true, allowed: true},
		{name: "read-only reconnect remains read-only", initial: readOnly, updated: readOnly, reconnect: true, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, manager, state := annotationServer(t)
			state.cache.Store(tc.cache)
			var calls atomic.Int32
			addAnnotatedTool(server, "change", tc.initial, &calls)
			set, err := manager.Tools(context.Background(), []string{"mcp__remote__change"})
			if err != nil || len(set.Tools) != 1 {
				t.Fatalf("initial binding = %+v, %v", set, err)
			}
			tool := set.Tools[0]
			if _, err := tool.Call(context.Background(), json.RawMessage(`{}`)); err != nil {
				t.Fatalf("initial call: %v", err)
			}
			if tc.removed {
				server.RemoveTools("change")
			} else {
				addAnnotatedTool(server, "change", tc.updated, &calls)
			}
			if tc.reconnect {
				entry, err := manager.acquire(context.Background(), "remote")
				if err != nil {
					t.Fatalf("acquire: %v", err)
				}
				entry.end()
				manager.discard(entry)
			}
			_, err = tool.Call(context.Background(), json.RawMessage(`{}`))
			if tc.allowed {
				if err != nil || calls.Load() != 2 {
					t.Fatalf("unchanged tool: calls=%d err=%v", calls.Load(), err)
				}
			} else if err == nil || calls.Load() != 1 {
				t.Fatalf("changed tool was called: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestMCPAnnotationsDuplicateAndListFailure(t *testing.T) {
	server, manager, state := annotationServer(t)
	state.cache.Store(true)
	var calls atomic.Int32
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	allowed := []string{"mcp__remote__change"}

	state.duplicate.Store(true)
	set, err := manager.Tools(context.Background(), allowed)
	if err != nil || len(set.Tools) != 0 || len(set.Warnings) != 1 {
		t.Fatalf("duplicate binding = %+v, %v; want a gap", set, err)
	}
	state.duplicate.Store(false)
	set, err = manager.Tools(context.Background(), allowed)
	if err != nil || len(set.Tools) != 1 {
		t.Fatalf("initial binding = %+v, %v", set, err)
	}
	tool := set.Tools[0]
	state.duplicate.Store(true)
	if _, err := tool.Call(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, ErrCallFailed) || calls.Load() != 0 {
		t.Fatalf("duplicate call: calls=%d err=%v", calls.Load(), err)
	}
	state.duplicate.Store(false)
	state.failList.Store(true)
	_, err = tool.Call(context.Background(), json.RawMessage(`{}`))
	var unavailable interface{ ToolUnavailable() bool }
	if err == nil || !errors.As(err, &unavailable) || !unavailable.ToolUnavailable() || strings.Contains(err.Error(), "private server error") || calls.Load() != 0 {
		t.Fatalf("failed recheck: calls=%d err=%v", calls.Load(), err)
	}
}

func TestMCPAnnotationsCachedPaginatedRecheck(t *testing.T) {
	server, manager, state := annotationServerWithPageSize(t, 1)
	state.cache.Store(true)
	var calls atomic.Int32
	addAnnotatedTool(server, "alpha", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	set, err := manager.Tools(context.Background(), []string{"mcp__remote__change"})
	if err != nil || len(set.Tools) != 1 {
		t.Fatalf("initial paginated binding = %+v, %v", set, err)
	}
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{}, &calls)
	if _, err := set.Tools[0].Call(context.Background(), json.RawMessage(`{}`)); err == nil || calls.Load() != 0 {
		t.Fatalf("stale later page was called: calls=%d err=%v", calls.Load(), err)
	}
	set, err = manager.Tools(context.Background(), []string{"mcp__remote__change"})
	if err != nil || len(set.Tools) != 0 || len(set.Warnings) != 1 {
		t.Fatalf("stale later page binding = %+v, %v", set, err)
	}
}

func TestMCPAnnotationsDuplicateAcrossCachedPages(t *testing.T) {
	server, manager, state := annotationServerWithPageSize(t, 1)
	state.cache.Store(true)
	var calls atomic.Int32
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	addAnnotatedTool(server, "zeta", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	allowed := []string{"mcp__remote__change"}
	set, err := manager.Tools(context.Background(), allowed)
	if err != nil || len(set.Tools) != 1 {
		t.Fatalf("initial binding = %+v, %v", set, err)
	}
	state.crossPageDuplicate.Store(true)
	if _, err := set.Tools[0].Call(context.Background(), json.RawMessage(`{}`)); err == nil || calls.Load() != 0 {
		t.Fatalf("duplicate across pages: calls=%d err=%v", calls.Load(), err)
	}
	set, err = manager.Tools(context.Background(), allowed)
	if err != nil || len(set.Tools) != 0 || len(set.Warnings) != 1 {
		t.Fatalf("duplicate binding = %+v, %v", set, err)
	}
}

func TestMCPAnnotationsCachedListCannotApproveStaleTool(t *testing.T) {
	server, manager, state := annotationServer(t)
	state.cache.Store(true)
	var calls atomic.Int32
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	if _, err := manager.ListTools(context.Background(), "remote"); err != nil {
		t.Fatalf("initial list: %v", err)
	}
	notDestructive := false
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{DestructiveHint: &notDestructive}, &calls)
	set, err := manager.Tools(context.Background(), []string{"mcp__remote__change"})
	if err != nil || len(set.Tools) != 0 || len(set.Warnings) != 1 {
		t.Fatalf("stale binding = %+v, %v; want an unsafe-tool gap", set, err)
	}
}

func TestMCPAnnotationsReconnectCannotCallCachedStaleTool(t *testing.T) {
	server, manager, state := annotationServer(t)
	state.cache.Store(true)
	var calls atomic.Int32
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
	set, err := manager.Tools(context.Background(), []string{"mcp__remote__change"})
	if err != nil || len(set.Tools) != 1 {
		t.Fatalf("initial binding = %+v, %v", set, err)
	}
	entry, err := manager.acquire(context.Background(), "remote")
	if err != nil {
		t.Fatal(err)
	}
	entry.end()
	manager.discard(entry)
	if _, err := manager.ListTools(context.Background(), "remote"); err != nil {
		t.Fatalf("list after reconnect: %v", err)
	}
	notDestructive := false
	addAnnotatedTool(server, "change", &sdk.ToolAnnotations{DestructiveHint: &notDestructive}, &calls)
	if _, err := set.Tools[0].Call(context.Background(), json.RawMessage(`{}`)); err == nil || calls.Load() != 0 {
		t.Fatalf("stale tool was called after reconnect: calls=%d err=%v", calls.Load(), err)
	}
}
