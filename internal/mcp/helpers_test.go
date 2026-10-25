package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func testServer() *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "test-mcp", Version: "1.0.0"}, nil)
	add := func(tool *sdk.Tool, handler func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error)) {
		server.AddTool(tool, handler)
	}
	add(&sdk.Tool{Name: "echo", Description: "echo text", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var in struct {
				Text string `json:"text"`
			}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
					return nil, err
				}
			}
			return textResult("echo:" + in.Text), nil
		})
	add(&sdk.Tool{Name: "env", Description: "environment", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			values := make(map[string]string)
			for _, entry := range os.Environ() {
				if key, value, ok := cutEnv(entry); ok {
					values[key] = value
				}
			}
			data, err := json.Marshal(values)
			if err != nil {
				return nil, err
			}
			return textResult(string(data)), nil
		})
	add(&sdk.Tool{Name: "slow", Description: "slow", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var in struct {
				MS int `json:"ms"`
			}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
					return nil, err
				}
			}
			if in.MS < 0 {
				in.MS = 0
			}
			select {
			case <-time.After(time.Duration(in.MS) * time.Millisecond):
				return textResult("slow:done"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	add(&sdk.Tool{Name: "pid", Description: "pid", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return textResult(strconv.Itoa(os.Getpid())), nil
		})
	add(&sdk.Tool{Name: "big", Description: "big output", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return textResult(strings.Repeat("x", 10000)), nil
		})
	add(&sdk.Tool{Name: "crash", Description: "crash", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			os.Exit(7)
			return nil, nil
		})
	destructive := true
	add(&sdk.Tool{
		Name:        "destroy",
		Description: "destructive",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive},
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult("destroyed"), nil
	})
	add(&sdk.Tool{Name: "noann", Description: "no annotations", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return textResult("no-annotation-ok"), nil
		})
	return server
}

func textResult(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

func cutEnv(entry string) (string, string, bool) {
	for i := 0; i < len(entry); i++ {
		if entry[i] == '=' {
			return entry[:i], entry[i+1:], true
		}
	}
	return "", "", false
}

func findTool(t *testing.T, tools []agent.Tool, name string) agent.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Def().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func pidOf(t *testing.T, tool agent.Tool) string {
	t.Helper()
	result, err := tool.Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("pid call: %v", err)
	}
	return strings.TrimSpace(result.Content)
}
