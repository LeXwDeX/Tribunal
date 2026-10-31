package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

type budgetForwardClient struct{ llm.Client }

type budgetAsUsageError struct{ failure *llm.Error }

func (e budgetAsUsageError) Error() string { return "classified failure" }
func (e budgetAsUsageError) As(target any) bool {
	if out, ok := target.(**llm.Error); ok {
		*out = e.failure
		return true
	}
	return false
}

func TestBudgetAttemptCustomAsErrorRetainsUsage(t *testing.T) {
	for _, kind := range []llm.ErrKind{llm.ErrAuth, llm.ErrUnsafeResponse} {
		client := &fakeClient{errs: []error{budgetAsUsageError{failure: &llm.Error{Kind: kind, Usage: llm.Usage{InputTokens: 5}}}}}
		result, err := Run(context.Background(), testSpec(client))
		var failure *llm.Error
		if !errors.As(err, &failure) || failure.Kind != kind || errors.Is(err, ErrUnsafeResponse) != (kind == llm.ErrUnsafeResponse) || result == nil || result.Usage.InputTokens != 5 {
			t.Fatalf("kind=%s err=%v result=%+v", kind, err, result)
		}
	}
}

func TestBudgetAttemptForwardingClientChecksOncePerAttempt(t *testing.T) {
	failed := []byte(`{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"private"},"output":[],"usage":{"input_tokens":11,"output_tokens":7}}`)
	ok := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"s", SubmitToolName, `{}`, false}}, false)
	client, calls, _, _ := safetyClient(t, "openai-responses", failed, ok)
	spec := testSpec(budgetForwardClient{client})
	var checks []llm.Usage
	spec.BeforeTurn = func(_ context.Context, usage llm.Usage) error { checks = append(checks, usage); return nil }
	result, err := Run(context.Background(), spec)
	if err != nil || calls.Load() != 2 || len(checks) != 2 || checks[1].InputTokens != 11 || result.Usage.InputTokens != 22 {
		t.Fatalf("err=%v calls=%d checks=%+v result=%+v", err, calls.Load(), checks, result)
	}
}

func TestBudgetAttemptInvalidUsageKeepsSafetyClassWithoutLeakingError(t *testing.T) {
	failure := &llm.Error{Kind: llm.ErrUnsafeResponse, Usage: llm.Usage{InputTokens: 5, OutputTokens: -1}}
	client := &fakeClient{errs: []error{fmt.Errorf("upstream secret-canary: %w", failure)}}
	result, err := Run(context.Background(), testSpec(client))
	var classified *llm.Error
	if !errors.As(err, &classified) || classified.Kind != llm.ErrUnsafeResponse || strings.Contains(err.Error(), "secret-canary") || result == nil || result.Usage.InputTokens != 5 || result.Usage.OutputTokens != 0 {
		t.Fatalf("err=%v result=%+v", err, result)
	}
}

func TestBudgetAttemptNestedErrorUsageIsNotLostOrDoubleCounted(t *testing.T) {
	for _, first := range []int64{0, 5} {
		client := &fakeClient{errs: []error{errors.Join(&llm.Error{Kind: llm.ErrBadRequest, Usage: llm.Usage{InputTokens: first}}, &llm.Error{Kind: llm.ErrUnsafeResponse, Usage: llm.Usage{InputTokens: 5}})}}
		spec := testSpec(client)
		spec.MaxTurns = 1
		result, err := Run(context.Background(), spec)
		if !errors.Is(err, ErrUnsafeResponse) || result == nil || result.Usage.InputTokens != 5 || len(client.requests()) != 1 {
			t.Fatalf("first=%d err=%v result=%+v calls=%d", first, err, result, len(client.requests()))
		}
	}
}

func TestBudgetAttemptInvalidNestedUsageRetainsUnsafeCause(t *testing.T) {
	unsafe := &llm.Error{Kind: llm.ErrUnsafeResponse, Usage: llm.Usage{InputTokens: 5, OutputTokens: -1}}
	client := &fakeClient{errs: []error{fmt.Errorf("secret-canary: %w", errors.Join(&llm.Error{Kind: llm.ErrBadRequest}, unsafe))}}
	result, err := Run(context.Background(), testSpec(client))
	if !errors.Is(err, unsafe) || !errors.Is(err, ErrUnsafeResponse) || strings.Contains(err.Error(), "secret-canary") || result == nil || result.Usage.InputTokens != 5 || result.Usage.OutputTokens != 0 {
		t.Fatalf("err=%v result=%+v", err, result)
	}
}

