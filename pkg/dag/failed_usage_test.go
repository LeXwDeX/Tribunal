package dag

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestEnginePersistsKnownUsageFromFailedAttempt(t *testing.T) {
	exec := funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Usage: Usage{InputTokens: 12, OutputTokens: 3}}, errors.New("model failed after one turn")
	})
	engine := fastEngine(exec, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes:  []NodeSpec{{ID: "a", Kind: "x", Required: true}},
		Budget: Budget{MaxTokens: 100},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowFailed {
		t.Fatalf("workflow = %s", wf.Status)
	}
	if wf.Usage.InputTokens != 12 || wf.Usage.OutputTokens != 3 {
		t.Fatalf("workflow usage = %+v, want known failure usage", wf.Usage)
	}
	node := wf.Nodes["a"]
	if node.Status != NodeFailed || node.Usage.InputTokens != 12 || node.Usage.OutputTokens != 3 {
		t.Fatalf("node = %+v, want failed usage", node)
	}
}

func TestEngineFailureUsagePayloadRoundTrip(t *testing.T) {
	ev, err := makeEvent(EventNodeFailed, NodeFailedPayload{
		NodeID: "a", Attempt: 1, Error: "boom",
		Usage: Usage{InputTokens: 4, CostMicros: 7},
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	var decoded NodeFailedPayload
	if err := json.Unmarshal(ev.Payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Usage.InputTokens != 4 || decoded.Usage.CostMicros != 7 {
		t.Fatalf("payload usage = %+v", decoded.Usage)
	}
}
