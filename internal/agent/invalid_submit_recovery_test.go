package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

// promptedInvalidClient explores until the submit prompt arrives, then
// answers each prompted turn with the next reply. It models a reasoning
// gateway whose large prompted submit_result arguments arrive corrupted.
type promptedInvalidClient struct {
	reqs     []*llm.Request
	prompted []*llm.Response
	window   int
}

func (c *promptedInvalidClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "prompted", ContextWindow: c.window}
}

func (c *promptedInvalidClient) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.reqs = append(c.reqs, req)
	usage := llm.Usage{InputTokens: 1000, OutputTokens: 10}
	if !hasSubmitPrompt(req) || len(c.prompted) == 0 {
		resp := toolResponse(llm.StopToolUse, call("e"+string(rune('a'+len(c.reqs))), "echo", `{}`))
		resp.Usage = usage
		return resp, nil
	}
	resp := c.prompted[0]
	c.prompted = c.prompted[1:]
	if llm.UsageZero(resp.Usage) {
		resp.Usage = usage
	}
	return resp, nil
}

const corruptSubmitInput = `{"summary":"PROVIDER-SECRET-TEXT` // truncated object

func budgetSpec(client llm.Client) RunSpec {
	spec := testSpec(client, echoTool{name: "echo"})
	spec.MaxTurns = 20
	spec.BudgetRemaining = func(_ context.Context, used llm.Usage) (int64, bool) {
		return 4500 - used.InputTokens - used.OutputTokens, true
	}
	return spec
}

func TestRunRecoversFromInvalidCallOnBudgetSubmitTurn(t *testing.T) {
	invalid := toolResponse(llm.StopToolUse, call("bad", SubmitToolName, corruptSubmitInput))
	invalid.Message.Parts = append([]llm.Part{{Type: llm.PartText, Text: "draft"}}, invalid.Message.Parts...)
	client := &promptedInvalidClient{prompted: []*llm.Response{
		invalid,
		toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
	}}
	result, err := Run(context.Background(), budgetSpec(client))
	if err != nil {
		t.Fatalf("Run: %v, want a recovered submit after one invalid prompted call", err)
	}
	if result.Turns != 4 {
		t.Fatalf("turns = %d, want 4 (two explore, one invalid, one resubmit)", result.Turns)
	}
	retry := client.reqs[3]
	if !hasSubmitPrompt(retry) {
		t.Fatal("the resubmit turn must still carry the submit prompt")
	}
	var feedback bool
	for _, msg := range retry.Messages {
		for _, part := range msg.Parts {
			if strings.Contains(part.Text, "PROVIDER-SECRET-TEXT") ||
				(part.ToolCall != nil && strings.Contains(string(part.ToolCall.Input), "PROVIDER-SECRET-TEXT")) {
				t.Fatal("provider-controlled tool input must not reach the next request")
			}
			if part.ToolCall != nil && part.ToolCall.ID == "" {
				t.Fatal("a rejected tool call must not be replayed")
			}
			if msg.Role == llm.RoleUser && part.Text == invalidToolCallFeedback(llm.ToolCallInvalidJSON) {
				feedback = true
			}
		}
	}
	if !feedback {
		t.Fatal("the resubmit turn must carry fixed-category feedback")
	}
	if result.Transcript[len(result.Transcript)-4].Role != llm.RoleAssistant ||
		result.Transcript[len(result.Transcript)-4].Parts[0].Text != "draft" {
		t.Fatal("assistant text of the rejected turn is kept without its tool calls")
	}
}

func TestRunFailsClosedOnSecondInvalidPromptedCall(t *testing.T) {
	client := &promptedInvalidClient{prompted: []*llm.Response{
		toolResponse(llm.StopToolUse, call("bad1", SubmitToolName, corruptSubmitInput)),
		toolResponse(llm.StopToolUse, call("bad2", SubmitToolName, corruptSubmitInput)),
	}}
	_, err := Run(context.Background(), budgetSpec(client))
	if !errors.Is(err, ErrUnsafeResponse) {
		t.Fatalf("err = %v, want unsafe after the single recovery is spent", err)
	}
}

func TestRunFailsClosedOnInvalidCallWithoutSubmitPrompt(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("bad", "echo", corruptSubmitInput)),
	}}
	_, err := Run(context.Background(), testSpec(client, echoTool{name: "echo"}))
	if !errors.Is(err, ErrUnsafeResponse) {
		t.Fatalf("err = %v, want unsafe on an ordinary turn", err)
	}
}

func TestRunFailsClosedOnInvalidCallOnLastTurn(t *testing.T) {
	client := &promptedInvalidClient{prompted: []*llm.Response{
		toolResponse(llm.StopToolUse, call("bad", SubmitToolName, corruptSubmitInput)),
	}}
	spec := testSpec(client, echoTool{name: "echo"})
	spec.MaxTurns = 2
	_, err := Run(context.Background(), spec)
	if !errors.Is(err, ErrUnsafeResponse) {
		t.Fatalf("err = %v, want unsafe when no turn is left", err)
	}
}

