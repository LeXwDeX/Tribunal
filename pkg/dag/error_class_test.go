package dag

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type classedTestError struct {
	class string
	text  string
}

func (e classedTestError) Error() string      { return e.text }
func (e classedTestError) ErrorClass() string { return e.class }

func TestNodeFailureReasonUsesErrorClass(t *testing.T) {
	const canary = "classified-error-private-canary"
	cases := []struct {
		name  string
		class string
		want  string
	}{
		{name: "auth", class: "auth", want: "auth"},
		{name: "rate limited", class: "rate_limited", want: "rate_limited"},
		{name: "empty", class: "", want: "executor_error"},
		{name: "invalid", class: "Auth Failure", want: "executor_error"},
		{name: "too long", class: strings.Repeat("a", 33), want: "executor_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryEventStore()
			engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
				return NodeOutput{}, classedTestError{class: tc.class, text: canary}
			}), nil, Options{})
			wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{{ID: "a", Kind: "x", Required: true}}}, nil)
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
			node := wf.Nodes["a"]
			if node.Status != NodeFailed || node.Reason != tc.want {
				t.Fatalf("node = %+v, want failed reason %q", node, tc.want)
			}
			if strings.Contains(node.Error, canary) {
				t.Fatalf("node error leaked: %q", node.Error)
			}
			payload := nodeFailedPayload(t, store, wfID)
			if payload.Reason != tc.want || payload.ErrorKind != tc.want {
				t.Fatalf("failure payload = %+v, want reason %q", payload, tc.want)
			}
			if strings.Contains(payload.Error, canary) {
				t.Fatalf("failure payload leaked: %+v", payload)
			}
		})
	}
}

func TestNodePanicUsesFixedClassWithoutValue(t *testing.T) {
	const canary = "panic-private-canary"
	store := NewMemoryEventStore()
	engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		panic(canary)
	}), nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{{ID: "a", Kind: "x", Required: true}}}, nil)
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
	node := wf.Nodes["a"]
	if node.Status != NodeFailed || node.Reason != "panic" {
		t.Fatalf("node = %+v, want failed reason panic", node)
	}
	payload := nodeFailedPayload(t, store, wfID)
	if payload.Reason != "panic" || payload.ErrorKind != "panic" {
		t.Fatalf("failure payload = %+v, want panic", payload)
	}
	if strings.Contains(string(mustFailureEvent(t, store, wfID).Payload), canary) {
		t.Fatal("failure event leaked the panic value")
	}

	_, err = executeSafely(context.Background(), funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		panic(canary)
	}), NodeInput{})
	var classed interface{ ErrorClass() string }
	if !errors.As(err, &classed) || classed.ErrorClass() != "panic" {
		t.Fatalf("executeSafely error = %v, want panic class", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("executeSafely error leaked panic value: %v", err)
	}
}

func nodeFailedPayload(t *testing.T, store *MemoryEventStore, wfID string) NodeFailedPayload {
	t.Helper()
	var payload NodeFailedPayload
	if err := json.Unmarshal(mustFailureEvent(t, store, wfID).Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func mustFailureEvent(t *testing.T, store *MemoryEventStore, wfID string) Event {
	t.Helper()
	events, err := store.Load(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == EventNodeFailed {
			return ev
		}
	}
	t.Fatal("missing node.failed event")
	return Event{}
}
