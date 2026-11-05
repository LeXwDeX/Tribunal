package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestLLMCallAuditRetainsEgressDeniedKind(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	if err := st.RecordLLMCall(ctx, llm.CallRecord{
		Provider: "p", Model: "m",
		Meta: llm.CallMeta{WorkflowID: "wf-egress-kind", NodeID: "n"},
		Err:  &llm.Error{Kind: llm.ErrEgress, Err: llm.ErrEgressDenied},
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

func TestRegistryAuditRetainsEgressDeniedKind(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	t.Setenv("EGRESS_TEST_KEY", "test")
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{
		"p": {Protocol: "openai-chat", Model: "m", BaseURL: "http://169.254.169.254", APIKeyEnv: "EGRESS_TEST_KEY"},
	}, llm.WithRecorder(st))
	if err != nil {
		t.Fatal(err)
	}
	client, err := registry.Client("p")
	if err != nil {
		t.Fatal(err)
	}
	callCtx := llm.WithCallMeta(ctx, llm.CallMeta{WorkflowID: "wf-registry-egress", NodeID: "n"})
	_, err = client.Generate(callCtx, &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "ping"}}}}})
	if !errors.Is(err, llm.ErrEgressDenied) {
		t.Fatalf("generate error = %v, want egress denied", err)
	}
	calls, _, err := st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf-registry-egress"})
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls = %d, err = %v", len(calls), err)
	}
	if calls[0].ErrorKind != "egress_denied" {
		t.Fatalf("error_kind = %q, want egress_denied", calls[0].ErrorKind)
	}
}