func TestRunRecoveryRejectsInvalidStopReasonMismatch(t *testing.T) {
	// A tool call with a non-tool stop reason is a protocol violation, not a
	// corrupted argument, and still fails closed.
	client := &promptedInvalidClient{prompted: []*llm.Response{
		toolResponse(llm.StopEndTurn, call("bad", SubmitToolName, `{}`)),
	}}
	_, err := Run(context.Background(), budgetSpec(client))
	if !errors.Is(err, ErrUnsafeResponse) {
		t.Fatalf("err = %v, want unsafe", err)
	}
}

func chatToolReply(id, name, args string) []byte {
	encoded, _ := json.Marshal(args)
	return []byte(`{"id":"r","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"` +
		id + `","type":"function","function":{"name":"` + name + `","arguments":` + string(encoded) + `}}]}}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010,"completion_tokens_details":{"reasoning_tokens":5}}}`)
}

func TestRunRecoversFromCorruptChatSubmitOnBudgetTurn(t *testing.T) {
	client, count, records, requests := safetyClient(t, "openai-chat",
		chatToolReply("e1", "echo", `{}`),
		chatToolReply("e2", "echo", `{}`),
		chatToolReply("s1", SubmitToolName, corruptSubmitInput),
		chatToolReply("s2", SubmitToolName, `{"done":true}`),
	)
	result, err := Run(context.Background(), budgetSpec(client))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Turns != 4 || count.Load() != 4 {
		t.Fatalf("turns=%d requests=%d, want 4 and 4", result.Turns, count.Load())
	}
	if strings.Contains((*requests)[3], "PROVIDER-SECRET-TEXT") || strings.Contains((*requests)[3], `"s1"`) {
		t.Fatal("the rejected call must not be replayed to the provider")
	}
	if !strings.Contains((*requests)[3], "invalid_json") {
		t.Fatal("the resubmit request must name the fixed category")
	}
	audit := (*records)[2].Audit
	if audit == nil || audit.Response == nil || audit.Response.InvalidToolCall != llm.ToolCallInvalidJSON ||
		audit.Response.InvalidToolInputBytes != len(corruptSubmitInput) || audit.Request.SubmitPrompt != llm.SubmitPromptBudget {
		t.Fatalf("audit = %+v, want invalid_json with the input length on the budget turn", audit)
	}
}

func TestRunBudgetReserveLeavesRoomForOneResubmit(t *testing.T) {
	const budget = 3500
	client := &promptedInvalidClient{prompted: []*llm.Response{
		toolResponse(llm.StopToolUse, call("bad", SubmitToolName, corruptSubmitInput)),
		toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo"})
	spec.MaxTurns = 20
	spec.BudgetRemaining = func(_ context.Context, used llm.Usage) (int64, bool) {
		return budget - used.InputTokens - used.OutputTokens, true
	}
	spec.BeforeTurn = func(_ context.Context, used llm.Usage) error {
		if used.InputTokens+used.OutputTokens >= budget {
			return errors.New("budget exhausted")
		}
		return nil
	}
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spent := result.Usage.InputTokens + result.Usage.OutputTokens; spent > budget {
		t.Fatalf("spent %d of %d: the submit prompt must start early enough for one resubmit", spent, budget)
	}
}

func TestRunRecoveryDropsReasoningOfRejectedTurn(t *testing.T) {
	// A Responses reasoning item must be followed by its function call; the
	// call is dropped, so its reasoning must go too.
	invalid := toolResponse(llm.StopToolUse, call("bad", SubmitToolName, corruptSubmitInput))
	invalid.Message.Parts = append([]llm.Part{
		{Type: llm.PartText, Text: "draft"},
		{Type: llm.PartReasoning, Reasoning: &llm.Reasoning{Protocol: "openai-responses", Raw: json.RawMessage(`{"type":"reasoning","id":"rs_2","summary":[]}`)}},
	}, invalid.Message.Parts...)
	client := &promptedInvalidClient{prompted: []*llm.Response{
		invalid,
		toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
	}}
	if _, err := Run(context.Background(), budgetSpec(client)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, msg := range client.reqs[3].Messages {
		for _, part := range msg.Parts {
			if part.Type == llm.PartReasoning {
				t.Fatal("reasoning of the rejected turn must not be replayed")
			}
		}
	}
}

func TestRunRecoverySizesContextWithoutRejectedOutput(t *testing.T) {
	// The rejected output is not replayed, so it must not count toward the
	// context estimate and trigger compaction on the resubmit turn.
	invalid := toolResponse(llm.StopToolUse, call("bad", SubmitToolName, corruptSubmitInput))
	invalid.Usage = llm.Usage{InputTokens: 1000, OutputTokens: 90000}
	client := &promptedInvalidClient{window: 100000, prompted: []*llm.Response{
		invalid,
		toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
	}}
	if _, err := Run(context.Background(), budgetSpec(client)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, msg := range client.reqs[len(client.reqs)-1].Messages {
		for _, part := range msg.Parts {
			if part.ToolResult != nil && part.ToolResult.Content == compactedToolResult {
				t.Fatal("the resubmit turn must not compact because of the rejected output")
			}
		}
	}
}
