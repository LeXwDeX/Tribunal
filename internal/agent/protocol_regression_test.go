package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

func protocolBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func protocolJSON(t *testing.T, body map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProtocolRegressionOpenAIArgumentWireTypes(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses"} {
		for _, value := range []struct {
			name string
			raw  any
		}{
			{"object", map[string]any{"secret": safetySecret}},
			{"array", []any{}}, {"number", 1}, {"bool", true}, {"null", nil},
		} {
			for _, submitFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/submit-first-%v", protocol, value.name, submitFirst), func(t *testing.T) {
					stop := ""
					if protocol == "openai-chat" {
						stop = "tool_calls"
					}
					body := protocolBody(t, safetyResponse(t, protocol, stop, false, []safetyCall{
						{"tool", "echo", `{}`, false}, {"submit", SubmitToolName, `{}`, false},
					}, false))
					if protocol == "openai-chat" {
						message := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
						calls := message["tool_calls"].([]any)
						calls[0].(map[string]any)["function"].(map[string]any)["arguments"] = value.raw
						if submitFirst {
							calls[0], calls[1] = calls[1], calls[0]
						}
					} else {
						calls := body["output"].([]any)
						calls[0].(map[string]any)["arguments"] = value.raw
						if submitFirst {
							calls[0], calls[1] = calls[1], calls[0]
						}
					}
					client, count, records, _ := safetyClient(t, protocol, protocolJSON(t, body))
					result, tools, submits, err := safetyRun(client)
					assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopToolUse)
				})
			}
		}
	}
}

func TestProtocolRegressionIdentityWireTypes(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		for _, field := range []string{"id", "name"} {
			for _, value := range []struct {
				name string
				raw  any
			}{{"number", 1}, {"object", map[string]any{"secret": safetySecret}}} {
				t.Run(fmt.Sprintf("%s/%s/%s", protocol, field, value.name), func(t *testing.T) {
					stop, container := "", "output"
					if protocol == "openai-chat" {
						stop = "tool_calls"
					} else if protocol == "anthropic" {
						stop, container = "tool_use", "content"
					}
					body := protocolBody(t, safetyResponse(t, protocol, stop, false, []safetyCall{
						{"tool", "echo", `{}`, false}, {"submit", SubmitToolName, `{}`, false},
					}, false))
					if protocol == "openai-chat" {
						message := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
						item := message["tool_calls"].([]any)[0].(map[string]any)
						if field == "id" {
							item["id"] = value.raw
						} else {
							item["function"].(map[string]any)["name"] = value.raw
						}
					} else {
						item := body[container].([]any)[0].(map[string]any)
						if protocol == "openai-responses" && field == "id" {
							item["call_id"] = value.raw
						} else {
							item[field] = value.raw
						}
					}
					client, count, records, _ := safetyClient(t, protocol, protocolJSON(t, body))
					result, tools, submits, err := safetyRun(client)
					assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopToolUse)
				})
			}
		}
	}
}

func TestProtocolRegressionChatMalformedAndLegacyCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		calls  any
		legacy any
		stop   string
	}{
		{"object-list", map[string]any{"input": safetySecret}, nil, "stop"},
		{"string-list", safetySecret, nil, "stop"},
		{"number-list", 1, nil, "stop"},
		{"bad-array-element", []any{nil}, nil, "tool_calls"},
		{"legacy-mixed", []any{map[string]any{"type": "function", "id": "submit", "function": map[string]any{"name": SubmitToolName, "arguments": `{}`}}}, map[string]any{"name": "echo", "arguments": `{"secret":"` + safetySecret + `"}`}, "tool_calls"},
		{"legacy-only", nil, map[string]any{"name": "echo", "arguments": `{"secret":"` + safetySecret + `"}`}, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := protocolBody(t, safetyResponse(t, "openai-chat", tc.stop, false, nil, false))
			message := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			if tc.calls == nil {
				delete(message, "tool_calls")
			} else {
				message["tool_calls"] = tc.calls
			}
			if tc.legacy != nil {
				message["function_call"] = tc.legacy
			}
			good := safetyResponse(t, "openai-chat", "tool_calls", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client, count, records, _ := safetyClient(t, "openai-chat", protocolJSON(t, body), good)
			result, tools, submits, err := safetyRun(client)
			stop := llm.StopToolUse
			if tc.stop == "stop" {
				stop = llm.StopEndTurn
			}
			assertSafetyFailure(t, result, err, tools, submits, count, *records, stop)
		})
	}
}

