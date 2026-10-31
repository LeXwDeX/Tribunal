package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for executor")
	}
}

func TestBudgetCancellationAccountsForRunningUsage(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprintf("required=%t", required), func(t *testing.T) {
			started := make(chan struct{})
			cancelled := make(chan struct{})
			settled := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			usage := Usage{InputTokens: 7, OutputTokens: 3, CostMicros: 11, CacheReadTokens: 2, CacheWriteTokens: 4, ReasoningTokens: 5}
			inner := NewMemoryEventStore()
			terminalType := EventNodeSkipped
			wantStatus, wantWorkflow := NodeSkipped, WorkflowCompleted
			if required {
				terminalType = EventNodeFailed
				wantStatus, wantWorkflow = NodeFailed, WorkflowFailed
			}
			store := &progressStore{EventStore: inner, targets: map[string]int{terminalType + "/b": 3}}
			engine := NewEngine(store, funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
				switch in.Node.ID {
				case "a":
					<-started
					return NodeOutput{Usage: Usage{InputTokens: 10}}, nil
				case "b":
					close(started)
					<-ctx.Done()
					close(cancelled)
					<-release
					return NodeOutput{Usage: usage}, ctx.Err()
				case "settle":
					close(settled)
				}
				return NodeOutput{}, nil
			}), nil, Options{MaxConcurrentNodes: 3})
			wfID, err := engine.Create(context.Background(), GraphSpec{
				Nodes: []NodeSpec{node("a"), {ID: "b", Kind: "x", Required: required},
					{ID: "settle", Kind: "x", DependsOn: []string{"a"}, Required: true, TokenFree: true}},
				Budget: Budget{MaxTokens: 5},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- engine.Run(context.Background(), wfID) }()
			waitSignal(t, cancelled)
			waitSignal(t, settled)
			wf, err := engine.Snapshot(context.Background(), wfID)
			if err != nil {
				t.Fatal(err)
			}
			if wf.Nodes["b"].Status != NodeRunning {
				t.Fatalf("running attempt finalized before it returned usage: %+v", wf.Nodes["b"])
			}
			// Release the executor so the cancelled attempt can settle; Run waits for it.
			release <- struct{}{}
			if err := waitRun(t, nil, done); err != nil {
				t.Fatal(err)
			}
			wf, err = engine.Snapshot(context.Background(), wfID)
			if err != nil {
				t.Fatal(err)
			}
			wantTotal := usage
			wantTotal.InputTokens += 10
			if wf.Status != wantWorkflow || wf.Nodes["b"].Status != wantStatus || wf.Nodes["b"].Reason != "budget_exhausted" || wf.Nodes["b"].Usage != usage || wf.Usage != wantTotal {
				t.Fatalf("workflow=%s usage=%+v b=%+v, want %s %+v %+v", wf.Status, wf.Usage, wf.Nodes["b"], wantWorkflow, wantTotal, usage)
			}
			if wf.UsageIncomplete || wf.Nodes["b"].UsageIncomplete {
				t.Fatalf("returned usage was marked incomplete: workflow=%+v b=%+v", wf, wf.Nodes["b"])
			}
			if wf.Nodes["settle"].Status != NodeCompleted {
				t.Fatalf("settlement = %+v", wf.Nodes["settle"])
			}
			if count := countNodeEvents(t, inner, wfID, terminalType, "b"); count != 1 {
				t.Fatalf("terminal events = %d, want one", count)
			}
			store.mu.Lock()
			injected := store.injected
			store.mu.Unlock()
			if injected != 3 {
				t.Fatalf("stale revision injections = %d, want 3", injected)
			}
		})
	}
}

func TestProjectBudgetUsageAndLegacyEvents(t *testing.T) {
	for _, typ := range []string{EventNodeSkipped, EventNodeCancelled} {
		t.Run(typ, func(t *testing.T) {
			events := seq([]Event{created(t, node("a"), node("b")),
				mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
				nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
				{Type: typ, At: testNow, Payload: json.RawMessage(`{"node_id":"a","reason":"budget_exhausted","usage":{"input_tokens":7,"output_tokens":3,"cost_micros":4}}`)},
				{Type: EventNodeSkipped, At: testNow, Payload: json.RawMessage(`{"node_id":"b","reason":"legacy"}`)},
			})
			wf, err := Project(events)
			if err != nil {
				t.Fatal(err)
			}
			want := Usage{InputTokens: 7, OutputTokens: 3, CostMicros: 4}
			if wf.Usage != want || wf.Nodes["a"].Usage != want || wf.Nodes["b"].Usage != (Usage{}) {
				t.Fatalf("usage=%+v a=%+v b=%+v", wf.Usage, wf.Nodes["a"], wf.Nodes["b"])
			}
			duplicate := append(append([]Event(nil), events...), events[4])
			duplicate[len(duplicate)-1].Seq = 0
			if _, err := Project(duplicate); err == nil {
				t.Fatal("duplicate terminal event was accepted")
			}
		})
	}
}

