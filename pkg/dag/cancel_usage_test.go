package dag

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type cancelUsageStore struct {
	EventStore
	cancelled chan struct{}
	once      sync.Once
}

func (s *cancelUsageStore) Append(ctx context.Context, id string, rev int64, events ...Event) (int64, error) {
	next, err := s.EventStore.Append(ctx, id, rev, events...)
	if err == nil {
		for _, event := range events {
			if event.Type == EventWorkflowCancelled {
				s.once.Do(func() { close(s.cancelled) })
			}
		}
	}
	return next, err
}

func TestCancelMarksRunningUsageIncomplete(t *testing.T) {
	for _, outer := range []bool{false, true} {
		name := "explicit"
		if outer {
			name = "parent_context"
		}
		t.Run(name, func(t *testing.T) {
			started, cancelled := make(chan struct{}, 2), make(chan struct{}, 2)
			release := make(chan struct{})
			var once sync.Once
			releaseExecutors := func() { once.Do(func() { close(release) }) }
			defer releaseExecutors()
			inner := NewMemoryEventStore()
			store := &cancelUsageStore{EventStore: inner, cancelled: make(chan struct{})}
			engine := NewEngine(store, funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
				if in.Node.ID == "a" {
					return NodeOutput{Usage: Usage{InputTokens: 7}}, nil
				}
				started <- struct{}{}
				<-ctx.Done()
				cancelled <- struct{}{}
				<-release
				return NodeOutput{Usage: Usage{InputTokens: 99}, UsageIncomplete: true}, nil
			}), nil, Options{MaxConcurrentNodes: 2})
			id, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
				node("a"),
				{ID: "b", Kind: "x", Required: true, DependsOn: []string{"a"}},
				{ID: "c", Kind: "x", Required: true, TokenFree: true, DependsOn: []string{"a"}},
				node("pending", "b"),
			}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- engine.Run(ctx, id) }()
			waitSignal(t, started)
			waitSignal(t, started)
			if outer {
				cancel()
			} else if err := engine.Cancel(context.Background(), id, "user"); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, cancelled)
			waitSignal(t, cancelled)
			waitSignal(t, store.cancelled)
			select {
			case err := <-done:
				t.Fatalf("Run returned before its executors exited: %v", err)
			default:
			}
			before, err := engine.Snapshot(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if before.Status != WorkflowCancelled || before.Usage != (Usage{InputTokens: 7}) || !before.UsageIncomplete || before.Nodes["a"].UsageIncomplete || before.Nodes["pending"].UsageIncomplete || before.Nodes["pending"].Status != NodeSkipped {
				t.Fatalf("cancelled workflow = %+v", before)
			}
			for _, id := range []string{"b", "c"} {
				if n := before.Nodes[id]; n.Status != NodeAborted || !n.UsageIncomplete || n.Usage != (Usage{}) {
					t.Fatalf("%s = %+v", id, n)
				}
			}
			releaseExecutors()
			runErr := waitRun(t, nil, done)
			if outer && !errors.Is(runErr, context.Canceled) {
				t.Fatalf("Run error = %v, want context.Canceled", runErr)
			}
			after, err := engine.Snapshot(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("late result changed cancelled workflow: before=%+v after=%+v", before, after)
			}
			if countNodeEvents(t, inner, id, EventNodeAborted, "b") != 1 || countNodeEvents(t, inner, id, EventNodeAborted, "c") != 1 || countNodeEvents(t, inner, id, EventNodeCompleted, "b") != 0 {
				t.Fatal("cancel produced an invalid or duplicate terminal event")
			}
			assertReplay(t, inner, id, after)
		})
	}
}

type cancelCompletionRaceStore struct {
	EventStore
	once sync.Once
	err  error
}

func (s *cancelCompletionRaceStore) Append(ctx context.Context, id string, rev int64, events ...Event) (int64, error) {
	for _, event := range events {
		if event.Type == EventWorkflowCancelled {
			s.once.Do(func() {
				var completed Event
				completed, s.err = makeEvent(EventNodeCompleted, NodeCompletedPayload{NodeID: "b", Attempt: 1, Usage: Usage{InputTokens: 3}}, testNow)
				if s.err == nil {
					_, s.err = s.EventStore.Append(ctx, id, rev, completed)
				}
			})
			if s.err != nil {
				return 0, s.err
			}
			break
		}
	}
	return s.EventStore.Append(ctx, id, rev, events...)
}