func TestProtocolRegressionUnsupportedCallsMixedWithSubmit(t *testing.T) {
	for _, tc := range []struct {
		protocol, kind string
	}{
		{"openai-responses", "custom_tool_call"},
		{"openai-responses", "web_search_call"},
		{"openai-responses", "mcp_call"},
		{"openai-responses", "future_tool_call"},
		{"anthropic", "server_tool_use"},
		{"anthropic", "future_tool_use"},
	} {
		for _, submitFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/submit-first-%v", tc.protocol, tc.kind, submitFirst), func(t *testing.T) {
				stop, field := "", "output"
				if tc.protocol == "anthropic" {
					stop, field = "tool_use", "content"
				}
				body := protocolBody(t, safetyResponse(t, tc.protocol, stop, false, []safetyCall{
					{"tool", "echo", `{"secret":"` + safetySecret + `"}`, false}, {"submit", SubmitToolName, `{}`, false},
				}, true))
				unsupported := map[string]any{"type": tc.kind, "id": "hidden-id", "call_id": "hidden-call", "name": "echo", "input": "arbitrary free text " + safetySecret}
				items := body[field].([]any)
				if submitFirst {
					items = append([]any{items[len(items)-1], unsupported}, items[:len(items)-1]...)
				} else {
					items = append([]any{unsupported}, items...)
				}
				body[field] = items
				client, count, records, _ := safetyClient(t, tc.protocol, protocolJSON(t, body))
				result, tools, submits, err := safetyRun(client)
				assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopToolUse)
			})
		}
	}
}

func TestProtocolRegressionUnsupportedFunctionContexts(t *testing.T) {
	for _, tc := range []struct {
		protocol, name, field string
		value                 any
	}{
		{"openai-responses", "program-caller", "caller", map[string]any{"type": "program", "caller_id": "program-1"}},
		{"openai-responses", "unknown-caller", "caller", map[string]any{"type": "future"}},
		{"openai-responses", "async", "async", true},
		{"openai-responses", "namespace", "namespace", "other"},
		{"anthropic", "server-caller", "caller", map[string]any{"type": "code_execution_20250825", "tool_id": "server-1"}},
		{"anthropic", "unknown-caller", "caller", map[string]any{"type": "future"}},
		{"anthropic", "toolset", "toolset_name", "other"},
	} {
		for _, submitFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/submit-first-%v", tc.protocol, tc.name, submitFirst), func(t *testing.T) {
				stop, field := "", "output"
				if tc.protocol == "anthropic" {
					stop, field = "tool_use", "content"
				}
				body := protocolBody(t, safetyResponse(t, tc.protocol, stop, false, []safetyCall{
					{"tool", "echo", `{"secret":"` + safetySecret + `"}`, false}, {"submit", SubmitToolName, `{}`, false},
				}, false))
				items := body[field].([]any)
				items[0].(map[string]any)[tc.field] = tc.value
				if submitFirst {
					items[0], items[1] = items[1], items[0]
				}
				client, count, records, _ := safetyClient(t, tc.protocol, protocolJSON(t, body))
				result, tools, submits, err := safetyRun(client)
				assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopToolUse)
			})
		}
	}
}

