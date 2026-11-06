package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// seedInterruptedEvents records one recovery restart of review.std.p1.
func seedInterruptedEvents(t *testing.T, fake *fakeReviewStore, wfID string) {
	t.Helper()
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	mk := func(typ string, payload any) dag.Event {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return dag.Event{Type: typ, Payload: raw, At: at}
	}
	es := dag.NewMemoryEventStore()
	events := []dag.Event{
		mk(dag.EventWorkflowCreated, dag.WorkflowCreatedPayload{WorkflowID: wfID, Spec: dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "plan", Kind: "plan"}, {ID: "review.std.p1", Kind: "review"}}}}),
		mk(dag.EventWorkflowStarted, struct{}{}),
		mk(dag.EventNodeQueued, dag.NodeQueuedPayload{NodeID: "review.std.p1", Attempt: 1}),
		mk(dag.EventNodeStarted, dag.NodeStartedPayload{NodeID: "review.std.p1", Attempt: 1}),
		mk(dag.EventNodeRestarted, dag.NodeRestartedPayload{NodeID: "review.std.p1", Attempt: 1, NextAttempt: 2, UsageIncomplete: true}),
	}
	if _, err := es.Append(context.Background(), wfID, 0, events...); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	fake.events = es
}

func TestReviewUsageInterruptionsAPIExposure(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	seedInterruptedEvents(t, fake, "wf-1")
	client := env.businessClient()

	detail := client.request(http.MethodGet, "/api/v1/reviews/wf-1", nil, nil)
	assertStatus(t, detail, http.StatusOK)
	body := env.decode(t, detail)
	if body["usage_interruptions"] != float64(1) {
		t.Fatalf("detail usage_interruptions = %v, want 1", body["usage_interruptions"])
	}
	nodes := nodesByID(t, body)
	if nodes["review.std.p1"]["usage_interruptions"] != float64(1) {
		t.Fatalf("detail node = %v, want usage_interruptions 1", nodes["review.std.p1"])
	}
	if _, present := nodes["plan"]["usage_interruptions"]; present {
		t.Fatalf("clean node carries usage_interruptions: %v", nodes["plan"])
	}

	graph := client.request(http.MethodGet, "/api/v1/reviews/wf-1/graph", nil, nil)
	assertStatus(t, graph, http.StatusOK)
	if got := nodesByID(t, env.decode(t, graph))["review.std.p1"]["usage_interruptions"]; got != float64(1) {
		t.Fatalf("graph node usage_interruptions = %v, want 1", got)
	}

	node := client.request(http.MethodGet, "/api/v1/reviews/wf-1/nodes/review.std.p1", nil, nil)
	assertStatus(t, node, http.StatusOK)
	if got := env.decode(t, node)["usage_interruptions"]; got != float64(1) {
		t.Fatalf("node usage_interruptions = %v, want 1", got)
	}
}

func TestReviewUsageInterruptionsOmittedWithoutInterruptions(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	client := env.businessClient()

	detail := client.request(http.MethodGet, "/api/v1/reviews/wf-1", nil, nil)
	assertStatus(t, detail, http.StatusOK)
	body := env.decode(t, detail)
	if _, present := body["usage_interruptions"]; present {
		t.Fatalf("detail carries usage_interruptions without events: %v", body["usage_interruptions"])
	}
	node := client.request(http.MethodGet, "/api/v1/reviews/wf-1/nodes/review.std.p1", nil, nil)
	assertStatus(t, node, http.StatusOK)
	if _, present := env.decode(t, node)["usage_interruptions"]; present {
		t.Fatal("node carries usage_interruptions without events")
	}
}
