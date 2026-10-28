package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type privateErrorExecutor struct{}

func (privateErrorExecutor) Execute(context.Context, dag.NodeInput) (dag.NodeOutput, error) {
	return dag.NodeOutput{Usage: dag.Usage{InputTokens: 12}}, errors.New("sqlite-executor-private-canary")
}

type expansionExecutor struct{}

func (expansionExecutor) Execute(context.Context, dag.NodeInput) (dag.NodeOutput, error) {
	return dag.NodeOutput{Data: json.RawMessage(`{}`)}, nil
}

type privateExpander struct{ validation bool }

func (e privateExpander) Expand(context.Context, dag.WorkflowView, dag.NodeSpec, json.RawMessage) (dag.Extension, error) {
	if e.validation {
		return dag.Extension{Nodes: []dag.NodeSpec{{ID: "extra", Kind: "x", DependsOn: []string{"sqlite-expansion-private-canary"}}}}, nil
	}
	return dag.Extension{}, errors.New("sqlite-expansion-private-canary")
}

func TestEngineExpansionFailureSQLiteEventAndProjectionAreSafe(t *testing.T) {
	const canary = "sqlite-expansion-private-canary"
	for _, validation := range []bool{false, true} {
		name := "expander"
		if validation {
			name = "validation"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := openTestStore(t, newFakeClock())
			mustProject(t, st, 42)
			engine := dag.NewEngine(st, expansionExecutor{}, privateExpander{validation}, dag.Options{})
			id, err := engine.Create(ctx, dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "a", Kind: "x", Required: true, Expands: true}}}, reviewMeta(42, 7, "head"))
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Run(ctx, id); err != nil {
				t.Fatal(err)
			}
			var payload, projected string
			if err := st.db.QueryRowContext(ctx, `SELECT payload FROM events WHERE workflow_id = ? AND type = ?`, id, dag.EventNodeFailed).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if err := st.db.QueryRowContext(ctx, `SELECT error FROM workflow_nodes WHERE workflow_id = ? AND id = 'a'`, id).Scan(&projected); err != nil {
				t.Fatal(err)
			}
			var failure dag.NodeFailedPayload
			if err := json.Unmarshal([]byte(payload), &failure); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(payload, canary) || strings.Contains(projected, canary) || failure.Reason != "expand_failed" || failure.Attempt != 1 || projected == "" {
				t.Fatalf("failure event %s, projection %q", payload, projected)
			}
		})
	}
}

func TestEngineFailureSQLiteEventAndProjectionAreSafe(t *testing.T) {
	const canary = "sqlite-executor-private-canary"
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)
	engine := dag.NewEngine(st, privateErrorExecutor{}, nil, dag.Options{})
	id, err := engine.Create(ctx, dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "a", Kind: "x", Required: true}}}, reviewMeta(42, 7, "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	events, err := st.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == dag.EventNodeFailed {
			found = true
			if strings.Contains(string(ev.Payload), canary) {
				t.Errorf("event leaked: %s", ev.Payload)
			}
		}
	}
	if !found {
		t.Fatal("no failure event")
	}
	nodes, err := st.GetWorkflowNodes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Status != "failed" || nodes[0].Attempt != 1 || nodes[0].TokensIn != 12 || nodes[0].Error == "" || strings.Contains(nodes[0].Error, canary) {
		t.Fatalf("node projection = %+v", nodes)
	}
	var stored string
	if err := st.db.QueryRowContext(ctx, `SELECT error FROM workflow_nodes WHERE workflow_id = ? AND id = 'a'`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, canary) {
		t.Fatalf("stored projection leaked: %s", stored)
	}
}

func TestHistoricalFailureReplayKeepsOriginalData(t *testing.T) {
	const canary = "historical-replay-canary"
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	id := "wf-legacy-failure"
	rev := mustCreateWorkflow(t, st, id, reviewMeta(42, 7, "head"), clock.Now())
	rev = mustAppendEvent(t, st, id, rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, clock.Now()))
	rev = mustAppendEvent(t, st, id, rev, queuedEvent(t, "a", 1, clock.Now()))
	rev = mustAppendEvent(t, st, id, rev, startedEvent(t, "a", 1, "p", "m", clock.Now()))
	rev = mustAppendEvent(t, st, id, rev, testEvent(t, dag.EventNodeFailed, dag.NodeFailedPayload{NodeID: "a", Attempt: 1, Error: canary, Reason: "executor_error", Usage: dag.Usage{InputTokens: 7}}, clock.Now()))
	_ = mustAppendEvent(t, st, id, rev, testEvent(t, dag.EventNodeFailed, dag.NodeFailedPayload{NodeID: "b", Attempt: 1, Error: "required dependency did not complete", Reason: "unsatisfied"}, clock.Now()))
	events, err := st.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := dag.Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["a"].Error != canary || wf.Nodes["a"].Reason != "executor_error" || wf.Nodes["a"].Usage.InputTokens != 7 {
		t.Fatalf("legacy replay changed: %+v", wf.Nodes["a"])
	}
	var payload, projected string
	if err := st.db.QueryRowContext(ctx, `SELECT payload FROM events WHERE workflow_id = ? AND type = ? ORDER BY seq LIMIT 1`, id, dag.EventNodeFailed).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT error FROM workflow_nodes WHERE workflow_id = ? AND id = 'a'`, id).Scan(&projected); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, canary) || projected != canary {
		t.Fatalf("historical database was changed: %s / %s", payload, projected)
	}
}
