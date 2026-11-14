package dag

import (
	"context"
	"strings"
	"testing"
)

type detailedTestError struct {
	class  string
	detail string
	text   string
}

func (e detailedTestError) Error() string      { return e.text }
func (e detailedTestError) ErrorClass() string { return e.class }
func (e detailedTestError) ErrorDetail() string {
	return e.detail
}

func runDetailedFailure(t *testing.T, exec NodeExecutor) (NodeView, NodeFailedPayload, string) {
	t.Helper()
	store := NewMemoryEventStore()
	engine := NewEngine(store, exec, nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{{ID: "a", Kind: "x", Required: true}},
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
	payload := nodeFailedPayload(t, store, wfID)
	events, err := store.Load(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	var raw strings.Builder
	for _, ev := range events {
		raw.Write(ev.Payload)
	}
	return wf.Nodes["a"], payload, raw.String()
}

// A code-owned fixed detail from the executor survives into the durable failure
// event and the projected node, alongside the class.
func TestNodeFailurePersistsCodeOwnedDetail(t *testing.T) {
	const canary = "synthetic-private-canary"
	node, payload, raw := runDetailedFailure(t, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{}, detailedTestError{
			class:  "no_submit",
			detail: "submit_result missing after 30 turns",
			text:   canary,
		}
	}))
	if node.Status != NodeFailed || node.Reason != "no_submit" || node.Error != "submit_result missing after 30 turns" {
		t.Fatalf("node = %+v", node)
	}
	if payload.Reason != "no_submit" || payload.ErrorKind != "no_submit" || payload.Error != node.Error {
		t.Fatalf("payload = %+v", payload)
	}
	if strings.Contains(raw, canary) {
		t.Fatalf("failure events leaked the raw error text: %s", raw)
	}
}

// Details are code-owned fixed summaries. Anything that could carry a raw
// upstream body or unbounded data is refused, not truncated.
func TestNodeFailureRejectsUnsafeDetail(t *testing.T) {
	const canary = "synthetic-private-canary"
	for name, detail := range map[string]string{
		"raw body":     `provider said {"error":"` + canary + `"}`,
		"control char": "submit_result missing\n" + canary,
		"too long":     strings.Repeat("x", 200),
		"empty":        "   ",
	} {
		t.Run(name, func(t *testing.T) {
			node, payload, raw := runDetailedFailure(t, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
				return NodeOutput{}, detailedTestError{class: "no_submit", detail: detail, text: canary}
			}))
			if node.Error != "node execution failed" || payload.Error != "node execution failed" {
				t.Fatalf("node=%+v payload=%+v, want the generic failure message", node, payload)
			}
			if node.Reason != "no_submit" {
				t.Fatalf("class was dropped: %+v", node)
			}
			if strings.Contains(raw, canary) {
				t.Fatalf("failure events leaked the raw error text: %s", raw)
			}
		})
	}
}

// The fixed detail is bounded and allows only the characters code-owned
// summaries use, so a future executor cannot smuggle payload through it.
func TestSanitizeFailureDetail(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"submit_result missing after 30 turns", "submit_result missing after 30 turns"},
		{"workflow token budget exhausted (1950000 of 2000000 tokens)", "workflow token budget exhausted (1950000 of 2000000 tokens)"},
		{"  model context window exceeded  ", "model context window exceeded"},
		{"tab\there", ""},
		{strings.Repeat("x", maxFailureDetailRunes+1), ""},
	} {
		if got := sanitizeFailureDetail(tc.in); got != tc.want {
			t.Fatalf("sanitizeFailureDetail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
