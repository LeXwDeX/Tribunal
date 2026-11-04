package llm

import (
	"encoding/json"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
)

// The agent loop appends a one-shot text prompt after tool results on the
// final turn (#255). Strict Anthropic-compatible gateways reject consecutive
// user messages, so the adapter must merge them with tool results first.
func TestAnthropicMergesTrailingUserTextIntoToolResultTurn(t *testing.T) {
	messages, err := anthropicMessages([]Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "task"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{}`)}}}},
		{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{CallID: "c1", Content: "data"}}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "submit now"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want user/assistant/user", len(messages))
	}
	last := messages[2]
	if last.Role != anthropic.MessageParamRoleUser || len(last.Content) != 2 ||
		last.Content[0].OfToolResult == nil || last.Content[1].OfText == nil || last.Content[1].OfText.Text != "submit now" {
		t.Fatalf("final user turn = %+v, want tool_result then text", last.Content)
	}
}

func TestAnthropicKeepsToolResultTurnAfterUserTextSeparate(t *testing.T) {
	messages, err := anthropicMessages([]Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "task"}}},
		{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{CallID: "c1", Content: "data"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Content[0].OfToolResult == nil {
		t.Fatalf("tool results must lead their own user turn: %+v", messages)
	}
}
