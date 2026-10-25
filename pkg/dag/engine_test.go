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

type funcExecutor func(ctx context.Context, in NodeInput) (NodeOutput, error)

func (f funcExecutor) Execute(ctx context.Context, in NodeInput) (NodeOutput, error) {
	return f(ctx, in)
}

type funcExpander func(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error)

func (f funcExpander) Expand(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error) {
	return f(ctx, wf, from, out)
}

func output(t *testing.T, value any) NodeOutput {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return NodeOutput{Data: data}
}

func fastEngine(exec NodeExecutor, opts Options) *Engine {
	return NewEngine(NewMemoryEventStore(), exec, nil, opts)
}

func waitRun(t *testing.T, cancel context.CancelFunc, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		if cancel != nil {
			cancel()
		}
		t.Fatal("Run did not finish")
		return nil
	}
}

func TestEngineHappyPathAndInputs(t *testing.T) {
	var (
		mu     sync.Mutex
		order  []string
		bInput string
	)
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		mu.Lock()
		order = append(order, in.Node.ID)
		mu.Unlock()
		switch in.Node.ID {
		case "a":
			return output(t, map[string]any{"value": "from-a"}), nil
		case "b":
			bInput = string(in.Inputs["in"])
			if in.Meta["project_id"] != "7" {
				t.Errorf("meta = %v", in.Meta)
			}
			return output(t, map[string]any{"ok": true}), nil
		}
		return NodeOutput{}, errors.New("unexpected node")
	})
	engine := fastEngine(exec, Options{MaxConcurrentNodes: 2})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "a", Kind: "x", Required: true},
		{ID: "b", Kind: "x", Required: true, DependsOn: []string{"a"}, InputMapping: map[string]string{"in": "a.output.value"}},
	}}, map[string]string{"project_id": "7"})
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
	if wf.Status != WorkflowCompleted {
		t.Fatalf("status = %s", wf.Status)
	}
	for _, id := range []string{"a", "b"} {
		if wf.Nodes[id].Status != NodeCompleted {
			t.Fatalf("%s = %+v", id, wf.Nodes[id])
		}
	}
	if bInput != `"from-a"` {
		t.Fatalf("b input = %s", bInput)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order = %v", order)
	}
}

func TestEngineRequiredFailureCascade(t *testing.T) {
	calls := make(map[string]int)
	var mu sync.Mutex
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		mu.Lock()
		calls[in.Node.ID]++
		mu.Unlock()
		if in.Node.ID == "a" {
			return NodeOutput{}, errors.New("boom")
		}
		return NodeOutput{}, nil
	})
	engine := fastEngine(exec, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		node("a"),
		node("b", "a"),
		node("c", "b"),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		events, _ := engine.store.Load(context.Background(), wfID)
		wf, _ := engine.Snapshot(context.Background(), wfID)
		t.Fatalf("run: %v events=%v nodes=%+v", err, eventTypes(events), wf.Nodes)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowFailed {
		t.Fatalf("workflow = %s", wf.Status)
	}
	for _, id := range []string{"a", "b", "c"} {
		if wf.Nodes[id].Status != NodeFailed {
			t.Fatalf("%s = %s", id, wf.Nodes[id].Status)
		}
	}
	if len(calls) != 1 || calls["a"] != 1 {
		t.Fatalf("calls = %v", calls)
	}
	if wf.Nodes["b"].Reason != "unsatisfied" {
		t.Fatalf("b reason = %q", wf.Nodes["b"].Reason)
	}
}

func TestEngineOptionalFailurePlaceholder(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "a" {
			return NodeOutput{}, errors.New("optional boom")
		}
		var placeholder map[string]any
		if err := json.Unmarshal(in.Inputs["x"], &placeholder); err != nil {
			t.Fatalf("placeholder: %v", err)
		}
		if placeholder["_unavailable"] != true || placeholder["reason"] != "dependency_failed" {
			t.Fatalf("placeholder = %v", placeholder)
		}
		return output(t, map[string]any{"ok": true}), nil
	})
	engine := fastEngine(exec, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "a", Kind: "x"},
		{ID: "b", Kind: "x", Required: true, DependsOn: []string{"a"}, InputMapping: map[string]string{"x": "a.output.value"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow = %s", wf.Status)
	}
	if wf.Nodes["a"].Status != NodeFailed || wf.Nodes["b"].Status != NodeCompleted {
		t.Fatalf("nodes: %+v", wf.Nodes)
	}
}

