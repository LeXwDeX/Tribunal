package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// progressStore injects real sibling extension events between an engine load
// and its append. Each injection advances the workflow revision, so the
// engine's original append is fenced until the configured budget is spent.
type progressStore struct {
	EventStore

	// targets maps "<event type>/<node id>" to a remaining injection count.
	// A negative count injects on every matching append.
	targets map[string]int
	notify  func(int)

	mu       sync.Mutex
	injected int
}

func (s *progressStore) Append(ctx context.Context, wfID string, expectRev int64, evs ...Event) (int64, error) {
	s.mu.Lock()
	key, ok := matchTarget(evs, s.targets)
	remaining := s.targets[key]
	allowed := ok && remaining != 0
	if ok && remaining > 0 {
		s.targets[key] = remaining - 1
	}
	n := s.injected
	if allowed {
		s.injected++
	}
	notify := s.notify
	s.mu.Unlock()
	if !allowed {
		return s.EventStore.Append(ctx, wfID, expectRev, evs...)
	}
	if err := s.inject(ctx, wfID, n); err != nil {
		return 0, err
	}
	if notify != nil {
		notify(n + 1)
	}
	// The injection advanced the revision. Forwarding the original expectRev
	// produces the stale result a real concurrent sibling would produce.
	return s.EventStore.Append(ctx, wfID, expectRev, evs...)
}

func (s *progressStore) inject(ctx context.Context, wfID string, n int) error {
	for attempt := 0; attempt < 100; attempt++ {
		events, err := s.EventStore.Load(ctx, wfID)
		if err != nil {
			return err
		}
		var rev int64
		if len(events) > 0 {
			rev = events[len(events)-1].Seq
		}
		ev, err := makeEvent(EventWorkflowExtended, WorkflowExtendedPayload{
			Nodes:  []NodeSpec{{ID: fmt.Sprintf("sibling-%d", n), Kind: "test"}},
			Source: ExtensionSourceFanOut,
		}, testNow.Add(time.Duration(n)*time.Millisecond))
		if err != nil {
			return err
		}
		_, err = s.EventStore.Append(ctx, wfID, rev, ev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrStaleRevision) {
			return err
		}
	}
	return errors.New("progressStore: injection did not commit")
}

// faultStore fails every matching node event append without changing the log.
type faultStore struct {
	EventStore

	targets map[string]struct{}
	err     error

	mu       sync.Mutex
	attempts int
}

func (s *faultStore) Append(ctx context.Context, wfID string, expectRev int64, evs ...Event) (int64, error) {
	if _, ok := matchTarget(evs, s.targets); ok {
		s.mu.Lock()
		s.attempts++
		s.mu.Unlock()
		return 0, s.err
	}
	return s.EventStore.Append(ctx, wfID, expectRev, evs...)
}

func matchTarget[V any](evs []Event, targets map[string]V) (string, bool) {
	for _, ev := range evs {
		key := ev.Type + "/" + eventNodeID(ev)
		if _, ok := targets[key]; ok {
			return key, true
		}
	}
	return "", false
}

func eventNodeID(ev Event) string {
	var payload struct {
		NodeID string `json:"node_id"`
	}
	if len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, &payload) != nil {
		return ""
	}
	return payload.NodeID
}

