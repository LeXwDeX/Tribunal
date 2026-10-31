package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestRejectInvalidToolCallHistoryBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	t.Setenv("TOOL_HISTORY_TEST_KEY", "key")
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		client := mustClient(t, protocol, config.ProviderConfig{Protocol: protocol, Model: "test", BaseURL: server.URL, APIKeyEnv: "TOOL_HISTORY_TEST_KEY", MaxTokens: 512})
		for _, input := range []string{"", "{", "null", "[]", "\u00a0{}"} {
			_, err := client.Generate(context.Background(), &Request{Messages: []Message{{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "id", Name: "tool", Input: json.RawMessage(input)}}}}}})
			requireKind(t, err, ErrBadRequest)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("sent %d invalid requests", requests.Load())
	}
}

const chatEmptyChoices = `{"id":"empty","object":"chat.completion","created":1,"model":"test","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

const responsesFailed = `{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"temporary"},"output":[],"usage":{"input_tokens":11,"output_tokens":7}}`

const responsesOK = `{"id":"ok","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`

func usageClient(t *testing.T, protocol string, replies ...string) (Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		idx := int(calls.Add(1)) - 1
		if idx >= len(replies) {
			http.Error(w, "unexpected retry", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(replies[idx]))
	}))
	t.Cleanup(server.Close)
	t.Setenv("USAGE_TEST_KEY", "key")
	return mustClient(t, protocol, config.ProviderConfig{
		Protocol: protocol, Model: "test", BaseURL: server.URL,
		APIKeyEnv: "USAGE_TEST_KEY", MaxTokens: 512,
	}, WithRetryBaseDelay(time.Millisecond)), &calls
}

func TestChatEmptyChoicesUsageAccumulatesAcrossRetries(t *testing.T) {
	client, calls := usageClient(t, "openai-chat", chatEmptyChoices, chatOK)
	resp, err := client.Generate(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", calls.Load())
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 8 {
		t.Fatalf("usage = %+v, want the failed and successful attempts", resp.Usage)
	}
}

func TestChatEmptyChoicesUsageRetainedWhenRetriesExhaust(t *testing.T) {
	replies := make([]string, maxRetries+1)
	for i := range replies {
		replies[i] = chatEmptyChoices
	}
	client, calls := usageClient(t, "openai-chat", replies...)
	resp, err := client.Generate(context.Background(), simpleRequest())
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Kind != ErrTransient {
		t.Fatalf("error = %v, want transient", err)
	}
	if calls.Load() != maxRetries+1 {
		t.Fatalf("requests = %d, want %d", calls.Load(), maxRetries+1)
	}
	if failure.Usage.InputTokens != 11*(maxRetries+1) || failure.Usage.OutputTokens != 7*(maxRetries+1) {
		t.Fatalf("usage = %+v, want every attempt", failure.Usage)
	}
}

func TestResponsesFailedUsageAccumulatesAcrossRetries(t *testing.T) {
	client, calls := usageClient(t, "openai-responses", responsesFailed, responsesOK)
	resp, err := client.Generate(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", calls.Load())
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 8 {
		t.Fatalf("usage = %+v, want the failed and successful attempts", resp.Usage)
	}
}

func TestResponsesFailedUsageRetainedWhenRetriesExhaust(t *testing.T) {
	replies := make([]string, maxRetries+1)
	for i := range replies {
		replies[i] = responsesFailed
	}
	client, calls := usageClient(t, "openai-responses", replies...)
	_, err := client.Generate(context.Background(), simpleRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.Kind != ErrTransient {
		t.Fatalf("error = %v, want transient", err)
	}
	if calls.Load() != maxRetries+1 {
		t.Fatalf("requests = %d, want %d", calls.Load(), maxRetries+1)
	}
	if failure.Usage.InputTokens != 11*(maxRetries+1) || failure.Usage.OutputTokens != 7*(maxRetries+1) {
		t.Fatalf("usage = %+v, want every attempt", failure.Usage)
	}
}