func TestEngineConditionAndSkippedCascade(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "a" {
			return output(t, map[string]any{"ok": false}), nil
		}
		return output(t, map[string]any{"ok": true}), nil
	})
	engine := fastEngine(exec, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		node("a"),
		{ID: "b", Kind: "x", Required: true, DependsOn: []string{"a"}, Condition: "a.output.ok == true"},
		node("c", "b"),
		{ID: "d", Kind: "x", Required: true, DependsOn: []string{"a"}, Condition: "a.output.ok == false"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow = %s", wf.Status)
	}
	if wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "condition_false" {
		t.Fatalf("b: %+v", wf.Nodes["b"])
	}
	if wf.Nodes["c"].Status != NodeSkipped || wf.Nodes["c"].Reason != "deps_skipped" {
		t.Fatalf("c: %+v", wf.Nodes["c"])
	}
	if wf.Nodes["d"].Status != NodeCompleted {
		t.Fatalf("d: %+v", wf.Nodes["d"])
	}
}

func TestEngineConditionWithDotNodeIDs(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		switch in.Node.ID {
		case "rv.std.p1":
			return output(t, map[string]any{"verdict": "BLOCKING"}), nil
		case "synthesize":
			return output(t, map[string]any{"ok": true}), nil
		}
		t.Fatalf("unexpected node %s", in.Node.ID)
		return NodeOutput{}, nil
	})
	engine := fastEngine(exec, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		node("rv.std.p1"),
		{ID: "synthesize", Kind: "x", Required: true, DependsOn: []string{"rv.std.p1"}, Condition: `rv.std.p1.output.verdict == "BLOCKING"`},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["synthesize"].Status != NodeCompleted {
		t.Fatalf("synthesize: %+v", wf.Nodes["synthesize"])
	}
}

func TestEngineAtomicFanOutExtension(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		switch in.Node.ID {
		case "plan":
			return output(t, map[string]any{"packages": []string{"p1"}}), nil
		case "cr.p1":
			return output(t, map[string]any{"finding": "f"}), nil
		case "gather":
			return output(t, map[string]any{"ok": true}), nil
		}
		return NodeOutput{}, nil
	})
	exp := funcExpander(func(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error) {
		return Extension{
			Nodes:      []NodeSpec{{ID: "cr.p1", Kind: "x", Required: true, DependsOn: []string{"plan"}}},
			DepPatches: []DepPatch{{Node: "gather", Add: []string{"cr.p1"}}},
			Source:     ExtensionSourceFanOut,
		}, nil
	})
	store := NewMemoryEventStore()
	engine := NewEngine(store, exec, exp, Options{MaxConcurrentNodes: 4})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "plan", Kind: "x", Required: true, Expands: true},
		{ID: "gather", Kind: "x", Required: true, DependsOn: []string{"plan"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow = %s (%+v)", wf.Status, wf.Nodes)
	}
	if len(wf.Spec.Nodes) != 3 || len(wf.Nodes["gather"].Spec.DependsOn) != 2 {
		t.Fatalf("spec: %+v", wf.Spec.Nodes)
	}
	events, _ := store.Load(context.Background(), wfID)
	completedPlan := false
	atomicPair := false
	for i, ev := range events {
		if ev.Type == EventNodeCompleted && len(ev.Payload) > 0 && containsNodeID(ev.Payload, "plan") {
			completedPlan = true
			if i+1 < len(events) && events[i+1].Type == EventWorkflowExtended {
				atomicPair = true
			}
		}
	}
	if !completedPlan || !atomicPair {
		t.Fatalf("planned completion is not adjacent to extension: %v", eventTypes(events))
	}
}