func TestCancelRetriesAfterCompletedResultWins(t *testing.T) {
	inner := NewMemoryEventStore()
	store := &cancelCompletionRaceStore{EventStore: inner}
	engine := NewEngine(store, nil, nil, Options{})
	id, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		node("b"), {ID: "c", Kind: "x", TokenFree: true}, node("queued"), node("pending"),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inner.Append(context.Background(), id, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "b", 1), nodeEvent(t, EventNodeStarted, "b", 1),
		nodeEvent(t, EventNodeQueued, "c", 1), nodeEvent(t, EventNodeStarted, "c", 1),
		nodeEvent(t, EventNodeQueued, "queued", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Cancel(context.Background(), id, "user"); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCancelled || wf.Usage != (Usage{InputTokens: 3}) || !wf.UsageIncomplete || wf.Nodes["b"].Status != NodeCompleted || wf.Nodes["b"].UsageIncomplete || wf.Nodes["c"].Status != NodeAborted || !wf.Nodes["c"].UsageIncomplete {
		t.Fatalf("cancellation race = %+v", wf)
	}
	for _, id := range []string{"queued", "pending"} {
		if n := wf.Nodes[id]; n.Status != NodeSkipped || n.UsageIncomplete {
			t.Fatalf("unstarted node %s = %+v", id, n)
		}
	}
	if countNodeEvents(t, inner, id, EventNodeCompleted, "b") != 1 || countNodeEvents(t, inner, id, EventNodeAborted, "b") != 0 || countNodeEvents(t, inner, id, EventNodeAborted, "c") != 1 {
		t.Fatal("stale cancel overwrote a committed result or duplicated an event")
	}
	assertReplay(t, inner, id, wf)
}

func TestCancelAfterAllResultsDoesNotInventUnknownUsage(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store, nil, nil, Options{})
	id, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), id, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
		mustEv(t, EventNodeCompleted, NodeCompletedPayload{NodeID: "a", Attempt: 1, Usage: Usage{InputTokens: 3}}, testNow),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Cancel(context.Background(), id, "user"); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCancelled || wf.UsageIncomplete || wf.Nodes["a"].UsageIncomplete || wf.Usage != (Usage{InputTokens: 3}) || countNodeEvents(t, store, id, EventNodeAborted, "a") != 0 {
		t.Fatalf("committed result was marked unknown: %+v", wf)
	}
	assertReplay(t, store, id, wf)
}

func TestAbortUsageIncompleteEventReplayCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name       string
		payload    json.RawMessage
		incomplete bool
	}{
		{name: "legacy_attempt", payload: json.RawMessage(`{"node_id":"a","attempt":1,"reason":"legacy"}`)},
		{name: "incomplete", payload: json.RawMessage(`{"node_id":"a","attempt":1,"reason":"user","usage_incomplete":true}`), incomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := seq([]Event{created(t, node("a")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
				nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
				{Type: EventNodeAborted, At: testNow, Payload: tc.payload},
				mustEv(t, EventWorkflowCancelled, WorkflowTerminalPayload{Reason: "user"}, testNow),
			})
			wf, err := Project(events)
			if err != nil {
				t.Fatal(err)
			}
			if wf.Nodes["a"].UsageIncomplete != tc.incomplete || wf.UsageIncomplete != tc.incomplete || wf.Usage != (Usage{}) {
				t.Fatalf("incomplete=%t workflow=%+v", tc.incomplete, wf)
			}
		})
	}
}

func TestAbortUsageIncompleteStillFencesAttempt(t *testing.T) {
	events := seq([]Event{created(t, node("a")), mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1), nodeEvent(t, EventNodeStarted, "a", 1),
		{Type: EventNodeAborted, At: testNow, Payload: json.RawMessage(`{"node_id":"a","attempt":2,"usage_incomplete":true}`)},
	})
	if _, err := Project(events); !errors.Is(err, ErrStaleAttempt) {
		t.Fatalf("stale abort = %v", err)
	}
}

func TestInterruptionMarksUsageOnRecoveryNotAbort(t *testing.T) {
	started := make(chan struct{})
	store := NewMemoryEventStore()
	engine := NewEngine(store, funcExecutor(func(ctx context.Context, _ NodeInput) (NodeOutput, error) {
		close(started)
		<-ctx.Done()
		return NodeOutput{Usage: Usage{InputTokens: 5}}, ctx.Err()
	}), nil, Options{DefaultNodeTimeout: time.Minute})
	id, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx, id) }()
	waitSignal(t, started)
	cancel(ErrInterrupted)
	if err := waitRun(t, nil, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption = %v", err)
	}
	before, err := engine.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Terminal() || before.Nodes["a"].Status != NodeRunning || before.UsageIncomplete {
		t.Fatalf("interruption wrote a durable cancellation: %+v", before)
	}
	if err := engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := engine.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Nodes["a"].Attempt != 2 || !after.UsageIncomplete || countNodeEvents(t, store, id, EventNodeAborted, "a") != 0 {
		t.Fatalf("recovery did not retain the missing attempt: %+v", after)
	}
	assertReplay(t, store, id, after)
}
