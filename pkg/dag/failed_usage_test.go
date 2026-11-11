package dag

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	want := Usage{InputTokens: 4, OutputTokens: 2, CacheReadTokens: 1}
	ev, err := makeEvent(EventNodeFailed, NodeFailedPayload{
		NodeID: "a", Attempt: 1, Error: "boom",
		Usage: want,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ev.Payload), "cost_micros") {
		t.Fatalf("new event payload exposes cost_micros: %s", ev.Payload)
	}
	var decoded NodeFailedPayload
	if err := json.Unmarshal(ev.Payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Usage != want {
		t.Fatalf("payload usage = %+v, want %+v", decoded.Usage, want)
	}
}

// Legacy events written before v1.4.0 carry cost_micros. They must still
// decode, and neither the projection nor a new encoding may expose the field.
func TestUsageDecodesLegacyCostMicrosWithoutExposingIt(t *testing.T) {
	var usage Usage
	raw := `{"input_tokens":7,"output_tokens":3,"cost_micros":42,"cache_read_tokens":1}`
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatalf("decode legacy usage: %v", err)
	}
	want := Usage{InputTokens: 7, OutputTokens: 3, CacheReadTokens: 1}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "cost_micros") {
		t.Fatalf("encoded usage exposes cost_micros: %s", encoded)
	}
}
