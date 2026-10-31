package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
)

const safetySecret = "safety-secret-canary"

type safetyCall struct {
	id, name, input string
	omit            bool
}

// safetyResponse builds valid HTTP JSON even when a tool's input is malformed.
func safetyResponse(t *testing.T, protocol, stop string, refusal bool, calls []safetyCall, reasoning bool) []byte {
	t.Helper()
	var body map[string]any
	switch protocol {
	case "openai-chat":
		tools := []any{}
		for _, call := range calls {
			fn := map[string]any{"name": call.name}
			if !call.omit {
				fn["arguments"] = call.input
			}
			tools = append(tools, map[string]any{"id": call.id, "type": "function", "function": fn})
		}
		message := map[string]any{"role": "assistant", "tool_calls": tools}
		if refusal {
			message["refusal"] = "request refused"
		}
		body = map[string]any{"id": "chat-1", "object": "chat.completion", "model": "test", "choices": []any{map[string]any{"index": 0, "finish_reason": stop, "message": message}}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7}}
	case "openai-responses":
		output := []any{}
		if reasoning {
			output = append(output, map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}, "encrypted_content": "encrypted-safe"})
		}
		if refusal {
			output = append(output, map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "refusal", "refusal": "request refused"}}})
		}
		for _, call := range calls {
			fn := map[string]any{"type": "function_call", "id": "fc_" + call.id, "call_id": call.id, "name": call.name, "status": "completed"}
			if !call.omit {
				fn["arguments"] = call.input
			}
			output = append(output, fn)
		}
		status := "completed"
		if stop != "" {
			status = "incomplete"
		}
		body = map[string]any{"id": "resp-1", "object": "response", "created_at": 1, "model": "test", "status": status, "output": output, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7}}
		if stop != "" {
			body["incomplete_details"] = map[string]any{"reason": stop}
		}
	case "anthropic":
		content := []any{}
		if reasoning {
			content = append(content, map[string]any{"type": "thinking", "thinking": "thinking", "signature": "signature-safe"})
		}
		for _, call := range calls {
			tool := map[string]any{"type": "tool_use", "id": call.id, "name": call.name}
			if !call.omit {
				if json.Valid([]byte(call.input)) {
					tool["input"] = json.RawMessage(call.input)
				} else {
					tool["input"] = call.input
				}
			}
			content = append(content, tool)
		}
		body = map[string]any{"id": "msg-1", "type": "message", "role": "assistant", "model": "test", "content": content, "stop_reason": stop, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7}}
	default:
		t.Fatal("unknown protocol", protocol)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func safetyClient(t *testing.T, protocol string, replies ...[]byte) (llm.Client, *atomic.Int32, *[]llm.CallRecord, *[]string) {
	t.Helper()
	var count atomic.Int32
	var records []llm.CallRecord
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, string(request))
		idx := int(count.Add(1)) - 1
		if idx >= len(replies) {
			http.Error(w, "unexpected retry", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(replies[idx])
	}))
	t.Cleanup(server.Close)
	t.Setenv("PROVIDER_SAFETY_TEST_KEY", "safety-key-canary")
	reg, err := llm.NewRegistry(map[string]config.ProviderConfig{"test": {
		Protocol: protocol, Model: "test", BaseURL: server.URL, APIKeyEnv: "PROVIDER_SAFETY_TEST_KEY", MaxTokens: 512,
	}}, llm.WithRecorder(llm.RecorderFunc(func(_ context.Context, record llm.CallRecord) { records = append(records, record) })),
		llm.WithRetryBaseDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	client, err := reg.Client("test")
	if err != nil {
		t.Fatal(err)
	}
	return client, &count, &records, &requests
}

func safetyRun(client llm.Client) (*RunResult, int, int, error) {
	tools, submits := 0, 0
	spec := testSpec(client, echoTool{name: "echo", fn: func(context.Context, json.RawMessage) (Result, error) {
		tools++
		return Result{Content: "ok"}, nil
	}})
	spec.Submit.Decode = func(raw json.RawMessage) (any, error) {
		submits++
		return decodeObject(raw)
	}
	result, err := Run(context.Background(), spec)
	return result, tools, submits, err
}