func countNodeEvents(t *testing.T, store EventStore, wfID, typ, node string) int {
	t.Helper()
	events, err := store.Load(context.Background(), wfID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	count := 0
	for _, ev := range events {
		if ev.Type == typ && eventNodeID(ev) == node {
			count++
		}
	}
	return count
}

func runWithTimeout(t *testing.T, engine *Engine, wfID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return engine.Run(ctx, wfID)
}

func TestStaleRetryProgressKeepsStartedCommit(t *testing.T) {
	const injections = 6
	inner := NewMemoryEventStore()
	store := &progressStore{
		EventStore: inner,
		targets:    map[string]int{EventNodeStarted + "/a": injections},
	}
	var calls atomic.Int32
	engine := NewEngine(store, funcExecutor(func(_ context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "a" {
			calls.Add(1)
		}
		return NodeOutput{Data: json.RawMessage(`{"ok":true}`)}, nil
	}), nil, Options{MaxConcurrentNodes: 4})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWithTimeout(t, engine, wfID); err != nil {
		t.Fatalf("Run = %v", err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCompleted || wf.Nodes["a"].Status != NodeCompleted {
		t.Fatalf("workflow=%s node=%+v", wf.Status, wf.Nodes["a"])
	}
	if calls.Load() != 1 {
		t.Fatalf("executor calls = %d, want 1", calls.Load())
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeStarted, "a"); got != 1 {
		t.Fatalf("started events = %d, want 1", got)
	}
}

func TestStaleRetryProgressKeepsCompletionCommit(t *testing.T) {
	const injections = 6
	inner := NewMemoryEventStore()
	store := &progressStore{
		EventStore: inner,
		targets:    map[string]int{EventNodeCompleted + "/a": injections},
	}
	engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: json.RawMessage(`{"ok":true}`)}, nil
	}), nil, Options{MaxConcurrentNodes: 4})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWithTimeout(t, engine, wfID); err != nil {
		t.Fatalf("Run = %v", err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCompleted || wf.Nodes["a"].Status != NodeCompleted {
		t.Fatalf("workflow=%s node=%+v", wf.Status, wf.Nodes["a"])
	}
	if string(wf.Nodes["a"].Output) != `{"ok":true}` {
		t.Fatalf("output = %s", wf.Nodes["a"].Output)
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeStarted, "a"); got != 1 {
		t.Fatalf("started events = %d, want 1", got)
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeCompleted, "a"); got != 1 {
		t.Fatalf("completed events = %d, want 1", got)
	}
}

func TestStaleRetryFinalizeReentersRunAfterConcurrentExtend(t *testing.T) {
	inner := NewMemoryEventStore()
	store := &progressStore{
		EventStore: inner,
		targets:    map[string]int{EventWorkflowCompleted + "/": 1},
	}
	var siblingRuns atomic.Int32
	engine := NewEngine(store, funcExecutor(func(_ context.Context, in NodeInput) (NodeOutput, error) {
		if strings.HasPrefix(in.Node.ID, "sibling-") {
			siblingRuns.Add(1)
		}
		return NodeOutput{Data: json.RawMessage(`{}`)}, nil
	}), nil, Options{MaxConcurrentNodes: 4})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWithTimeout(t, engine, wfID); err != nil {
		t.Fatalf("Run = %v", err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow = %s reason=%q error=%q, want completed", wf.Status, wf.CancelReason, wf.Error)
	}
	var sibling string
	for _, id := range wf.Order {
		if strings.HasPrefix(id, "sibling-") {
			sibling = id
			break
		}
	}
	if sibling == "" {
		t.Fatalf("injected sibling is missing from order %v", wf.Order)
	}
	if wf.Nodes[sibling].Status != NodeCompleted || siblingRuns.Load() != 1 {
		t.Fatalf("sibling %s status=%s runs=%d, want completed once", sibling, wf.Nodes[sibling].Status, siblingRuns.Load())
	}
}

func TestStaleRetryProgressKeepsScheduleCommit(t *testing.T) {
	const injections = 20 // above the old fixed cap of maxCommitAttempts*4
	inner := NewMemoryEventStore()
	store := &progressStore{
		EventStore: inner,
		targets:    map[string]int{EventNodeQueued + "/a": injections},
	}
	engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: json.RawMessage(`{}`)}, nil
	}), nil, Options{MaxConcurrentNodes: 4})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWithTimeout(t, engine, wfID); err != nil {
		t.Fatalf("Run = %v", err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCompleted || wf.Nodes["a"].Status != NodeCompleted {
		t.Fatalf("workflow=%s node=%+v", wf.Status, wf.Nodes["a"])
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeCompleted, "a"); got != 1 {
		t.Fatalf("completed events = %d, want 1", got)
	}
}

func TestStaleRetryProgressKeepsFailedRun(t *testing.T) {
	const injections = 6
	inner := NewMemoryEventStore()
	store := &progressStore{
		EventStore: inner,
		targets:    map[string]int{EventNodeFailed + "/a": injections},
	}
	engine := NewEngine(store, funcExecutor(func(_ context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "a" {
			return NodeOutput{}, errors.New("boom")
		}
		return NodeOutput{Data: json.RawMessage(`{}`)}, nil
	}), nil, Options{MaxConcurrentNodes: 4})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWithTimeout(t, engine, wfID); err != nil {
		t.Fatalf("Run = %v", err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowFailed || wf.Nodes["a"].Status != NodeFailed || wf.Nodes["a"].Reason != "executor_error" {
		t.Fatalf("workflow=%s node=%+v", wf.Status, wf.Nodes["a"])
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeStarted, "a"); got != 1 {
		t.Fatalf("started events = %d, want 1", got)
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeFailed, "a"); got != 1 {
		t.Fatalf("failed events = %d, want 1", got)
	}
}

func TestStaleRetryFailureAndSkippedCommits(t *testing.T) {
	const injections = 6
	inner := NewMemoryEventStore()
	store := &progressStore{
		EventStore: inner,
		targets: map[string]int{
			EventNodeFailed + "/fail":  injections,
			EventNodeSkipped + "/skip": injections,
		},
	}
	engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		node("fail"),
		{ID: "skip", Kind: "test"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inner.Append(context.Background(), wfID, wf.Revision,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "fail", 1),
		nodeEvent(t, EventNodeQueued, "skip", 1),
	); err != nil {
		t.Fatal(err)
	}
	if err := engine.commitNodeFailure(context.Background(), wfID, "fail", 1, "executor_error", "boom"); err != nil {
		t.Fatalf("commitNodeFailure = %v", err)
	}
	if err := engine.commitNodeSkipped(context.Background(), wfID, "skip", 1, "budget_exhausted"); err != nil {
		t.Fatalf("commitNodeSkipped = %v", err)
	}
	wf, err = engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["fail"].Status != NodeFailed || wf.Nodes["fail"].Reason != "executor_error" {
		t.Fatalf("fail = %+v", wf.Nodes["fail"])
	}
	if wf.Nodes["skip"].Status != NodeSkipped || wf.Nodes["skip"].Reason != "budget_exhausted" {
		t.Fatalf("skip = %+v", wf.Nodes["skip"])
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeFailed, "fail"); got != 1 {
		t.Fatalf("failed events = %d, want 1", got)
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeSkipped, "skip"); got != 1 {
		t.Fatalf("skipped events = %d, want 1", got)
	}
}

func TestStaleRetryPersistErrorPropagates(t *testing.T) {
	inner := NewMemoryEventStore()
	persistErr := errors.New("dag: persistence unavailable")
	store := &faultStore{
		EventStore: inner,
		targets:    map[string]struct{}{EventNodeCompleted + "/a": {}},
		err:        persistErr,
	}
	engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: json.RawMessage(`{"ok":true}`)}, nil
	}), nil, Options{MaxConcurrentNodes: 4})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runErr := runWithTimeout(t, engine, wfID)
	if !errors.Is(runErr, persistErr) {
		t.Fatalf("Run = %v, want wrapped %v", runErr, persistErr)
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeCompleted, "a"); got != 0 {
		t.Fatalf("completed events = %d, want 0", got)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Nodes["a"].Status != NodeRunning {
		t.Fatalf("a = %+v, want running without accepted output", wf.Nodes["a"])
	}
}