func TestBudgetCancellationConcurrentAttempts(t *testing.T) {
	const n = 8
	started := make(chan struct{}, n)
	var nodes []NodeSpec
	nodes = append(nodes, node("trigger"))
	for i := 0; i < n; i++ {
		nodes = append(nodes, NodeSpec{ID: fmt.Sprintf("b%d", i), Kind: "x", Required: i%2 == 0})
	}
	store := NewMemoryEventStore()
	engine := NewEngine(store, funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "trigger" {
			for i := 0; i < n; i++ {
				<-started
			}
			return NodeOutput{Usage: Usage{InputTokens: 10}}, nil
		}
		started <- struct{}{}
		<-ctx.Done()
		if in.Node.Required {
			// A returned result must not win over an already committed budget.
			return NodeOutput{Usage: Usage{InputTokens: 1, OutputTokens: 2}}, nil
		}
		return NodeOutput{Usage: Usage{InputTokens: 1, OutputTokens: 2}}, ctx.Err()
	}), nil, Options{MaxConcurrentNodes: n + 1})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: nodes, Budget: Budget{MaxTokens: 5}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWithTimeout(t, engine, wfID); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Usage.InputTokens != 10+n || wf.Usage.OutputTokens != 2*n {
		t.Fatalf("usage = %+v", wf.Usage)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("b%d", i)
		typ, status := EventNodeSkipped, NodeSkipped
		if i%2 == 0 {
			typ, status = EventNodeFailed, NodeFailed
		}
		if wf.Nodes[id].Status != status || wf.Nodes[id].Usage.InputTokens != 1 || countNodeEvents(t, store, wfID, typ, id) != 1 {
			t.Fatalf("%s = %+v", id, wf.Nodes[id])
		}
	}
}

func TestBudgetCancellationWaitsForNonCooperativeExecutor(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	var releaseOnce sync.Once
	releaseExecutor := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseExecutor()
	store := NewMemoryEventStore()
	engine := NewEngine(store, funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		switch in.Node.ID {
		case "a":
			<-started
			return NodeOutput{Usage: Usage{InputTokens: 10}}, nil
		case "b":
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-release // Deliberately ignores context cancellation.
			close(exited)
			return NodeOutput{Data: json.RawMessage(`{"late":true}`), Usage: Usage{InputTokens: 99}}, nil
		default:
			return NodeOutput{}, nil
		}
	}), nil, Options{MaxConcurrentNodes: 2})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{node("a"), {ID: "b", Kind: "x"},
			{ID: "settle", Kind: "x", Required: true, TokenFree: true, DependsOn: []string{"a", "b"}}},
		Budget: Budget{MaxTokens: 5},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	waitSignal(t, cancelled)
	select {
	case err := <-done:
		t.Fatalf("Run returned before the non-cooperative executor exited: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["b"].Status != NodeRunning || wf.Usage.InputTokens != 10 || len(engine.sem) != 1 {
		t.Fatalf("budget cancellation released the lifecycle barrier: usage=%+v b=%+v permits=%d", wf.Usage, wf.Nodes["b"], len(engine.sem))
	}
	releaseExecutor()
	waitSignal(t, exited)
	if err := waitRun(t, nil, done); err != nil {
		t.Fatal(err)
	}
	wf, err = engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCompleted || wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "budget_exhausted" || wf.Nodes["b"].Attempt != 1 || wf.Nodes["settle"].Status != NodeCompleted {
		t.Fatalf("workflow=%s nodes=%+v", wf.Status, wf.Nodes)
	}
	want := Usage{InputTokens: 109}
	if wf.Usage != want || wf.Nodes["b"].Usage != (Usage{InputTokens: 99}) || wf.UsageIncomplete || wf.Nodes["b"].UsageIncomplete {
		t.Fatalf("usage was lost, duplicated or misreported: workflow=%+v b=%+v", wf.Usage, wf.Nodes["b"])
	}
	if countNodeEvents(t, store, wfID, EventNodeSkipped, "b") != 1 || countNodeEvents(t, store, wfID, EventNodeCompleted, "b") != 0 {
		t.Fatal("non-cooperative budget terminal event was not recorded exactly once")
	}
	if got := len(engine.sem); got != 0 {
		t.Fatalf("executor still holds %d permits after Run", got)
	}
}