func assertSafetyFailure(t *testing.T, result *RunResult, err error, tools, submits int, count *atomic.Int32, records []llm.CallRecord, stop llm.StopReason) {
	t.Helper()
	if !errors.Is(err, ErrUnsafeResponse) || tools != 0 || submits != 0 {
		t.Fatalf("unsafe response accepted: err=%v tool calls=%d submits=%d", err, tools, submits)
	}
	if count.Load() != 1 || len(records) != 1 {
		t.Fatalf("provider retried: requests=%d records=%d", count.Load(), len(records))
	}
	if result == nil || result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 || result.Turns != 1 {
		t.Fatalf("partial usage lost: %+v", result)
	}
	if records[0].Response == nil || records[0].Response.StopReason != stop {
		t.Fatalf("provider stop reason not preserved: %+v", records[0].Response)
	}
	for name, value := range map[string]any{"audit record": records[0], "transcript": result.Transcript} {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatalf("%s not serializable: %v", name, marshalErr)
		}
		if strings.Contains(string(raw), safetySecret) || strings.Contains(string(raw), "safety-key-canary") {
			t.Fatalf("%s leaked parameters or key", name)
		}
	}
	if strings.Contains(err.Error(), safetySecret) || strings.Contains(err.Error(), "safety-key-canary") {
		t.Fatal("error leaked parameters or key")
	}
}

func TestProviderSafetyInvalidToolInputs(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		for _, bad := range []struct {
			name, input string
			omit        bool
		}{
			{"empty", "", false},
			{"missing", "", true},
			{"malformed", `{"secret":"` + safetySecret, false},
			{"non-json-space", "\u00a0{}", false},
			{"null", "null", false},
			{"array", "[]", false},
		} {
			for _, badName := range []string{"echo", SubmitToolName} {
				for _, submitFirst := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/submit-first-%v", protocol, bad.name, badName, submitFirst), func(t *testing.T) {
						calls := []safetyCall{{"bad", badName, bad.input, bad.omit}, {"tool", "echo", `{"value":"` + safetySecret + `"}`, false}, {"ok", SubmitToolName, `{"value":"` + safetySecret + `"}`, false}}
						if submitFirst {
							calls[0], calls[2] = calls[2], calls[0]
						}
						stop := "tool_use"
						if protocol == "openai-chat" {
							stop = "tool_calls"
						} else if protocol == "openai-responses" {
							stop = ""
						}
						client, count, records, _ := safetyClient(t, protocol, safetyResponse(t, protocol, stop, false, calls, false))
						result, tools, submits, err := safetyRun(client)
						assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopToolUse)
					})
				}
			}
		}
	}
}

func TestProviderSafetyRefusalWithoutToolDoesNotRetry(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			stop, refusal := "refusal", false
			if protocol == "openai-chat" {
				stop, refusal = "stop", true
			} else if protocol == "openai-responses" {
				stop, refusal = "", true
			}
			client, count, records, _ := safetyClient(t, protocol, safetyResponse(t, protocol, stop, refusal, nil, false))
			result, tools, submits, err := safetyRun(client)
			assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopRefusal)
		})
	}
}

func TestResponsesIncompleteFunctionItemDoesNotExecute(t *testing.T) {
	for _, itemStatus := range []string{"incomplete", "in_progress"} {
		t.Run(itemStatus, func(t *testing.T) {
			calls := []safetyCall{{"tool", "echo", `{"value":"` + safetySecret + `"}`, false}, {"submit", SubmitToolName, `{}`, false}}
			raw := safetyResponse(t, "openai-responses", "", false, calls, false)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			body["output"].([]any)[0].(map[string]any)["status"] = itemStatus
			raw, _ = json.Marshal(body)
			client, count, records, _ := safetyClient(t, "openai-responses", raw)
			result, tools, submits, err := safetyRun(client)
			assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopOther)
		})
	}
}

func TestResponsesFailedStatusWithRefusalDoesNotRetry(t *testing.T) {
	calls := []safetyCall{{"submit", SubmitToolName, `{"value":"` + safetySecret + `"}`, false}}
	raw := safetyResponse(t, "openai-responses", "", true, calls, false)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["status"] = "failed"
	raw, _ = json.Marshal(body)
	client, count, records, _ := safetyClient(t, "openai-responses", raw)
	result, tools, submits, err := safetyRun(client)
	assertFailedProviderCall(t, result, err, tools, submits, count, *records, llm.StopRefusal)
}