func TestStaleRetryCancellationStopsProgress(t *testing.T) {
	inner := NewMemoryEventStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &progressStore{
		EventStore: inner,
		targets:    map[string]int{EventNodeStarted + "/a": -1},
		notify: func(n int) {
			if n == 6 {
				cancel()
			}
		},
	}
	engine := NewEngine(store, funcExecutor(func(_ context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID != "a" {
			// Injected siblings are legitimate dependency-free nodes; only a is
			// fenced by the start commit this test keeps stale.
			return NodeOutput{Data: json.RawMessage(`{}`)}, nil
		}
		t.Error("executor must not run while the start commit keeps staling")
		return NodeOutput{}, nil
	}), nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(ctx, wfID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCancelled || wf.CancelReason != "context_cancelled" {
		t.Fatalf("workflow = %s reason=%q", wf.Status, wf.CancelReason)
	}
	if node := wf.Nodes["a"]; node.Status != NodeSkipped || node.Reason != "workflow_cancelled" {
		t.Fatalf("a = %+v, want skipped by cancellation", node)
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeStarted, "a"); got != 0 {
		t.Fatalf("started events = %d, want 0", got)
	}
	if got := countNodeEvents(t, inner, wfID, EventNodeCompleted, "a"); got != 0 {
		t.Fatalf("completed events = %d, want 0", got)
	}
}

func TestCancelRetriesAfterConcurrentExtension(t *testing.T) {
	ctx := context.Background()
	inner := NewMemoryEventStore()
	store := &progressStore{
		EventStore: inner,
		targets:    map[string]int{EventWorkflowCancelled + "/": 1},
	}
	engine := NewEngine(store, nil, nil, Options{})
	wfID, err := engine.Create(ctx, GraphSpec{Nodes: []NodeSpec{
		node("running"), node("queued"), node("pending"), node("completed"),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision, err := inner.Append(ctx, wfID, wf.Revision,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "running", 1),
		nodeEvent(t, EventNodeStarted, "running", 1),
		nodeEvent(t, EventNodeQueued, "queued", 1),
		nodeEvent(t, EventNodeQueued, "completed", 1),
		nodeEvent(t, EventNodeStarted, "completed", 1),
		nodeEvent(t, EventNodeCompleted, "completed", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Cancel(ctx, wfID, "manual"); err != nil {
		t.Fatalf("Cancel after concurrent extension: %v", err)
	}
	store.mu.Lock()
	injected := store.injected
	store.mu.Unlock()
	if injected != 1 {
		t.Fatalf("revision injections = %d, want 1", injected)
	}
	events, err := inner.Load(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) <= int(beforeRevision) || events[beforeRevision].Type != EventWorkflowExtended {
		t.Fatalf("missing concurrent extension after revision %d: %v", beforeRevision, eventTypes(events))
	}
	wf, err = engine.Snapshot(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCancelled || wf.CancelReason != "manual" {
		t.Fatalf("workflow status=%s reason=%q, want cancelled by manual", wf.Status, wf.CancelReason)
	}
	wantNodes := map[string]struct {
		status NodeStatus
		reason string
	}{
		"running":   {NodeAborted, "manual"},
		"queued":    {NodeSkipped, "workflow_cancelled"},
		"pending":   {NodeSkipped, "workflow_cancelled"},
		"sibling-0": {NodeSkipped, "workflow_cancelled"},
		"completed": {NodeCompleted, ""},
	}
	for id, want := range wantNodes {
		got, ok := wf.Nodes[id]
		if !ok || got.Status != want.status || got.Reason != want.reason {
			t.Errorf("node %s = %+v (present=%v), want status=%s reason=%q", id, got, ok, want.status, want.reason)
		}
	}
	if string(wf.Nodes["completed"].Output) != `{"ok":true}` {
		t.Fatalf("completed output = %s", wf.Nodes["completed"].Output)
	}
	if got := countNodeEvents(t, inner, wfID, EventWorkflowCancelled, ""); got != 1 {
		t.Fatalf("workflow cancelled events = %d, want 1", got)
	}
}

func TestExecuteSafelySkipsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := executeSafely(ctx, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		called = true
		return NodeOutput{}, nil
	}), NodeInput{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeSafely error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("executor ran with a cancelled context")
	}
}

func TestStaleRetryWithoutProgressFailsBounded(t *testing.T) {
	tests := []struct {
		name  string
		event string
	}{
		{name: "node start", event: EventNodeStarted + "/a"},
		{name: "schedule", event: EventNodeQueued + "/a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := NewMemoryEventStore()
			store := &faultStore{
				EventStore: inner,
				targets:    map[string]struct{}{tc.event: {}},
				err:        fmt.Errorf("%w: injected", ErrStaleRevision),
			}
			engine := NewEngine(store, funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
				t.Error("executor must not run when the commit cannot progress")
				return NodeOutput{}, nil
			}), nil, Options{})
			wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			runErr := runWithTimeout(t, engine, wfID)
			if !errors.Is(runErr, ErrStaleRevision) {
				t.Fatalf("Run = %v, want ErrStaleRevision", runErr)
			}
			store.mu.Lock()
			attempts := store.attempts
			store.mu.Unlock()
			if attempts != maxCommitAttempts {
				t.Fatalf("append attempts = %d, want %d", attempts, maxCommitAttempts)
			}
		})
	}
}
