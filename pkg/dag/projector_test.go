package dag

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func mustEv(t *testing.T, typ string, payload any, at time.Time) Event {
	t.Helper()
	ev, err := makeEvent(typ, payload, at)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func created(t *testing.T, nodes ...NodeSpec) Event {
	t.Helper()
	return mustEv(t, EventWorkflowCreated, WorkflowCreatedPayload{
		WorkflowID: "wf",
		Spec:       GraphSpec{Nodes: nodes},
		Meta:       map[string]string{"project_id": "42"},
	}, testNow)
}

func nodeEvent(t *testing.T, typ, id string, attempt int) Event {
	t.Helper()
	var payload any
	switch typ {
	case EventNodeQueued:
		payload = NodeQueuedPayload{NodeID: id, Attempt: attempt}
	case EventNodeStarted:
		payload = NodeStartedPayload{NodeID: id, Attempt: attempt}
	case EventNodeCompleted:
		payload = NodeCompletedPayload{NodeID: id, Attempt: attempt, Output: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: 2, OutputTokens: 3}}
	case EventNodeFailed:
		payload = NodeFailedPayload{NodeID: id, Attempt: attempt, Error: "boom"}
	case EventNodeAborted:
		payload = NodeAbortedPayload{NodeID: id, Attempt: attempt}
	case EventNodeSkipped, EventNodeCancelled:
		payload = NodeSkippedPayload{NodeID: id, Attempt: attempt}
	case EventNodeRestarted:
		payload = NodeRestartedPayload{NodeID: id, Attempt: attempt}
	default:
		t.Fatalf("unsupported node event %s", typ)
	}
	return mustEv(t, typ, payload, testNow.Add(time.Duration(attempt)*time.Second))
}

func seq(events []Event) []Event {
	for i := range events {
		events[i].Seq = int64(i + 1)
	}
	return events
}

func TestProjectCreatedAndStarted(t *testing.T) {
	events := seq([]Event{created(t, node("a")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow)})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if wf.ID != "wf" || wf.Status != WorkflowRunning || wf.Revision != 2 {
		t.Fatalf("unexpected view: %+v", wf)
	}
	if wf.Meta["project_id"] != "42" {
		t.Fatal("meta lost")
	}
	if wf.Nodes["a"].Attempt != 1 || wf.Nodes["a"].Status != NodePending {
		t.Fatalf("node: %+v", wf.Nodes["a"])
	}
}