func TestResponsesFailedStatusWithToolDoesNotRetry(t *testing.T) {
	calls := []safetyCall{{"submit", SubmitToolName, `{"value":"` + safetySecret, false}}
	raw := safetyResponse(t, "openai-responses", "", false, calls, false)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["status"] = "failed"
	raw, _ = json.Marshal(body)
	client, count, records, _ := safetyClient(t, "openai-responses", raw)
	result, tools, submits, err := safetyRun(client)
	assertFailedProviderCall(t, result, err, tools, submits, count, *records, llm.StopOther)
}

func assertFailedProviderCall(t *testing.T, result *RunResult, err error, tools, submits int, count *atomic.Int32, records []llm.CallRecord, stop ...llm.StopReason) {
	t.Helper()
	var failure *llm.Error
	if !errors.Is(err, ErrUnsafeResponse) || !errors.As(err, &failure) || failure.Kind != llm.ErrUnsafeResponse || failure.Retryable() || tools != 0 || submits != 0 {
		t.Fatalf("failed provider call accepted: err=%v tools=%d submits=%d", err, tools, submits)
	}
	if failure.ErrorCode() != "unsafe_model_response" {
		t.Fatalf("failed provider call has the wrong safety class: %s", failure.ErrorCode())
	}
	if count.Load() != 1 || len(records) != 1 || records[0].Audit == nil || records[0].Audit.Response == nil {
		t.Fatalf("failed provider call retried or lost its audit: requests=%d records=%+v", count.Load(), records)
	}
	if len(stop) > 0 && (records[0].Response == nil || records[0].Response.StopReason != stop[0] || records[0].Audit.Response.StopReason != string(stop[0])) {
		t.Fatalf("failed provider call lost its safe stop reason: records=%+v", records)
	}
	if result == nil || result.Turns != 1 || result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 || records[0].Audit.Response.Usage != result.Usage {
		t.Fatalf("partial usage lost: result=%+v audit=%+v", result, records[0].Audit.Response)
	}
	for _, value := range []any{records, result.Transcript} {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil || strings.Contains(string(raw), safetySecret) || strings.Contains(string(raw), "safety-key-canary") {
			t.Fatalf("failed provider call leaked data or is not serializable: %v", marshalErr)
		}
	}
	if strings.Contains(err.Error(), safetySecret) || strings.Contains(err.Error(), "safety-key-canary") {
		t.Fatal("error leaked parameters or key")
	}
}

func TestProviderSafetyBlockedStops(t *testing.T) {
	for _, tc := range []struct {
		protocol, stop string
		refusal        bool
		want           llm.StopReason
	}{
		{"openai-chat", "length", false, llm.StopMaxTokens},
		{"openai-chat", "content_filter", false, llm.StopRefusal},
		{"openai-chat", "tool_calls", true, llm.StopRefusal},
		{"openai-chat", "stop", false, llm.StopEndTurn},
		{"openai-responses", "max_output_tokens", false, llm.StopMaxTokens},
		{"openai-responses", "content_filter", false, llm.StopRefusal},
		{"openai-responses", "max_messages", false, llm.StopOther},
		{"openai-responses", "", true, llm.StopRefusal},
		{"anthropic", "max_tokens", false, llm.StopMaxTokens},
		{"anthropic", "model_context_window_exceeded", false, llm.StopMaxTokens},
		{"anthropic", "refusal", false, llm.StopRefusal},
		{"anthropic", "pause_turn", false, llm.StopOther},
		{"anthropic", "end_turn", false, llm.StopEndTurn},
	} {
		t.Run(fmt.Sprintf("%s/%s/refusal-%v", tc.protocol, tc.stop, tc.refusal), func(t *testing.T) {
			calls := []safetyCall{{"tool", "echo", `{"value":"` + safetySecret + `"}`, false}, {"submit", SubmitToolName, `{"value":"` + safetySecret + `"}`, false}}
			client, count, records, _ := safetyClient(t, tc.protocol, safetyResponse(t, tc.protocol, tc.stop, tc.refusal, calls, false))
			result, tools, submits, err := safetyRun(client)
			assertSafetyFailure(t, result, err, tools, submits, count, *records, tc.want)
		})
	}
}

