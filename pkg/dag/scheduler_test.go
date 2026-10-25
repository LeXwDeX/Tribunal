package dag

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCascadeStatusTable(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		nodes  []NodeSpec
		status map[string]NodeStatus
		want   string
		stop   bool
	}{
		{
			name:   "required failed",
			nodes:  []NodeSpec{node("a"), node("b", "a")},
			status: map[string]NodeStatus{"a": NodeFailed},
			want:   "unsatisfied",
			stop:   true,
		},
		{
			name:   "required aborted",
			nodes:  []NodeSpec{node("a"), node("b", "a")},
			status: map[string]NodeStatus{"a": NodeAborted},
			want:   "unsatisfied",
			stop:   true,
		},
		{
			name:   "optional failed",
			nodes:  []NodeSpec{{ID: "a", Kind: "x"}, node("b", "a")},
			status: map[string]NodeStatus{"a": NodeFailed},
			stop:   false,
		},
		{
			name:   "all skipped",
			nodes:  []NodeSpec{node("a"), node("b", "a")},
			status: map[string]NodeStatus{"a": NodeSkipped},
			want:   "deps_skipped",
			stop:   true,
		},
		{
			name:   "skipped plus completed",
			nodes:  []NodeSpec{node("a"), node("c"), node("b", "a", "c")},
			status: map[string]NodeStatus{"a": NodeSkipped, "c": NodeCompleted},
			stop:   false,
		},
		{
			name:   "condition false",
			nodes:  []NodeSpec{node("a"), {ID: "b", Kind: "x", DependsOn: []string{"a"}, Condition: "a.output.ok == true"}},
			status: map[string]NodeStatus{"a": NodeCompleted},
			want:   "condition_false",
			stop:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wf := WorkflowView{Nodes: map[string]NodeView{}}
			for _, spec := range tc.nodes {
				status, ok := tc.status[spec.ID]
				if !ok {
					status = NodeCompleted
				}
				output := json.RawMessage(`{"ok":false}`)
				wf.Nodes[spec.ID] = NodeView{ID: spec.ID, Spec: spec, Status: status, Output: output}
			}
			target := tc.nodes[len(tc.nodes)-1]
			reason, stop := cascadeStatus(wf, target)
			if reason != tc.want || stop != tc.stop {
				t.Fatalf("got (%q,%v), want (%q,%v)", reason, stop, tc.want, tc.stop)
			}
		})
	}
	_ = now
}

func TestResolveInputPlaceholders(t *testing.T) {
	wf := WorkflowView{Nodes: map[string]NodeView{
		"ok":      {ID: "ok", Status: NodeCompleted, Output: json.RawMessage(`{"x":{"y":3}}`)},
		"badjson": {ID: "badjson", Status: NodeCompleted, Output: json.RawMessage(`{`)},
		"failed":  {ID: "failed", Status: NodeFailed},
		"aborted": {ID: "aborted", Status: NodeAborted},
		"skipped": {ID: "skipped", Status: NodeSkipped},
		"pending": {ID: "pending", Status: NodePending},
		"empty":   {ID: "empty", Status: NodeCompleted},
	}}
	tests := []struct {
		ref      string
		want     string
		reason   string
		hasValue bool
	}{
		{ref: "ok.output.x", hasValue: true},
		{ref: "ok.output.x.y", hasValue: true},
		{ref: "ok.output.missing", reason: "output_missing"},
		{ref: "badjson.output.x", reason: "output_missing"},
		{ref: "empty.output.x", reason: "output_missing"},
		{ref: "failed.output.x", reason: "dependency_failed"},
		{ref: "aborted.output.x", reason: "dependency_aborted"},
		{ref: "skipped.output.x", reason: "dependency_skipped"},
		{ref: "pending.output.x", reason: "dependency_pending"},
		{ref: "ghost.output.x", reason: "invalid_reference"},
		{ref: "nonsense", reason: "invalid_reference"},
	}
	for _, tc := range tests {
		raw := resolveInput(wf, tc.ref)
		if tc.hasValue {
			if string(raw) != `{"y":3}` && string(raw) != `3` {
				t.Fatalf("%s = %s", tc.ref, raw)
			}
			continue
		}
		var placeholder map[string]any
		if err := json.Unmarshal(raw, &placeholder); err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if placeholder["_unavailable"] != true || placeholder["reason"] != tc.reason {
			t.Fatalf("%s = %v, want reason %s", tc.ref, placeholder, tc.reason)
		}
	}
}

