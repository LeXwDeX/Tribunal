package llm

import (
	"context"
	"fmt"
	"math"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func TestUsageWireFieldValues(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		want     Usage
		complete bool
	}{
		{"integers", `{"prompt_tokens":5,"completion_tokens":7}`, Usage{InputTokens: 5, OutputTokens: 7}, true},
		{"zero", `{"prompt_tokens":0,"completion_tokens":0}`, Usage{}, true},
		{"negative zero", `{"prompt_tokens":-0,"completion_tokens":0}`, Usage{}, true},
		{"maximum", `{"prompt_tokens":9223372036854775807,"completion_tokens":0}`, Usage{InputTokens: math.MaxInt64}, true},
		{"missing core fields", `{}`, Usage{Incomplete: true}, true},
		{"missing output tokens", `{"prompt_tokens":5}`, Usage{InputTokens: 5, Incomplete: true}, true},
		{"missing input tokens", `{"completion_tokens":7}`, Usage{OutputTokens: 7, Incomplete: true}, true},
		{"empty raw", ``, Usage{}, true},
		{"null core field", `{"prompt_tokens":5,"completion_tokens":null}`, Usage{InputTokens: 5, Incomplete: true}, true},
		{"details null", `{"prompt_tokens":0,"completion_tokens":0,"prompt_tokens_details":null}`, Usage{}, true},
		{"optional details missing", `{"prompt_tokens":5,"completion_tokens":7}`, Usage{InputTokens: 5, OutputTokens: 7}, true},
		{"negative fraction", `{"prompt_tokens":-0.5}`, Usage{Incomplete: true}, false},
		{"fraction", `{"completion_tokens":7.0}`, Usage{Incomplete: true}, false},
		{"exponent", `{"prompt_tokens":1e100}`, Usage{Incomplete: true}, false},
		{"negative", `{"completion_tokens":-1}`, Usage{Incomplete: true}, false},
		{"overflow", `{"prompt_tokens":9223372036854775808}`, Usage{Incomplete: true}, false},
		{"string", `{"prompt_tokens":"5"}`, Usage{Incomplete: true}, false},
		{"boolean", `{"prompt_tokens":true}`, Usage{Incomplete: true}, false},
		{"details fraction", `{"prompt_tokens_details":{"cached_tokens":-0.5}}`, Usage{Incomplete: true}, false},
		{"details not object", `{"prompt_tokens_details":[]}`, Usage{Incomplete: true}, false},
		{"root not object", `[]`, Usage{Incomplete: true}, false},
		{"mixed", `{"prompt_tokens":5,"completion_tokens":-0.5}`, Usage{InputTokens: 5, Incomplete: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, complete := wireUsage(tc.raw, chatUsageFields)
			if got != tc.want || complete != tc.complete {
				t.Fatalf("usage=%+v complete=%t want=%+v/%t", got, complete, tc.want, tc.complete)
			}
		})
	}
}

func chatUsageBoundaryResponse(prompt, completion string) string {
	return fmt.Sprintf(`{"id":"x","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":%s,"completion_tokens":%s,"total_tokens":12}}`, prompt, completion)
}

func responsesUsageBoundaryResponse(input, output string) string {
	return fmt.Sprintf(`{"id":"x","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":%s,"output_tokens":%s}}`, input, output)
}

func anthropicUsageBoundaryResponse(input, output string) string {
	return fmt.Sprintf(`{"id":"msg","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":%s,"output_tokens":%s,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens_details":{"thinking_tokens":0}}}`, input, output)
}

func TestUsageWireBoundary(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		response string
		want     Usage
	}{
		{"chat negative fraction", "openai-chat", chatUsageBoundaryResponse("5", "-0.5"), Usage{InputTokens: 5, Incomplete: true}},
		{"chat huge exponent", "openai-chat", chatUsageBoundaryResponse("1e100", "7"), Usage{OutputTokens: 7, Incomplete: true}},
		{"chat negative integer", "openai-chat", chatUsageBoundaryResponse("5", "-1"), Usage{InputTokens: 5, Incomplete: true}},
		{"chat positive overflow", "openai-chat", chatUsageBoundaryResponse("9223372036854775808", "7"), Usage{OutputTokens: 7, Incomplete: true}},

		{"responses negative fraction", "openai-responses", responsesUsageBoundaryResponse("5", "-0.5"), Usage{InputTokens: 5, Incomplete: true}},
		{"responses huge exponent", "openai-responses", responsesUsageBoundaryResponse("1e100", "7"), Usage{OutputTokens: 7, Incomplete: true}},
		{"responses negative integer", "openai-responses", responsesUsageBoundaryResponse("5", "-1"), Usage{InputTokens: 5, Incomplete: true}},
		{"responses positive overflow", "openai-responses", responsesUsageBoundaryResponse("9223372036854775808", "7"), Usage{OutputTokens: 7, Incomplete: true}},

		{"anthropic negative fraction", "anthropic", anthropicUsageBoundaryResponse("5", "-0.5"), Usage{InputTokens: 5, Incomplete: true}},
		{"anthropic huge exponent", "anthropic", anthropicUsageBoundaryResponse("1e100", "7"), Usage{OutputTokens: 7, Incomplete: true}},
		{"anthropic negative integer", "anthropic", anthropicUsageBoundaryResponse("5", "-1"), Usage{InputTokens: 5, Incomplete: true}},
		{"anthropic positive overflow", "anthropic", anthropicUsageBoundaryResponse("9223372036854775808", "7"), Usage{OutputTokens: 7, Incomplete: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, calls := usageClient(t, tc.protocol, tc.response)
			resp, err := client.Generate(context.Background(), simpleRequest())
			usage, valid := ErrorUsage(err)
			if resp != nil || !IsUnsafeResponse(err) || calls.Load() != 1 || !valid || usage != tc.want {
				t.Fatalf("resp=%+v err=%v usage=%+v valid=%t calls=%d want=%+v", resp, err, usage, valid, calls.Load(), tc.want)
			}
		})
	}
}