func TestProjectRestartUsageIncompleteStaysSticky(t *testing.T) {
	events := seq([]Event{
		created(t, node("a")),
		mustEv(t, EventNodeStarted, NodeStartedPayload{NodeID: "a", Attempt: 1}, testNow),
		mustEv(t, EventNodeRestarted, NodeRestartedPayload{NodeID: "a", Attempt: 2, NextAttempt: 2, UsageIncomplete: true}, testNow),
		mustEv(t, EventNodeStarted, NodeStartedPayload{NodeID: "a", Attempt: 2}, testNow),
		mustEv(t, EventNodeCompleted, NodeCompletedPayload{NodeID: "a", Attempt: 2, Output: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: 2}}, testNow),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if !wf.UsageIncomplete || !wf.Nodes["a"].UsageIncomplete {
		t.Fatalf("incomplete marker was cleared after recovery: workflow=%t node=%t", wf.UsageIncomplete, wf.Nodes["a"].UsageIncomplete)
	}
}

func TestProjectNodeTransitionPairMatrix(t *testing.T) {
	types := map[NodeStatus]string{
		NodeQueued:    EventNodeQueued,
		NodeRunning:   EventNodeStarted,
		NodeCompleted: EventNodeCompleted,
		NodeFailed:    EventNodeFailed,
		NodeAborted:   EventNodeAborted,
		NodeSkipped:   EventNodeSkipped,
	}
	// drive builds the shortest event prefix that reaches a source state.
	drive := map[NodeStatus][]Event{
		NodePending:   nil,
		NodeQueued:    {nodeEvent(t, EventNodeQueued, "a", 1)},
		NodeRunning:   {nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1)},
		NodeCompleted: {nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1), nodeEvent(t, EventNodeCompleted, "a", 1)},
		NodeFailed:    {nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1), nodeEvent(t, EventNodeFailed, "a", 1)},
		NodeAborted:   {nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1), nodeEvent(t, EventNodeAborted, "a", 1)},
		NodeSkipped:   {nodeEvent(t, EventNodeSkipped, "a", 1)},
	}
	all := []NodeStatus{NodePending, NodeQueued, NodeRunning, NodeCompleted, NodeFailed, NodeAborted, NodeSkipped}
	for _, from := range all {
		for _, to := range all {
			if to == NodePending {
				// pending has no plain event; queued and running use restarted below.
				continue
			}
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				events := []Event{created(t, node("a"))}
				events = append(events, drive[from]...)
				events = append(events, nodeEvent(t, types[to], "a", 1))
				events = seq(events)
				wf, err := Project(events)
				want := NodeTransitionAllowed(from, to)
				if want {
					if err != nil {
						t.Fatalf("legal transition rejected: %v", err)
					}
					if wf.Nodes["a"].Status != to {
						t.Fatalf("status = %s, want %s", wf.Nodes["a"].Status, to)
					}
					return
				}
				if err == nil {
					t.Fatalf("illegal transition accepted: %s", wf.Nodes["a"].Status)
				}
			})
		}
	}
	for _, from := range []NodeStatus{NodeQueued, NodeRunning} {
		t.Run(string(from)+"->pending", func(t *testing.T) {
			events := []Event{created(t, node("a"))}
			events = append(events, drive[from]...)
			events = append(events, nodeEvent(t, EventNodeRestarted, "a", 2))
			wf, err := Project(seq(events))
			if err != nil {
				t.Fatalf("restart from %s rejected: %v", from, err)
			}
			if wf.Nodes["a"].Status != NodePending || wf.Nodes["a"].Attempt != 2 {
				t.Fatalf("view: %+v", wf.Nodes["a"])
			}
		})
	}
	for _, from := range []NodeStatus{NodePending, NodeCompleted, NodeFailed, NodeAborted, NodeSkipped} {
		t.Run(string(from)+"->pending illegal restart", func(t *testing.T) {
			events := []Event{created(t, node("a"))}
			events = append(events, drive[from]...)
			events = append(events, nodeEvent(t, EventNodeRestarted, "a", 2))
			if _, err := Project(seq(events)); err == nil {
				t.Fatal("restart accepted")
			}
		})
	}
}

