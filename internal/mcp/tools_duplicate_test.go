package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestListToolsMarksUnusableSchemaUnbindable checks that a tool whose input
// schema the runtime rejects is not offered as bindable by the test API.
func TestListToolsMarksUnusableSchemaUnbindable(t *testing.T) {
	server := testServer()
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
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
		if json.Unmarshal(body, &req) != nil || req.Method != "tools/list" {
			inner.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"tools":[` +
			`{"name":"usable","inputSchema":{"type":"object"}},` +
			`{"name":"bad-schema","inputSchema":{"type":"array"}}` +
			`]}}`))
	}))
	t.Cleanup(httpServer.Close)

	manager := newHTTPManager(t, httpServer.URL, 2*time.Second)
	infos, err := manager.ListTools(context.Background(), "remote")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	bindable := make(map[string]bool, len(infos))
	for _, info := range infos {
		bindable[info.Name] = info.Bindable
	}
	if !bindable["usable"] {
		t.Fatalf("usable tool is not bindable: %+v", infos)
	}
	if value, ok := bindable["bad-schema"]; !ok || value {
		t.Fatalf("unusable-schema tool bindable=%v, present=%v: %+v", value, ok, infos)
	}
}

// TestListToolsCollapsesDuplicateNames checks the test API contract for a
// server that advertises one tool name twice. The runtime refuses duplicate
// definitions, so the test response must expose a single entry that is not
// bindable instead of offering a tool that can never be called.
func TestListToolsCollapsesDuplicateNames(t *testing.T) {
	server := testServer()
	inner := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
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
		if json.Unmarshal(body, &req) != nil || req.Method != "tools/list" {
			inner.ServeHTTP(w, r)
			return
		}
		// The second definition carries an invalid x-mcp-header annotation, so
		// the SDK filters it out and ListTools reconstructs the duplicate as a
		// synthetic entry, matching the runtime conflict that fails at call time.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"tools":[` +
			`{"name":"dup","description":"first","inputSchema":{"type":"object"}},` +
			`{"name":"dup","description":"second","inputSchema":{"type":"object","properties":{"bad":{"type":"object","x-mcp-header":"X-Bad"}}}}` +
			`]}}`))
	}))
	t.Cleanup(httpServer.Close)

	manager := newHTTPManager(t, httpServer.URL, 2*time.Second)
	infos, err := manager.ListTools(context.Background(), "remote")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "dup" {
		t.Fatalf("infos = %+v, want one dup entry", infos)
	}
	if infos[0].Bindable {
		t.Fatalf("duplicate tool is bindable: %+v", infos[0])
	}
}
