package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func assertReplay(t *testing.T, store EventStore, wfID string, want WorkflowView) {
	t.Helper()
	events, err := store.Load(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event replay differs: got=%+v want=%+v", got, want)
	}
}

func TestNodeUsageIncompletePropagation(t *testing.T) {
	for _, mode := range []string{"success", "zero", "executor_error", "invalid_output", "expand_error", "invalid_extension"} {
		t.Run(mode, func(t *testing.T) {
			out := NodeOutput{Data: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: 5}, UsageIncomplete: true}
			var execErr error
			spec := NodeSpec{ID: "a", Kind: "x", Required: true}
			var exp Expander
			wantReason := ""
			switch mode {
			case "zero":
				out.Usage = Usage{}
			case "executor_error":
				execErr = classedTestError{class: "auth", status: 403, text: "private"}
				wantReason = "auth"
			case "invalid_output":
				out.Data = json.RawMessage(`{`)
				wantReason = "invalid_output"
			case "expand_error", "invalid_extension":
				spec.Expands = true
				wantReason = "expand_failed"
				exp = funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
					if mode == "expand_error" {
						return Extension{}, errors.New("private")
					}
					return Extension{Nodes: []NodeSpec{{ID: "child", Kind: "x", DependsOn: []string{"missing"}}}}, nil
				})
			}
			store := NewMemoryEventStore()
			engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
				return out, execErr
			}), exp, Options{})
			wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{spec}}, nil)
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
			n := wf.Nodes["a"]
			wantStatus, eventType := NodeCompleted, EventNodeCompleted
			if wantReason != "" {
				wantStatus, eventType = NodeFailed, EventNodeFailed
			}
			if n.Status != wantStatus || n.Reason != wantReason || n.Usage != out.Usage || wf.Usage != out.Usage || !n.UsageIncomplete || !wf.UsageIncomplete {
				t.Fatalf("workflow usage=%+v incomplete=%t node=%+v", wf.Usage, wf.UsageIncomplete, n)
			}
			if mode == "executor_error" && n.ErrorStatus != 403 {
				t.Fatalf("lost sanitized error status: %+v", n)
			}
			if countNodeEvents(t, store, wfID, eventType, "a") != 1 {
				t.Fatalf("node %s must have exactly one %s", "a", eventType)
			}
			assertReplay(t, store, wfID, wf)
		})
	}
}

func TestLateTimeoutResultDoesNotExpand(t *testing.T) {
	cases := []struct {
		name       string
		required   bool
		usage      Usage
		incomplete bool
		err        error
		reason     string
	}{
		{name: "required_invalid", required: true, usage: Usage{InputTokens: 5, OutputTokens: -2}, reason: "timeout"},
		{name: "optional_invalid", usage: Usage{InputTokens: 5, OutputTokens: -2}, reason: "timeout"},
		{name: "zero_incomplete", required: true, incomplete: true, reason: "timeout"},
		{name: "classified_error", required: true, usage: Usage{InputTokens: 5}, incomplete: true, err: classedTestError{class: "auth", status: 403, text: "private"}, reason: "auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timedOut, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseExecutor := func() { once.Do(func() { close(release) }) }
			defer releaseExecutor()
			childInput := make(chan string, 1)
			var expansions atomic.Int32
			store := NewMemoryEventStore()
			engine := NewEngine(store, funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
				if in.Node.ID == "child" {
					childInput <- string(in.Inputs["value"])
					return NodeOutput{}, nil
				}
				if in.Node.ID != "a" {
					return NodeOutput{}, nil
				}
				<-ctx.Done()
				close(timedOut)
				<-release
				return NodeOutput{Data: json.RawMessage(`{"late":true}`), Usage: tc.usage, UsageIncomplete: tc.incomplete}, tc.err
			}), funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
				expansions.Add(1)
				return Extension{Nodes: []NodeSpec{{ID: "extra", Kind: "x"}}}, nil
			}), Options{})
			wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
				{ID: "a", Kind: "x", Required: tc.required, Expands: true, Timeout: 80 * time.Millisecond},
				{ID: "child", Kind: "x", Required: true, DependsOn: []string{"a"}, InputMapping: map[string]string{"value": "a.output.value"}},
			}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- engine.Run(context.Background(), wfID) }()
			waitSignal(t, timedOut)
			select {
			case err := <-done:
				t.Fatalf("Run returned before the executor: %v", err)
			default:
			}
			releaseExecutor()
			if err := waitRun(t, nil, done); err != nil {
				t.Fatal(err)
			}
			wf, err := engine.Snapshot(context.Background(), wfID)
			if err != nil {
				t.Fatal(err)
			}
			wantUsage, _ := sanitizeUsage(tc.usage)
			n := wf.Nodes["a"]
			if n.Status != NodeFailed || n.Reason != tc.reason || n.Usage != wantUsage || wf.Usage != wantUsage || !n.UsageIncomplete || !wf.UsageIncomplete || len(n.Output) != 0 {
				t.Fatalf("workflow usage=%+v incomplete=%t node=%+v", wf.Usage, wf.UsageIncomplete, n)
			}
			if tc.err != nil && n.ErrorStatus != 403 {
				t.Fatalf("lost sanitized classified error status: %+v", n)
			}
			if tc.required {
				if wf.Status != WorkflowFailed || wf.Nodes["child"].Reason != "unsatisfied" {
					t.Fatalf("required failure did not cascade: %+v", wf)
				}
			} else {
				if wf.Status != WorkflowCompleted || wf.Nodes["child"].Status != NodeCompleted || <-childInput != `{"_unavailable":true,"reason":"dependency_failed"}` {
					t.Fatalf("optional failure did not allow the dependent: %+v", wf)
				}
			}
			if expansions.Load() != 0 || countNodeEvents(t, store, wfID, EventWorkflowExtended, "") != 0 || countNodeEvents(t, store, wfID, EventNodeCompleted, "a") != 0 || countNodeEvents(t, store, wfID, EventNodeFailed, "a") != 1 {
				t.Fatal("late result expanded or produced an invalid terminal event")
			}
			assertReplay(t, store, wfID, wf)
		})
	}
}