func TestProjectIncompleteUsageEvents(t *testing.T) {
	for _, typ := range []string{EventNodeFailed, EventNodeSkipped} {
		t.Run(typ, func(t *testing.T) {
			events := seq([]Event{created(t, node("a")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
				nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
				{Type: typ, At: testNow, Payload: json.RawMessage(`{"node_id":"a","attempt":1,"reason":"budget_exhausted","usage_incomplete":true}`)},
			})
			wf, err := Project(events)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(wf)
			if err != nil {
				t.Fatal(err)
			}
			var projected struct {
				UsageIncomplete bool `json:"usage_incomplete"`
				Nodes           map[string]struct {
					UsageIncomplete bool `json:"usage_incomplete"`
				} `json:"nodes"`
			}
			if err := json.Unmarshal(data, &projected); err != nil {
				t.Fatal(err)
			}
			if !projected.UsageIncomplete || !projected.Nodes["a"].UsageIncomplete {
				t.Fatalf("unknown usage lost during projection: %s", data)
			}
		})
	}
}

func TestNodeTimeoutWaitsForNonCooperativeExecutor(t *testing.T) {
	started := make(chan struct{})
	timedOut := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	releaseExecutor := func() { once.Do(func() { close(release) }) }
	defer releaseExecutor()
	engine := fastEngine(funcExecutor(func(ctx context.Context, _ NodeInput) (NodeOutput, error) {
		close(started)
		<-ctx.Done()
		close(timedOut)
		<-release
		close(exited)
		return NodeOutput{Usage: Usage{InputTokens: 99}}, nil
	}), Options{MaxConcurrentNodes: 1, DefaultNodeTimeout: 250 * time.Millisecond})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	waitSignal(t, started)
	waitSignal(t, timedOut)
	select {
	case err := <-done:
		t.Fatalf("Run returned before the non-cooperative executor exited: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["a"].Status != NodeRunning || wf.Nodes["a"].Attempt != 1 || len(engine.sem) != 1 {
		t.Fatalf("node timeout released the lifecycle barrier: node=%+v permits=%d", wf.Nodes["a"], len(engine.sem))
	}
	releaseExecutor()
	waitSignal(t, exited)
	if err := waitRun(t, nil, done); err != nil {
		t.Fatal(err)
	}
	wf, err = engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowFailed || wf.Nodes["a"].Status != NodeFailed || wf.Nodes["a"].Reason != "timeout" || wf.Nodes["a"].Attempt != 1 || wf.Usage != (Usage{InputTokens: 99}) || wf.UsageIncomplete || len(engine.sem) != 0 {
		t.Fatalf("late timeout result was lost or reopened: workflow=%s node=%+v usage=%+v", wf.Status, wf.Nodes["a"], wf.Usage)
	}
}

func TestBudgetUsageRecoveryFencesOldAttempt(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		t.Error("recovered budget-exhausted node must not execute")
		return NodeOutput{}, errors.New("unexpected")
	}), nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{node("a"), {ID: "b", Kind: "x"}}, Budget: Budget{MaxTokens: 5},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
		mustEv(t, EventNodeCompleted, NodeCompletedPayload{NodeID: "a", Attempt: 1, Usage: Usage{InputTokens: 10}}, testNow),
		nodeEvent(t, EventNodeQueued, "b", 1), nodeEvent(t, EventNodeStarted, "b", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.commitNodeFailureUsage(context.Background(), wfID, "b", 1, "budget_exhausted", 0, "late", Usage{InputTokens: 99}, false); err != nil {
		t.Fatal(err)
	}
	if err := runWithTimeout(t, engine, wfID); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["b"].Attempt != 2 || wf.Nodes["b"].Status != NodeSkipped || wf.Usage.InputTokens != 10 {
		t.Fatalf("recovered workflow=%+v", wf)
	}
	if !wf.UsageIncomplete {
		t.Fatal("restarting a running attempt lost its unknown usage marker")
	}
}

func TestBudgetCancelDoesNotTargetAnotherAttempt(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	handle := &runHandle{nodeCancels: map[string]nodeCancel{"b": {attempt: 1, cancel: cancel}}}
	wf := WorkflowView{Nodes: map[string]NodeView{"b": {ID: "b", Attempt: 2, Status: NodeRunning}}}
	if owned := handle.cancelTokenBudgetNodes(wf); len(owned) != 0 || ctx.Err() != nil {
		t.Fatalf("old attempt was cancelled or treated as owner: owned=%v err=%v", owned, ctx.Err())
	}
	wf.Nodes["b"] = NodeView{ID: "b", Attempt: 1, Status: NodeRunning}
	if owned := handle.cancelTokenBudgetNodes(wf); owned["b"] != 1 || !errors.Is(context.Cause(ctx), errTokenBudget) {
		t.Fatalf("matching attempt was not cancelled: owned=%v cause=%v", owned, context.Cause(ctx))
	}
}

func TestBudgetStartRechecksCommittedUsage(t *testing.T) {
	engine := fastEngine(funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		t.Error("exhausted queued node must not start")
		return NodeOutput{}, nil
	}), Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{node("a"), node("b")}, Budget: Budget{MaxTokens: 5},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.store.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
		mustEv(t, EventNodeCompleted, NodeCompletedPayload{NodeID: "a", Attempt: 1, Usage: Usage{InputTokens: 10}}, testNow),
		nodeEvent(t, EventNodeQueued, "b", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	started, err := engine.commitNodeStarted(context.Background(), wfID, "b", 1, NodeMetadata{})
	if err != nil || started {
		t.Fatalf("start after exhaustion = %t, err=%v", started, err)
	}
}

type budgetRaceStore struct {
	EventStore
	once sync.Once
	err  error
}

func (s *budgetRaceStore) Append(ctx context.Context, wfID string, expect int64, events ...Event) (int64, error) {
	if _, ok := matchTarget(events, map[string]bool{EventNodeCompleted + "/b": true}); ok {
		s.once.Do(func() {
			var ev Event
			ev, s.err = makeEvent(EventNodeCompleted, NodeCompletedPayload{NodeID: "a", Attempt: 1, Usage: Usage{InputTokens: 10}}, testNow)
			if s.err == nil {
				_, s.err = s.EventStore.Append(ctx, wfID, expect, ev)
			}
		})
		if s.err != nil {
			return 0, s.err
		}
	}
	return s.EventStore.Append(ctx, wfID, expect, events...)
}

func TestBudgetRacesWithSuccessCommit(t *testing.T) {
	inner := NewMemoryEventStore()
	store := &budgetRaceStore{EventStore: inner}
	engine := NewEngine(store, nil, nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{node("a"), {ID: "b", Kind: "x"}}, Budget: Budget{MaxTokens: 5},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inner.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
		nodeEvent(t, EventNodeQueued, "b", 1), nodeEvent(t, EventNodeStarted, "b", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	usage := Usage{InputTokens: 2, OutputTokens: 3}
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", NodeSpec{ID: "b", Kind: "x"}, 1, NodeOutput{Data: json.RawMessage(`{"value":1}`), Usage: usage, UsageIncomplete: true}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Usage.InputTokens != 12 || wf.Usage.OutputTokens != 3 || wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "budget_exhausted" || wf.Nodes["b"].Usage != usage || !wf.UsageIncomplete || !wf.Nodes["b"].UsageIncomplete || len(wf.Nodes["b"].Output) != 0 {
		t.Fatalf("workflow usage=%+v b=%+v", wf.Usage, wf.Nodes["b"])
	}
	if countNodeEvents(t, inner, wfID, EventNodeSkipped, "b") != 1 || countNodeEvents(t, inner, wfID, EventNodeCompleted, "b") != 0 {
		t.Fatal("racing result was not recorded as exactly one budget terminal event")
	}
}

func TestBudgetRunningResultCannotOverwriteRecoveredAttempt(t *testing.T) {
	started := make(chan struct{})
	triggerStarted := make(chan struct{})
	allowTrigger := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	releaseOld := func() { once.Do(func() { close(release) }) }
	defer releaseOld()
	var triggerOnce sync.Once
	startTrigger := func() { triggerOnce.Do(func() { close(allowTrigger) }) }
	defer startTrigger()
	store := NewMemoryEventStore()
	engine := NewEngine(store, funcExecutor(func(_ context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "a" {
			close(triggerStarted)
			<-started
			<-allowTrigger
			return NodeOutput{Usage: Usage{InputTokens: 10}}, nil
		}
		close(started)
		<-release
		close(exited)
		return NodeOutput{Data: json.RawMessage(`{"late":true}`), Usage: Usage{InputTokens: 99}}, nil
	}), nil, Options{MaxConcurrentNodes: 2})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{node("a"), {ID: "b", Kind: "x"}}, Budget: Budget{MaxTokens: 5},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	waitSignal(t, started)
	waitSignal(t, triggerStarted)
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), wfID, wf.Revision,
		nodeEvent(t, EventNodeRestarted, "b", 2), nodeEvent(t, EventNodeQueued, "b", 2), nodeEvent(t, EventNodeStarted, "b", 2),
	)
	if err != nil {
		t.Fatal(err)
	}
	startTrigger()
	releaseOld()
	waitSignal(t, exited)
	if err := waitRun(t, nil, done); err != nil {
		t.Fatal(err)
	}
	wf, err = engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["b"].Attempt != 2 || wf.Nodes["b"].Status != NodeSkipped || wf.Usage.InputTokens != 10 {
		t.Fatalf("recovered attempt = %+v usage=%+v", wf.Nodes["b"], wf.Usage)
	}
	if !wf.UsageIncomplete || !wf.Nodes["b"].UsageIncomplete {
		t.Fatalf("unowned running attempt did not mark unknown usage: %+v", wf)
	}
	after, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != wf.Revision || after.Usage != wf.Usage || after.Nodes["b"].Attempt != 2 {
		t.Fatalf("late old attempt changed workflow: before=%+v after=%+v", wf, after)
	}
}

