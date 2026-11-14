package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// Fixed failure details persisted by the review executor must reach the API
// unchanged; only unknown legacy text keeps the historical "node failed"
// collapse.
func TestSafeNodeErrorKeepsFixedFailureDetails(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", ""},
		{"node execution failed", "node execution failed"},
		{"submit_result missing after 30 turns", "submit_result missing after 30 turns"},
		{"submit_result rejected after 2 turns", "submit_result rejected after 2 turns"},
		{"model context window exceeded", "model context window exceeded"},
		{"workflow token budget exhausted", "workflow token budget exhausted"},
		{"workflow token budget exhausted (1950000 of 2000000 tokens)", "workflow token budget exhausted (1950000 of 2000000 tokens)"},
		{"queue wait exceeded node timeout", "queue wait exceeded node timeout"},
		{"required dependency did not complete", "required dependency did not complete"},
		{"provider body: raw upstream canary", "node failed"},
		{"submit_result missing after many turns", "node failed"},
	} {
		if got := safeNodeError(tc.in); got != tc.want {
			t.Fatalf("safeNodeError(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A store without an event log still reports the fixed class the persisted
// detail implies, so the API cannot fall back to executor_error for these
// failures.
func TestNodeReasonFallbackFromStoredDetail(t *testing.T) {
	for _, tc := range []struct {
		status string
		err    string
		want   string
	}{
		{"failed", "submit_result missing after 30 turns", "no_submit"},
		{"failed", "submit_result rejected after 2 turns", "invalid_submit"},
		{"failed", "model context window exceeded", "context_too_long"},
		{"failed", "workflow token budget exhausted (1 of 2 tokens)", "budget_exhausted"},
		{"failed", "queue wait exceeded node timeout", "queue_timeout"},
		{"failed", "node execution failed", "executor_error"},
		{"skipped", "required dependency did not complete", "required_node_failed"},
		{"completed", "", ""},
	} {
		node := store.WorkflowNode{ID: "n", Kind: "review", Status: tc.status, Error: tc.err}
		if got := nodeReason(node, nil); got != tc.want {
			t.Fatalf("%s %q reason = %q, want %q", tc.status, tc.err, got, tc.want)
		}
	}
}

// The list and detail endpoints expose the sanitized cause, and a skipped node
// no longer returns a node error next to its skipped status.
func TestReviewNodesExposeSanitizedFailureDetail(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	wfID := "wf-node-failure"
	mrIID := int64(11)
	started := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	finished := started.Add(31 * time.Minute)
	fake.workflows = append(fake.workflows, store.Workflow{
		ID: wfID, Kind: "review", ProjectID: 42, MRIID: &mrIID, Trigger: "mr_update",
		Status: "failed", CreatedAt: started, StartedAt: &started, FinishedAt: &finished,
	})
	fake.setNodes(wfID,
		store.WorkflowNode{
			WorkflowID: wfID, ID: "rv.p1", Kind: "review", Status: "failed", Required: true,
			Attempt: 1, Error: "submit_result missing after 30 turns", TokensIn: 1950000, TokensOut: 1200,
		},
		store.WorkflowNode{
			WorkflowID: wfID, ID: "verify.f1", Kind: "verify", Status: "skipped", Attempt: 1,
			Error: "required dependency did not complete",
		},
	)
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/reviews/"+wfID, nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	nodes := map[string]map[string]any{}
	for _, item := range body["nodes"].([]any) {
		node := item.(map[string]any)
		nodes[node["id"].(string)] = node
	}
	failed := nodes["rv.p1"]
	if failed["status"] != "failed" || failed["reason"] != "no_submit" {
		t.Fatalf("failed node = %v", failed)
	}
	if failed["failure"] != "submit_result missing after 30 turns" {
		t.Fatalf("failed node failure = %v", failed["failure"])
	}
	skipped := nodes["verify.f1"]
	if skipped["status"] != "skipped" || skipped["reason"] != "required_node_failed" {
		t.Fatalf("skipped node = %v", skipped)
	}
	if _, present := skipped["failure"]; present {
		t.Fatalf("skipped node carries a failure: %v", skipped)
	}

	failedDetail := client.request(http.MethodGet, "/api/v1/reviews/"+wfID+"/nodes/rv.p1", nil, nil)
	assertStatus(t, failedDetail, http.StatusOK)
	failedBody := env.decode(t, failedDetail)
	if failedBody["error"] != "submit_result missing after 30 turns" || failedBody["reason"] != "no_submit" {
		t.Fatalf("failed node detail = %v", failedBody)
	}

	skippedDetail := client.request(http.MethodGet, "/api/v1/reviews/"+wfID+"/nodes/verify.f1", nil, nil)
	assertStatus(t, skippedDetail, http.StatusOK)
	skippedBody := env.decode(t, skippedDetail)
	if skippedBody["status"] != "skipped" || skippedBody["reason"] != "required_node_failed" {
		t.Fatalf("skipped node detail = %v", skippedBody)
	}
	if _, present := skippedBody["error"]; present {
		t.Fatalf("skipped node detail carries an error: %v", skippedBody)
	}
}
