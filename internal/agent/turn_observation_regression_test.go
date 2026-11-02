package agent

import (
	"context"
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

const zeroUsageFailed = `{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"secret-canary"},"output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
const zeroUsageUnsafe = `{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"secret-canary"},"output":[{"type":"function_call","id":"fc","call_id":"c","name":"submit_result","arguments":"{\"secret\":\"secret-canary\"}","status":"completed"}],"usage":{"input_tokens":0,"output_tokens":0}}`

func turnHTTPClient(t *testing.T, handler http.HandlerFunc) llm.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("TURN_OBSERVATION_TEST_KEY", "key-canary")
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"test": {
		Protocol: "openai-responses", Model: "test", BaseURL: server.URL, APIKeyEnv: "TURN_OBSERVATION_TEST_KEY", MaxTokens: 512,
	}}, llm.WithRetryBaseDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	client, err := registry.Client("test")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestTurnObservationZeroUsageUnsafeResponse(t *testing.T) {
	client, calls, records, _ := safetyClient(t, "openai-responses", []byte(zeroUsageUnsafe))
	result, tools, submits, err := safetyRun(client)
	var failure *llm.Error
	if !errors.Is(err, ErrUnsafeResponse) || !errors.As(err, &failure) || failure.Kind != llm.ErrUnsafeResponse || failure.Retryable() ||
		tools != 0 || submits != 0 || calls.Load() != 1 || len(*records) != 1 || strings.Contains(err.Error(), "secret-canary") {
		t.Fatalf("err=%v tools=%d submits=%d calls=%d records=%d", err, tools, submits, calls.Load(), len(*records))
	}
	if result == nil || result.Turns != 1 || result.Usage != (llm.Usage{}) || len(result.Transcript) != 1 {
		t.Fatalf("result=%+v", result)
	}
	if (*records)[0].Response == nil || (*records)[0].Response.StopReason != llm.StopOther || (*records)[0].Audit == nil || (*records)[0].Audit.Response == nil || (*records)[0].Audit.Response.StopReason != string(llm.StopOther) {
		t.Fatalf("zero-usage failed response lost its safe stop: %+v", (*records)[0])
	}
}

func TestTurnObservationAfterSuccessfulTurn(t *testing.T) {
	first := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"tool", "echo", `{}`, false}}, false)
	client, calls, _, _ := safetyClient(t, "openai-responses", first, []byte(zeroUsageUnsafe))
	result, tools, submits, err := safetyRun(client)
	if !errors.Is(err, ErrUnsafeResponse) || calls.Load() != 2 || tools != 1 || submits != 0 || result == nil ||
		result.Turns != 2 || result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 || len(result.Transcript) != 3 {
		t.Fatalf("err=%v calls=%d tools=%d submits=%d result=%+v", err, calls.Load(), tools, submits, result)
	}
}

func TestTurnObservationRetryGuard(t *testing.T) {
	for _, body := range []string{zeroUsageFailed, strings.Replace(zeroUsageFailed, `"input_tokens":0,"output_tokens":0`, `"input_tokens":11,"output_tokens":7`, 1)} {
		client, calls, records, _ := safetyClient(t, "openai-responses", []byte(body))
		spec := testSpec(client)
		stopped := errors.New("budget exhausted")
		checks := 0
		spec.BeforeTurn = func(context.Context, llm.Usage) error {
			checks++
			if checks == 2 {
				return stopped
			}
			return nil
		}
		result, err := Run(context.Background(), spec)
		var guard *llm.AttemptGuardError
		var failure *llm.Error
		if !errors.Is(err, stopped) || !errors.As(err, &guard) || !errors.As(err, &failure) || failure.Kind != llm.ErrTransient ||
			calls.Load() != 1 || len(*records) != 1 || checks != 2 || result == nil || result.Turns != 1 ||
			strings.Contains(err.Error(), "secret-canary") {
			t.Fatalf("err=%v calls=%d records=%d checks=%d result=%+v", err, calls.Load(), len(*records), checks, result)
		}
		want := llm.Usage{}
		if body != zeroUsageFailed {
			want = llm.Usage{InputTokens: 11, OutputTokens: 7}
		}
		if result.Usage != want {
			t.Fatalf("usage=%+v want=%+v", result.Usage, want)
		}
	}
}

func TestTurnObservationEmptyChatResponseAndRetryExhaustion(t *testing.T) {
	t.Run("chat empty choices", func(t *testing.T) {
		body := `{"id":"empty","object":"chat.completion","created":1,"model":"test","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`
		client, calls, _, _ := safetyClient(t, "openai-chat", []byte(body))
		spec := testSpec(client)
		checks := 0
		blocked := errors.New("budget exhausted")
		spec.BeforeTurn = func(context.Context, llm.Usage) error {
			checks++
			if checks == 2 {
				return blocked
			}
			return nil
		}
		result, err := Run(context.Background(), spec)
		if !errors.Is(err, blocked) || calls.Load() != 1 || result == nil || result.Turns != 1 || result.Usage != (llm.Usage{}) {
			t.Fatalf("err=%v result=%+v calls=%d", err, result, calls.Load())
		}
	})
	t.Run("retry exhaustion", func(t *testing.T) {
		bodies := [][]byte{[]byte(zeroUsageFailed), []byte(zeroUsageFailed), []byte(zeroUsageFailed), []byte(zeroUsageFailed), []byte(zeroUsageFailed)}
		client, calls, _, _ := safetyClient(t, "openai-responses", bodies...)
		result, err := Run(context.Background(), testSpec(client))
		var failure *llm.Error
		if !errors.As(err, &failure) || failure.Kind != llm.ErrTransient || calls.Load() != 5 || result == nil ||
			result.Turns != 1 || result.Usage != (llm.Usage{}) {
			t.Fatalf("err=%v result=%+v calls=%d", err, result, calls.Load())
		}
	})
}

