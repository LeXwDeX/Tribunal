package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	tribunalmcp "github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type actualMCPTools struct{ tool agent.Tool }

func (p actualMCPTools) Tools(context.Context, string) ([]agent.Tool, error) {
	return []agent.Tool{p.tool}, nil
}

type runtimeGapModel struct {
	t     *testing.T
	turn  int
	input json.RawMessage
}

func (m *runtimeGapModel) Info() llm.ProviderInfo { return llm.ProviderInfo{} }
func (m *runtimeGapModel) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.turn++
	name, input := "mcp__remote__inspect", m.input
	if m.turn == 2 {
		last := req.Messages[len(req.Messages)-1]
		if len(last.Parts) != 1 || last.Parts[0].ToolResult == nil || !last.Parts[0].ToolResult.IsError {
			m.t.Fatalf("model did not receive tool error: %+v", last)
		}
		if strings.Contains(last.Parts[0].ToolResult.Content, "private-server-text") {
			m.t.Fatal("server error leaked")
		}
		name, input = agent.SubmitToolName, json.RawMessage(`{"summary":"no issues","findings":[]}`)
	} else if m.turn != 1 {
		m.t.Fatalf("unexpected model turn %d", m.turn)
	}
	return &llm.Response{StopReason: llm.StopToolUse, Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: name, Name: name, Input: input}}}}}, nil
}

func localReviewMCP(t *testing.T, handler sdk.ToolHandler, timeout time.Duration) agent.Tool {
	return localReviewMCPWithDisconnect(t, handler, timeout, false)
}

func localReviewMCPWithDisconnect(t *testing.T, handler sdk.ToolHandler, timeout time.Duration, disconnect bool) agent.Tool {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "inspect", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, handler)
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if disconnect && r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(body, &req) == nil && req.Method == "tools/call" {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	manager, err := tribunalmcp.New(tribunalmcp.Options{Workspace: t.TempDir(), Project: tribunalmcp.Config{MCPServers: map[string]tribunalmcp.ServerConfig{"remote": {URL: httpServer.URL, XReview: tribunalmcp.XReviewConfig{CallTimeout: tribunalmcp.Duration(timeout)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	set, err := manager.Tools(context.Background(), []string{"mcp__remote__inspect"})
	if err != nil || len(set.Tools) != 1 {
		t.Fatalf("binding: %+v %v", set, err)
	}
	return set.Tools[0]
}

func TestRuntimeMCPFailurePreventsCleanPass(t *testing.T) {
	for _, tc := range []struct {
		name       string
		input      json.RawMessage
		handler    sdk.ToolHandler
		gap        bool
		disconnect bool
	}{
		{name: "timeout", input: json.RawMessage(`{}`), gap: true, handler: func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		{name: "tool result error", input: json.RawMessage(`{}`), handler: func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "ordinary tool error"}}}, nil
		}},
		{name: "invalid arguments", input: json.RawMessage(`[]`), handler: func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			t.Fatal("invalid arguments reached server")
			return nil, nil
		}},
		{name: "remote invalid parameters", input: json.RawMessage(`{}`), handler: func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "private-server-text"}
		}},
		{name: "remote internal error", input: json.RawMessage(`{}`), gap: true, handler: func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return nil, errors.New("private-server-text")
		}},
		{name: "connection dropped", input: json.RawMessage(`{}`), gap: true, disconnect: true, handler: func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := localReviewMCPWithDisconnect(t, tc.handler, 150*time.Millisecond, tc.disconnect)
			model := &runtimeGapModel{t: t, input: tc.input}
			executor := newTestExecutor(t, ExecutorDeps{Dossier: validDossier(t), Blocks: map[string]config.BlockBinding{BlockCodeReview: {MCPTools: []string{tool.Def().Name}}}, Tools: actualMCPTools{tool}, AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				spec.Client = model
				return agent.Run(ctx, spec)
			}})
			out, err := executor.Execute(context.Background(), codeReviewInput(t))
			if tc.name == "invalid arguments" {
				if !errors.Is(err, agent.ErrUnsafeResponse) || model.turn != 1 || len(out.Data) != 0 {
					t.Fatalf("invalid arguments should fail closed: error=%v turns=%d output=%s", err, model.turn, out.Data)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeStrict[*CodeReviewOut](out.Data)
			if err != nil {
				t.Fatal(err)
			}
			if (len(decoded.ToolGaps) > 0) != tc.gap {
				t.Errorf("tool gaps = %q, want gap=%t", decoded.ToolGaps, tc.gap)
			}
			node := completedNode("cr.a", KindCodeReview, reviewNodeParams{Package: "a", Files: []string{"a.go"}, Focus: "f", Provider: "local"}, *decoded)
			wf := evidenceWorkflow(node)
			result := runEvidenceSynthesis(t, evidenceExecutor(t, wf), wf, AdjudicateOut{Groups: []Group{}})
			want := VerdictPass
			if tc.gap {
				want = VerdictIncomplete
			}
			if result.Verdict != want {
				t.Errorf("verdict = %s, want %s", result.Verdict, want)
			}
			if tc.gap {
				proposal := ProposalFromReview("rv.std.p1", KindReviewStd, "local", "m", 1, ReviewOut{
					Verdict: "BLOCKING", EvidenceStatus: "sufficient", Findings: []FindingOut{finding("a.go", 10, SeverityP1, "real defect")}, Summary: "one blocking finding",
				})
				gathered, err := Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal}}, GatherOptions{ChallengeProvider: "claude", AllowedProviders: []string{"local", "claude"}})
				if err != nil {
					t.Fatal(err)
				}
				gathered.Groups[0].Challenge = &Challenge{Status: ChallengeConfirmed, Reason: "reproduced"}
				adjudicated, err := Adjudicate(gathered.Groups, AdjudicateInput{ChallengeOnly: []Severity{SeverityP0, SeverityP1}, OnUncertain: OnUncertainMark})
				if err != nil {
					t.Fatal(err)
				}
				blocking := runEvidenceSynthesis(t, evidenceExecutor(t, wf), wf, AdjudicateOut(adjudicated))
				if blocking.Verdict != VerdictBlocking || !containsText(blocking.Warnings, "inspect") {
					t.Errorf("blocking synthesis = %+v", blocking)
				}
			}
		})
	}
}

func TestRuntimeMCPCallCancellation(t *testing.T) {
	started := make(chan struct{})
	tool := localReviewMCP(t, func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}, 3*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	var gaps []string
	wrapped := gapRecordingTool{Tool: tool, block: BlockCodeReview, gaps: &gaps, mu: &sync.Mutex{}}
	_, err := wrapped.Call(ctx, json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) || len(gaps) != 0 {
		t.Fatalf("canceled err=%v gaps=%q", err, gaps)
	}
}

func TestRuntimeMCPNodeCancellationStopsAgent(t *testing.T) {
	started := make(chan struct{})
	tool := localReviewMCP(t, func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}, 3*time.Second)
	model := &runtimeGapModel{t: t, input: json.RawMessage(`{}`)}
	executor := newTestExecutor(t, ExecutorDeps{Dossier: validDossier(t), Blocks: map[string]config.BlockBinding{BlockCodeReview: {MCPTools: []string{tool.Def().Name}}}, Tools: actualMCPTools{tool}, AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		spec.Client = model
		return agent.Run(ctx, spec)
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	_, err := executor.Execute(ctx, codeReviewInput(t))
	if !errors.Is(err, context.Canceled) || model.turn != 1 {
		t.Fatalf("node cancellation: err=%v turns=%d", err, model.turn)
	}
}
