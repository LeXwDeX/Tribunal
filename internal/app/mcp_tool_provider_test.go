package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/review"
)

// newSearchOnlyMCP serves one read-only tool, search_graph.
func newSearchOnlyMCP(t *testing.T) string {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "graph", Version: "1.0.0"}, nil)
	server.AddTool(&sdk.Tool{
		Name:        "search_graph",
		Description: "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer.URL + "/mcp"
}

func newGraphProvider(t *testing.T, tools ...string) review.ToolProvider {
	t.Helper()
	manager, err := mcp.New(mcp.Options{
		Workspace: t.TempDir(),
		Platform:  mcp.Config{MCPServers: map[string]mcp.ServerConfig{"graph": {URL: newSearchOnlyMCP(t)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return NewMCPToolProvider(manager, map[string]config.BlockBinding{
		"code-review": {MCPTools: tools},
	})
}

func TestMCPToolProviderReportsUnadvertisedToolsAsGaps(t *testing.T) {
	provider := newGraphProvider(t, "mcp__graph__search_graph", "mcp__graph__trace_path")
	tools, err := provider.Tools(context.Background(), "code-review")
	var partial *review.PartialToolsError
	if !errors.As(err, &partial) {
		t.Fatalf("Tools = %d, %v; want a partial tool set error", len(tools), err)
	}
	if len(partial.Tools) != 1 || partial.Tools[0].Def().Name != "mcp__graph__search_graph" {
		t.Fatalf("partial tools = %d, want the advertised tool", len(partial.Tools))
	}
	if len(partial.Gaps) != 1 || !strings.Contains(partial.Gaps[0], "trace_path") {
		t.Fatalf("gaps = %q, want the unadvertised tool", partial.Gaps)
	}
}

func TestMCPToolProviderFullSetHasNoGap(t *testing.T) {
	provider := newGraphProvider(t, "mcp__graph__search_graph")
	tools, err := provider.Tools(context.Background(), "code-review")
	if err != nil || len(tools) != 1 {
		t.Fatalf("Tools = %d, %v; want the full set without error", len(tools), err)
	}
}

// The MCP runtime parser trims allowlist entries, so a stored tool name with
// trailing whitespace still reaches and resolves the advertised tool.
func TestMCPToolProviderTrimsTrailingSpaceFromBoundToolName(t *testing.T) {
	provider := newGraphProvider(t, "mcp__graph__search_graph ")
	tools, err := provider.Tools(context.Background(), "code-review")
	if err != nil || len(tools) != 1 {
		t.Fatalf("Tools = %d, %v; want the trimmed tool", len(tools), err)
	}
	if got := tools[0].Def().Name; got != "mcp__graph__search_graph" {
		t.Fatalf("tool name = %q, want the canonical name", got)
	}
}
