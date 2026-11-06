package dag

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// loadHookStore runs hook once, inside the first Load after it is armed.
type loadHookStore struct {
	*MemoryEventStore
	armed atomic.Bool
	hook  func()
}

func (s *loadHookStore) Load(ctx context.Context, wfID string) ([]Event, error) {
	if s.armed.CompareAndSwap(true, false) {
		s.hook()
	}
	return s.MemoryEventStore.Load(ctx, wfID)
}

func countRestarts(t *testing.T, store EventStore, wfID string) int {
	t.Helper()
	events, err := store.Load(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type == EventNodeRestarted {
			n++
		}
	}
	return n
}

// A Run that starts while recovery is loading the workflow must never see its
// freshly started node restarted: recovery owns the run slot while it appends.
func TestRecoverCannotRestartNodeOfConcurrentRun(t *testing.T) {
	for name, recover := range map[string]func(*Engine, string) error{
		"global":   func(e *Engine, _ string) error { return e.Recover(context.Background()) },
		"workflow": func(e *Engine, id string) error { return e.RecoverWorkflow(context.Background(), id) },
	} {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
				close(started)
				<-release
				return NodeOutput{}, nil
			})
			store := &loadHookStore{MemoryEventStore: NewMemoryEventStore()}
			engine := NewEngine(store, exec, nil, Options{})
			wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			runDone := make(chan error, 1)
			store.hook = func() {
				go func() { runDone <- engine.Run(context.Background(), wfID) }()
				// Either the node starts (recovery lost the slot) or Run is
				// refused (recovery holds it); both settle without timers.
				select {
				case <-started:
				case err := <-runDone:
					runDone <- err
				}
			}
			store.armed.Store(true)
			if err := recover(engine, wfID); err != nil {
				t.Fatal(err)
			}
			if n := countRestarts(t, store, wfID); n != 0 {
				t.Fatalf("recovery restarted a node of a concurrent Run: %d NodeRestarted events", n)
			}
			select {
			case <-started:
				close(release)
				if err := <-runDone; err != nil {
					t.Fatal(err)
				}
			case err := <-runDone:
				if !errors.Is(err, ErrAlreadyRunning) {
					t.Fatalf("Run during recovery = %v, want ErrAlreadyRunning", err)
				}
			}
		})
	}
}
