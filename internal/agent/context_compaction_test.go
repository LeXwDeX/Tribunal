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
	calls     int // tool calls per turn; default 1
	ignore    int // submit prompts to ignore before submitting
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
		if c.ignore == 0 {
			resp := toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
			resp.Usage = usage
			return resp, nil
		}
		c.ignore--
	}
	var calls []*llm.ToolCall
	for i := 0; i < max(c.calls, 1); i++ {
		calls = append(calls, call(fmt.Sprintf("c%d-%d", len(c.reqs), i), "big", `{}`))
	}
	resp := toolResponse(llm.StopToolUse, calls...)
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
	if result.Transcript[0].Parts[0].Text != spec.Prompt {
		t.Fatal("the task prompt was compacted")
	}
	kept := 0
	for i := len(result.Transcript) - 1; i >= 0 && kept < compactKeepToolTurns; i-- {
		msg := result.Transcript[i]
		if msg.Role != llm.RoleTool {
			continue
		}
		kept++
		if msg.Parts[0].ToolResult.Content == compactedToolResult {
			t.Fatalf("recent tool turn %d was compacted", kept)
		}
	}
}

func TestRunCompactsRecentTurnsWhenTheyFillTheWindow(t *testing.T) {
	// Four parallel 8000-byte outputs per turn: three kept turns alone are
	// larger than the window.
	client := &windowClient{window: 20000, calls: 4}
	spec := testSpec(client, bigTool(8000))
	spec.MaxTurns = 30
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v (largest request %d tokens, window %d)", err, client.maxTokens, client.window)
	}
	if client.maxTokens > int64(client.window) {
		t.Fatalf("largest request = %d tokens, window %d", client.maxTokens, client.window)
	}
	if result.Output == nil {
		t.Fatal("no submit_result")
	}
}

func TestRunDoesNotExecuteToolsPastTheContextLimit(t *testing.T) {
	executed := 0
	client := &windowClient{window: 10000, ignore: 1}
	spec := testSpec(client, echoTool{name: "big", fn: func(context.Context, json.RawMessage) (Result, error) {
		executed++
		return Result{Content: "small"}, nil
	}})
	spec.Prompt = strings.Repeat("p", 36000)
	result, err := Run(context.Background(), spec)
	if err != nil || result == nil || result.Turns != 3 {
		t.Fatalf("err=%v, want submit on turn 3", err)
	}
	// Turn 1 runs the tool; turn 2 is past the limit, so its call is refused.
	if executed != 1 {
		t.Fatalf("tool executed %d times, want 1", executed)
	}
}

func TestCompactionKeepsErrorResults(t *testing.T) {
	long := strings.Repeat("e", 500)
	messages := []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "task"}}}}
	for i := 0; i < 2; i++ {
		messages = append(messages,
			assistant(call(fmt.Sprintf("c%d", i), "big", `{}`)),
			llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult,
				ToolResult: &llm.ToolResult{CallID: fmt.Sprintf("c%d", i), Content: long, IsError: i == 0}}}})
	}
	_, replaced := compactToolResults(messages, 0)
	if replaced != 1 || messages[2].Parts[0].ToolResult.Content != long || messages[4].Parts[0].ToolResult.Content != compactedToolResult {
		t.Fatalf("replaced=%d; error result must stay and success result must be compacted", replaced)
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
		return 4500 - used.InputTokens - used.OutputTokens, true
	}
	result, err := Run(context.Background(), spec)
	// After two turns 2480 tokens remain, less than three 1010-token turns.
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

// retryUsageClient reports a retried call: Usage counts a failed attempt as
// well, AttemptUsage only the successful one.
type retryUsageClient struct{ reqs []*llm.Request }

func (c *retryUsageClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "retry", ContextWindow: 10000}
}

func (c *retryUsageClient) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.reqs = append(c.reqs, req)
	var resp *llm.Response
	if hasSubmitPrompt(req) {
		resp = toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
	} else {
		resp = toolResponse(llm.StopToolUse, call(fmt.Sprintf("c%d", len(c.reqs)), "big", `{}`))
	}
	resp.Usage = llm.Usage{InputTokens: 9000, OutputTokens: 10}
	resp.AttemptUsage = llm.Usage{InputTokens: 4500, OutputTokens: 5}
	return resp, nil
}

func TestRunSizesContextFromTheSuccessfulAttempt(t *testing.T) {
	client := &retryUsageClient{}
	spec := testSpec(client, bigTool(10))
	spec.MaxTurns = 3
	result, err := Run(context.Background(), spec)
	if err != nil || result.Turns != 3 {
		t.Fatalf("err=%v, want submit on the last turn", err)
	}
	for i, req := range client.reqs[:2] {
		if hasSubmitPrompt(req) {
			t.Fatalf("request %d asked for submit: retry usage was taken as context size", i+1)
		}
	}
	if result.Usage.InputTokens != 27000 {
		t.Fatalf("budget usage = %d, want the aggregate 27000", result.Usage.InputTokens)
	}
}

func TestRunKeepsExploringWhenFewerRecentTurnsFit(t *testing.T) {
	// A large task prompt leaves room for only about two full tool turns.
	// After three turns, keeping three is over the limit but keeping two
	// fits, so the run keeps exploring instead of being told to submit.
	client := &windowClient{window: 20000}
	spec := testSpec(client, bigTool(8000))
	spec.Prompt = strings.Repeat("p", 44000)
	spec.MaxTurns = 8
	result, err := Run(context.Background(), spec)
	if err != nil || result.Turns != 8 {
		t.Fatalf("err=%v, want submit on the last turn", err)
	}
	for i, req := range client.reqs[:7] {
		if hasSubmitPrompt(req) {
			t.Fatalf("request %d asked for submit although fewer kept turns fit", i+1)
		}
	}
}