func TestBudgetAttemptResponsesFailedToolIsUnsafe(t *testing.T) {
	failed := []byte(`{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"secret-canary"},"output":[{"type":"function_call","id":"fc_1","call_id":"c1","name":"submit_result","arguments":"{\"secret\":\"secret-canary\"}","status":"completed"}],"usage":{"input_tokens":11,"output_tokens":7}}`)
	ok := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"s", SubmitToolName, `{}`, false}}, false)
	client, calls, records, _ := safetyClient(t, "openai-responses", failed, ok)
	var submits int
	spec := testSpec(client)
	spec.MaxTurns = 1
	spec.Submit.Decode = func(json.RawMessage) (any, error) { submits++; return nil, nil }
	result, err := Run(context.Background(), spec)
	var failure *llm.Error
	if !errors.Is(err, ErrUnsafeResponse) || !errors.As(err, &failure) || failure.Kind != llm.ErrUnsafeResponse || calls.Load() != 1 || len(*records) != 1 || submits != 0 || result == nil || result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 || strings.Contains(err.Error(), "secret-canary") {
		t.Fatalf("err=%v calls=%d records=%d submits=%d result=%+v", err, calls.Load(), len(*records), submits, result)
	}
}

func TestBudgetAttemptHTTPRetry(t *testing.T) {
	failed := []byte(`{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"private"},"output":[],"usage":{"input_tokens":11,"output_tokens":7}}`)
	ok := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"s", SubmitToolName, `{}`, false}}, false)
	for _, tc := range []struct {
		name      string
		limit     int64
		wantCalls int32
		wantErr   bool
	}{{"stop", 18, 1, true}, {"continue", 100, 2, false}} {
		t.Run(tc.name, func(t *testing.T) {
			client, calls, records, _ := safetyClient(t, "openai-responses", failed, ok)
			spec := testSpec(client)
			var seen []llm.Usage
			blocked := errors.New("budget stopped")
			spec.BeforeTurn = func(_ context.Context, usage llm.Usage) error {
				seen = append(seen, usage)
				if usage.InputTokens+usage.OutputTokens >= tc.limit {
					return blocked
				}
				return nil
			}
			result, err := Run(context.Background(), spec)
			if tc.wantErr != errors.Is(err, blocked) || calls.Load() != tc.wantCalls || len(*records) != int(tc.wantCalls) {
				t.Fatalf("err=%v calls=%d audit=%d", err, calls.Load(), len(*records))
			}
			if len(seen) != 2 || seen[0] != (llm.Usage{}) || seen[1].InputTokens != 11 || seen[1].OutputTokens != 7 {
				t.Fatalf("budget checks = %+v", seen)
			}
			if result == nil || result.Usage.InputTokens != int64(tc.wantCalls)*11 || result.Usage.OutputTokens != int64(tc.wantCalls)*7 {
				t.Fatalf("usage = %+v", result)
			}
		})
	}
}

func TestBudgetAttemptHTTPFailureRetainsClassification(t *testing.T) {
	failed := []byte(`{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"secret-canary"},"output":[],"usage":{"input_tokens":11,"output_tokens":7}}`)
	for _, tc := range []struct {
		name  string
		stop  bool
		calls int32
	}{{"guard", true, 2}, {"retry-limit", false, 5}} {
		t.Run(tc.name, func(t *testing.T) {
			client, calls, records, _ := safetyClient(t, "openai-responses", failed, failed, failed, failed, failed)
			spec := testSpec(client)
			blocked := errors.New("budget")
			checks := 0
			spec.BeforeTurn = func(_ context.Context, u llm.Usage) error {
				checks++
				if tc.stop && u.InputTokens+u.OutputTokens >= 36 {
					return blocked
				}
				return nil
			}
			result, err := Run(context.Background(), spec)
			var failure *llm.Error
			if !errors.As(err, &failure) || failure.Kind != llm.ErrTransient || errors.Is(err, blocked) != tc.stop || strings.Contains(err.Error(), "secret-canary") {
				t.Fatalf("wrong error: %v", err)
			}
			wantChecks := int(tc.calls)
			if tc.stop {
				wantChecks++
			}
			if calls.Load() != tc.calls || len(*records) != int(tc.calls) || checks != wantChecks || result == nil || result.Usage.InputTokens != int64(tc.calls)*11 || result.Usage.OutputTokens != int64(tc.calls)*7 {
				t.Fatalf("calls=%d records=%d checks=%d result=%+v", calls.Load(), len(*records), checks, result)
			}
		})
	}
}

func TestBudgetAttemptCancelAndBadRequestGuardDoNotRetry(t *testing.T) {
	failed := []byte(`{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"private"},"output":[],"usage":{"input_tokens":11,"output_tokens":7}}`)
	for _, guardErr := range []error{context.Canceled, &llm.Error{Kind: llm.ErrBadRequest}} {
		client, calls, records, _ := safetyClient(t, "openai-responses", failed)
		spec := testSpec(client)
		spec.MaxTurns = 1
		spec.BeforeTurn = func(_ context.Context, u llm.Usage) error {
			if u.InputTokens > 0 {
				return guardErr
			}
			return nil
		}
		result, err := Run(context.Background(), spec)
		if calls.Load() != 1 || len(*records) != 1 || !errors.Is(err, guardErr) || result == nil || result.Usage.InputTokens != 11 {
			t.Fatalf("guard=%v err=%v calls=%d records=%d result=%+v", guardErr, err, calls.Load(), len(*records), result)
		}
	}
}