func TestTurnObservationBeforeRequestAndUnobservedFailure(t *testing.T) {
	blocked := errors.New("budget exhausted")
	t.Run("guard before first request", func(t *testing.T) {
		client := &fakeClient{}
		spec := testSpec(client)
		spec.BeforeTurn = func(context.Context, llm.Usage) error { return blocked }
		result, err := Run(context.Background(), spec)
		if !errors.Is(err, blocked) || result != nil || len(client.requests()) != 0 {
			t.Fatalf("err=%v result=%+v calls=%d", err, result, len(client.requests()))
		}
	})
	t.Run("preflight validation", func(t *testing.T) {
		var calls atomic.Int32
		client := turnHTTPClient(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
		spec := testSpec(client)
		spec.Submit.Schema = []byte(`[]`)
		result, err := Run(context.Background(), spec)
		var failure *llm.Error
		if !errors.As(err, &failure) || failure.Kind != llm.ErrBadRequest || result != nil || calls.Load() != 0 {
			t.Fatalf("err=%v result=%+v calls=%d", err, result, calls.Load())
		}
	})
	t.Run("canceled before first request", func(t *testing.T) {
		client := &fakeClient{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := Run(ctx, testSpec(client))
		if !errors.Is(err, context.Canceled) || result != nil || len(client.requests()) != 0 {
			t.Fatalf("err=%v result=%+v calls=%d", err, result, len(client.requests()))
		}
	})
	t.Run("nonzero usage is not observation", func(t *testing.T) {
		failure := &llm.Error{Kind: llm.ErrTransient, Usage: llm.Usage{InputTokens: 5}}
		client := &fakeClient{errs: []error{fmt.Errorf("wrapped: %w", failure)}}
		result, err := Run(context.Background(), testSpec(client))
		if !errors.Is(err, failure) || result == nil || result.Turns != 0 || result.Usage.InputTokens != 5 {
			t.Fatalf("err=%v result=%+v", err, result)
		}
	})
	t.Run("fallback guard after unobserved failure", func(t *testing.T) {
		failure := &llm.Error{Kind: llm.ErrBadRequest, Usage: llm.Usage{InputTokens: 5}}
		client := &fakeClient{errs: []error{failure}}
		spec := testSpec(client)
		spec.MaxTurns = 1
		checks := 0
		spec.BeforeTurn = func(context.Context, llm.Usage) error {
			checks++
			if checks == 2 {
				return blocked
			}
			return nil
		}
		result, err := Run(context.Background(), spec)
		var classified *llm.Error
		if !errors.Is(err, blocked) || !errors.As(err, &classified) || classified.Kind != llm.ErrBadRequest ||
			result == nil || result.Turns != 0 || result.Usage.InputTokens != 5 || len(client.requests()) != 1 {
			t.Fatalf("err=%v result=%+v calls=%d", err, result, len(client.requests()))
		}
	})
	t.Run("guard before second request", func(t *testing.T) {
		first := toolResponse(llm.StopToolUse, call("tool", "echo", `{}`))
		client := &fakeClient{replies: []*llm.Response{first}}
		spec := testSpec(client, echoTool{name: "echo"})
		checks := 0
		spec.BeforeTurn = func(context.Context, llm.Usage) error {
			checks++
			if checks == 2 {
				return blocked
			}
			return nil
		}
		result, err := Run(context.Background(), spec)
		if !errors.Is(err, blocked) || result == nil || result.Turns != 1 || len(client.requests()) != 1 {
			t.Fatalf("err=%v result=%+v calls=%d", err, result, len(client.requests()))
		}
	})
}

func TestTurnObservationInvalidUsageStillCountsResponse(t *testing.T) {
	response := toolResponse(llm.StopToolUse, call("submit", SubmitToolName, `{}`))
	response.Usage = llm.Usage{InputTokens: 5, OutputTokens: -1}
	client := &fakeClient{replies: []*llm.Response{response}}
	result, err := Run(context.Background(), testSpec(client))
	if !errors.Is(err, ErrUnsafeResponse) || result == nil || result.Turns != 1 ||
		result.Usage != (llm.Usage{InputTokens: 5, Incomplete: true}) || len(client.requests()) != 1 {
		t.Fatalf("err=%v result=%+v calls=%d", err, result, len(client.requests()))
	}
}

type cancelAfterTurnClient struct {
	llm.Client
	cancel context.CancelFunc
}

func (c cancelAfterTurnClient) Generate(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	resp, err := c.Client.Generate(ctx, req)
	c.cancel()
	return resp, err
}

func TestTurnObservationForcedChoiceFallback(t *testing.T) {
	for _, mode := range []string{"success", "guard", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			good := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client := turnHTTPClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":{"message":"secret-canary","type":"invalid_request_error"}}`))
					return
				}
				_, _ = w.Write(good)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				client = cancelAfterTurnClient{Client: client, cancel: cancel}
			}
			spec := testSpec(client)
			spec.MaxTurns = 1
			blocked := errors.New("budget exhausted")
			checks := 0
			spec.BeforeTurn = func(context.Context, llm.Usage) error {
				checks++
				if mode == "guard" && checks == 2 {
					return blocked
				}
				return nil
			}
			result, err := Run(ctx, spec)
			wantCalls := int32(1)
			if mode == "success" {
				wantCalls = 2
			}
			if calls.Load() != wantCalls || result == nil || result.Turns != 1 ||
				(mode == "success" && err != nil) || (mode == "guard" && !errors.Is(err, blocked)) ||
				(mode == "cancel" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("err=%v result=%+v calls=%d checks=%d", err, result, calls.Load(), checks)
			}
			if mode == "guard" {
				var failure *llm.Error
				if !errors.As(err, &failure) || failure.Kind != llm.ErrBadRequest || strings.Contains(err.Error(), "secret-canary") {
					t.Fatalf("guard lost safe original classification: %v", err)
				}
			}
		})
	}
}

func TestTurnObservationRetryCanceledAfterResponse(t *testing.T) {
	client, calls, _, _ := safetyClient(t, "openai-responses", []byte(zeroUsageFailed))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec := testSpec(client)
	checks := 0
	spec.BeforeTurn = func(context.Context, llm.Usage) error {
		checks++
		if checks == 2 {
			cancel()
		}
		return nil
	}
	result, err := Run(ctx, spec)
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 || checks != 2 || result == nil || result.Turns != 1 || result.Usage != (llm.Usage{}) {
		t.Fatalf("err=%v result=%+v calls=%d checks=%d", err, result, calls.Load(), checks)
	}
}