func TestProjectRunningToPendingRequiresRestart(t *testing.T) {
	events := seq([]Event{
		created(t, node("a")),
		nodeEvent(t, EventNodeQueued, "a", 1),
		nodeEvent(t, EventNodeStarted, "a", 1),
		nodeEvent(t, EventNodeRestarted, "a", 2),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	view := wf.Nodes["a"]
	if view.Status != NodePending || view.Attempt != 2 || !view.StartedAt.IsZero() {
		t.Fatalf("restart view: %+v", view)
	}
	late := seq([]Event{
		created(t, node("a")),
		nodeEvent(t, EventNodeQueued, "a", 1),
		nodeEvent(t, EventNodeStarted, "a", 1),
		nodeEvent(t, EventNodeRestarted, "a", 2),
		nodeEvent(t, EventNodeCompleted, "a", 1),
	})
	if _, err := Project(late); !errors.Is(err, ErrStaleAttempt) {
		t.Fatalf("late attempt error = %v", err)
	}
}

func TestProjectAttemptFencing(t *testing.T) {
	events := seq([]Event{created(t, node("a")), nodeEvent(t, EventNodeQueued, "a", 2)})
	if _, err := Project(events); !errors.Is(err, ErrStaleAttempt) {
		t.Fatalf("error = %v", err)
	}
	badRestart := seq([]Event{
		created(t, node("a")),
		nodeEvent(t, EventNodeQueued, "a", 1),
		nodeEvent(t, EventNodeStarted, "a", 1),
		nodeEvent(t, EventNodeRestarted, "a", 3),
	})
	if _, err := Project(badRestart); !errors.Is(err, ErrStaleAttempt) {
		t.Fatalf("error = %v", err)
	}
	restartPending := seq([]Event{created(t, node("a")), nodeEvent(t, EventNodeRestarted, "a", 2)})
	if _, err := Project(restartPending); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error = %v", err)
	}
}

func TestProjectUsageAndTimes(t *testing.T) {
	events := seq([]Event{
		created(t, node("a")),
		nodeEvent(t, EventNodeQueued, "a", 1),
		nodeEvent(t, EventNodeStarted, "a", 1),
		nodeEvent(t, EventNodeCompleted, "a", 1),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	view := wf.Nodes["a"]
	if view.Output == nil || view.Usage.InputTokens != 2 || view.Usage.OutputTokens != 3 {
		t.Fatalf("output/usage: %+v", view)
	}
	if wf.Usage.InputTokens != 2 || wf.Usage.OutputTokens != 3 {
		t.Fatalf("workflow usage: %+v", wf.Usage)
	}
	if view.QueuedAt.IsZero() || view.StartedAt.IsZero() || view.FinishedAt.IsZero() {
		t.Fatalf("times: %+v", view)
	}
	if view.Revision != 4 || wf.Revision != 4 {
		t.Fatalf("revisions: node %d wf %d", view.Revision, wf.Revision)
	}
}

func TestProjectInvalidSequences(t *testing.T) {
	events := []Event{mustEv(t, EventWorkflowStarted, struct{}{}, testNow)}
	if _, err := Project(events); err == nil {
		t.Fatal("first event must be created")
	}
	if _, err := Project(nil); !errors.Is(err, ErrNotFound) {
		t.Fatal("empty log must be not found")
	}
	bad := seq([]Event{created(t, node("a")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow)})
	bad[1].Seq = 5
	if _, err := Project(bad); err == nil {
		t.Fatal("non-consecutive seq accepted")
	}
	dup := []Event{created(t, node("a")), created(t, node("a"))}
	if _, err := Project(seq(dup)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("duplicate created error = %v", err)
	}
}

func TestProjectWorkflowTerminal(t *testing.T) {
	base := []Event{
		created(t, node("a")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1),
		nodeEvent(t, EventNodeStarted, "a", 1),
		nodeEvent(t, EventNodeCompleted, "a", 1),
	}
	tests := []struct {
		typ  string
		want WorkflowStatus
	}{
		{EventWorkflowCompleted, WorkflowCompleted},
		{EventWorkflowFailed, WorkflowFailed},
		{EventWorkflowCancelled, WorkflowCancelled},
	}
	for _, tc := range tests {
		events := seq(append(append([]Event(nil), base...), mustEv(t, tc.typ, WorkflowTerminalPayload{}, testNow)))
		wf, err := Project(events)
		if err != nil {
			t.Fatalf("%s: %v", tc.typ, err)
		}
		if wf.Status != tc.want || wf.FinishedAt.IsZero() {
			t.Fatalf("%s view: %+v", tc.typ, wf)
		}
	}
	unfinished := seq(append(append([]Event(nil), base[:2]...), mustEv(t, EventWorkflowCompleted, WorkflowTerminalPayload{}, testNow)))
	if _, err := Project(unfinished); err == nil {
		t.Fatal("completed workflow with pending nodes accepted")
	}
	terminalThenEvent := seq(append(append([]Event(nil), base...),
		mustEv(t, EventWorkflowCompleted, WorkflowTerminalPayload{}, testNow),
		mustEv(t, EventWorkflowFailed, WorkflowTerminalPayload{}, testNow),
	))
	if _, err := Project(terminalThenEvent); !errors.Is(err, ErrTerminalViolation) {
		t.Fatalf("error = %v", err)
	}
}

func TestProjectCancelPendingWorkflow(t *testing.T) {
	events := seq([]Event{
		created(t, node("a"), node("b")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeSkipped, "a", 1),
		nodeEvent(t, EventNodeSkipped, "b", 1),
		mustEv(t, EventWorkflowCancelled, WorkflowTerminalPayload{Reason: "test"}, testNow),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCancelled || wf.CancelReason != "test" {
		t.Fatalf("view: %+v", wf)
	}
}

func TestProjectNodeCancelledCompatibility(t *testing.T) {
	events := seq([]Event{
		created(t, node("a")),
		nodeEvent(t, EventNodeCancelled, "a", 1),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["a"].Status != NodeSkipped {
		t.Fatalf("status = %s", wf.Nodes["a"].Status)
	}
}

func TestProjectExtensionAndPatches(t *testing.T) {
	events := seq([]Event{
		created(t, node("plan"), node("gather", "plan")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{
			Nodes:  []NodeSpec{node("cr.p1", "plan")},
			Source: ExtensionSourceFanOut,
		}, testNow),
		mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{
			DepPatches: []DepPatch{{Node: "gather", Add: []string{"cr.p1"}}},
			Source:     ExtensionSourceFanOut,
		}, testNow),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if wf.ExtendsUsed != 0 {
		t.Fatalf("fan out consumed extends: %d", wf.ExtendsUsed)
	}
	if len(wf.Spec.Nodes) != 3 || len(wf.Spec.Nodes[1].DependsOn) != 2 || wf.Spec.Nodes[1].DependsOn[1] != "cr.p1" {
		t.Fatalf("spec: %+v", wf.Spec.Nodes)
	}
	if wf.Order[len(wf.Order)-1] != "cr.p1" {
		t.Fatalf("order: %v", wf.Order)
	}
}

func TestProjectExtensionErrors(t *testing.T) {
	allTerminal := func() []Event {
		return []Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			nodeEvent(t, EventNodeQueued, "a", 1),
			nodeEvent(t, EventNodeStarted, "a", 1),
			nodeEvent(t, EventNodeCompleted, "a", 1),
		}
	}
	t.Run("patch non pending", func(t *testing.T) {
		events := seq([]Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			nodeEvent(t, EventNodeQueued, "a", 1),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{
				DepPatches: []DepPatch{{Node: "a", Add: []string{"a"}}},
			}, testNow),
		})
		if _, err := Project(events); err == nil {
			t.Fatal("patch on queued node accepted")
		}
	})
	t.Run("unknown patch node", func(t *testing.T) {
		events := seq([]Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{
				DepPatches: []DepPatch{{Node: "ghost", Add: []string{"a"}}},
			}, testNow),
		})
		if _, err := Project(events); err == nil {
			t.Fatal("unknown patch node accepted")
		}
	})
	t.Run("unknown source", func(t *testing.T) {
		events := seq([]Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{Source: "bogus"}, testNow),
		})
		if _, err := Project(events); err == nil {
			t.Fatal("unknown source accepted")
		}
	})
	t.Run("duplicate node", func(t *testing.T) {
		events := seq([]Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{Nodes: []NodeSpec{node("a")}}, testNow),
		})
		if _, err := Project(events); err == nil {
			t.Fatal("duplicate node accepted")
		}
	})
	t.Run("cycle", func(t *testing.T) {
		events := seq([]Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{Nodes: []NodeSpec{node("b")}}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{DepPatches: []DepPatch{{Node: "a", Add: []string{"b"}}}}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{DepPatches: []DepPatch{{Node: "b", Add: []string{"a"}}}}, testNow),
		})
		if _, err := Project(events); err == nil {
			t.Fatal("cycle accepted")
		}
	})
	t.Run("extend budget", func(t *testing.T) {
		events := seq([]Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{Source: ExtensionSourceExtend, Nodes: []NodeSpec{node("b")}}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{Source: ExtensionSourceExtend, Nodes: []NodeSpec{node("c")}}, testNow),
		})
		if _, err := Project(events); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("extend on terminal", func(t *testing.T) {
		events := append(allTerminal(), mustEv(t, EventWorkflowCompleted, WorkflowTerminalPayload{}, testNow))
		events = append(events, mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{Nodes: []NodeSpec{node("b")}}, testNow))
		if _, err := Project(seq(events)); err == nil {
			t.Fatal("extension after terminal accepted")
		}
	})
	t.Run("from node checks", func(t *testing.T) {
		events := seq([]Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{FromNode: "a"}, testNow),
		})
		if _, err := Project(events); err == nil {
			t.Fatal("from pending node accepted")
		}
		completed := []Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			nodeEvent(t, EventNodeQueued, "a", 1),
			nodeEvent(t, EventNodeStarted, "a", 1),
			nodeEvent(t, EventNodeCompleted, "a", 1),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{FromNode: "a"}, testNow),
		}
		if _, err := Project(seq(completed)); err == nil {
			t.Fatal("from non-expanding completed node accepted")
		}
		if _, err := Project(seq([]Event{
			created(t, node("a")),
			mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
			mustEv(t, EventWorkflowExtended, WorkflowExtendedPayload{FromNode: "ghost"}, testNow),
		})); err == nil {
			t.Fatal("unknown from node accepted")
		}
	})
}

