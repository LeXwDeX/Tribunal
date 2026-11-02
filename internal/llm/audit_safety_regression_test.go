package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type auditAdapter struct {
	response *Response
	calls    int
}

func (a *auditAdapter) Generate(context.Context, *Request) (*Response, error) {
	a.calls++
	return a.response, nil
}

func TestAuditSafetyAgentResponsesAreRecordedWithoutChangingProviderResult(t *testing.T) {
	const canary = "response-id-secret-canary"
	valid := Part{Type: PartToolCall, ToolCall: &ToolCall{ID: "ok", Name: "submit_result", Input: json.RawMessage(`{}`)}}
	bad := Part{Type: PartToolCall, ToolCall: &ToolCall{ID: canary, Name: canary, Input: json.RawMessage(`[]`)}}
	for _, tc := range []struct {
		name   string
		stop   StopReason
		parts  []Part
		unsafe bool
	}{
		{"invalid", StopToolUse, []Part{bad}, true},
		{"mismatch", StopEndTurn, []Part{valid}, true},
		{"empty-tool-turn", StopToolUse, nil, true},
		{"refusal", StopRefusal, nil, true},
		{"max-tokens", StopMaxTokens, nil, true},
		{"valid", StopToolUse, []Part{valid}, false},
		{"plain-end-turn", StopEndTurn, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &auditAdapter{response: &Response{StopReason: tc.stop, RequestID: canary, Message: Message{Parts: tc.parts}, Usage: Usage{InputTokens: 3}}}
			var records []CallRecord
			client := &providerClient{adapter: a, recorder: RecorderFunc(func(_ context.Context, r CallRecord) { records = append(records, r) })}
			ctx := WithCallMeta(context.Background(), CallMeta{Turn: 1})
			resp, err := client.Generate(ctx, simpleRequest())
			if err != nil || a.calls != 1 || resp == nil || resp.RequestID != canary {
				t.Fatalf("provider result changed: %+v %v, attempts=%d", resp, err, a.calls)
			}
			if len(records) != 1 || records[0].Response == nil || records[0].Response.RequestID != "" {
				t.Fatalf("unsafe response ID recorded: %+v", records)
			}
			if IsUnsafeResponse(records[0].Err) != tc.unsafe {
				t.Fatalf("recorded error=%v want unsafe=%v", records[0].Err, tc.unsafe)
			}
			raw, _ := json.Marshal(records[0])
			if strings.Contains(string(raw), canary) || strings.Contains(fmt.Sprintf("%+v", records[0].Err), canary) {
				t.Fatalf("record leaked: %s", raw)
			}
		})
	}
}

type auditRetryAdapter struct{ calls int }

func (a *auditRetryAdapter) Generate(context.Context, *Request) (*Response, error) {
	a.calls++
	return nil, &Error{Kind: ErrTransient, Usage: Usage{InputTokens: int64(a.calls)}, Err: errors.New("upstream-secret-canary")}
}

func TestAuditSafetyAsyncRecorderRetainsAttemptSnapshots(t *testing.T) {
	adapter := &auditRetryAdapter{}
	queue := make(chan CallRecord, 5)
	client := &providerClient{adapter: adapter, baseDelay: time.Nanosecond, recorder: RecorderFunc(func(_ context.Context, r CallRecord) { queue <- r })}
	_, err := client.Generate(context.Background(), simpleRequest())
	var final *Error
	if !errors.As(err, &final) || final.Usage.InputTokens != 15 || adapter.calls != 5 {
		t.Fatalf("final=%v calls=%d", err, adapter.calls)
	}
	for i := 1; i <= 5; i++ {
		record := <-queue
		var failure *Error
		if !errors.As(record.Err, &failure) || failure.Usage.InputTokens != int64(i) || record.Audit.Response.Usage.InputTokens != int64(i) {
			t.Fatalf("attempt %d: %+v, %v", i, record.Audit, record.Err)
		}
		if strings.Contains(fmt.Sprintf("%+v", failure), "upstream-secret-canary") || failure.Err != nil {
			t.Fatalf("recorder error contains upstream data: %+v", failure)
		}
	}
}

func TestAuditSafetyUntrustedMetadataIsNotRecorded(t *testing.T) {
	const secret = "metadata-secret-canary"
	adapter := &auditAdapter{response: &Response{StopReason: StopReason(secret), RequestID: secret}}
	var got CallRecord
	client := &providerClient{adapter: adapter, recorder: RecorderFunc(func(_ context.Context, r CallRecord) { got = r })}
	req := simpleRequest()
	req.Reasoning = &ReasoningOpts{Effort: secret}
	response, err := client.Generate(WithCallMeta(context.Background(), CallMeta{Turn: 1}), req)
	if err != nil || response.StopReason != StopReason(secret) || response.RequestID != secret {
		t.Fatalf("live response changed: %+v %v", response, err)
	}
	raw, err := json.Marshal(got)
	if err != nil || strings.Contains(string(raw), secret) || got.Audit.Response.StopReason != "other" || got.Audit.Request.Reasoning.Effort != "" {
		t.Fatalf("unsafe audit metadata: %s %v", raw, err)
	}
	if req.Reasoning.Effort != secret {
		t.Fatal("audit mutated the request")
	}
}

func TestAuditSafetyDoesNotApplyAgentPolicyToOtherCallers(t *testing.T) {
	adapter := &auditAdapter{response: &Response{StopReason: StopMaxTokens, Usage: Usage{InputTokens: 3}}}
	var got CallRecord
	client := &providerClient{adapter: adapter, recorder: RecorderFunc(func(_ context.Context, r CallRecord) { got = r })}
	response, err := client.Generate(WithCallMeta(context.Background(), CallMeta{WorkflowID: "wf", NodeID: "node"}), simpleRequest())
	if err != nil || response == nil || response.StopReason != StopMaxTokens || got.Err != nil || adapter.calls != 1 {
		t.Fatalf("non-agent response=%+v error=%v audit error=%v attempts=%d", response, err, got.Err, adapter.calls)
	}
}