func TestLateTimeoutBudgetCancellationAndStaleRevision(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprintf("required=%t", required), func(t *testing.T) {
			timedOut, release, settled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseExecutor := func() { once.Do(func() { close(release) }) }
			defer releaseExecutor()
			terminalType, wantStatus, wantWorkflow := EventNodeSkipped, NodeSkipped, WorkflowCompleted
			if required {
				terminalType, wantStatus, wantWorkflow = EventNodeFailed, NodeFailed, WorkflowFailed
			}
			inner := NewMemoryEventStore()
			store := &progressStore{EventStore: inner, targets: map[string]int{terminalType + "/b": 3}}
			var expansions atomic.Int32
			engine := NewEngine(store, funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
				switch in.Node.ID {
				case "a":
					<-timedOut
					return NodeOutput{Usage: Usage{InputTokens: 10}}, nil
				case "b":
					<-ctx.Done()
					close(timedOut)
					<-release
					return NodeOutput{Data: json.RawMessage(`{"late":true}`), Usage: Usage{InputTokens: 2, OutputTokens: -1}, UsageIncomplete: true}, nil
				case "settle":
					close(settled)
				}
				return NodeOutput{}, nil
			}), funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
				expansions.Add(1)
				return Extension{}, nil
			}), Options{MaxConcurrentNodes: 3})
			wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
				{ID: "a", Kind: "x", Required: true},
				{ID: "b", Kind: "x", Required: required, Expands: true, Timeout: 80 * time.Millisecond},
				{ID: "settle", Kind: "x", Required: true, TokenFree: true, DependsOn: []string{"a"}},
			}, Budget: Budget{MaxTokens: 5}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- engine.Run(context.Background(), wfID) }()
			waitSignal(t, settled) // The scheduler cancelled budget-owned workers before launching this node.
			releaseExecutor()
			if err := waitRun(t, nil, done); err != nil {
				t.Fatal(err)
			}
			wf, err := engine.Snapshot(context.Background(), wfID)
			if err != nil {
				t.Fatal(err)
			}
			b := wf.Nodes["b"]
			if wf.Status != wantWorkflow || b.Status != wantStatus || b.Reason != "budget_exhausted" || b.Usage != (Usage{InputTokens: 2}) || wf.Usage != (Usage{InputTokens: 12}) || !b.UsageIncomplete || !wf.UsageIncomplete {
				t.Fatalf("workflow=%s usage=%+v incomplete=%t b=%+v", wf.Status, wf.Usage, wf.UsageIncomplete, b)
			}
			if expansions.Load() != 0 || countNodeEvents(t, inner, wfID, terminalType, "b") != 1 || countNodeEvents(t, inner, wfID, EventNodeCompleted, "b") != 0 {
				t.Fatal("budget race expanded or duplicated the result")
			}
			store.mu.Lock()
			injected := store.injected
			store.mu.Unlock()
			if injected != 3 {
				t.Fatalf("stale revision injections = %d, want 3", injected)
			}
			assertReplay(t, inner, wfID, wf)
		})
	}
}

func TestLateTimeoutOldAttemptIsFenced(t *testing.T) {
	timedOut, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseExecutor := func() { once.Do(func() { close(release) }) }
	defer releaseExecutor()
	store := NewMemoryEventStore()
	engine := NewEngine(store, funcExecutor(func(ctx context.Context, _ NodeInput) (NodeOutput, error) {
		<-ctx.Done()
		close(timedOut)
		<-release
		return NodeOutput{Usage: Usage{InputTokens: 99}, UsageIncomplete: true}, nil
	}), nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{{ID: "a", Kind: "x", Required: true, Timeout: 80 * time.Millisecond}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, time.Now()),
		mustEv(t, EventNodeQueued, NodeQueuedPayload{NodeID: "a", Attempt: 1}, time.Now()),
	)
	if err != nil {
		t.Fatal(err)
	}
	handle := &runHandle{active: map[string]bool{"a": true}, wake: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() { engine.runNode(context.Background(), handle, wfID, "a"); close(done) }()
	waitSignal(t, timedOut)
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), wfID, wf.Revision,
		mustEv(t, EventNodeRestarted, NodeRestartedPayload{NodeID: "a", Attempt: 2, NextAttempt: 2}, time.Now()),
		mustEv(t, EventNodeQueued, NodeQueuedPayload{NodeID: "a", Attempt: 2}, time.Now()),
		mustEv(t, EventNodeStarted, NodeStartedPayload{NodeID: "a", Attempt: 2}, time.Now()),
	)
	if err != nil {
		t.Fatal(err)
	}
	releaseExecutor()
	waitSignal(t, done)
	wf, err = engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["a"].Attempt != 2 || wf.Nodes["a"].Status != NodeRunning || wf.Usage != (Usage{}) || wf.UsageIncomplete || countNodeEvents(t, store, wfID, EventNodeFailed, "a") != 0 || countNodeEvents(t, store, wfID, EventNodeCompleted, "a") != 0 {
		t.Fatalf("late attempt changed the recovered attempt: %+v", wf)
	}
	assertReplay(t, store, wfID, wf)
}

