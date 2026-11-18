package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// #551: without an explicit request timeout the Anthropic SDK rejects
// non-streaming requests whose estimated duration exceeds ten minutes
// (max_tokens >= 21334) before sending them. The adapter must still send the
// configured request.
func TestAnthropicSendsLargeNonStreamingMaxTokens(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-anthropic-key")
	response := `{"id":"msg_big","type":"message","role":"assistant","model":"claude-big","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	server, capture := newCapabilityWireServer(t, response)
	client := mustClient(t, "anthropic", config.ProviderConfig{
		Protocol:  "anthropic",
		BaseURL:   server.URL,
		Model:     "claude-big",
		APIKeyEnv: "TEST_ANTHROPIC_KEY",
		MaxTokens: 32000,
	})
	result, err := client.Generate(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}}},
	})
	if err != nil {
		t.Fatalf("Generate() = %v, want a sent request", err)
	}
	if capture.count() != 1 {
		t.Fatalf("expected one request, got %d", capture.count())
	}
	if body := capture.body(t, 0); !strings.Contains(body, `"max_tokens":32000`) {
		t.Fatalf("request body missing max_tokens: %s", body)
	}
	if findPart(result.Message, PartText) == nil {
		t.Fatalf("unexpected response: %+v", result.Message)
	}
}

// The SDK raises its non-streaming estimate as a plain local error, not an
// *anthropic.Error. It is a caller parameter problem and must not be retried
// as a transport failure.
func TestSDKErrorLocalParameterFailureIsBadRequest(t *testing.T) {
	err := sdkError(errors.New("streaming is required for operations that may take longer than 10 minutes"))
	if err == nil || err.Kind != ErrBadRequest {
		t.Fatalf("sdkError() = %+v, want bad_request", err)
	}
	if err.Retryable() {
		t.Fatal("local parameter failure must not be retryable")
	}
	if strings.Contains(err.Error(), "streaming is required") {
		t.Fatalf("error leaks raw SDK text: %v", err)
	}
}

// Unrecognised plain errors keep the previous transport classification.
func TestSDKErrorUnknownFailureStaysTransient(t *testing.T) {
	err := sdkError(errors.New("dial tcp: connection refused"))
	if err == nil || err.Kind != ErrTransient {
		t.Fatalf("sdkError() = %+v, want transient", err)
	}
	if !err.Retryable() {
		t.Fatal("transport failure must stay retryable")
	}
}