func TestBudgetAllowsFiniteUsageBelowLimit(t *testing.T) {
	started := make(chan struct{})
	engine := fastEngine(funcExecutor(func(_ context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "a" {
			<-started
		} else {
			close(started)
		}
		return NodeOutput{Usage: Usage{InputTokens: 2, OutputTokens: 1}}, nil
	}), Options{MaxConcurrentNodes: 2})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{node("a"), node("b")}, Budget: Budget{MaxTokens: 10},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWithTimeout(t, engine, wfID); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCompleted || wf.Nodes["a"].Status != NodeCompleted || wf.Nodes["b"].Status != NodeCompleted || wf.Usage.InputTokens != 4 || wf.Usage.OutputTokens != 2 {
		t.Fatalf("under-budget workflow: %+v", wf)
	}
}

func completedWithUsage(t *testing.T, id string, attempt int, usage Usage) Event {
	t.Helper()
	return mustEv(t, EventNodeCompleted, NodeCompletedPayload{
		NodeID: id, Attempt: attempt, Output: json.RawMessage(`{"ok":true}`), Usage: usage,
	}, testNow)
}

func spentEngine(t *testing.T, budget Budget, specs ...NodeSpec) (*Engine, *MemoryEventStore, string) {
	t.Helper()
	store := NewMemoryEventStore()
	engine := NewEngine(store, nil, nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: append([]NodeSpec{node("a")}, specs...), Budget: budget}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
		completedWithUsage(t, "a", 1, Usage{InputTokens: 1}),
		nodeEvent(t, EventNodeQueued, "b", 1), nodeEvent(t, EventNodeStarted, "b", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	return engine, store, wfID
}

func TestProjectUsageRejectsNegativeCounters(t *testing.T) {
	valid := Usage{InputTokens: 10, OutputTokens: 5, CostMicros: 3, CacheReadTokens: 2, CacheWriteTokens: 1, ReasoningTokens: 4}
	invalid := Usage{InputTokens: -10, OutputTokens: -5, CostMicros: -3, CacheReadTokens: -2, CacheWriteTokens: -1, ReasoningTokens: -4}
	events := seq([]Event{created(t, node("a"), node("b")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1), completedWithUsage(t, "a", 1, valid),
		nodeEvent(t, EventNodeQueued, "b", 1), nodeEvent(t, EventNodeStarted, "b", 1), completedWithUsage(t, "b", 1, invalid),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Usage != valid {
		t.Fatalf("negative usage changed committed spend: %+v, want %+v", wf.Usage, valid)
	}
	if !wf.UsageIncomplete || !wf.Nodes["b"].UsageIncomplete || wf.Nodes["b"].Usage != (Usage{}) {
		t.Fatalf("invalid usage was reported as exact: workflow=%t node=%+v", wf.UsageIncomplete, wf.Nodes["b"])
	}
}

func TestProjectUsageSaturatesOverflow(t *testing.T) {
	first := Usage{InputTokens: math.MaxInt64 - 5, OutputTokens: 1, CostMicros: math.MaxInt64, CacheReadTokens: math.MaxInt64 - 3, CacheWriteTokens: math.MaxInt64 - 1, ReasoningTokens: math.MaxInt64 - 2}
	second := Usage{InputTokens: 10, OutputTokens: 2, CostMicros: 1, CacheReadTokens: 5, CacheWriteTokens: 3, ReasoningTokens: 4}
	events := seq([]Event{created(t, node("a"), node("b")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1), completedWithUsage(t, "a", 1, first),
		nodeEvent(t, EventNodeQueued, "b", 1), nodeEvent(t, EventNodeStarted, "b", 1), completedWithUsage(t, "b", 1, second),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{InputTokens: math.MaxInt64, OutputTokens: 3, CostMicros: math.MaxInt64, CacheReadTokens: math.MaxInt64, CacheWriteTokens: math.MaxInt64, ReasoningTokens: math.MaxInt64}
	if wf.Usage != want || !wf.UsageIncomplete || wf.Usage.totalTokens() != math.MaxInt64 {
		t.Fatalf("usage=%+v incomplete=%t total=%d", wf.Usage, wf.UsageIncomplete, wf.Usage.totalTokens())
	}
}

func TestProjectUsageMarksInputOutputOverflow(t *testing.T) {
	usage := Usage{InputTokens: math.MaxInt64, OutputTokens: 1}
	events := seq([]Event{created(t, node("a")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1), completedWithUsage(t, "a", 1, usage),
	})
	wf, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Usage != usage || !wf.UsageIncomplete || !wf.Nodes["a"].UsageIncomplete || wf.Usage.totalTokens() != math.MaxInt64 {
		t.Fatalf("usage=%+v incomplete=%t node=%+v", wf.Usage, wf.UsageIncomplete, wf.Nodes["a"])
	}
}

func TestCommitNodeSuccessSanitizesInvalidUsage(t *testing.T) {
	engine, store, wfID := spentEngine(t, Budget{MaxTokens: 1}, NodeSpec{ID: "b", Kind: "x"}, NodeSpec{ID: "c", Kind: "x", DependsOn: []string{"a", "b"}})
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", NodeSpec{ID: "b", Kind: "x"}, 1, NodeOutput{
		Data: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: -10, OutputTokens: -5, CostMicros: -1},
	}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Usage != (Usage{InputTokens: 1}) || !wf.UsageIncomplete || !wf.Nodes["b"].UsageIncomplete || wf.Nodes["b"].Usage != (Usage{}) || wf.Nodes["b"].Status != NodeSkipped {
		t.Fatalf("workflow=%+v b=%+v", wf.Usage, wf.Nodes["b"])
	}
	if _, err := store.Append(context.Background(), wfID, wf.Revision, nodeEvent(t, EventNodeQueued, "c", 1)); err != nil {
		t.Fatal(err)
	}
	if started, err := engine.commitNodeStarted(context.Background(), wfID, "c", 1, NodeMetadata{}); err != nil || started {
		t.Fatalf("invalid usage reopened the budget: started=%t err=%v", started, err)
	}
}

func TestCommitNodeSuccessMarksInvalidUsageWhenNotBudgetBlocked(t *testing.T) {
	engine, _, wfID := spentEngine(t, Budget{}, NodeSpec{ID: "b", Kind: "x"})
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", NodeSpec{ID: "b", Kind: "x"}, 1, NodeOutput{
		Data: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: -10, OutputTokens: -5},
	}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Usage != (Usage{InputTokens: 1}) || !wf.UsageIncomplete || !wf.Nodes["b"].UsageIncomplete || wf.Nodes["b"].Usage != (Usage{}) || wf.Nodes["b"].Status != NodeCompleted {
		t.Fatalf("workflow=%+v b=%+v", wf.Usage, wf.Nodes["b"])
	}
}

func TestCommitNodeFailureSanitizesInvalidUsage(t *testing.T) {
	engine, _, wfID := spentEngine(t, Budget{}, NodeSpec{ID: "b", Kind: "x", Required: true})
	if err := engine.commitNodeFailureUsage(context.Background(), wfID, "b", 1, "executor_error", 0, "boom", Usage{InputTokens: -5, OutputTokens: -2}, false); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Usage != (Usage{InputTokens: 1}) || !wf.UsageIncomplete || !wf.Nodes["b"].UsageIncomplete || wf.Nodes["b"].Usage != (Usage{}) || wf.Nodes["b"].Status != NodeFailed {
		t.Fatalf("workflow=%+v b=%+v", wf.Usage, wf.Nodes["b"])
	}
}

func TestUsageAddSaturatesAndFailsClosed(t *testing.T) {
	all := func(v int64) Usage {
		return Usage{InputTokens: v, OutputTokens: v, CostMicros: v, CacheReadTokens: v, CacheWriteTokens: v, ReasoningTokens: v}
	}
	saturated := all(math.MaxInt64)
	if !saturated.add(Usage{InputTokens: 1, OutputTokens: 1, CostMicros: 1, CacheReadTokens: 1, CacheWriteTokens: 1, ReasoningTokens: 1}) {
		t.Fatal("saturated addition was reported as exact")
	}
	if saturated != all(math.MaxInt64) {
		t.Fatalf("counters did not saturate: %+v", saturated)
	}
	valid := Usage{InputTokens: 7, OutputTokens: 2}
	if !valid.add(all(-1)) || valid != (Usage{InputTokens: 7, OutputTokens: 2}) {
		t.Fatalf("negative usage changed valid spend: %+v", valid)
	}
	if (Usage{InputTokens: math.MaxInt64, OutputTokens: 1}).totalTokens() != math.MaxInt64 {
		t.Fatal("input plus output overflow was not saturated")
	}
	if (Usage{InputTokens: -1, OutputTokens: 1}).totalTokens() != math.MaxInt64 {
		t.Fatal("negative counter did not fail closed")
	}
}

func spentEngineWith(t *testing.T, budget Budget, exp Expander, specs ...NodeSpec) (*Engine, *MemoryEventStore, string) {
	t.Helper()
	store := NewMemoryEventStore()
	engine := NewEngine(store, nil, exp, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: append([]NodeSpec{node("a")}, specs...), Budget: budget}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
		completedWithUsage(t, "a", 1, Usage{InputTokens: 1}),
		nodeEvent(t, EventNodeQueued, "b", 1), nodeEvent(t, EventNodeStarted, "b", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	return engine, store, wfID
}

func TestCommitNodeInvalidOutputKeepsUsage(t *testing.T) {
	spec := NodeSpec{ID: "b", Kind: "x", Required: true}
	engine, store, wfID := spentEngine(t, Budget{}, spec)
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", spec, 1, NodeOutput{
		Data: json.RawMessage(`{"ok":`), Usage: Usage{InputTokens: 3, OutputTokens: 2},
	}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	b := wf.Nodes["b"]
	if b.Status != NodeFailed || b.Reason != "invalid_output" || b.Usage != (Usage{InputTokens: 3, OutputTokens: 2}) || b.UsageIncomplete || b.Output != nil {
		t.Fatalf("b=%+v", b)
	}
	if wf.Usage != (Usage{InputTokens: 4, OutputTokens: 2}) || wf.UsageIncomplete {
		t.Fatalf("workflow usage=%+v incomplete=%t", wf.Usage, wf.UsageIncomplete)
	}
	if countNodeEvents(t, store, wfID, EventNodeCompleted, "b") != 0 || countNodeEvents(t, store, wfID, EventNodeFailed, "b") != 1 {
		t.Fatal("invalid output produced a success or duplicate terminal event")
	}
}

func TestCommitNodeExpanderErrorKeepsUsage(t *testing.T) {
	exp := funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
		return Extension{}, errors.New("expand failed")
	})
	spec := NodeSpec{ID: "b", Kind: "x", Required: true, Expands: true}
	engine, store, wfID := spentEngineWith(t, Budget{}, exp, spec)
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", spec, 1, NodeOutput{
		Data: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: 2, OutputTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	b := wf.Nodes["b"]
	if b.Status != NodeFailed || b.Reason != "expand_failed" || b.Usage != (Usage{InputTokens: 2, OutputTokens: 1}) || b.UsageIncomplete || b.Output != nil {
		t.Fatalf("b=%+v", b)
	}
	if wf.Usage != (Usage{InputTokens: 3, OutputTokens: 1}) || wf.UsageIncomplete {
		t.Fatalf("workflow usage=%+v incomplete=%t", wf.Usage, wf.UsageIncomplete)
	}
	if countNodeEvents(t, store, wfID, EventNodeCompleted, "b") != 0 || countNodeEvents(t, store, wfID, EventNodeFailed, "b") != 1 {
		t.Fatal("expander failure produced a success or duplicate terminal event")
	}
}