func TestEngineExtensionFailureFailsOrigin(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "gather" {
			t.Fatal("gather must not run after expansion failure")
		}
		return output(t, map[string]any{}), nil
	})
	exp := funcExpander(func(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error) {
		return Extension{Nodes: []NodeSpec{node("orphan", "ghost")}}, nil
	})
	store := NewMemoryEventStore()
	engine := NewEngine(store, exec, exp, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "plan", Kind: "x", Required: true, Expands: true},
		node("gather", "plan"),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowFailed {
		t.Fatalf("workflow = %s", wf.Status)
	}
	if wf.Nodes["plan"].Status != NodeFailed || wf.Nodes["plan"].Reason != "expand_failed" {
		t.Fatalf("plan: %+v", wf.Nodes["plan"])
	}
	if wf.Nodes["gather"].Status != NodeFailed {
		t.Fatalf("gather: %+v", wf.Nodes["gather"])
	}
	events, _ := store.Load(context.Background(), wfID)
	for _, ev := range events {
		if ev.Type == EventNodeCompleted && containsNodeID(ev.Payload, "plan") {
			t.Fatal("failed expansion persisted plan completed before failing")
		}
	}
}

func TestEngineExpanderErrorFailsOrigin(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return output(t, map[string]any{}), nil
	})
	exp := funcExpander(func(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error) {
		return Extension{}, errors.New("cannot plan")
	})
	engine := NewEngine(NewMemoryEventStore(), exec, exp, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{{ID: "plan", Kind: "x", Required: true, Expands: true}}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["plan"].Status != NodeFailed || wf.Nodes["plan"].Reason != "expand_failed" {
		t.Fatalf("plan: %+v", wf.Nodes["plan"])
	}
}

func TestEngineConcurrencyBound(t *testing.T) {
	var active, maxActive int64
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		now := atomic.AddInt64(&active, 1)
		for {
			old := atomic.LoadInt64(&maxActive)
			if now <= old || atomic.CompareAndSwapInt64(&maxActive, old, now) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt64(&active, -1)
		return NodeOutput{}, nil
	})
	engine := fastEngine(exec, Options{MaxConcurrentNodes: 1})
	var nodes []NodeSpec
	for _, id := range []string{"a", "b", "c", "d"} {
		nodes = append(nodes, node(id))
	}
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: nodes}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&maxActive); got != 1 {
		t.Fatalf("max concurrency = %d", got)
	}
}

func TestEngineQueueTimeout(t *testing.T) {
	var called atomic.Bool
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		called.Store(true)
		return NodeOutput{}, nil
	})
	engine := fastEngine(exec, Options{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Second})
	engine.sem <- struct{}{}
	defer func() { <-engine.sem }()
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "a", Kind: "x", Required: true, Timeout: 40 * time.Millisecond},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["a"].Status != NodeFailed || wf.Nodes["a"].Reason != "queue_timeout" {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
	if called.Load() {
		t.Fatal("executor must not run after queue timeout")
	}
}

func TestEngineCancelAbortsAndSkips(t *testing.T) {
	started := make(chan struct{})
	observedCancel := make(chan struct{})
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "a" {
			close(started)
			<-ctx.Done()
			close(observedCancel)
			return NodeOutput{}, ctx.Err()
		}
		return NodeOutput{}, errors.New("b must be skipped")
	})
	engine := fastEngine(exec, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		node("a"),
		node("b", "a"),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("node did not start")
	}
	if err := engine.Cancel(context.Background(), wfID, "user"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observedCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("executor context was not cancelled")
	}
	_ = waitRun(t, nil, done)
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCancelled || wf.CancelReason != "user" {
		t.Fatalf("workflow: %+v", wf)
	}
	if wf.Nodes["a"].Status != NodeAborted || wf.Nodes["a"].Reason != "user" {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
	if wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "workflow_cancelled" {
		t.Fatalf("b: %+v", wf.Nodes["b"])
	}
	before, _ := engine.store.Load(context.Background(), wfID)
	if err := engine.Cancel(context.Background(), wfID, "again"); err != nil {
		t.Fatal(err)
	}
	after, _ := engine.Snapshot(context.Background(), wfID)
	if after.CancelReason != "user" {
		t.Fatalf("idempotent cancel changed reason: %+v", after)
	}
	afterEvents, _ := engine.store.Load(context.Background(), wfID)
	if len(afterEvents) != len(before) {
		t.Fatal("idempotent cancel appended events")
	}
}