func TestProjectRegisteredErrors(t *testing.T) {
	terminal := []Event{
		created(t, node("a")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeSkipped, "a", 1),
		mustEv(t, EventWorkflowCancelled, WorkflowTerminalPayload{}, testNow),
		mustEv(t, EventNodeRegistered, NodeRegisteredPayload{Spec: node("b")}, testNow),
	}
	if _, err := Project(seq(terminal)); !errors.Is(err, ErrTerminalViolation) {
		t.Fatalf("terminal registered error = %v", err)
	}
	badGraph := []Event{
		created(t, node("a")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		mustEv(t, EventNodeRegistered, NodeRegisteredPayload{Spec: node("b", "ghost")}, testNow),
	}
	if _, err := Project(seq(badGraph)); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("invalid registered error = %v", err)
	}
}

func TestProjectNodeEventErrors(t *testing.T) {
	base := []Event{created(t, node("a")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow)}
	cases := []struct {
		name string
		ev   Event
	}{
		{name: "missing node id", ev: mustEv(t, EventNodeQueued, struct{ Anyone string }{}, testNow)},
		{name: "unknown node", ev: mustEv(t, EventNodeQueued, NodeQueuedPayload{NodeID: "ghost", Attempt: 1}, testNow)},
		{name: "restart unknown", ev: mustEv(t, EventNodeRestarted, NodeRestartedPayload{NodeID: "ghost", Attempt: 2}, testNow)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := append(append([]Event(nil), base...), tc.ev)
			if _, err := Project(seq(events)); err == nil {
				t.Fatal("event accepted")
			}
		})
	}
	restartTerminal := []Event{
		created(t, node("a")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeSkipped, "a", 1),
		mustEv(t, EventWorkflowCancelled, WorkflowTerminalPayload{}, testNow),
		mustEv(t, EventNodeRestarted, NodeRestartedPayload{NodeID: "a", Attempt: 2}, testNow),
	}
	if _, err := Project(seq(restartTerminal)); !errors.Is(err, ErrTerminalViolation) {
		t.Fatalf("restart after terminal error = %v", err)
	}
}

