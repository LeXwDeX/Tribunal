package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestHistoricalNodeErrorIsNotExposed(t *testing.T) {
	const canary = "historical-private-canary"
	env, fake := isolationEnv(t)
	env.grant(8, 43, store.ProjectAccessRead)
	fake.setWorkflow(store.Workflow{ID: "wf-secret", ProjectID: 43, Kind: "review", Trigger: "api", Status: "running"})
	fake.setNodes("wf-secret", store.WorkflowNode{WorkflowID: "wf-secret", ID: "plan", Kind: "plan", Status: "running", Attempt: 1})
	client := env.loginAs("code-viewer")
	_, events := streamEventsAs(t, env, client, "wf-secret")
	fake.setNodes("wf-secret", store.WorkflowNode{WorkflowID: "wf-secret", ID: "plan", Kind: "plan", Status: "failed", Attempt: 1, Error: canary, TokensIn: 12})
	fake.setWorkflow(store.Workflow{ID: "wf-secret", ProjectID: 43, Kind: "review", Trigger: "api", Status: "failed"})
	for _, path := range []string{"/api/v1/reviews/wf-secret", "/api/v1/reviews/wf-secret/graph", "/api/v1/reviews/wf-secret/nodes/plan"} {
		response := client.request(http.MethodGet, path, nil, nil)
		assertStatus(t, response, http.StatusOK)
		if strings.Contains(string(response.body), canary) {
			t.Errorf("%s exposed historical node error: %s", path, response.body)
		}
		if strings.HasSuffix(path, "/nodes/plan") && (!strings.Contains(string(response.body), `"status":"failed"`) || !strings.Contains(string(response.body), `"attempt":1`) || !strings.Contains(string(response.body), `"tokens_in":12`)) {
			t.Errorf("lost node details: %s", response.body)
		}
	}
	sawFailed := false
	for _, event := range drainEvents(t, events) {
		if strings.Contains(event.data, canary) {
			t.Errorf("SSE exposed historical node error: %+v", event)
		}
		if event.name == "node" && strings.Contains(event.data, `"status":"failed"`) && strings.Contains(event.data, `"attempt":1`) {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Error("SSE did not report the failed node and attempt")
	}
	nodes, err := fake.GetWorkflowNodes(context.Background(), "wf-secret")
	if err != nil || nodes[0].Error != canary {
		t.Fatalf("historical node changed: %+v %v", nodes, err)
	}
}

func TestHistoricalNodeOutputsAreSafe(t *testing.T) {
	const canary = "historical-verifier-canary"
	env, fake := isolationEnv(t)
	env.grant(8, 43, store.ProjectAccessRead)
	fake.setWorkflow(store.Workflow{ID: "wf-output", ProjectID: 43, Kind: "review", Trigger: "api", Status: "completed"})
	output := json.RawMessage(`{"groups":[{"ID":"g","Body":"business body","Challenge":{"Status":"uncertain","Reason":"challenge failed: ` + canary + `"},"Decision":{"Action":"mark","Reason":"challenge failed: ` + canary + `; on_uncertain=mark","Source":"rules"}}],"warnings":["verify failed for group g: ` + canary + `"],"verdict":"BLOCKING","summary":"summary","output":{"verdict":"BLOCKING","summary":"summary","decisions":[{"finding_id":"g","action":"mark","reason":"challenge failed: ` + canary + `; on_uncertain=mark"}],"new_findings":[],"extend_requests":[]}}`)
	fake.setNodes("wf-output", store.WorkflowNode{ID: "verify.1", Kind: "verify", Status: "failed", Params: json.RawMessage(`{"group_id":"g"}`), Error: canary}, store.WorkflowNode{ID: "synthesize", Kind: "synthesize", Status: "completed", Output: output}, store.WorkflowNode{ID: "adjudicate", Kind: "adjudicate", Status: "completed", Output: output})
	client := env.loginAs("code-viewer")
	for _, id := range []string{"synthesize", "adjudicate"} {
		response := client.request(http.MethodGet, "/api/v1/reviews/wf-output/nodes/"+id, nil, nil)
		assertStatus(t, response, http.StatusOK)
		if strings.Contains(string(response.body), canary) || !strings.Contains(string(response.body), "business body") {
			t.Fatalf("unsafe output or changed business body: %s", response.body)
		}
	}
	nodes, err := fake.GetWorkflowNodes(context.Background(), "wf-output")
	if err != nil || !strings.Contains(string(nodes[1].Output), canary) {
		t.Fatalf("stored history changed: %+v %v", nodes, err)
	}
}

func TestHistoricalFindingDiagnosticsAreSafe(t *testing.T) {
	const canary = "historical-verifier-canary"
	env, fake := isolationEnv(t)
	env.grant(8, 43, store.ProjectAccessManage)
	fake.setWorkflow(store.Workflow{ID: "wf-finding", ProjectID: 43, Kind: "review", Trigger: "api", Status: "completed"})
	fake.setNodes("wf-finding", store.WorkflowNode{ID: "verify.1", Kind: "verify", Status: "failed", Params: json.RawMessage(`{"group_id":"g"}`), Error: canary})
	fake.setFindings("wf-finding", store.Finding{ID: "g", WorkflowID: "wf-finding", Title: "title", Body: "business body", Decision: "mark", DecisionReason: "challenge failed: " + canary + "; on_uncertain=mark", ChallengeEvidence: json.RawMessage(`{"Status":"uncertain","Reason":"challenge failed: ` + canary + `"}`)})
	client := env.loginAs("code-viewer")
	for _, response := range []apiResponse{
		client.request(http.MethodGet, "/api/v1/reviews/wf-finding/findings", nil, nil),
		client.json(http.MethodPut, "/api/v1/reviews/wf-finding/findings/g/annotation", map[string]any{"annotation": "valid"}),
	} {
		assertStatus(t, response, http.StatusOK)
		if strings.Contains(string(response.body), canary) || !strings.Contains(string(response.body), "business body") {
			t.Errorf("unsafe finding response: %s", response.body)
		}
	}
	findings, err := fake.ListFindings(context.Background(), store.FindingFilter{WorkflowID: "wf-finding"})
	if err != nil || !strings.Contains(findings[0].DecisionReason, canary) {
		t.Fatalf("historical finding changed: %+v %v", findings, err)
	}
}