func TestValidateExtensionTable(t *testing.T) {
	base := WorkflowView{
		Spec: GraphSpec{Nodes: []NodeSpec{node("plan"), node("gather", "plan")}},
		Nodes: map[string]NodeView{
			"plan":   {ID: "plan", Spec: node("plan"), Status: NodeCompleted},
			"gather": {ID: "gather", Spec: node("gather", "plan"), Status: NodePending},
		},
		ExtendsUsed: 0,
	}
	base.Spec.Budget = NormalizeBudget(base.Spec.Budget)
	tests := []struct {
		name string
		ext  *Extension
		err  error
	}{
		{name: "fan out", ext: &Extension{Nodes: []NodeSpec{node("cr.p1", "plan")}, DepPatches: []DepPatch{{Node: "gather", Add: []string{"cr.p1"}}}}},
		{name: "explicit extend", ext: &Extension{Nodes: []NodeSpec{node("cr.p1", "plan")}, Source: ExtensionSourceExtend}, err: nil},
		{name: "unknown source", ext: &Extension{Source: "nope"}, err: ErrInvalidGraph},
		{name: "patch unknown", ext: &Extension{DepPatches: []DepPatch{{Node: "ghost", Add: []string{"plan"}}}}, err: ErrInvalidGraph},
		{name: "duplicate", ext: &Extension{Nodes: []NodeSpec{node("plan")}}, err: ErrInvalidGraph},
		{name: "missing dep", ext: &Extension{Nodes: []NodeSpec{node("x", "ghost")}}, err: ErrInvalidGraph},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := validateExtension(base, tc.ext)
			if tc.err == nil {
				if err != nil {
					t.Fatal(err)
				}
				if payload.Source == "" {
					t.Fatal("source not normalized")
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	// non-pending patch target
	queued := cloneView(base)
	view := queued.Nodes["gather"]
	view.Status = NodeQueued
	queued.Nodes["gather"] = view
	if _, err := validateExtension(queued, &Extension{DepPatches: []DepPatch{{Node: "gather", Add: []string{"plan"}}}}); err == nil {
		t.Fatal("queued patch target accepted")
	}
	// exhausted extend budget
	exhausted := cloneView(base)
	exhausted.ExtendsUsed = exhausted.Spec.Budget.MaxExtends
	if _, err := validateExtension(exhausted, &Extension{Nodes: []NodeSpec{node("x", "plan")}, Source: ExtensionSourceExtend}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v", err)
	}
	// max node budget
	small := cloneView(base)
	small.Spec.Budget.MaxNodes = 2
	if _, err := validateExtension(small, &Extension{Nodes: []NodeSpec{node("x", "plan")}}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanScheduleQueueTimeout(t *testing.T) {
	engine := NewEngine(NewMemoryEventStore(), nil, nil, Options{DefaultNodeTimeout: time.Minute})
	wf := WorkflowView{
		Spec: GraphSpec{Nodes: []NodeSpec{node("a")}},
		Nodes: map[string]NodeView{
			"a": {ID: "a", Spec: node("a"), Status: NodeQueued, QueuedAt: testNow, Attempt: 1},
		},
		Order: []string{"a"},
	}
	events, starts := engine.planSchedule(wf, testNow.Add(2*time.Minute))
	if len(starts) != 0 || len(events) != 1 || events[0].Type != EventNodeFailed {
		t.Fatalf("events=%v starts=%v", events, starts)
	}
	events, starts = engine.planSchedule(wf, testNow)
	if len(events) != 0 || len(starts) != 1 {
		t.Fatalf("events=%v starts=%v", events, starts)
	}
}

func TestSchedulerHelpers(t *testing.T) {
	wf := WorkflowView{
		Spec:  GraphSpec{Nodes: []NodeSpec{node("a"), node("b", "a")}},
		Nodes: map[string]NodeView{"a": {ID: "a", Spec: node("a"), Status: NodeCompleted}, "b": {ID: "b", Spec: node("b", "a"), Status: NodeRunning}},
	}
	if dependenciesTerminal(wf, node("b", "a")) != true {
		t.Fatal("terminal deps")
	}
	if dependenciesTerminal(wf, node("c", "missing")) {
		t.Fatal("missing dep")
	}
	if allTerminal(wf) {
		t.Fatal("running node")
	}
	wf.Nodes["b"] = NodeView{ID: "b", Spec: node("b"), Status: NodeCompleted}
	if !allTerminal(wf) {
		t.Fatal("all terminal")
	}
}

func TestBuildNodeInputCopies(t *testing.T) {
	wf := WorkflowView{
		ID:   "wf",
		Meta: map[string]string{"m": "v"},
		Nodes: map[string]NodeView{
			"a": {ID: "a", Status: NodeCompleted, Output: json.RawMessage(`{"v":1}`)},
		},
	}
	spec := NodeSpec{ID: "b", Kind: "x", DependsOn: []string{"a"}, InputMapping: map[string]string{"in": "a.output.v"}}
	in := buildNodeInput(wf, spec, 2)
	if in.WorkflowID != "wf" || in.Attempt != 2 || string(in.Inputs["in"]) != "1" || in.Meta["m"] != "v" {
		t.Fatalf("input: %+v", in)
	}
	in.Node.DependsOn[0] = "changed"
	if spec.DependsOn[0] != "a" {
		t.Fatal("spec was not copied")
	}
	in.Meta["m"] = "changed"
	if wf.Meta["m"] != "v" {
		t.Fatal("meta was not copied")
	}
}

func TestTokenBudgetEventsOptional(t *testing.T) {
	wf := WorkflowView{
		Spec:  GraphSpec{Nodes: []NodeSpec{node("a"), {ID: "b", Kind: "x", DependsOn: []string{"a"}}}},
		Nodes: map[string]NodeView{"a": {ID: "a", Spec: node("a"), Status: NodeCompleted}, "b": {ID: "b", Spec: NodeSpec{ID: "b", Kind: "x"}, Status: NodePending}},
		Order: []string{"a", "b"},
	}
	events := tokenBudgetEvents(wf, testNow)
	if len(events) != 1 || events[0].Type != EventNodeSkipped {
		t.Fatalf("events: %v", events)
	}
}

func TestEngineOptionalTokenBudgetCompletes(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: json.RawMessage(`{}`), Usage: Usage{OutputTokens: 9}}, nil
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{
			node("a"),
			{ID: "b", Kind: "x", DependsOn: []string{"a"}},
		},
		Budget: Budget{MaxTokens: 1},
	}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "budget_exhausted" {
		t.Fatalf("b: %+v", wf.Nodes["b"])
	}
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow: %s", wf.Status)
	}
}

func TestTokenBudgetEventsSkipTokenFreeNodes(t *testing.T) {
	wf := WorkflowView{
		Spec: GraphSpec{Nodes: []NodeSpec{
			node("a"),
			{ID: "b", Kind: "x", DependsOn: []string{"a"}, Required: true, TokenFree: true},
		}},
		Nodes: map[string]NodeView{
			"a": {ID: "a", Spec: node("a"), Status: NodeCompleted},
			"b": {ID: "b", Spec: NodeSpec{ID: "b", Kind: "x", Required: true, TokenFree: true}, Status: NodePending},
		},
		Order: []string{"a", "b"},
	}
	if events := tokenBudgetEvents(wf, testNow); len(events) != 0 {
		t.Fatalf("token-free node received budget events: %v", events)
	}
}

func TestEngineTokenFreeRequiredNodeCompletesAfterBudget(t *testing.T) {
	var calls int
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		if in.Node.ID == "a" {
			return NodeOutput{Data: json.RawMessage(`{}`), Usage: Usage{InputTokens: 10}}, nil
		}
		calls++
		return NodeOutput{Data: json.RawMessage(`{"settled":true}`)}, nil
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{
			node("a"),
			{ID: "b", Kind: "x", DependsOn: []string{"a"}, Required: true, TokenFree: true},
		},
		Budget: Budget{MaxTokens: 5},
	}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow = %s (%s)", wf.Status, wf.Error)
	}
	if wf.Nodes["b"].Status != NodeCompleted || calls != 1 {
		t.Fatalf("b = %+v calls=%d", wf.Nodes["b"], calls)
	}
}