func TestCommitNodeInvalidExtensionKeepsUsage(t *testing.T) {
	exp := funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
		return Extension{Nodes: []NodeSpec{{ID: "child", Kind: "x", DependsOn: []string{"missing"}}}}, nil
	})
	spec := NodeSpec{ID: "b", Kind: "x", Required: true, Expands: true}
	engine, store, wfID := spentEngineWith(t, Budget{}, exp, spec)
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", spec, 1, NodeOutput{
		Data: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: 2, OutputTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	b := wf.Nodes["b"]
	if b.Status != NodeFailed || b.Reason != "expand_failed" || b.Usage != (Usage{InputTokens: 2, OutputTokens: 1}) || b.Output != nil {
		t.Fatalf("b=%+v", b)
	}
	if wf.Usage != (Usage{InputTokens: 3, OutputTokens: 1}) {
		t.Fatalf("workflow usage=%+v", wf.Usage)
	}
	if countNodeEvents(t, store, wfID, EventWorkflowExtended, "") != 0 || countNodeEvents(t, store, wfID, EventNodeCompleted, "b") != 0 {
		t.Fatal("invalid extension was committed")
	}
}

func TestCommitNodeFailurePreservesUsageUnderBudgetRace(t *testing.T) {
	spec := NodeSpec{ID: "b", Kind: "x"}
	child := NodeSpec{ID: "c", Kind: "x", DependsOn: []string{"a", "b"}}
	engine, store, wfID := spentEngine(t, Budget{MaxTokens: 1}, spec, child)
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", spec, 1, NodeOutput{
		Data: json.RawMessage(`{"ok":`), Usage: Usage{InputTokens: 3, OutputTokens: 2},
	}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	b := wf.Nodes["b"]
	if b.Status != NodeSkipped || b.Reason != "budget_exhausted" || b.Usage != (Usage{InputTokens: 3, OutputTokens: 2}) || b.UsageIncomplete {
		t.Fatalf("b=%+v", b)
	}
	if wf.Usage != (Usage{InputTokens: 4, OutputTokens: 2}) || wf.UsageIncomplete {
		t.Fatalf("workflow usage=%+v incomplete=%t", wf.Usage, wf.UsageIncomplete)
	}
	if _, err := store.Append(context.Background(), wfID, wf.Revision, nodeEvent(t, EventNodeQueued, "c", 1)); err != nil {
		t.Fatal(err)
	}
	if started, err := engine.commitNodeStarted(context.Background(), wfID, "c", 1, NodeMetadata{}); err != nil || started {
		t.Fatalf("failure usage reopened the budget: started=%t err=%v", started, err)
	}
}