type delayedStartStore struct {
	EventStore
	started chan struct{}
	release chan struct{}
}

type elapsedDeadlineContext struct{ context.Context }

func (elapsedDeadlineContext) Deadline() (time.Time, bool) {
	return time.Now().Add(-time.Second), true
}

func TestExecuteSafelySkipsElapsedDeadlineBeforeTimerFires(t *testing.T) {
	var calls atomic.Int32
	_, err := executeSafely(elapsedDeadlineContext{context.Background()}, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		calls.Add(1)
		return NodeOutput{}, nil
	}), NodeInput{})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatalf("elapsed deadline: err=%v executor calls=%d", err, calls.Load())
	}
}

func (s *delayedStartStore) Append(ctx context.Context, id string, rev int64, events ...Event) (int64, error) {
	next, err := s.EventStore.Append(ctx, id, rev, events...)
	if err == nil {
		for _, event := range events {
			if event.Type == EventNodeStarted {
				close(s.started)
				<-s.release
				break
			}
		}
	}
	return next, err
}

func TestNodeDeadlineIncludesStartCommit(t *testing.T) {
	inner := NewMemoryEventStore()
	store := &delayedStartStore{EventStore: inner, started: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(store.release) }) }
	defer release()
	var calls atomic.Int32
	engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		calls.Add(1)
		return NodeOutput{Data: json.RawMessage(`{"late":true}`)}, nil
	}), nil, Options{DefaultNodeTimeout: 80 * time.Millisecond})
	id, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), id) }()
	waitSignal(t, store.started)
	<-time.After(120 * time.Millisecond)
	release()
	if err := waitRun(t, nil, done); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if n := wf.Nodes["a"]; n.Status != NodeFailed || n.Reason != "timeout" || calls.Load() != 0 || countNodeEvents(t, inner, id, EventNodeCompleted, "a") != 0 {
		t.Fatalf("expired start ran executor: node=%+v calls=%d", n, calls.Load())
	}
	assertReplay(t, inner, id, wf)
}

func TestExhaustedOrStaleResultDoesNotCallExpander(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "budget_exhausted"
		budget := Budget{MaxTokens: 1}
		if stale {
			name, budget = "stale_attempt", Budget{}
		}
		t.Run(name, func(t *testing.T) {
			var expansions atomic.Int32
			exp := funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
				expansions.Add(1)
				return Extension{}, nil
			})
			spec := NodeSpec{ID: "b", Kind: "x", Expands: true}
			engine, store, id := spentEngineWith(t, budget, exp, spec)
			if stale {
				wf, err := engine.Snapshot(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				_, err = store.Append(context.Background(), id, wf.Revision,
					nodeEvent(t, EventNodeRestarted, "b", 2), nodeEvent(t, EventNodeQueued, "b", 2), nodeEvent(t, EventNodeStarted, "b", 2),
				)
				if err != nil {
					t.Fatal(err)
				}
			}
			out := NodeOutput{Data: json.RawMessage(`{"ok":true}`), Usage: Usage{InputTokens: 3}, UsageIncomplete: true}
			if err := engine.commitNodeSuccess(context.Background(), id, "b", spec, 1, out); err != nil {
				t.Fatal(err)
			}
			wf, err := engine.Snapshot(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if expansions.Load() != 0 || countNodeEvents(t, store, id, EventWorkflowExtended, "") != 0 || countNodeEvents(t, store, id, EventNodeCompleted, "b") != 0 {
				t.Fatal("an exhausted or stale result invoked the expander")
			}
			if stale {
				if wf.Nodes["b"].Status != NodeRunning || wf.Nodes["b"].Attempt != 2 || wf.Usage != (Usage{InputTokens: 1}) || wf.UsageIncomplete {
					t.Fatalf("stale result changed workflow: %+v", wf)
				}
			} else if wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "budget_exhausted" || wf.Usage != (Usage{InputTokens: 4}) || !wf.UsageIncomplete || !wf.Nodes["b"].UsageIncomplete {
				t.Fatalf("exhausted result was not settled: %+v", wf)
			}
			assertReplay(t, store, id, wf)
		})
	}
}