func TestEngineTokenBudgetCancelsRunningNonTokenFreeNode(t *testing.T) {
	startedB := make(chan struct{})
	canceledB := make(chan struct{})
	var once sync.Once
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		switch in.Node.ID {
		case "a":
			<-startedB
			return NodeOutput{Data: json.RawMessage(`{}`), Usage: Usage{InputTokens: 10}}, nil
		case "b":
			once.Do(func() { close(startedB) })
			select {
			case <-ctx.Done():
				close(canceledB)
				return NodeOutput{}, ctx.Err()
			case <-time.After(300 * time.Millisecond):
				return NodeOutput{Data: json.RawMessage(`{"late":true}`)}, nil
			}
		case "c":
			// Keep the workflow alive after the budget event so only a per-node
			// cancel can stop the running proposal.
			select {
			case <-canceledB:
			case <-time.After(time.Second):
			}
			return NodeOutput{Data: json.RawMessage(`{"settled":true}`)}, nil
		}
		return NodeOutput{}, errors.New("unexpected node")
	})
	engine := fastEngine(exec, Options{MaxConcurrentNodes: 3})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{
			node("a"),
			{ID: "b", Kind: "x"},
			{ID: "c", Kind: "x", DependsOn: []string{"a"}, Required: true, TokenFree: true},
		},
		Budget: Budget{MaxTokens: 5},
	}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceledB:
	default:
		t.Fatal("running non-token-free node was not canceled after budget exhaustion")
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "budget_exhausted" {
		t.Fatalf("b = %+v, want skipped budget_exhausted", wf.Nodes["b"])
	}
	if wf.Nodes["c"].Status != NodeCompleted {
		t.Fatalf("c = %+v, want completed", wf.Nodes["c"])
	}
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow = %s", wf.Status)
	}
}

