package dag

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resultGateStore blocks the first terminal event of node "a" inside Append.
// The test uses the gate to hold the run between "executor returned usage" and
// "usage is committed", then checks whether another queued node starts from
// the stale usage snapshot.
type resultGateStore struct {
	*MemoryEventStore
	eventType string
	reached   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *resultGateStore) Append(ctx context.Context, wfID string, expect int64, events ...Event) (int64, error) {
	if _, ok := matchTarget(events, map[string]bool{s.eventType + "/a": true}); ok {
		s.once.Do(func() {
			close(s.reached)
			<-s.release
		})
	}
	return s.MemoryEventStore.Append(ctx, wfID, expect, events...)
}

// TestBudgetSerializesQueuedStartUntilTerminalCommit is a barrier regression
// test for the concurrency=1 window between a returned provider usage and its
// terminal event. Node a holds the only permit while its terminal append is
// delayed; node b, already queued and dispatched, must not start and send
// another request from the old committed usage.
func TestBudgetSerializesQueuedStartUntilTerminalCommit(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		execErr   error
	}{
		{name: "success", eventType: EventNodeCompleted},
		{name: "failure", eventType: EventNodeFailed, execErr: errors.New("provider failed after usage")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &resultGateStore{
				MemoryEventStore: NewMemoryEventStore(),
				eventType:        tc.eventType,
				reached:          make(chan struct{}),
				release:          make(chan struct{}),
			}
			var releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(func() { close(store.release) }) }
			defer releaseGate()

			var bCalls atomic.Int32
			bStarted := make(chan struct{})
			var bOnce sync.Once
			exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
				if in.Node.ID == "a" {
					return NodeOutput{Data: json.RawMessage(`{}`), Usage: Usage{InputTokens: 10}}, tc.execErr
				}
				bCalls.Add(1)
				bOnce.Do(func() { close(bStarted) })
				return NodeOutput{Data: json.RawMessage(`{}`)}, nil
			})
			engine := NewEngine(store, exec, nil, Options{
				MaxConcurrentNodes: 1,
				Clock:              func() time.Time { return testNow },
			})
			wfID, err := engine.Create(context.Background(), GraphSpec{
				Nodes:  []NodeSpec{node("a"), {ID: "b", Kind: "test"}},
				Budget: Budget{MaxTokens: 5},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.MemoryEventStore.Append(context.Background(), wfID, 1,
				mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
				nodeEvent(t, EventNodeQueued, "a", 1),
				nodeEvent(t, EventNodeQueued, "b", 1),
			); err != nil {
				t.Fatal(err)
			}
			handle := &runHandle{
				active:      make(map[string]bool),
				nodeCancels: make(map[string]nodeCancel),
				wake:        make(chan struct{}, 1),
			}

			var workers sync.WaitGroup
			workers.Add(1)
			go func() {
				defer workers.Done()
				engine.runNode(context.Background(), handle, wfID, "a")
			}()
			waitSignal(t, store.reached)

			// a still owns the only permit. Starting b now mirrors a scheduler
			// that already claimed the second queued node.
			workers.Add(1)
			go func() {
				defer workers.Done()
				engine.runNode(context.Background(), handle, wfID, "b")
			}()
			select {
			case <-bStarted:
				t.Fatal("node b sent a request before node a's usage was committed")
			case <-time.After(500 * time.Millisecond):
			}

			releaseGate()
			workers.Wait()
			if got := bCalls.Load(); got != 0 {
				t.Fatalf("node b executed %d times after the budget was reached", got)
			}
			wf, err := engine.Snapshot(context.Background(), wfID)
			if err != nil {
				t.Fatal(err)
			}
			if b := wf.Nodes["b"]; b.Status != NodeSkipped || b.Reason != "budget_exhausted" {
				t.Fatalf("node b = %+v, want skipped budget_exhausted", b)
			}
		})
	}
}

// TestBudgetAllowsConcurrentCommittedUsageWindow pins the allowed boundary:
// nodes already dispatched under a concurrency limit of 2 keep running while
// another node commits its usage. The fix may not serialize in-flight requests.
func TestBudgetAllowsConcurrentCommittedUsageWindow(t *testing.T) {
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		ready <- struct{}{}
		<-release
		return NodeOutput{Data: json.RawMessage(`{}`), Usage: Usage{InputTokens: 1}}, nil
	})
	engine := fastEngine(exec, Options{MaxConcurrentNodes: 2})
	wfID, err := engine.Create(context.Background(), GraphSpec{
		Nodes:  []NodeSpec{node("a"), node("b")},
		Budget: Budget{MaxTokens: 100},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	waitSignal(t, ready)
	waitSignal(t, ready)
	close(release)
	if err := waitRun(t, nil, done); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow = %s", wf.Status)
	}
	for _, id := range []string{"a", "b"} {
		if wf.Nodes[id].Status != NodeCompleted {
			t.Fatalf("%s = %+v", id, wf.Nodes[id])
		}
	}
}