func TestProviderSafetyValidMultiTurn(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			stop := "tool_use"
			if protocol == "openai-chat" {
				stop = "tool_calls"
			} else if protocol == "openai-responses" {
				stop = ""
			}
			first := safetyResponse(t, protocol, stop, false, []safetyCall{{"one", "echo", `{}`, false}, {"two", "echo", `{"value":"yes"}`, false}}, true)
			second := safetyResponse(t, protocol, stop, false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client, count, _, requests := safetyClient(t, protocol, first, second)
			result, tools, submits, err := safetyRun(client)
			if err != nil || tools != 2 || submits != 1 || count.Load() != 2 {
				t.Fatalf("valid run: err=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
			}
			if result.Usage.InputTokens != 22 || result.Usage.OutputTokens != 14 {
				t.Fatalf("usage = %+v", result.Usage)
			}
			if protocol == "openai-responses" && !strings.Contains((*requests)[1], `"encrypted_content":"encrypted-safe"`) {
				t.Fatal("encrypted reasoning was not sent back")
			}
			if protocol == "anthropic" && !strings.Contains((*requests)[1], `"signature":"signature-safe"`) {
				t.Fatal("signed thinking was not sent back")
			}
		})
	}
}

func TestProviderSafetyAuditRedactsValidToolArguments(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			stop := "tool_use"
			if protocol == "openai-chat" {
				stop = "tool_calls"
			} else if protocol == "openai-responses" {
				stop = ""
			}
			first := safetyResponse(t, protocol, stop, false, []safetyCall{{"tool", "echo", `{"value":"` + safetySecret + `"}`, false}}, false)
			second := safetyResponse(t, protocol, stop, false, []safetyCall{{"submit", SubmitToolName, `{"value":"` + safetySecret + `"}`, false}}, false)
			client, _, records, _ := safetyClient(t, protocol, first, second)
			result, tools, submits, err := safetyRun(client)
			if err != nil || tools != 1 || submits != 1 {
				t.Fatalf("valid run failed: %v tools=%d submits=%d", err, tools, submits)
			}
			transcript, err := json.Marshal(result.Transcript)
			if err != nil || !strings.Contains(string(transcript), safetySecret) {
				t.Fatal("audit redaction modified the live transcript")
			}
			for _, record := range *records {
				raw, marshalErr := json.Marshal(record)
				if marshalErr != nil || strings.Contains(string(raw), safetySecret) {
					t.Fatalf("audit leaked tool arguments or could not serialize: %v", marshalErr)
				}
			}
		})
	}
}

func TestProviderSafetyRetainsUsageFromEarlierToolTurn(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			goodStop, badStop := "tool_use", "max_tokens"
			if protocol == "openai-chat" {
				goodStop, badStop = "tool_calls", "length"
			} else if protocol == "openai-responses" {
				goodStop, badStop = "", "max_output_tokens"
			}
			first := safetyResponse(t, protocol, goodStop, false, []safetyCall{{"one", "echo", `{}`, false}}, false)
			second := safetyResponse(t, protocol, badStop, false, []safetyCall{{"submit", SubmitToolName, `{"value":"` + safetySecret + `"}`, false}}, false)
			client, count, records, _ := safetyClient(t, protocol, first, second)
			result, tools, submits, err := safetyRun(client)
			if !errors.Is(err, ErrUnsafeResponse) || tools != 1 || submits != 0 || count.Load() != 2 || len(*records) != 2 {
				t.Fatalf("truncated second turn: error=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
			}
			if result == nil || result.Turns != 2 || result.Usage.InputTokens != 22 || result.Usage.OutputTokens != 14 {
				t.Fatalf("partial usage = %+v", result)
			}
			raw, marshalErr := json.Marshal((*records)[1])
			if marshalErr != nil || strings.Contains(string(raw), safetySecret) {
				t.Fatalf("rejected turn audit is unsafe: %v", marshalErr)
			}
		})
	}
}

func TestProviderSafetyDuplicateSubmitStillAcceptsOnlyFirst(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			stop := "tool_use"
			if protocol == "openai-chat" {
				stop = "tool_calls"
			} else if protocol == "openai-responses" {
				stop = ""
			}
			calls := []safetyCall{{"s1", SubmitToolName, `{"answer":"first"}`, false}, {"s2", SubmitToolName, `{"answer":"second"}`, false}}
			client, count, _, _ := safetyClient(t, protocol, safetyResponse(t, protocol, stop, false, calls, false))
			result, tools, submits, err := safetyRun(client)
			if err != nil || tools != 0 || submits != 1 || count.Load() != 1 || result == nil || result.Output.(map[string]any)["answer"] != "first" {
				t.Fatalf("duplicate submit changed: error=%v tools=%d submits=%d result=%+v", err, tools, submits, result)
			}
			if !result.Transcript[2].Parts[1].ToolResult.IsError {
				t.Fatal("second submit was not rejected")
			}
		})
	}
}