func TestEngineQueuedNodeDoesNotStartAfterBudgetExhaustion(t *testing.T) {
	calls := 0
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		calls++
		return NodeOutput{Data: json.RawMessage(`{}`)}, nil
	})
	engine := fastEngine(exec, Options{Clock: func() time.Time { return testNow }})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes:  []NodeSpec{node("a"), {ID: "b", Kind: "x"}},
		Budget: Budget{MaxTokens: 5},
	}, nil)
	wf, _ := engine.Snapshot(context.Background(), wfID)
	events := []struct {
		typ     string
		payload any
	}{
		{EventNodeQueued, NodeQueuedPayload{NodeID: "a", Attempt: 1, Reason: "ready"}},
		{EventNodeStarted, NodeStartedPayload{NodeID: "a", Attempt: 1}},
		{EventNodeCompleted, NodeCompletedPayload{NodeID: "a", Attempt: 1, Output: json.RawMessage(`{}`), Usage: Usage{InputTokens: 10}}},
		{EventNodeQueued, NodeQueuedPayload{NodeID: "b", Attempt: 1, Reason: "ready"}},
	}
	batch := make([]Event, 0, len(events))
	for _, item := range events {
		ev, err := makeEvent(item.typ, item.payload, testNow)
		if err != nil {
			t.Fatal(err)
		}
		batch = append(batch, ev)
	}
	if _, err := engine.store.Append(context.Background(), wfID, wf.Revision, batch...); err != nil {
		t.Fatal(err)
	}
	handle := &runHandle{
		active:      map[string]bool{"b": true},
		nodeCancels: map[string]context.CancelCauseFunc{},
		wake:        make(chan struct{}, 1),
	}
	engine.runNode(context.Background(), handle, wfID, "b")
	if calls != 0 {
		t.Fatalf("queued node b entered the executor %d times after budget exhaustion", calls)
	}
	wf, _ = engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "budget_exhausted" {
		t.Fatalf("b = %+v, want skipped budget_exhausted", wf.Nodes["b"])
	}
}

// metadataExecutor is a NodeMetadataProvider used to prove the scheduler
// stamps node.started with the executor's selected provider and model.
type metadataExecutor struct {
	funcExecutor
	provider string
	model    string
	codeKind string
}

func (m metadataExecutor) NodeMetadata(in NodeInput) NodeMetadata {
	if m.codeKind != "" && in.Node.Kind == m.codeKind {
		return NodeMetadata{}
	}
	return NodeMetadata{Provider: m.provider, Model: m.model}
}

func metadataPayloads(t *testing.T, store *MemoryEventStore, wfID string) map[string]NodeStartedPayload {
	t.Helper()
	events, err := store.Load(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]NodeStartedPayload)
	for _, ev := range events {
		if ev.Type != EventNodeStarted {
			continue
		}
		var payload NodeStartedPayload
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decode node.started: %v", err)
		}
		out[payload.NodeID] = payload
	}
	return out
}

