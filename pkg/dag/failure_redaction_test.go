package dag

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFailureEventsDoNotPersistErrorDetails(t *testing.T) {
	const canary = "synthetic-private-canary"
	for _, kind := range []string{"executor", "timeout", "expander", "validation"} {
		t.Run(kind, func(t *testing.T) {
			exec := funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
				if kind == "executor" {
					return NodeOutput{Usage: Usage{InputTokens: 12, OutputTokens: 3}}, errors.New(canary)
				}
				if kind == "timeout" {
					return NodeOutput{}, errors.Join(context.DeadlineExceeded, errors.New(canary))
				}
				return NodeOutput{Data: json.RawMessage(`{}`)}, nil
			})
			exp := funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
				if kind == "expander" {
					return Extension{}, errors.New(canary)
				}
				return Extension{Nodes: []NodeSpec{node("extra", canary)}}, nil
			})
			store := NewMemoryEventStore()
			engine := NewEngine(store, exec, exp, Options{})
			ctx := context.Background()
			id, err := engine.Create(ctx, GraphSpec{Nodes: []NodeSpec{{ID: "a", Kind: "x", Required: true, Expands: kind == "expander" || kind == "validation"}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Run(ctx, id); err != nil {
				t.Fatal(err)
			}
			wf, err := engine.Snapshot(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			want := "expand_failed"
			if kind == "executor" {
				want = "executor_error"
			}
			if kind == "timeout" {
				want = "timeout"
			}
			v := wf.Nodes["a"]
			if v.Status != NodeFailed || v.Reason != want || v.Attempt != 1 || v.Error == "" || strings.Contains(v.Error, canary) {
				t.Errorf("failed node = %+v, want safe error and reason %s", v, want)
			}
			if kind == "executor" && (v.Usage.InputTokens != 12 || wf.Usage.OutputTokens != 3) {
				t.Errorf("usage: node=%+v workflow=%+v", v.Usage, wf.Usage)
			}
			events, err := store.Load(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, ev := range events {
				if ev.Type != EventNodeFailed {
					continue
				}
				found = true
				var payload NodeFailedPayload
				if err := json.Unmarshal(ev.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(ev.Payload), canary) || payload.Error == "" || payload.Reason != want || payload.ErrorKind != want || payload.Attempt != 1 {
					t.Errorf("failure payload = %s", ev.Payload)
				}
			}
			if !found {
				t.Fatal("missing failed event")
			}
		})
	}
}
