package dag

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func planScheduleView(timeout time.Duration, queuedAt time.Time) WorkflowView {
	return WorkflowView{
		ID:     "wf-negative-timeout",
		Status: WorkflowRunning,
		Order:  []string{"a"},
		Nodes: map[string]NodeView{
			"a": {
				ID:       "a",
				Spec:     NodeSpec{ID: "a", Kind: "x", Required: true, Timeout: timeout},
				Status:   NodeQueued,
				Attempt:  1,
				QueuedAt: queuedAt,
			},
		},
	}
}

// A negative node timeout disables the node deadline (#451); queue waiting must
// therefore not fail with queue_timeout even after the engine default elapses.
func TestEngineQueueTimeoutNegativeTimeout(t *testing.T) {
	engine := fastEngine(funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{DefaultNodeTimeout: 10 * time.Minute})
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	events, starts := engine.planSchedule(planScheduleView(-1, now.Add(-11*time.Minute)), now)
	if len(events) != 0 {
		t.Fatalf("negative timeout produced events: %+v", events)
	}
	if len(starts) != 1 || starts[0] != "a" {
		t.Fatalf("negative timeout starts = %v, want [a]", starts)
	}
}

// Zero timeout still means the engine default, so the existing queue deadline
// must keep expiring; the fix must not disable default timeouts.
func TestEngineQueueTimeoutDefaultStillExpires(t *testing.T) {
	engine := fastEngine(funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{DefaultNodeTimeout: 10 * time.Minute})
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	events, starts := engine.planSchedule(planScheduleView(0, now.Add(-11*time.Minute)), now)
	if len(starts) != 0 {
		t.Fatalf("expired default timeout still started: %v", starts)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one queue timeout", events)
	}
	var payload NodeFailedPayload
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if payload.Reason != "queue_timeout" || payload.NodeID != "a" {
		t.Fatalf("payload = %+v, want a queue_timeout for a", payload)
	}
}