func TestSafetyCustomClientInvalidInputIsSerializable(t *testing.T) {
	response := toolResponse(llm.StopToolUse, call("tool", "echo", `{"secret":"`+safetySecret))
	response.Usage = llm.Usage{InputTokens: 11, OutputTokens: 7}
	client := &fakeClient{replies: []*llm.Response{response}}
	result, tools, submits, err := safetyRun(client)
	if err == nil || tools != 0 || submits != 0 || result == nil {
		t.Fatalf("unsafe custom response accepted: err=%v result=%+v", err, result)
	}
	raw, marshalErr := json.Marshal(result.Transcript)
	if marshalErr != nil || strings.Contains(string(raw), safetySecret) {
		t.Fatalf("unsafe transcript: marshal error=%v", marshalErr)
	}
}

func TestSafetySubmitValidationDoesNotEchoParameters(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic-%v", panics), func(t *testing.T) {
			response := toolResponse(llm.StopToolUse, call("submit", SubmitToolName, `{"value":"`+safetySecret+`"}`))
			client := &fakeClient{replies: []*llm.Response{response, response, response}}
			spec := testSpec(client)
			spec.Submit.Decode = func(raw json.RawMessage) (any, error) {
				if panics {
					panic(string(raw))
				}
				return nil, fmt.Errorf("invalid input: %s", raw)
			}
			result, err := Run(context.Background(), spec)
			if err == nil || strings.Contains(err.Error(), safetySecret) {
				t.Fatal("submission error leaked input or was accepted")
			}
			for _, message := range result.Transcript {
				for _, part := range message.Parts {
					if part.ToolResult != nil && strings.Contains(part.ToolResult.Content, safetySecret) {
						t.Fatal("submission feedback leaked input")
					}
				}
			}
		})
	}
}

func chatToolCallResponse(t *testing.T, calls ...any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id": "chat-1", "object": "chat.completion", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{"role": "assistant", "tool_calls": calls},
		}},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProviderSafetyChatUnknownToolTypesCannotSubmit(t *testing.T) {
	const idCanary = "unknown-call-id-canary"
	for _, invalid := range []map[string]any{
		{"id": idCanary, "type": "custom", "custom": map[string]any{"name": "echo", "input": safetySecret}},
		{"id": idCanary, "function": map[string]any{"name": "echo", "arguments": `{"value":"` + safetySecret + `"}`}},
	} {
		for _, submitFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("type-%q/submit-first-%v", invalid["type"], submitFirst), func(t *testing.T) {
				submit := map[string]any{"id": "submit-1", "type": "function", "function": map[string]any{
					"name": SubmitToolName, "arguments": `{"value":"` + safetySecret + `"}`,
				}}
				calls := []any{invalid, submit}
				if submitFirst {
					calls[0], calls[1] = calls[1], calls[0]
				}
				client, count, records, _ := safetyClient(t, "openai-chat", chatToolCallResponse(t, calls...))
				result, tools, submits, err := safetyRun(client)
				assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopToolUse)
				if strings.Contains(err.Error(), idCanary) {
					t.Fatal("error echoed an attacker-controlled call id")
				}
			})
		}
	}
}

func TestProviderSafetyMissingOrDuplicateToolIdentity(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		for _, tc := range []struct {
			name  string
			calls []safetyCall
		}{
			{"missing-id", []safetyCall{{"", "echo", `{}`, false}, {"s1", SubmitToolName, `{"value":"` + safetySecret + `"}`, false}}},
			{"missing-name", []safetyCall{{"c1", "", `{}`, false}, {"s1", SubmitToolName, `{"value":"` + safetySecret + `"}`, false}}},
			{"duplicate-id", []safetyCall{{"c1", "echo", `{}`, false}, {"c1", SubmitToolName, `{"value":"` + safetySecret + `"}`, false}}},
		} {
			for _, submitFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/submit-first-%v", protocol, tc.name, submitFirst), func(t *testing.T) {
					calls := append([]safetyCall(nil), tc.calls...)
					if submitFirst {
						calls[0], calls[1] = calls[1], calls[0]
					}
					stop := "tool_use"
					if protocol == "openai-chat" {
						stop = "tool_calls"
					} else if protocol == "openai-responses" {
						stop = ""
					}
					client, count, records, _ := safetyClient(t, protocol, safetyResponse(t, protocol, stop, false, calls, false))
					result, tools, submits, err := safetyRun(client)
					assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopToolUse)
					if strings.Contains(err.Error(), "c1") || strings.Contains(err.Error(), "s1") {
						t.Fatal("error echoed an attacker-controlled call identity")
					}
				})
			}
		}
	}
}

