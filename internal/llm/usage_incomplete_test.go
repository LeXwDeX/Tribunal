package llm

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
)

func TestUsageIncompleteSanitizesAndSaturatesEachCategory(t *testing.T) {
	cases := []struct {
		name string
		a, b Usage
		want Usage
	}{
		{"negative", Usage{InputTokens: 5, OutputTokens: 9}, Usage{InputTokens: -2, OutputTokens: 1, CacheReadTokens: -3}, Usage{InputTokens: 5, OutputTokens: 10, Incomplete: true}},
		{"overflow", Usage{InputTokens: math.MaxInt64 - 1, OutputTokens: math.MaxInt64, CacheReadTokens: math.MaxInt64, CacheWriteTokens: math.MaxInt64, ReasoningTokens: math.MaxInt64}, Usage{InputTokens: 2, OutputTokens: 1, CacheReadTokens: 1, CacheWriteTokens: 1, ReasoningTokens: 1}, Usage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64, CacheReadTokens: math.MaxInt64, CacheWriteTokens: math.MaxInt64, ReasoningTokens: math.MaxInt64, Incomplete: true}},
		{"exact maximum", Usage{InputTokens: math.MaxInt64 - 1}, Usage{InputTokens: 1}, Usage{InputTokens: math.MaxInt64}},
		{"total overflow", Usage{InputTokens: math.MaxInt64}, Usage{OutputTokens: 1}, Usage{InputTokens: math.MaxInt64, OutputTokens: 1, Incomplete: true}},
		{"sticky", Usage{Incomplete: true}, Usage{InputTokens: 5}, Usage{InputTokens: 5, Incomplete: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AddUsage(tc.a, tc.b); got != tc.want {
				t.Fatalf("usage=%+v want=%+v", got, tc.want)
			}
		})
	}
}

func TestUsageIncompleteEachNegativeCategory(t *testing.T) {
	for _, usage := range []Usage{
		{InputTokens: -1}, {OutputTokens: -1}, {CacheReadTokens: -1},
		{CacheWriteTokens: -1}, {ReasoningTokens: -1},
	} {
		got := AddUsage(Usage{InputTokens: 7}, usage)
		if got != (Usage{InputTokens: 7, Incomplete: true}) {
			t.Fatalf("raw=%+v sanitized=%+v", usage, got)
		}
	}
}

type incompleteRetryAdapter struct{ calls int }

func (a *incompleteRetryAdapter) Generate(context.Context, *Request) (*Response, error) {
	a.calls++
	if a.calls == 1 {
		return nil, &Error{Kind: ErrTransient, Usage: Usage{InputTokens: math.MaxInt64 - 1}}
	}
	return nil, &Error{Kind: ErrTransient, Usage: Usage{InputTokens: 2}}
}

type incompleteSuccessAdapter struct{ calls int }

func (a *incompleteSuccessAdapter) Generate(context.Context, *Request) (*Response, error) {
	a.calls++
	if a.calls == 1 {
		return nil, &Error{Kind: ErrTransient, Usage: Usage{InputTokens: math.MaxInt64 - 1}}
	}
	return &Response{StopReason: StopEndTurn, Usage: Usage{InputTokens: 2}}, nil
}

type incompleteDualAdapter struct{ calls int }

func (a *incompleteDualAdapter) Generate(context.Context, *Request) (*Response, error) {
	a.calls++
	return &Response{Usage: Usage{InputTokens: 3, OutputTokens: 1}}, &Error{Kind: ErrUnsafeResponse, Usage: Usage{InputTokens: 5, OutputTokens: -2}}
}

func TestUsageIncompleteRegistryResponseAndErrorUseKnownLowerBound(t *testing.T) {
	a := &incompleteDualAdapter{}
	client := &providerClient{adapter: a}
	resp, err := client.Generate(context.Background(), simpleRequest())
	usage, valid := ErrorUsage(err)
	if resp != nil || !IsUnsafeResponse(err) || !valid || a.calls != 1 || usage != (Usage{InputTokens: 5, OutputTokens: 1, Incomplete: true}) {
		t.Fatalf("resp=%+v err=%v usage=%+v valid=%t calls=%d", resp, err, usage, valid, a.calls)
	}
}