func TestBudgetAttemptCancelInCustomGuardMakesNoRequest(t *testing.T) {
	for _, reject := range []bool{false, true} {
		client := &fakeClient{}
		spec := testSpec(client)
		ctx, cancel := context.WithCancel(context.Background())
		blocked := errors.New("budget")
		spec.BeforeTurn = func(context.Context, llm.Usage) error {
			cancel()
			if reject {
				return blocked
			}
			return nil
		}
		result, err := Run(ctx, spec)
		if !errors.Is(err, context.Canceled) || errors.Is(err, blocked) != reject || len(client.requests()) != 0 || result != nil {
			t.Fatalf("reject=%v err=%v calls=%d result=%+v", reject, err, len(client.requests()), result)
		}
	}
}

func TestBudgetAttemptZeroUsageRetry(t *testing.T) {
	failed := []byte(`{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"temporary"},"output":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	ok := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"s", SubmitToolName, `{}`, false}}, false)
	client, calls, _, _ := safetyClient(t, "openai-responses", failed, ok)
	spec := testSpec(client)
	var checks []llm.Usage
	spec.BeforeTurn = func(_ context.Context, u llm.Usage) error { checks = append(checks, u); return nil }
	result, err := Run(context.Background(), spec)
	if err != nil || calls.Load() != 2 || len(checks) != 2 || checks[0] != (llm.Usage{}) || checks[1] != (llm.Usage{}) || result.Usage.InputTokens != 11 {
		t.Fatalf("err=%v calls=%d checks=%+v result=%+v", err, calls.Load(), checks, result)
	}
}

func TestBudgetAttemptForcedChoiceFallback(t *testing.T) {
	for _, spent := range []int64{0, 5} {
		t.Run(map[int64]string{0: "zero", 5: "exhausted"}[spent], func(t *testing.T) {
			response := toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
			response.Usage.InputTokens = 1
			client := &fakeClient{errs: []error{&llm.Error{Kind: llm.ErrBadRequest, Usage: llm.Usage{InputTokens: spent}}}, replies: []*llm.Response{nil, response}}
			spec := testSpec(client)
			spec.MaxTurns = 1
			blocked := errors.New("budget")
			var seen []llm.Usage
			spec.BeforeTurn = func(_ context.Context, u llm.Usage) error {
				seen = append(seen, u)
				if u.InputTokens >= 5 {
					return blocked
				}
				return nil
			}
			result, err := Run(context.Background(), spec)
			wantCalls := 2
			if spent > 0 {
				wantCalls = 1
			}
			if len(client.requests()) != wantCalls || errors.Is(err, blocked) != (spent > 0) || len(seen) != 2 || seen[1].InputTokens != spent {
				t.Fatalf("err=%v calls=%d seen=%+v", err, len(client.requests()), seen)
			}
			if spent > 0 {
				var failure *llm.Error
				if !errors.As(err, &failure) || failure.Kind != llm.ErrBadRequest {
					t.Fatalf("guard lost original failure classification: %v", err)
				}
			}
			if result == nil || result.Usage.InputTokens != spent+int64(wantCalls-1) {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestBudgetAttemptSafetyOrCancellationCannotTriggerChoiceFallback(t *testing.T) {
	for _, blocker := range []error{ErrUnsafeResponse, context.Canceled, &llm.Error{Kind: llm.ErrUnsafeResponse}} {
		client := &fakeClient{errs: []error{errors.Join(&llm.Error{Kind: llm.ErrBadRequest, Usage: llm.Usage{InputTokens: 5}}, blocker)}, replies: []*llm.Response{nil, toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))}}
		spec := testSpec(client)
		spec.MaxTurns = 1
		result, err := Run(context.Background(), spec)
		if !errors.Is(err, blocker) || len(client.requests()) != 1 || result == nil || result.Usage.InputTokens != 5 {
			t.Fatalf("blocker=%v err=%v calls=%d result=%+v", blocker, err, len(client.requests()), result)
		}
	}
}

func TestBudgetAttemptUsageOverflowAndNegative(t *testing.T) {
	first := toolResponse(llm.StopToolUse, call("a", "echo", `{}`))
	first.Usage.InputTokens = math.MaxInt64 - 2
	second := toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
	second.Usage = llm.Usage{InputTokens: 9, OutputTokens: -50}
	client := &fakeClient{replies: []*llm.Response{first, second}}
	spec := testSpec(client, echoTool{name: "echo"})
	result, err := Run(context.Background(), spec)
	if err == nil || result == nil || result.Usage.InputTokens != math.MaxInt64 || result.Usage.OutputTokens < 0 {
		t.Fatalf("invalid usage accepted or lost: result=%+v err=%v", result, err)
	}
}
