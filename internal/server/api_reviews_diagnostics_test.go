package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// seedDiagnosticWorkflow is a completed INCOMPLETE review whose synthesize
// node carries the review-level summary, warnings and unresolved signals.
func seedDiagnosticWorkflow(env *m6Env) {
	wfID := "wf-diag"
	mrIID := int64(9)
	started := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	finished := started.Add(3 * time.Minute)
	fake := env.store.(*fakeReviewStore)
	fake.workflows = append(fake.workflows, store.Workflow{
		ID: wfID, Kind: "review", ProjectID: 42, MRIID: &mrIID,
		Trigger: "mr_update", Status: "completed", Verdict: "INCOMPLETE",
		CreatedAt: started, StartedAt: &started, FinishedAt: &finished,
	})
	synthesis, _ := json.Marshal(map[string]any{
		"verdict": "INCOMPLETE",
		"summary": "评审未完成：上游证据不足，不能给出 PASS。",
		"groups":  []any{},
		"warnings": []string{
			"验证通道 BLOCKED：没有流水线",
			"verify failed for group f1: leaked upstream body",
		},
		"unresolved": []map[string]string{
			{"node": "coverage", "kind": "evidence_gap", "message": "缺少流水线日志"},
			{"node": "lanes.review", "kind": "review_blocking", "message": "档位给出阻断但没有任何 finding"},
		},
		"output": map[string]any{"verdict": "INCOMPLETE", "summary": "未完成", "decisions": []any{}},
	})
	fake.setNodes(wfID,
		store.WorkflowNode{WorkflowID: wfID, ID: "plan", Kind: "plan", Status: "completed", Required: true, Attempt: 1},
		store.WorkflowNode{
			WorkflowID: wfID, ID: "verify.f1", Kind: "verify", Status: "failed", Required: false,
			Attempt: 1, Params: json.RawMessage(`{"group_id":"f1"}`),
			Error: "leaked upstream body",
		},
		store.WorkflowNode{
			WorkflowID: wfID, ID: "verify.f2", Kind: "verify", Status: "skipped", Required: false,
			Attempt: 1, Error: "required dependency did not complete",
		},
		store.WorkflowNode{
			WorkflowID: wfID, ID: "synthesize", Kind: "synthesize", Status: "completed",
			Required: true, Attempt: 1, Output: synthesis,
		},
	)
}

func TestReviewDetailExposesReviewLevelDiagnostics(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedDiagnosticWorkflow(env)
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/reviews/wf-diag", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	if body["summary"] != "评审未完成：上游证据不足，不能给出 PASS。" {
		t.Fatalf("summary = %v", body["summary"])
	}
	warnings := body["warnings"].([]any)
	if len(warnings) != 2 || warnings[0] != "验证通道 BLOCKED：没有流水线" {
		t.Fatalf("warnings = %v", warnings)
	}
	if warnings[1] != "verify failed for group f1: node failed" {
		t.Fatalf("warning leaked the stored verify diagnostic: %v", warnings[1])
	}
	unresolved := body["unresolved"].([]any)
	if len(unresolved) != 2 {
		t.Fatalf("unresolved = %v", unresolved)
	}
	first := unresolved[0].(map[string]any)
	if first["node"] != "coverage" || first["kind"] != "evidence_gap" || first["message"] != "缺少流水线日志" {
		t.Fatalf("unresolved[0] = %v", first)
	}

	nodes := map[string]map[string]any{}
	for _, item := range body["nodes"].([]any) {
		node := item.(map[string]any)
		nodes[node["id"].(string)] = node
	}
	if nodes["verify.f2"]["status"] != "skipped" || nodes["verify.f2"]["reason"] != "required_node_failed" {
		t.Fatalf("skipped node = %v", nodes["verify.f2"])
	}
	if _, present := nodes["synthesize"]["reason"]; present {
		t.Fatalf("completed node carries a reason: %v", nodes["synthesize"]["reason"])
	}

	skipped := client.request(http.MethodGet, "/api/v1/reviews/wf-diag/nodes/verify.f2", nil, nil)
	assertStatus(t, skipped, http.StatusOK)
	skippedBody := env.decode(t, skipped)
	if skippedBody["status"] != "skipped" || skippedBody["reason"] != "required_node_failed" {
		t.Fatalf("skipped node detail = %v", skippedBody)
	}

	failed := client.request(http.MethodGet, "/api/v1/reviews/wf-diag/nodes/verify.f1", nil, nil)
	assertStatus(t, failed, http.StatusOK)
	failedBody := env.decode(t, failed)
	if failedBody["error"] != "node failed" {
		t.Fatalf("stored verify diagnostic was returned raw: %v", failedBody["error"])
	}
	if failedBody["reason"] != "executor_error" {
		t.Fatalf("failed node reason = %v", failedBody["reason"])
	}
}

func TestReviewNodeCallsReportRetentionPurge(t *testing.T) {
	fake := newFakeReviewStore()
	platform := &config.Config{}
	platform.Storage.LLMCallRetention = 720 * time.Hour
	env := newM6Env(t, m6Options{store: fake, platform: platform})
	wfID := "wf-old"
	mrIID := int64(4)
	finished := time.Now().Add(-1000 * time.Hour)
	started := finished.Add(-time.Minute)
	fake.workflows = append(fake.workflows, store.Workflow{
		ID: wfID, Kind: "review", ProjectID: 42, MRIID: &mrIID, Trigger: "mr_update",
		Status: "completed", CreatedAt: started, StartedAt: &started, FinishedAt: &finished,
	})
	fake.setNodes(wfID,
		store.WorkflowNode{WorkflowID: wfID, ID: "review.std.p1", Kind: "review.standards", Status: "completed",
			Attempt: 1, TokensIn: 120, TokensOut: 30, StartedAt: &started, FinishedAt: &finished},
	)
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/reviews/"+wfID+"/nodes/review.std.p1/calls", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	retention := body["retention"].(map[string]any)
	if retention["details_purged"] != true {
		t.Fatalf("retention = %v, want details_purged", retention)
	}
	if retention["llm_call_retention_hours"] != float64(720) {
		t.Fatalf("retention hours = %v", retention["llm_call_retention_hours"])
	}

	// A recent node with no stored calls has not reached the retention cutoff.
	fake.setNodes(wfID,
		store.WorkflowNode{WorkflowID: wfID, ID: "review.std.p1", Kind: "review.standards", Status: "running",
			Attempt: 1, TokensIn: 120, TokensOut: 30},
	)
	fake.workflows[0].FinishedAt = nil
	recent := client.request(http.MethodGet, "/api/v1/reviews/"+wfID+"/nodes/review.std.p1/calls", nil, nil)
	assertStatus(t, recent, http.StatusOK)
	recentBody := env.decode(t, recent)
	if recentBody["retention"].(map[string]any)["details_purged"] != false {
		t.Fatalf("recent retention = %v", recentBody["retention"])
	}
}