func TestUsageIncompleteRetrySuccessDoesNotDoubleCount(t *testing.T) {
	a := &incompleteSuccessAdapter{}
	var records []CallRecord
	client := &providerClient{adapter: a, baseDelay: time.Nanosecond, recorder: RecorderFunc(func(_ context.Context, r CallRecord) { records = append(records, r) })}
	resp, err := client.Generate(context.Background(), simpleRequest())
	if err != nil || resp == nil || a.calls != 2 || resp.Usage.InputTokens != math.MaxInt64 || !resp.Usage.Incomplete || len(records) != 2 || records[0].Audit.Response.Usage.InputTokens != math.MaxInt64-1 || records[1].Audit.Response.Usage.InputTokens != 2 || records[1].Audit.Response.Usage.Incomplete {
		t.Fatalf("resp=%+v err=%v calls=%d records=%+v", resp, err, a.calls, records)
	}
}

func TestUsageIncompleteRealResponsesNegative(t *testing.T) {
	client, calls := usageClient(t, "openai-responses", `{"id":"secret-canary","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c","name":"submit_result","arguments":"{}","status":"completed"}],"usage":{"input_tokens":5,"output_tokens":-2}}`)
	resp, err := client.Generate(context.Background(), simpleRequest())
	usage, valid := ErrorUsage(err)
	if resp != nil || !IsUnsafeResponse(err) || calls.Load() != 1 || !valid || usage != (Usage{InputTokens: 5, Incomplete: true}) {
		t.Fatalf("resp=%+v err=%v usage=%+v valid=%t calls=%d", resp, err, usage, valid, calls.Load())
	}
}

func TestUsageIncompleteMissingProviderUsageIsMarked(t *testing.T) {
	client, calls := usageClient(t, "openai-responses", `{"id":"fake","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"message","id":"msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]}`)
	resp, err := client.Generate(context.Background(), simpleRequest())
	if err != nil || resp == nil || calls.Load() != 1 || resp.Usage != (Usage{Incomplete: true}) {
		t.Fatalf("resp=%+v err=%v calls=%d", resp, err, calls.Load())
	}
}

func TestUsageIncompleteRetryGuardAndCancel(t *testing.T) {
	for _, mode := range []string{"guard", "cancel", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			a := &incompleteRetryAdapter{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &providerClient{adapter: a, baseDelay: time.Nanosecond}
			stopped := errors.New("guard stopped")
			_, err := GenerateGuarded(ctx, client, simpleRequest(), func(_ context.Context, usage Usage) error {
				if a.calls == 2 {
					if !usage.Incomplete || usage.InputTokens != math.MaxInt64 {
						t.Fatalf("guard usage=%+v", usage)
					}
					if mode == "guard" {
						return stopped
					}
					if mode == "cancel" {
						cancel()
					}
				}
				return nil
			})
			usage, valid := ErrorUsage(err)
			if err == nil || !valid || !usage.Incomplete || usage.InputTokens != math.MaxInt64 {
				t.Fatalf("err=%v usage=%+v valid=%t", err, usage, valid)
			}
			if mode == "guard" && (!errors.Is(err, stopped) || a.calls != 2) || mode == "cancel" && (!errors.Is(err, context.Canceled) || a.calls != 2) || mode == "exhausted" && a.calls != maxRetries+1 {
				t.Fatalf("mode=%s err=%v calls=%d", mode, err, a.calls)
			}
		})
	}
}

func TestUsageIncompleteAnthropicCacheCounts(t *testing.T) {
	for _, tc := range []struct {
		name               string
		input, read, write int64
		want               int64
		unsafe             bool
	}{
		{"overflow", math.MaxInt64 - 1, 5, 2, math.MaxInt64, false},
		{"negative uncached", -2, 5, 3, 8, true},
		{"negative read", 5, -2, 3, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var message anthropic.Message
			message.Usage.InputTokens, message.Usage.CacheReadInputTokens, message.Usage.CacheCreationInputTokens = tc.input, tc.read, tc.write
			resp, err := anthropicResponse(&message, "model")
			var usage Usage
			if tc.unsafe {
				if !IsUnsafeResponse(err) || resp != nil {
					t.Fatalf("resp=%+v err=%v", resp, err)
				}
				usage, _ = ErrorUsage(err)
			} else {
				if err != nil || resp == nil {
					t.Fatalf("resp=%+v err=%v", resp, err)
				}
				usage = resp.Usage
			}
			if usage.InputTokens != tc.want || !usage.Incomplete {
				t.Fatalf("usage=%+v want input=%d incomplete", usage, tc.want)
			}
		})
	}
}