// TestEnginePersistsStartedMetadata covers success and failure attribution.
// The code node proves code-mode work stays unattributed.
func TestEnginePersistsStartedMetadata(t *testing.T) {
	exec := metadataExecutor{
		funcExecutor: func(ctx context.Context, in NodeInput) (NodeOutput, error) {
			if in.Node.ID == "fail" {
				return NodeOutput{}, errors.New("boom")
			}
			return NodeOutput{Data: json.RawMessage(`{}`)}, nil
		},
		provider: "fake",
		model:    "fake-model",
		codeKind: "code",
	}
	engine := fastEngine(exec, Options{MaxConcurrentNodes: 3})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		node("ok"),
		node("fail"),
		{ID: "code", Kind: "code", Required: true},
	}}, nil)
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
	for _, id := range []string{"ok", "fail"} {
		if wf.Nodes[id].Provider != "fake" || wf.Nodes[id].Model != "fake-model" {
			t.Fatalf("%s attribution = (%q,%q), want (fake,fake-model) status=%s",
				id, wf.Nodes[id].Provider, wf.Nodes[id].Model, wf.Nodes[id].Status)
		}
	}
	if wf.Nodes["code"].Provider != "" || wf.Nodes["code"].Model != "" {
		t.Fatalf("code node attribution = (%q,%q), want empty",
			wf.Nodes["code"].Provider, wf.Nodes["code"].Model)
	}
	payloads := metadataPayloads(t, engine.store.(*MemoryEventStore), wfID)
	for _, id := range []string{"ok", "fail"} {
		if payloads[id].Provider != "fake" || payloads[id].Model != "fake-model" {
			t.Fatalf("%s node.started payload = %+v", id, payloads[id])
		}
	}
	if payloads["code"].Provider != "" || payloads["code"].Model != "" {
		t.Fatalf("code node.started payload = %+v, want empty", payloads["code"])
	}
}

// TestEnginePersistsStartedMetadataForExpandedNodes proves dynamically added
// nodes use the same attribution path as initial nodes.
func TestEnginePersistsStartedMetadataForExpandedNodes(t *testing.T) {
	exec := metadataExecutor{
		funcExecutor: func(ctx context.Context, in NodeInput) (NodeOutput, error) {
			return NodeOutput{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
		provider: "fake",
		model:    "fake-model",
	}
	exp := funcExpander(func(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error) {
		return Extension{Nodes: []NodeSpec{{ID: "dyn", Kind: "model", DependsOn: []string{from.ID}}}}, nil
	})
	store := NewMemoryEventStore()
	engine := NewEngine(store, exec, exp, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "plan", Kind: "test", Required: true, Expands: true},
	}}, nil)
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
	node, ok := wf.Nodes["dyn"]
	if !ok || node.Status != NodeCompleted {
		t.Fatalf("dyn = %+v", node)
	}
	if node.Provider != "fake" || node.Model != "fake-model" {
		t.Fatalf("dyn attribution = (%q,%q), want (fake,fake-model)", node.Provider, node.Model)
	}
	if payloads := metadataPayloads(t, store, wfID); payloads["dyn"].Provider != "fake" || payloads["dyn"].Model != "fake-model" {
		t.Fatalf("dyn node.started payload = %+v", payloads["dyn"])
	}
}

// panickingMetadataExecutor has a working Execute but a metadata lookup that
// panics. Attribution is advisory and must never fail or crash the node.
type panickingMetadataExecutor struct {
	funcExecutor
}

func (panickingMetadataExecutor) NodeMetadata(NodeInput) NodeMetadata {
	panic("metadata lookup failed")
}

// TestNodeMetadataAdvisoryPanicFallsBackToEmpty checks the helper in isolation
// so a regression fails the test instead of crashing the test process.
func TestNodeMetadataAdvisoryPanicFallsBackToEmpty(t *testing.T) {
	exec := panickingMetadataExecutor{funcExecutor: func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: json.RawMessage(`{}`)}, nil
	}}
	var got NodeMetadata
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("nodeMetadata panic escaped: %v", recovered)
			}
		}()
		got = nodeMetadata(exec, NodeInput{Node: NodeSpec{ID: "a", Kind: "x"}})
	}()
	if got != (NodeMetadata{}) {
		t.Fatalf("metadata = %+v, want empty", got)
	}
}

// TestEngineMetadataPanicLeavesNodeUnattributed drives the real scheduler: a
// panicking metadata lookup must not escape runNode, and Execute must still
// complete the node with no attribution.
func TestEngineMetadataPanicLeavesNodeUnattributed(t *testing.T) {
	exec := panickingMetadataExecutor{funcExecutor: func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: json.RawMessage(`{"ok":true}`)}, nil
	}}
	store := NewMemoryEventStore()
	engine := NewEngine(store, exec, nil, Options{})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{node("a")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wf, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != WorkflowCompleted || wf.Nodes["a"].Status != NodeCompleted {
		t.Fatalf("workflow = %s node = %+v, want completed", wf.Status, wf.Nodes["a"])
	}
	if wf.Nodes["a"].Provider != "" || wf.Nodes["a"].Model != "" {
		t.Fatalf("node attribution = (%q,%q), want empty", wf.Nodes["a"].Provider, wf.Nodes["a"].Model)
	}
	payloads := metadataPayloads(t, store, wfID)
	if payloads["a"].Provider != "" || payloads["a"].Model != "" {
		t.Fatalf("node.started payload = %+v, want empty attribution", payloads["a"])
	}
}