func TestProjectTerminalPayloadStrict(t *testing.T) {
	events := []Event{
		created(t, node("a")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeSkipped, "a", 1),
		{Type: EventWorkflowFailed, Payload: []byte(`{"unknown":1}`), At: testNow},
	}
	if _, err := Project(seq(events)); err == nil {
		t.Fatal("unknown terminal payload field accepted")
	}
}

func TestProjectRegisteredCompatibility(t *testing.T) {
	events := seq([]Event{
		created(t, node("a")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		mustEv(t, EventNodeRegistered, NodeRegisteredPayload{Spec: node("b", "a")}, testNow),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["b"].Status != NodePending {
		t.Fatalf("node b: %+v", wf.Nodes["b"])
	}
	dup := seq([]Event{
		created(t, node("a")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		mustEv(t, EventNodeRegistered, NodeRegisteredPayload{Spec: node("a")}, testNow),
	})
	if _, err := Project(dup); err == nil {
		t.Fatal("duplicate registered node accepted")
	}
	bare := seq([]Event{
		created(t, node("a")),
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		mustEv(t, EventNodeRegistered, NodeRegisteredPayload{NodeID: "a"}, testNow),
	})
	wf, err = Project(bare)
	if err != nil {
		t.Fatalf("bare registration should be idempotent: %v", err)
	}
	if len(wf.Nodes) != 1 || wf.Nodes["a"].Spec.Kind != "test" {
		t.Fatalf("bare registration changed spec: %+v", wf.Nodes)
	}
}

func TestProjectUnknownEvent(t *testing.T) {
	events := seq([]Event{created(t, node("a")), mustEv(t, "bogus.event", struct{}{}, testNow)})
	if _, err := Project(events); err == nil {
		t.Fatal("unknown event accepted")
	}
}

func TestProjectInvalidPayloads(t *testing.T) {
	cases := []Event{
		{Type: EventWorkflowCreated, Payload: []byte(`{"spec":{"nodes":[{"id":"a","kind":"x","unknown":1}]}}`)},
		{Type: EventNodeQueued, Payload: []byte(`{"node_id":"a","attempt":1,"unknown":1}`)},
	}
	for _, event := range cases {
		events := seq([]Event{created(t, node("a")), event})
		if _, err := Project(events); err == nil {
			t.Fatalf("payload for %s accepted", event.Type)
		}
	}
}

func TestProjectNodeCompletedInvalidJSON(t *testing.T) {
	events := seq([]Event{
		created(t, node("a")),
		nodeEvent(t, EventNodeQueued, "a", 1),
		nodeEvent(t, EventNodeStarted, "a", 1),
		{Type: EventNodeCompleted, Payload: []byte(`{"node_id":"a","attempt":1,"output":{]}`), At: testNow},
	})
	if _, err := Project(events); err == nil {
		t.Fatal("invalid output accepted")
	}
}

func TestProjectEmptyPayload(t *testing.T) {
	events := []Event{{Type: EventWorkflowCreated, At: testNow, Seq: 1}}
	if _, err := Project(events); err == nil {
		t.Fatal("empty created payload accepted")
	}
}
