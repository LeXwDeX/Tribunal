package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

// windowClient reports InputTokens as the request size in bytes/4 and fails
// with context_too_long above its window. It submits after the submit prompt.
type windowClient struct {
	mu        sync.Mutex
	window    int
	usage     int64 // fixed InputTokens when positive
	reqs      []*llm.Request
	maxTokens int64
}

func (c *windowClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "window", ContextWindow: c.window}
}

func requestBytes(req *llm.Request) int {
	n := 0
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			n += len(part.Text)
			if part.ToolResult != nil {
				n += len(part.ToolResult.Content)
			}
			if part.ToolCall != nil {
				n += len(part.ToolCall.Input) + len(part.ToolCall.Name)
			}
		}
	}
	return n
}

func (c *windowClient) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	tokens := int64(requestBytes(req) / 4)
	if c.usage > 0 {
		tokens = c.usage
	}
	c.maxTokens = max(c.maxTokens, tokens)
	if c.window > 0 && tokens > int64(c.window) {
		return nil, &llm.Error{Kind: llm.ErrContextTooLong, Status: 400}
	}
	usage := llm.Usage{InputTokens: tokens, OutputTokens: 10}
	if hasSubmitPrompt(req) {
		resp := toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
		resp.Usage = usage
		return resp, nil
	}
	resp := toolResponse(llm.StopToolUse, call(fmt.Sprintf("c%d", len(c.reqs)), "big", `{}`))
	resp.Usage = usage
	return resp, nil
}

func bigTool(size int) Tool {
	return echoTool{name: "big", fn: func(context.Context, json.RawMessage) (Result, error) {
		return Result{Content: strings.Repeat("x", size)}, nil
	}}
}

func TestRunCompactsContextNearWindow(t *testing.T) {
	client := &windowClient{window: 20000}
	spec := testSpec(client, bigTool(8000))
	spec.MaxTurns = 30
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v (largest request %d tokens, window %d)", err, client.maxTokens, client.window)
	}
	if result.Turns != 30 || client.maxTokens > int64(client.window) {
		t.Fatalf("turns = %d, largest request = %d tokens, want 30 turns within %d", result.Turns, client.maxTokens, client.window)
	}
	omitted := 0
	for i, msg := range result.Transcript {
		if msg.Role != llm.RoleTool {
			continue
		}
		calls, _ := responseToolCalls(result.Transcript[i-1])
		if len(calls) != len(msg.Parts) {
			t.Fatalf("message %d: %d results for %d calls", i, len(msg.Parts), len(calls))
		}
		for j, part := range msg.Parts {
			if part.ToolResult.CallID != calls[j].ID {
				t.Fatalf("message %d: result %q does not match call %q", i, part.ToolResult.CallID, calls[j].ID)
			}
			if part.ToolResult.Content == compactedToolResult {
				omitted++
			}
		}
	}
	if omitted == 0 {
		t.Fatal("no tool output was compacted")
	}
}

func TestRunAsksForSubmitWhenCompactionCannotFreeContext(t *testing.T) {
	client := &windowClient{window: 10000}
	spec := testSpec(client, bigTool(10))
	spec.Prompt = strings.Repeat("p", 36000) // 9000 tokens, 90% of the window
	result, err := Run(context.Background(), spec)
	if err != nil || result == nil || result.Turns != 2 {
		t.Fatalf("err=%v, want submit on turn 2 after the context reached 85%%", err)
	}
	if hasSubmitPrompt(client.reqs[0]) || !hasSubmitPrompt(client.reqs[1]) {
		t.Fatal("submit prompt must start once the context is near the window")
	}
}

func TestRunAsksForSubmitWhenBudgetNearlySpent(t *testing.T) {
	client := &windowClient{usage: 1000}
	spec := testSpec(client, bigTool(10))
	spec.BudgetRemaining = func(_ context.Context, used llm.Usage) (int64, bool) {
		return 3500 - used.InputTokens - used.OutputTokens, true
	}
	result, err := Run(context.Background(), spec)
	// After two turns 1480 tokens remain, less than two 1010-token turns.
	if err != nil || result == nil || result.Turns != 3 {
		t.Fatalf("err=%v, want submit on turn 3", err)
	}
	if hasSubmitPrompt(client.reqs[1]) || !hasSubmitPrompt(client.reqs[2]) {
		t.Fatal("submit prompt must start once the budget is nearly spent")
	}
}

func TestRunWithoutContextWindowNeverCompacts(t *testing.T) {
	client := &windowClient{}
	spec := testSpec(client, bigTool(8000))
	spec.MaxTurns = 12
	result, err := Run(context.Background(), spec)
	if err != nil || result.Turns != 12 {
		t.Fatalf("err=%v, want submit on the last turn", err)
	}
	for i, req := range client.reqs[:11] {
		if hasSubmitPrompt(req) {
			t.Fatalf("request %d asked for submit without a window or budget", i+1)
		}
		for _, msg := range req.Messages {
			for _, part := range msg.Parts {
				if part.ToolResult != nil && part.ToolResult.Content == compactedToolResult {
					t.Fatalf("request %d compacted without a context window", i+1)
				}
			}
		}
	}
}
