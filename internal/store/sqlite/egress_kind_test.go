package sqlite

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestLLMCallAuditRetainsEgressDeniedKind(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	if err := st.RecordLLMCall(ctx, llm.CallRecord{
		Provider: "p", Model: "m",
		Meta: llm.CallMeta{WorkflowID: "wf-egress-kind", NodeID: "n"},
		Err:  llm.ErrEgressDenied,
	}); err != nil {
		t.Fatal(err)
	}
	calls, _, err := st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf-egress-kind"})
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls = %d, err = %v", len(calls), err)
	}
	if calls[0].ErrorKind != "egress_denied" {
		t.Fatalf("error_kind = %q, want egress_denied", calls[0].ErrorKind)
	}
}
