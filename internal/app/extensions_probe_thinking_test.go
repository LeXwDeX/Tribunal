package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

// thinkingProbeClient fails the way the Anthropic API does when extended
// thinking is enabled and max_tokens does not exceed the thinking budget.
type thinkingProbeClient struct {
	info     llm.ProviderInfo
	toolCall bool
	requests []*llm.Request
}

func (c *thinkingProbeClient) Info() llm.ProviderInfo { return c.info }

func (c *thinkingProbeClient) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.requests = append(c.requests, req)
	if c.info.ThinkingBudget > 0 && req.MaxTokens <= c.info.ThinkingBudget {
		return nil, errors.New("anthropic: max_tokens must be greater than thinking.budget_tokens")
	}
	parts := []llm.Part{{Type: llm.PartText, Text: "pong"}}
	if c.toolCall {
		parts = []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{
			ID: "call_1", Name: "submit_result", Input: json.RawMessage(`{"summary":"ok"}`),
		}}}
	}
	return &llm.Response{
		Model:   c.info.Model,
		Message: llm.Message{Role: llm.RoleAssistant, Parts: parts},
	}, nil
}

func anthropicThinkingClient(budget int) *thinkingProbeClient {
	return &thinkingProbeClient{info: llm.ProviderInfo{
		Name: "claude", Protocol: "anthropic", Model: "claude-sonnet", ThinkingBudget: budget,
	}}
}

func TestProbeProviderKeepsAnthropicThinkingBudget(t *testing.T) {
	client := anthropicThinkingClient(2048)
	result, err := ProbeProvider(context.Background(), client)
	if err != nil {
		t.Fatalf("ProbeProvider: %v", err)
	}
	if result.Model != "claude-sonnet" {
		t.Fatalf("model = %q", result.Model)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
	if got := client.requests[0].MaxTokens; got <= 2048 {
		t.Fatalf("max_tokens = %d, want more than the 2048 thinking budget", got)
	}
}

func TestProbeProviderToolsKeepsAnthropicThinkingBudget(t *testing.T) {
	client := anthropicThinkingClient(2048)
	client.toolCall = true
	result, err := ProbeProviderTools(context.Background(), client)
	if err != nil {
		t.Fatalf("ProbeProviderTools: %v", err)
	}
	if !result.ToolCall || result.Probe != ProbeToolCall {
		t.Fatalf("result = %+v", result)
	}
	request := client.requests[0]
	if request.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %q, want auto for a thinking provider", request.ToolChoice)
	}
	if got := request.MaxTokens; got <= 2048 {
		t.Fatalf("max_tokens = %d, want more than the 2048 thinking budget", got)
	}
}

func TestProbesKeepFixedBudgetsWithoutThinking(t *testing.T) {
	client := &thinkingProbeClient{info: llm.ProviderInfo{Name: "gw", Protocol: "openai-chat", Model: "m"}, toolCall: true}
	if _, err := ProbeProvider(context.Background(), client); err != nil {
		t.Fatalf("ProbeProvider: %v", err)
	}
	if got := client.requests[0].MaxTokens; got != probeConnectivityMaxTokens {
		t.Fatalf("connectivity max_tokens = %d, want %d", got, probeConnectivityMaxTokens)
	}
	if _, err := ProbeProviderTools(context.Background(), client); err != nil {
		t.Fatalf("ProbeProviderTools: %v", err)
	}
	if got := client.requests[1].MaxTokens; got != probeToolMaxTokens {
		t.Fatalf("tool probe max_tokens = %d, want %d", got, probeToolMaxTokens)
	}
}