func TestEngineConcurrentCancel(t *testing.T) {
	started := make(chan struct{})
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		close(started)
		<-ctx.Done()
		return NodeOutput{}, ctx.Err()
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	<-started
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := engine.Cancel(context.Background(), wfID, "parallel"); err != nil {
				t.Errorf("cancel: %v", err)
				return
			}
			snap, err := engine.Snapshot(context.Background(), wfID)
			if err != nil {
				t.Errorf("snapshot: %v", err)
				return
			}
			if snap.Status != WorkflowCancelled {
				t.Errorf("Cancel returned before durable terminal state: %s", snap.Status)
			}
		}()
	}
	wg.Wait()
	waitRun(t, nil, done)
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCancelled {
		t.Fatalf("workflow: %s", wf.Status)
	}
	if wf.Nodes["a"].Status != NodeAborted {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
}

func TestEngineContextCancelDurable(t *testing.T) {
	started := make(chan struct{})
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		close(started)
		<-ctx.Done()
		return NodeOutput{}, ctx.Err()
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx, wfID) }()
	<-started
	cancel()
	err := waitRun(t, nil, done)
	if !errors.Is(err, context.Canceled) {
		wf, _ := engine.Snapshot(context.Background(), wfID)
		events, _ := engine.store.Load(context.Background(), wfID)
		t.Fatalf("Run error = %v, want context.Canceled; status=%s ctx=%v events=%v", err, wf.Status, ctx.Err(), eventTypes(events))
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCancelled || wf.CancelReason != "context_cancelled" {
		t.Fatalf("workflow: %+v", wf)
	}
}

func TestEngineLateResultAfterCancel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		close(started)
		<-release
		return output(t, map[string]any{"late": true}), nil
	})
	store := NewMemoryEventStore()
	engine := NewEngine(store, exec, nil, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	<-started
	if err := engine.Cancel(context.Background(), wfID, "stop"); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitRun(t, nil, done)
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["a"].Status != NodeAborted {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
	events, _ := store.Load(context.Background(), wfID)
	for _, ev := range events {
		if ev.Type == EventNodeCompleted {
			t.Fatal("late result was persisted")
		}
	}
}

func TestEngineSingleOwner(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		close(started)
		<-release
		return NodeOutput{}, nil
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	<-started
	if err := engine.Run(context.Background(), wfID); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run error = %v", err)
	}
	close(release)
	if err := waitRun(t, nil, done); err != nil {
		t.Fatal(err)
	}
}

func TestEngineExtendAndStaleRevision(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if err := engine.Extend(context.Background(), wfID, wf.Revision+10, []NodeSpec{node("b", "a")}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale extend error = %v", err)
	}
	if err := engine.Extend(context.Background(), wfID, wf.Revision, nil); err == nil {
		t.Fatal("empty extend accepted")
	}
	if err := engine.Extend(context.Background(), wfID, wf.Revision, []NodeSpec{node("b", "a")}); err != nil {
		t.Fatal(err)
	}
	wf, _ = engine.Snapshot(context.Background(), wfID)
	if len(wf.Nodes) != 2 || wf.ExtendsUsed != 1 {
		t.Fatalf("view: %+v", wf)
	}
	if err := engine.Extend(context.Background(), wfID, wf.Revision, []NodeSpec{node("c", "b")}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("extend budget error = %v", err)
	}
}

func TestEngineFanOutDoesNotConsumeExtends(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	})
	exp := funcExpander(func(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error) {
		return Extension{Nodes: []NodeSpec{node("cr.p1", from.ID)}, Source: ExtensionSourceFanOut}, nil
	})
	engine := NewEngine(NewMemoryEventStore(), exec, exp, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes:  []NodeSpec{{ID: "plan", Kind: "x", Required: true, Expands: true}},
		Budget: Budget{MaxExtends: 1},
	}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCompleted || wf.ExtendsUsed != 0 {
		t.Fatalf("view: %s extends=%d", wf.Status, wf.ExtendsUsed)
	}
}

