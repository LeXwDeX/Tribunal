package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestAuditSafetyMixedHistoryAndResponseIDs(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	const historicalID = "historical-upstream-id-secret"
	const newID = "new-upstream-id-secret"
	legacyRequest := `{"messages":[{"role":"user","parts":[{"type":"text","text":"historical prompt"}]}]}`
	legacyResponse := `{"message":{"role":"assistant","parts":[]},"request_id":"` + historicalID + `"}`
	req, err := encodeGzipJSON(json.RawMessage(legacyRequest))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := encodeGzipJSON(json.RawMessage(legacyResponse))
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.ExecContext(ctx, `INSERT INTO llm_calls
		(id, workflow_id, node_id, turn, provider, model, request, response, latency_ms, request_id, created_at)
		VALUES ('legacy','wf','node',1,'p','m',?,?,0,?,'2020-01-01T00:00:00Z')`, req, resp, historicalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordLLMCall(ctx, llm.CallRecord{
		Provider: "p", Model: "m", Meta: llm.CallMeta{WorkflowID: "wf", NodeID: "node"},
		Request:  &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser}}},
		Response: &llm.Response{RequestID: newID, StopReason: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 7}},
	}); err != nil {
		t.Fatal(err)
	}
	var stored sql.NullString
	if err := st.db.QueryRowContext(ctx, `SELECT request_id FROM llm_calls WHERE id <> 'legacy'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid && stored.String != "" {
		t.Fatalf("new request_id persisted: %q", stored.String)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT request_id FROM llm_calls WHERE id = 'legacy'`).Scan(&stored); err != nil || stored.String != historicalID {
		t.Fatalf("history changed: %q %v", stored.String, err)
	}
	for _, include := range []bool{false, true} {
		calls, _, err := st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf", IncludePayload: include})
		if err != nil || len(calls) != 2 {
			t.Fatalf("calls=%+v err=%v", calls, err)
		}
		for _, call := range calls {
			if call.RequestID != "" {
				t.Fatalf("exposed upstream id: %+v", call)
			}
			if !include {
				raw, _ := json.Marshal(call)
				if strings.Contains(string(raw), historicalID) || strings.Contains(string(raw), newID) {
					t.Fatalf("id in unexpanded call: %s", raw)
				}
				continue
			}
			if call.ID == "legacy" {
				if string(call.Request) != legacyRequest || string(call.Response) != legacyResponse {
					t.Fatalf("legacy changed: %s / %s", call.Request, call.Response)
				}
			} else {
				for _, payload := range []json.RawMessage{call.Request, call.Response} {
					var value map[string]any
					if err := json.Unmarshal(payload, &value); err != nil {
						t.Fatal(err)
					}
					if value["_format"] != "audit_metadata_v1" || strings.Contains(string(payload), newID) {
						t.Fatalf("new metadata: %s", payload)
					}
				}
			}
		}
	}
}

func TestAuditSafetyRejectsFreeTextInMetadata(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	const secret = "untrusted-metadata-secret"
	call := llm.CallRecord{
		Provider: "p", Model: "m", Meta: llm.CallMeta{WorkflowID: "wf", NodeID: "node"},
		Response: &llm.Response{RequestID: secret},
		Audit: &llm.CallAudit{
			Request:  &llm.AuditRequest{Format: secret, Reasoning: &llm.ReasoningOpts{Effort: secret}},
			Response: &llm.AuditResponse{Format: secret, StopReason: secret, Usage: llm.Usage{InputTokens: 3}},
		},
		Err: &llm.Error{Kind: llm.ErrKind(secret)},
	}
	if err := st.RecordLLMCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	calls, _, err := st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf", IncludePayload: true})
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls=%+v err=%v", calls, err)
	}
	raw, _ := json.Marshal(calls[0])
	if strings.Contains(string(raw), secret) || calls[0].StopReason != "other" || calls[0].ErrorKind != "error" {
		t.Fatalf("unsafe metadata: %s", raw)
	}
	if call.Audit.Request.Format != secret || call.Audit.Response.StopReason != secret {
		t.Fatal("recording mutated the caller's metadata")
	}
}