func TestProtocolRegressionDirectFunctionCaller(t *testing.T) {
	for _, protocol := range []string{"openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			stop, field := "", "output"
			if protocol == "anthropic" {
				stop, field = "tool_use", "content"
			}
			body := protocolBody(t, safetyResponse(t, protocol, stop, false, []safetyCall{
				{"tool", "echo", `{}`, false}, {"submit", SubmitToolName, `{}`, false},
			}, true))
			for _, item := range body[field].([]any) {
				entry := item.(map[string]any)
				if entry["type"] == "function_call" || entry["type"] == "tool_use" {
					entry["caller"] = map[string]any{"type": "direct"}
				}
			}
			client, count, _, _ := safetyClient(t, protocol, protocolJSON(t, body))
			result, tools, submits, err := safetyRun(client)
			if err != nil || result == nil || tools != 1 || submits != 1 || count.Load() != 1 {
				t.Fatalf("valid direct caller rejected: err=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
			}
		})
	}
}

func TestProtocolRegressionMalformedOutputListDoesNotRetry(t *testing.T) {
	for _, protocol := range []string{"openai-responses", "anthropic"} {
		for _, invalid := range []struct {
			name  string
			value any
		}{
			{"object", map[string]any{"type": "tool_use", "input": safetySecret}},
			{"string", safetySecret},
			{"null", nil},
			{"missing", nil},
		} {
			t.Run(protocol+"/"+invalid.name, func(t *testing.T) {
				stop, field := "", "output"
				if protocol == "anthropic" {
					stop, field = "end_turn", "content"
				}
				body := protocolBody(t, safetyResponse(t, protocol, stop, false, nil, false))
				body[field] = invalid.value
				if invalid.name == "missing" {
					delete(body, field)
				}
				if protocol == "openai-responses" {
					body["status"] = "failed"
				}
				goodStop := ""
				if protocol == "anthropic" {
					goodStop = "tool_use"
				}
				good := safetyResponse(t, protocol, goodStop, false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
				client, count, records, _ := safetyClient(t, protocol, protocolJSON(t, body), good)
				result, tools, submits, err := safetyRun(client)
				if protocol == "openai-responses" {
					assertFailedProviderCall(t, result, err, tools, submits, count, *records)
				} else {
					assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopEndTurn)
				}
			})
		}
	}
}

func TestProtocolRegressionResponsesIncompleteSibling(t *testing.T) {
	for _, kind := range []string{"message", "reasoning"} {
		for _, status := range []string{"incomplete", "in_progress"} {
			for _, submitFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/submit-first-%v", kind, status, submitFirst), func(t *testing.T) {
					body := protocolBody(t, safetyResponse(t, "openai-responses", "", false, []safetyCall{
						{"tool", "echo", `{"secret":"` + safetySecret + `"}`, false}, {"submit", SubmitToolName, `{}`, false},
					}, false))
					item := map[string]any{"type": kind, "id": "partial", "status": status}
					if kind == "message" {
						item["role"], item["content"] = "assistant", []any{map[string]any{"type": "output_text", "text": "partial"}}
					} else {
						item["summary"], item["encrypted_content"] = []any{}, "encrypted"
					}
					items := body["output"].([]any)
					if submitFirst {
						items[0], items[1] = items[1], items[0]
					}
					body["output"] = append([]any{item}, items...)
					client, count, records, _ := safetyClient(t, "openai-responses", protocolJSON(t, body))
					result, tools, submits, err := safetyRun(client)
					assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopOther)
				})
			}
		}
	}
}