func TestEngineTokenBudget(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		mu.Lock()
		calls[in.Node.ID]++
		mu.Unlock()
		return NodeOutput{Data: json.RawMessage(`{}`), Usage: Usage{InputTokens: 10}}, nil
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes:  []NodeSpec{node("a"), node("b", "a")},
		Budget: Budget{MaxTokens: 5},
	}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["b"].Status != NodeFailed || wf.Nodes["b"].Reason != "budget_exhausted" {
		t.Fatalf("b: %+v", wf.Nodes["b"])
	}
	if wf.Status != WorkflowFailed {
		t.Fatalf("workflow: %s", wf.Status)
	}
	if calls["b"] != 0 {
		t.Fatal("b executed despite exhausted budget")
	}
}

func TestEngineDeadline(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		<-ctx.Done()
		return NodeOutput{}, ctx.Err()
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes:  []NodeSpec{node("a")},
		Budget: Budget{Deadline: 40 * time.Millisecond},
	}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatalf("Run = %v", err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCancelled || wf.CancelReason != "deadline" {
		t.Fatalf("workflow: %+v", wf)
	}
	if wf.Nodes["a"].Status != NodeAborted {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
}

func TestEngineRecover(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		mu.Lock()
		calls[in.Node.ID]++
		mu.Unlock()
		return NodeOutput{}, nil
	})
	store2 := NewMemoryEventStore()
	engine2 := NewEngine(store2, exec, nil, Options{})
	wfID2, _ := engine2.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a"), node("b")}}, nil)
	seed := []Event{
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1),
		nodeEvent(t, EventNodeStarted, "a", 1),
		nodeEvent(t, EventNodeQueued, "b", 1),
		nodeEvent(t, EventNodeStarted, "b", 1),
		nodeEvent(t, EventNodeCompleted, "b", 1),
	}
	if _, err := store2.Append(context.Background(), wfID2, 1, seed...); err != nil {
		t.Fatal(err)
	}
	if err := engine2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, _ := engine2.Snapshot(context.Background(), wfID2)
	if recovered.Nodes["a"].Status != NodePending || recovered.Nodes["a"].Attempt != 2 {
		t.Fatalf("a after recover: %+v", recovered.Nodes["a"])
	}
	if recovered.Nodes["b"].Status != NodeCompleted || recovered.Nodes["b"].Attempt != 1 {
		t.Fatalf("b not preserved: %+v", recovered.Nodes["b"])
	}
	if err := engine2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, _ := engine2.Snapshot(context.Background(), wfID2)
	if again.Nodes["a"].Attempt != 2 || again.Nodes["a"].Status != NodePending {
		t.Fatalf("second recover changed pending node: %+v", again.Nodes["a"])
	}
	if err := engine2.Run(context.Background(), wfID2); err != nil {
		t.Fatal(err)
	}
	final, _ := engine2.Snapshot(context.Background(), wfID2)
	if final.Status != WorkflowCompleted {
		t.Fatalf("workflow: %s", final.Status)
	}
	if calls["b"] != 0 || calls["a"] != 1 {
		t.Fatalf("calls after recovery = %v", calls)
	}
}

func containsNodeID(payload json.RawMessage, id string) bool {
	var meta struct {
		NodeID string `json:"node_id"`
	}
	_ = json.Unmarshal(payload, &meta)
	return meta.NodeID == id
}

func eventTypes(events []Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

func TestEngineNotFound(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) { return NodeOutput{}, nil }), Options{})
	if _, err := engine.Snapshot(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
	if err := engine.Run(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run error = %v", err)
	}
	if err := engine.Cancel(context.Background(), "missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestEngineNilDependencies(t *testing.T) {
	engine := NewEngine(nil, nil, nil, Options{})
	if _, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil); err == nil {
		t.Fatal("nil store create accepted")
	}
	if err := engine.Run(context.Background(), "x"); err == nil {
		t.Fatal("nil store run accepted")
	}
	engine2 := NewEngine(NewMemoryEventStore(), nil, nil, Options{})
	if err := engine2.Run(context.Background(), "x"); err == nil {
		t.Fatal("nil executor run accepted")
	}
}