func TestUsageWireResponsesFailedUnsafeUsage(t *testing.T) {
	client, calls := usageClient(t, "openai-responses", `{"id":"x","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"temporary"},"output":[],"usage":{"input_tokens":5,"output_tokens":-0.5}}`)
	resp, err := client.Generate(context.Background(), simpleRequest())
	usage, valid := ErrorUsage(err)
	if resp != nil || !IsUnsafeResponse(err) || calls.Load() != 1 || !valid || usage != (Usage{InputTokens: 5, Incomplete: true}) {
		t.Fatalf("resp=%+v err=%v usage=%+v valid=%t calls=%d", resp, err, usage, valid, calls.Load())
	}
}

func TestUsageWireKeepsValidIntegers(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		response string
		want     Usage
	}{
		{"chat", "openai-chat", chatUsageBoundaryResponse("5", "7"), Usage{InputTokens: 5, OutputTokens: 7}},
		{"responses", "openai-responses", responsesUsageBoundaryResponse("5", "7"), Usage{InputTokens: 5, OutputTokens: 7}},
		{"anthropic", "anthropic", anthropicUsageBoundaryResponse("5", "7"), Usage{InputTokens: 5, OutputTokens: 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, calls := usageClient(t, tc.protocol, tc.response)
			resp, err := client.Generate(context.Background(), simpleRequest())
			if err != nil || resp == nil || calls.Load() != 1 || resp.Usage != tc.want {
				t.Fatalf("resp=%+v err=%v calls=%d want=%+v", resp, err, calls.Load(), tc.want)
			}
		})
	}
}

func TestUsageWireMissingProviderFieldsMarkedIncomplete(t *testing.T) {
	chatNoUsage := `{"id":"x","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`
	responsesNoUsage := `{"id":"x","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]}`
	anthropicNoUsage := `{"id":"msg","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null}`
	cases := []struct {
		name     string
		protocol string
		response string
		want     Usage
	}{
		{"chat missing usage", "openai-chat", chatNoUsage, Usage{Incomplete: true}},
		{"chat empty usage", "openai-chat", `{"id":"x","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{}}`, Usage{Incomplete: true}},
		{"chat missing output core", "openai-chat", chatUsageBoundaryResponse("5", "null"), Usage{InputTokens: 5, Incomplete: true}},

		{"responses missing usage", "openai-responses", responsesNoUsage, Usage{Incomplete: true}},
		{"responses empty usage", "openai-responses", `{"id":"x","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{}}`, Usage{Incomplete: true}},
		{"responses missing output core", "openai-responses", responsesUsageBoundaryResponse("5", "null"), Usage{InputTokens: 5, Incomplete: true}},

		{"anthropic missing usage", "anthropic", anthropicNoUsage, Usage{Incomplete: true}},
		{"anthropic empty usage", "anthropic", `{"id":"msg","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{}}`, Usage{Incomplete: true}},
		{"anthropic missing input core", "anthropic", anthropicUsageBoundaryResponse("null", "7"), Usage{OutputTokens: 7, Incomplete: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, calls := usageClient(t, tc.protocol, tc.response)
			resp, err := client.Generate(context.Background(), simpleRequest())
			if err != nil || resp == nil || calls.Load() != 1 || resp.Usage != tc.want {
				t.Fatalf("resp=%+v err=%v calls=%d want=%+v", resp, err, calls.Load(), tc.want)
			}
		})
	}
}

func TestUsageWireDirectConstructionStaysComplete(t *testing.T) {
	if got, complete := wireUsage("", chatUsageFields); !complete || got != (Usage{}) {
		t.Fatalf("empty wire usage=%+v complete=%t", got, complete)
	}
	chat, complete := chatUsage(openai.CompletionUsage{PromptTokens: 5, CompletionTokens: 7}, "")
	if !complete || chat != (Usage{InputTokens: 5, OutputTokens: 7}) {
		t.Fatalf("chat usage=%+v complete=%t", chat, complete)
	}
	response, complete := responsesUsage(responses.ResponseUsage{InputTokens: 5, OutputTokens: 7}, "")
	if !complete || response != (Usage{InputTokens: 5, OutputTokens: 7}) {
		t.Fatalf("responses usage=%+v complete=%t", response, complete)
	}
	anthropicUsageValue, complete := anthropicUsage(anthropic.Usage{InputTokens: 5, OutputTokens: 7}, "")
	if !complete || anthropicUsageValue != (Usage{InputTokens: 5, OutputTokens: 7}) {
		t.Fatalf("anthropic usage=%+v complete=%t", anthropicUsageValue, complete)
	}
}