func TestProtocolRegressionResponsesUnsupportedMessageContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content any
	}{
		{"unknown", []any{map[string]any{"type": "future_tool_call", "input": safetySecret}}},
		{"malformed", map[string]any{"type": "future_tool_call", "input": safetySecret}},
		{"null", nil},
		{"missing", nil},
	} {
		for _, submitFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/submit-first-%v", tc.name, submitFirst), func(t *testing.T) {
				body := protocolBody(t, safetyResponse(t, "openai-responses", "", false, []safetyCall{
					{"tool", "echo", `{"secret":"` + safetySecret + `"}`, false}, {"submit", SubmitToolName, `{}`, false},
				}, false))
				message := map[string]any{"type": "message", "id": "msg", "role": "assistant", "status": "completed", "content": tc.content}
				if tc.name == "missing" {
					delete(message, "content")
				}
				items := body["output"].([]any)
				if submitFirst {
					items[0], items[1] = items[1], items[0]
				}
				body["output"] = append([]any{message}, items...)
				client, count, records, _ := safetyClient(t, "openai-responses", protocolJSON(t, body))
				result, tools, submits, err := safetyRun(client)
				assertSafetyFailure(t, result, err, tools, submits, count, *records, llm.StopToolUse)
			})
		}
	}
}

func TestProtocolRegressionResponsesFailedCallDirectClient(t *testing.T) {
	for _, tc := range []struct {
		name     string
		calls    []safetyCall
		addition map[string]any
		refusal  bool
		code     string
	}{
		{"function", []safetyCall{{"submit", SubmitToolName, `{"secret":"` + safetySecret + `"}`, false}}, nil, false, ""},
		{"refusal-and-function", []safetyCall{{"submit", SubmitToolName, `{}`, false}}, nil, true, ""},
		{"refusal-only", nil, nil, true, "server_error"},
		{"custom", nil, map[string]any{"type": "custom_tool_call", "id": "custom", "input": safetySecret}, false, ""},
		{"server-error-and-function", []safetyCall{{"submit", SubmitToolName, `{}`, false}}, nil, false, "server_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := protocolBody(t, safetyResponse(t, "openai-responses", "", tc.refusal, tc.calls, false))
			body["status"] = "failed"
			if tc.addition != nil {
				body["output"] = append(body["output"].([]any), tc.addition)
			}
			if tc.code != "" {
				body["error"] = map[string]any{"code": tc.code, "message": safetySecret}
			}
			good := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client, count, records, _ := safetyClient(t, "openai-responses", protocolJSON(t, body), good)
			resp, err := client.Generate(context.Background(), &llm.Request{
				Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "test"}}}},
				Tools:    []llm.ToolDef{{Name: SubmitToolName}}, ToolChoice: SubmitToolName,
			})
			var failure *llm.Error
			if resp != nil || !errors.As(err, &failure) || failure.Retryable() || count.Load() != 1 || len(*records) != 1 {
				t.Fatalf("failed call returned success or retried: resp=%+v err=%v requests=%d records=%d", resp, err, count.Load(), len(*records))
			}
			if failure.Usage.InputTokens != 11 || failure.Usage.OutputTokens != 7 {
				t.Fatalf("usage lost: %+v", failure.Usage)
			}
			wantStop := llm.StopOther
			if tc.refusal {
				wantStop = llm.StopRefusal
			}
			record := (*records)[0]
			if record.Response == nil || record.Response.StopReason != wantStop || record.Response.Usage != failure.Usage || record.Response.RequestID != "" || len(record.Response.Message.Parts) != 0 || record.Audit == nil || record.Audit.Response == nil || record.Audit.Response.StopReason != string(wantStop) {
				t.Fatalf("safe failed-call metadata lost or exposed content: %+v", record)
			}
			raw, marshalErr := json.Marshal(*records)
			if marshalErr != nil || strings.Contains(string(raw), safetySecret) || strings.Contains(err.Error(), safetySecret) || strings.Contains(failure.Unwrap().Error(), safetySecret) {
				t.Fatalf("failed response leaked data or is not serializable: %v", marshalErr)
			}
		})
	}
}