func TestSafetyToolErrorDoesNotEchoInput(t *testing.T) {
	first := safetyResponse(t, "openai-chat", "tool_calls", false, []safetyCall{{"c1", "echo", `{"value":"` + safetySecret + `"}`, false}}, false)
	second := safetyResponse(t, "openai-chat", "tool_calls", false, []safetyCall{{"s1", SubmitToolName, `{}`, false}}, false)
	client, _, _, _ := safetyClient(t, "openai-chat", first, second)
	spec := testSpec(client, echoTool{name: "echo", fn: func(context.Context, json.RawMessage) (Result, error) {
		return Result{}, fmt.Errorf("failed for %s", safetySecret)
	}})
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	part := result.Transcript[2].Parts[0].ToolResult
	if !part.IsError || !strings.Contains(part.Content, "tool execution failed") || strings.Contains(part.Content, safetySecret) {
		t.Fatalf("tool error feedback = %#v", part)
	}
}

const chatNoChoices = `{"id":"empty","object":"chat.completion","created":1,"model":"test","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

func TestProviderSafetyChatRetryUsageAccumulates(t *testing.T) {
	ok := safetyResponse(t, "openai-chat", "tool_calls", false, []safetyCall{{"s1", SubmitToolName, `{}`, false}}, false)
	client, count, records, _ := safetyClient(t, "openai-chat", []byte(chatNoChoices), ok)
	result, tools, submits, err := safetyRun(client)
	if err != nil || tools != 0 || submits != 1 || count.Load() != 2 {
		t.Fatalf("run: err=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
	}
	if result.Usage.InputTokens != 22 || result.Usage.OutputTokens != 14 {
		t.Fatalf("usage = %+v, want both attempts", result.Usage)
	}
	if len(*records) != 2 || (*records)[0].Audit == nil || (*records)[0].Audit.Response == nil ||
		(*records)[0].Audit.Response.Usage.InputTokens != 11 ||
		(*records)[1].Audit == nil || (*records)[1].Audit.Response == nil ||
		(*records)[1].Audit.Response.Usage.InputTokens != 11 {
		t.Fatalf("per-attempt audit usage was not preserved: %+v", *records)
	}
}

func TestProviderSafetyChatRetryExhaustionKeepsUsage(t *testing.T) {
	replies := make([][]byte, 5)
	for i := range replies {
		replies[i] = []byte(chatNoChoices)
	}
	client, count, records, _ := safetyClient(t, "openai-chat", replies...)
	result, tools, submits, err := safetyRun(client)
	if err == nil || tools != 0 || submits != 0 || count.Load() != 5 {
		t.Fatalf("run: err=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
	}
	if result == nil || result.Usage.InputTokens != 55 || result.Usage.OutputTokens != 35 {
		t.Fatalf("partial usage = %+v, want every attempt", result)
	}
	if len(*records) != 5 {
		t.Fatalf("records = %d, want one per attempt", len(*records))
	}
	for _, record := range *records {
		if record.Audit == nil || record.Audit.Response == nil ||
			record.Audit.Response.Usage.InputTokens != 11 || record.Audit.Response.Usage.OutputTokens != 7 {
			t.Fatalf("attempt audit usage = %+v", record.Audit)
		}
	}
}

func TestProviderSafetyResponsesFailedRetryUsageAccumulates(t *testing.T) {
	failed := safetyResponse(t, "openai-responses", "", false, nil, false)
	var body map[string]any
	if err := json.Unmarshal(failed, &body); err != nil {
		t.Fatal(err)
	}
	body["status"] = "failed"
	body["error"] = map[string]any{"code": "server_error"}
	failed, _ = json.Marshal(body)
	ok := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"s1", SubmitToolName, `{}`, false}}, false)
	client, count, records, _ := safetyClient(t, "openai-responses", failed, ok)
	result, tools, submits, err := safetyRun(client)
	if err != nil || tools != 0 || submits != 1 || count.Load() != 2 {
		t.Fatalf("run: err=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
	}
	if result.Usage.InputTokens != 22 || result.Usage.OutputTokens != 14 {
		t.Fatalf("usage = %+v, want both attempts", result.Usage)
	}
	if len(*records) != 2 || (*records)[0].Audit == nil || (*records)[0].Audit.Response == nil ||
		(*records)[0].Audit.Response.Usage.InputTokens != 11 ||
		(*records)[1].Audit == nil || (*records)[1].Audit.Response == nil ||
		(*records)[1].Audit.Response.Usage.InputTokens != 11 {
		t.Fatalf("per-attempt audit usage was not preserved: %+v", *records)
	}
}

func TestProviderSafetyResponsesRetryExhaustionKeepsUsage(t *testing.T) {
	failed := safetyResponse(t, "openai-responses", "", false, nil, false)
	var body map[string]any
	if err := json.Unmarshal(failed, &body); err != nil {
		t.Fatal(err)
	}
	body["status"] = "failed"
	body["error"] = map[string]any{"code": "server_error"}
	failed, _ = json.Marshal(body)
	replies := make([][]byte, 5)
	for i := range replies {
		replies[i] = failed
	}
	client, count, records, _ := safetyClient(t, "openai-responses", replies...)
	result, tools, submits, err := safetyRun(client)
	if err == nil || tools != 0 || submits != 0 || count.Load() != 5 {
		t.Fatalf("run: err=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
	}
	if result == nil || result.Usage.InputTokens != 55 || result.Usage.OutputTokens != 35 {
		t.Fatalf("partial usage = %+v, want every attempt", result)
	}
	if len(*records) != 5 {
		t.Fatalf("records = %d, want one per attempt", len(*records))
	}
	for _, record := range *records {
		if record.Audit == nil || record.Audit.Response == nil ||
			record.Audit.Response.Usage.InputTokens != 11 || record.Audit.Response.Usage.OutputTokens != 7 {
			t.Fatalf("attempt audit usage = %+v", record.Audit)
		}
	}
}

func TestProviderSafetyAuditOmitsRefusalText(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses"} {
		t.Run(protocol, func(t *testing.T) {
			stop, refusal := "refusal", false
			if protocol == "openai-chat" {
				stop, refusal = "stop", true
			} else {
				refusal = true
			}
			raw := safetyResponse(t, protocol, stop, refusal, nil, false)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			switch protocol {
			case "openai-chat":
				message := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
				message["refusal"] = safetySecret
			case "openai-responses":
				content := body["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
				content["refusal"] = safetySecret
			}
			raw, _ = json.Marshal(body)
			client, count, records, _ := safetyClient(t, protocol, raw)
			result, tools, submits, err := safetyRun(client)
			if !errors.Is(err, ErrUnsafeResponse) || tools != 0 || submits != 0 || count.Load() != 1 {
				t.Fatalf("refusal accepted: err=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
			}
			if result == nil || result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 {
				t.Fatalf("partial usage = %+v", result)
			}
			for _, record := range *records {
				encoded, marshalErr := json.Marshal(record)
				if marshalErr != nil || strings.Contains(string(encoded), safetySecret) {
					t.Fatalf("refusal text reached the audit record: %v", marshalErr)
				}
			}
		})
	}
}

func TestProviderSafetyAuditOmitsToolOutput(t *testing.T) {
	first := safetyResponse(t, "openai-chat", "tool_calls", false, []safetyCall{{"c1", "echo", `{}`, false}}, false)
	second := safetyResponse(t, "openai-chat", "tool_calls", false, []safetyCall{{"s1", SubmitToolName, `{}`, false}}, false)
	client, count, records, requests := safetyClient(t, "openai-chat", first, second)
	spec := testSpec(client, echoTool{name: "echo", fn: func(context.Context, json.RawMessage) (Result, error) {
		return Result{Content: safetySecret}, nil
	}})
	spec.Submit.Decode = decodeObject
	_, err := Run(context.Background(), spec)
	if err != nil || count.Load() != 2 {
		t.Fatalf("Run: err=%v requests=%d", err, count.Load())
	}
	if !strings.Contains((*requests)[1], safetySecret) {
		t.Fatal("the live request lost tool output")
	}
	for _, record := range *records {
		encoded, marshalErr := json.Marshal(record)
		if marshalErr != nil || strings.Contains(string(encoded), safetySecret) {
			t.Fatalf("tool output reached the audit record: %v", marshalErr)
		}
	}
}