func TestEngineValidationRejectsBadSpec(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) { return NodeOutput{}, nil }), Options{})
	if _, err := engine.Create(context.Background(), GraphSpec{}, nil); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("error = %v", err)
	}
}

func TestEnginePendingCancel(t *testing.T) {
	var called atomic.Bool
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		called.Store(true)
		return NodeOutput{}, nil
	}), Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a"), node("b", "a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Cancel(context.Background(), wfID, "before-start"); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCancelled || wf.Nodes["a"].Status != NodeSkipped || wf.Nodes["b"].Status != NodeSkipped {
		t.Fatalf("view: %+v", wf)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
	if called.Load() {
		t.Fatal("executor ran after cancel")
	}
}

func TestEngineCancelWithCancelledCallerContext(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.Cancel(ctx, wfID, "durable"); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCancelled || wf.CancelReason != "durable" || wf.Nodes["a"].Status != NodeSkipped {
		t.Fatalf("view: %+v", wf)
	}
}

func TestEngineCancelTerminalNoEvents(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.store.Load(context.Background(), wfID)
	if err := engine.Cancel(context.Background(), wfID, "late"); err != nil {
		t.Fatal(err)
	}
	after, _ := engine.store.Load(context.Background(), wfID)
	if len(after) != len(before) {
		t.Fatal("cancel changed terminal workflow")
	}
}

func TestEngineRecoverQueuedPreserved(t *testing.T) {
	var calls atomic.Int64
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		calls.Add(1)
		return NodeOutput{}, nil
	})
	store := NewMemoryEventStore()
	engine := NewEngine(store, exec, nil, Options{Clock: func() time.Time { return testNow }})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if _, err := store.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1),
	); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.store.Load(context.Background(), wfID)
	if err := engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	mid, _ := engine.Snapshot(context.Background(), wfID)
	if mid.Nodes["a"].Status != NodeQueued || mid.Nodes["a"].Attempt != 1 || mid.Nodes["a"].QueuedAt.IsZero() {
		t.Fatalf("queued node changed: %+v", mid.Nodes["a"])
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	final, _ := engine.Snapshot(context.Background(), wfID)
	if final.Status != WorkflowCompleted || calls.Load() != 1 {
		t.Fatalf("final=%s calls=%d", final.Status, calls.Load())
	}
	_ = before
}

func TestEngineRecoverDeadline(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	})
	store := NewMemoryEventStore()
	engine := NewEngine(store, exec, nil, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes:  []NodeSpec{node("a")},
		Budget: Budget{Deadline: 20 * time.Millisecond},
	}, nil)
	if _, err := store.Append(context.Background(), wfID, 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeQueued, "a", 1),
		nodeEvent(t, EventNodeStarted, "a", 1),
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCancelled || wf.CancelReason != "deadline" {
		t.Fatalf("workflow: %+v", wf)
	}
	if wf.Nodes["a"].Status != NodeAborted {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
}

func TestEngineExtendMaxNodes(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes:  []NodeSpec{node("a")},
		Budget: Budget{MaxNodes: 2, MaxExtends: 5},
	}, nil)
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if err := engine.Extend(context.Background(), wfID, wf.Revision, []NodeSpec{node("b"), node("c")}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("node budget error = %v", err)
	}
	if err := engine.Extend(context.Background(), wfID, wf.Revision, []NodeSpec{node("b")}); err != nil {
		t.Fatal(err)
	}
	wf, _ = engine.Snapshot(context.Background(), wfID)
	if err := engine.Extend(context.Background(), wfID, wf.Revision, []NodeSpec{node("c")}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("node budget error = %v", err)
	}
}

func TestEngineExpanderFromNodeMismatch(t *testing.T) {
	exp := funcExpander(func(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error) {
		return Extension{Nodes: []NodeSpec{node("b", from.ID)}, FromNode: "other"}, nil
	})
	engine := NewEngine(NewMemoryEventStore(), funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), exp, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{{ID: "plan", Kind: "x", Required: true, Expands: true}}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["plan"].Status != NodeFailed || wf.Nodes["plan"].Reason != "expand_failed" {
		t.Fatalf("plan: %+v", wf.Nodes["plan"])
	}
}

