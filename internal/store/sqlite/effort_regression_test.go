package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestLLMCallAuditRetainsMaxEffort(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	for _, audit := range []*llm.CallAudit{
		llm.NewCallAudit(&llm.Request{Reasoning: &llm.ReasoningOpts{Effort: "max"}}, nil),
		{Request: &llm.AuditRequest{Reasoning: &llm.ReasoningOpts{Effort: "max"}}},
	} {
		if err := st.RecordLLMCall(ctx, llm.CallRecord{Provider: "p", Model: "m", Meta: llm.CallMeta{WorkflowID: "wf", NodeID: "n"}, Audit: audit}); err != nil {
			t.Fatal(err)
		}
	}
	calls, _, err := st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf", IncludePayload: true})
	if err != nil || len(calls) != 2 {
		t.Fatalf("calls = %d, err = %v", len(calls), err)
	}
	for _, call := range calls {
		var request llm.AuditRequest
		if err := json.Unmarshal(call.Request, &request); err != nil {
			t.Fatal(err)
		}
		if request.Reasoning == nil || request.Reasoning.Effort != "max" {
			t.Fatalf("reasoning = %+v, want effort max", request.Reasoning)
		}
	}
}