func TestProtocolRegressionResponsesFailedPolicyDoesNotRetry(t *testing.T) {
	for _, code := range []string{"bio_policy", "misalignment_policy_violation", "image_content_policy_violation", "content_filter", "future_error"} {
		t.Run(code, func(t *testing.T) {
			body := protocolBody(t, safetyResponse(t, "openai-responses", "", false, nil, false))
			body["status"] = "failed"
			body["error"] = map[string]any{"code": code, "message": safetySecret}
			good := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client, count, records, _ := safetyClient(t, "openai-responses", protocolJSON(t, body), good)
			result, tools, submits, err := safetyRun(client)
			assertFailedProviderCall(t, result, err, tools, submits, count, *records)
		})
	}
}

func TestProtocolRegressionResponsesUnclassifiedFailureDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		error any
	}{
		{"omitted", nil}, {"null", nil}, {"empty-object", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := protocolBody(t, safetyResponse(t, "openai-responses", "", false, nil, false))
			body["status"] = "failed"
			if tc.name != "omitted" {
				body["error"] = tc.error
			}
			good := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client, count, records, _ := safetyClient(t, "openai-responses", protocolJSON(t, body), good)
			result, tools, submits, err := safetyRun(client)
			assertFailedProviderCall(t, result, err, tools, submits, count, *records)
		})
	}
}

func TestProtocolRegressionResponsesRetryableCodeConflictingSafety(t *testing.T) {
	for _, extra := range []map[string]any{
		{"type": "misalignment_policy_violation"},
		{"misalignment": map[string]any{"detailed_explanation": safetySecret}},
	} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			body := protocolBody(t, safetyResponse(t, "openai-responses", "", false, nil, false))
			body["status"] = "failed"
			errorBody := map[string]any{"code": "server_error", "message": safetySecret}
			for key, value := range extra {
				errorBody[key] = value
			}
			body["error"] = errorBody
			good := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client, count, records, _ := safetyClient(t, "openai-responses", protocolJSON(t, body), good)
			result, tools, submits, err := safetyRun(client)
			assertFailedProviderCall(t, result, err, tools, submits, count, *records)
		})
	}
}

func TestProtocolRegressionFailedFinalTurnDoesNotFallback(t *testing.T) {
	body := protocolBody(t, safetyResponse(t, "openai-responses", "", false, []safetyCall{
		{"submit", SubmitToolName, `{"secret":"` + safetySecret + `"}`, false},
	}, false))
	body["status"] = "failed"
	good := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
	client, count, records, _ := safetyClient(t, "openai-responses", protocolJSON(t, body), good)
	spec := testSpec(client)
	spec.MaxTurns = 1
	result, err := Run(context.Background(), spec)
	assertFailedProviderCall(t, result, err, 0, 0, count, *records)
}

func TestProtocolRegressionResponsesKnownRetryableFailure(t *testing.T) {
	for _, code := range []string{"server_error", "rate_limit_exceeded"} {
		t.Run(code, func(t *testing.T) {
			body := protocolBody(t, safetyResponse(t, "openai-responses", "", false, nil, false))
			body["status"] = "failed"
			errorType := code
			if code == "rate_limit_exceeded" {
				errorType = "rate_limit_error"
			}
			body["error"] = map[string]any{"code": code, "message": safetySecret, "type": errorType, "misalignment": nil}
			good := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client, count, records, _ := safetyClient(t, "openai-responses", protocolJSON(t, body), good)
			result, tools, submits, err := safetyRun(client)
			if err != nil || result == nil || tools != 0 || submits != 1 || count.Load() != 2 || len(*records) != 2 {
				t.Fatalf("retryable failure: err=%v tools=%d submits=%d requests=%d", err, tools, submits, count.Load())
			}
			if result.Usage.InputTokens != 22 || result.Usage.OutputTokens != 14 {
				t.Fatalf("usage lost: %+v", result.Usage)
			}
			raw, marshalErr := json.Marshal(*records)
			if marshalErr != nil || strings.Contains(string(raw), safetySecret) {
				t.Fatalf("retry audit leaked data or is not serializable: %v", marshalErr)
			}
		})
	}
}