func TestSnapshotIsolation(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, map[string]string{"k": "v"})
	wf, _ := engine.Snapshot(context.Background(), wfID)
	wf.Meta["k"] = "changed"
	wf.Order[0] = "changed"
	wf.Spec.Nodes[0].Kind = "changed"
	view := wf.Nodes["a"]
	view.Spec.Kind = "changed"
	wf.Nodes["a"] = view
	again, _ := engine.Snapshot(context.Background(), wfID)
	if again.Meta["k"] != "v" || again.Order[0] != "a" || again.Spec.Nodes[0].Kind != "test" || again.Nodes["a"].Spec.Kind != "test" {
		t.Fatalf("snapshot aliased stored view: %+v", again)
	}
	if _, ok := again.Node("a"); !ok {
		t.Fatal("Node lookup failed")
	}
}

func TestEngineExecutorPanic(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		panic("kaboom")
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["a"].Status != NodeFailed || wf.Status != WorkflowFailed {
		t.Fatalf("view: %+v", wf.Nodes["a"])
	}
	if wf.Nodes["a"].Error == "" {
		t.Fatal("panic not reported")
	}
}

func TestEngineNodeTimeout(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		<-ctx.Done()
		return NodeOutput{}, ctx.Err()
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "a", Kind: "x", Required: true, Timeout: 40 * time.Millisecond},
	}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["a"].Status != NodeFailed || wf.Nodes["a"].Reason != "timeout" {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
}

func TestEngineExtendTerminal(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	err := engine.Extend(context.Background(), wfID, wf.Revision, []NodeSpec{node("b")})
	if !errors.Is(err, ErrTerminalViolation) {
		t.Fatalf("error = %v", err)
	}
}

func TestEngineGlobalSemaphoreAcrossWorkflows(t *testing.T) {
	var active, maxActive int64
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		now := atomic.AddInt64(&active, 1)
		for {
			old := atomic.LoadInt64(&maxActive)
			if now <= old || atomic.CompareAndSwapInt64(&maxActive, old, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&active, -1)
		return NodeOutput{}, nil
	})
	engine := fastEngine(exec, Options{MaxConcurrentNodes: 2})
	makeWF := func() string {
		spec := GraphSpec{Nodes: []NodeSpec{node("a"), node("b"), node("c")}}
		id, err := engine.Create(context.Background(), spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id1, id2 := makeWF(), makeWF()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{id1, id2} {
		wg.Add(1)
		go func(wfID string) {
			defer wg.Done()
			errs <- engine.Run(context.Background(), wfID)
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(&maxActive); got > 2 {
		t.Fatalf("global concurrency = %d", got)
	}
	for _, id := range []string{id1, id2} {
		wf, _ := engine.Snapshot(context.Background(), id)
		if wf.Status != WorkflowCompleted {
			t.Fatalf("%s = %s", id, wf.Status)
		}
	}
}

func TestEngineQueueWaitThenRun(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Second})
	engine.sem <- struct{}{}
	go func() {
		time.Sleep(30 * time.Millisecond)
		<-engine.sem
	}()
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow: %s", wf.Status)
	}
}

func TestEngineRecoverSkipsActiveRun(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		close(started)
		<-release
		return NodeOutput{}, nil
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	done := make(chan error, 1)
	go func() { done <- engine.Run(context.Background(), wfID) }()
	<-started
	if err := engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["a"].Status != NodeRunning || wf.Nodes["a"].Attempt != 1 {
		t.Fatalf("active run was recovered: %+v", wf.Nodes["a"])
	}
	close(release)
	if err := waitRun(t, nil, done); err != nil {
		t.Fatal(err)
	}
}

func TestEngineCancelEmptyReasonDefaults(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err := engine.Cancel(context.Background(), wfID, ""); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.CancelReason != "cancelled" {
		t.Fatalf("reason = %q", wf.CancelReason)
	}
}

func TestEngineInvalidExecutorOutput(t *testing.T) {
	engine := fastEngine(funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: []byte(`{`)}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["a"].Status != NodeFailed || wf.Nodes["a"].Reason != "invalid_output" {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
}