func TestCommitNodeFailureSaturatesOverflowUsage(t *testing.T) {
	spec := NodeSpec{ID: "b", Kind: "x", Required: true}
	engine, _, wfID := spentEngine(t, Budget{}, spec)
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", spec, 1, NodeOutput{
		Data: json.RawMessage(`{`), Usage: Usage{InputTokens: math.MaxInt64, OutputTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	b := wf.Nodes["b"]
	if b.Status != NodeFailed || b.Usage != (Usage{InputTokens: math.MaxInt64, OutputTokens: 1}) || !b.UsageIncomplete {
		t.Fatalf("b=%+v", b)
	}
	if wf.Usage.InputTokens != math.MaxInt64 || !wf.UsageIncomplete || wf.Usage.totalTokens() != math.MaxInt64 {
		t.Fatalf("workflow usage=%+v incomplete=%t total=%d", wf.Usage, wf.UsageIncomplete, wf.Usage.totalTokens())
	}
}

func TestCommitNodeExpandFailureRetriesStaleRevision(t *testing.T) {
	inner := NewMemoryEventStore()
	store := &progressStore{EventStore: inner, targets: map[string]int{EventNodeFailed + "/b": 3}}
	exp := funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
		return Extension{}, errors.New("expand failed")
	})
	engine := NewEngine(store, nil, exp, Options{})
	spec := NodeSpec{ID: "b", Kind: "x", Required: true, Expands: true}
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a"), spec}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inner.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
		completedWithUsage(t, "a", 1, Usage{InputTokens: 1}),
		nodeEvent(t, EventNodeQueued, "b", 1), nodeEvent(t, EventNodeStarted, "b", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.commitNodeSuccess(context.Background(), wfID, "b", spec, 1, NodeOutput{
		Data: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: 2, OutputTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["b"].Status != NodeFailed || wf.Nodes["b"].Usage != (Usage{InputTokens: 2, OutputTokens: 1}) || wf.Usage != (Usage{InputTokens: 3, OutputTokens: 1}) {
		t.Fatalf("b=%+v usage=%+v", wf.Nodes["b"], wf.Usage)
	}
	if countNodeEvents(t, inner, wfID, EventNodeFailed, "b") != 1 || countNodeEvents(t, inner, wfID, EventNodeCompleted, "b") != 0 {
		t.Fatal("stale retry duplicated the terminal event")
	}
	store.mu.Lock()
	injected := store.injected
	store.mu.Unlock()
	if injected != 3 {
		t.Fatalf("stale revision injections = %d, want 3", injected)
	}
}
