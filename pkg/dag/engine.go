package dag

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxCommitAttempts  = 4
	defaultNodeTimeout = 10 * time.Minute
)

// staleRetry retries an optimistic commit while the store revision advances.
// A newer revision means another writer made progress, so the retry has no
// fixed attempt limit. Repeated stale results at an unchanged revision bound
// the loop and surface a real failure; context cancellation bounds the
// progress case, and cancelDurable adds a write deadline.
type staleRetry struct {
	fenced    int64
	pending   bool
	unchanged int
}

func (r *staleRetry) mark(expect int64) {
	r.fenced = expect
	r.pending = true
}

func (r *staleRetry) observe(rev int64) error {
	if !r.pending {
		return nil
	}
	r.pending = false
	if rev > r.fenced {
		r.unchanged = 0
		return nil
	}
	r.unchanged++
	if r.unchanged >= maxCommitAttempts {
		return fmt.Errorf("%w: revision %d did not advance after %d stale commits", ErrStaleRevision, rev, r.unchanged)
	}
	return nil
}

func (r *staleRetry) reset() {
	r.fenced = 0
	r.pending = false
	r.unchanged = 0
}

// Engine drives workflows. One Engine process owns at most one Run per
// workflow; the event store revision fences every write.
type Engine struct {
	store EventStore
	exec  NodeExecutor
	exp   Expander
	opts  Options
	clock func() time.Time
	sem   chan struct{}

	mu   sync.Mutex
	runs map[string]*runHandle
}

// errTokenBudget identifies a per-node cancellation caused by the workflow
// token budget. It does not cancel the workflow or token-free nodes.
var errTokenBudget = errors.New("dag: token budget exhausted")

type runHandle struct {
	mu          sync.Mutex
	cancel      context.CancelFunc
	nodeCancels map[string]context.CancelCauseFunc
	active      map[string]bool
	wake        chan struct{}
	err         error
}

func (h *runHandle) setErr(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	if h.err == nil {
		h.err = err
	}
	h.mu.Unlock()
}

func (h *runHandle) getErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *runHandle) setCancel(cancel context.CancelFunc) {
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()
}

func (h *runHandle) stop() {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *runHandle) setNodeCancel(id string, cancel context.CancelCauseFunc) {
	h.mu.Lock()
	if h.nodeCancels == nil {
		h.nodeCancels = make(map[string]context.CancelCauseFunc)
	}
	h.nodeCancels[id] = cancel
	h.mu.Unlock()
}

func (h *runHandle) clearNodeCancel(id string) {
	h.mu.Lock()
	delete(h.nodeCancels, id)
	h.mu.Unlock()
}

// cancelTokenBudgetNodes stops running nodes that consume model tokens. The
// workflow and token-free settlement nodes keep running.
func (h *runHandle) cancelTokenBudgetNodes(wf WorkflowView) {
	h.mu.Lock()
	cancels := make([]context.CancelCauseFunc, 0, len(h.nodeCancels))
	for id, view := range wf.Nodes {
		if view.Status != NodeRunning || view.Spec.TokenFree {
			continue
		}
		if cancel := h.nodeCancels[id]; cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel(errTokenBudget)
	}
}

// NewEngine builds an engine. A nil store or executor is reported by the calls
// that need it; the constructor itself does not panic.
func NewEngine(es EventStore, exec NodeExecutor, exp Expander, opts Options) *Engine {
	if opts.MaxConcurrentNodes <= 0 {
		opts.MaxConcurrentNodes = 8
	}
	if opts.DefaultNodeTimeout <= 0 {
		opts.DefaultNodeTimeout = defaultNodeTimeout
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Engine{
		store: es,
		exec:  exec,
		exp:   exp,
		opts:  opts,
		clock: clock,
		sem:   make(chan struct{}, opts.MaxConcurrentNodes),
		runs:  make(map[string]*runHandle),
	}
}

// Create validates and stores a new workflow in pending state.
func (e *Engine) Create(ctx context.Context, spec GraphSpec, meta map[string]string) (string, error) {
	if e.store == nil {
		return "", errors.New("dag: nil EventStore")
	}
	spec = cloneGraph(spec)
	spec.Budget = NormalizeBudget(spec.Budget)
	if err := ValidateGraph(spec); err != nil {
		return "", err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("dag: create workflow id: %w", err)
	}
	wfID := id.String()
	now := e.clock().UTC()
	ev, err := makeEvent(EventWorkflowCreated, WorkflowCreatedPayload{
		WorkflowID: wfID,
		Spec:       spec,
		Meta:       cloneStrings(meta),
	}, now)
	if err != nil {
		return "", err
	}
	if _, err := e.store.Append(ctx, wfID, 0, ev); err != nil {
		return "", err
	}
	return wfID, nil
}

// Snapshot returns the current read model.
func (e *Engine) Snapshot(ctx context.Context, wfID string) (WorkflowView, error) {
	if e.store == nil {
		return WorkflowView{}, errors.New("dag: nil EventStore")
	}
	wf, err := e.loadView(ctx, wfID)
	if err != nil {
		return WorkflowView{}, err
	}
	return cloneView(wf), nil
}

// Extend appends caller-supplied nodes and consumes one MaxExtends slot.
// expectRev fences the write against a stale caller view.
func (e *Engine) Extend(ctx context.Context, wfID string, expectRev int64, nodes []NodeSpec) error {
	if e.store == nil {
		return errors.New("dag: nil EventStore")
	}
	if len(nodes) == 0 {
		return &GraphError{Field: "nodes", Problem: "extend requires at least one node"}
	}
	wf, err := e.loadView(ctx, wfID)
	if err != nil {
		return err
	}
	if wf.Terminal() {
		return &TransitionError{Entity: "workflow", From: string(wf.Status), To: string(wf.Status), Terminal: true}
	}
	if wf.Revision != expectRev {
		return fmt.Errorf("%w: workflow %s has revision %d, extend expects %d", ErrStaleRevision, wfID, wf.Revision, expectRev)
	}
	if wf.ExtendsUsed >= wf.Spec.Budget.MaxExtends {
		return fmt.Errorf("%w: max_extends %d reached", ErrBudgetExceeded, wf.Spec.Budget.MaxExtends)
	}
	candidate := cloneGraph(wf.Spec)
	candidate.Nodes = append(candidate.Nodes, cloneSpecs(nodes)...)
	if err := ValidateGraph(candidate); err != nil {
		return err
	}
	ev, err := makeEvent(EventWorkflowExtended, WorkflowExtendedPayload{
		Nodes:  cloneSpecs(nodes),
		Source: ExtensionSourceExtend,
	}, e.clock())
	if err != nil {
		return err
	}
	if _, err := e.store.Append(ctx, wfID, expectRev, ev); err != nil {
		return err
	}
	e.signal(wfID)
	return nil
}

// Cancel durably aborts running nodes, skips pending and queued nodes, and
// marks the workflow cancelled. It cancels live executor contexts first. The
// durable write does not inherit caller cancellation.
func (e *Engine) Cancel(ctx context.Context, wfID, reason string) error {
	e.cancelRun(wfID)
	return e.cancelDurable(ctx, wfID, reason)
}

func (e *Engine) cancelDurable(ctx context.Context, wfID, reason string) error {
	if e.store == nil {
		return errors.New("dag: nil EventStore")
	}
	if reason == "" {
		reason = "cancelled"
	}
	// Cancellation must be durable even when the caller's context is already
	// cancelled or is cancelled while the events are being appended.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	var retry staleRetry
	for {
		wf, err := e.loadView(writeCtx, wfID)
		if err != nil {
			return err
		}
		if wf.Terminal() {
			return nil
		}
		now := e.clock().UTC()
		events := make([]Event, 0, len(wf.Nodes)+1)
		for _, id := range wf.Order {
			view := wf.Nodes[id]
			switch view.Status {
			case NodeRunning:
				ev, err := makeEvent(EventNodeAborted, map[string]any{
					"node_id": id,
					"attempt": view.Attempt,
					"reason":  reason,
				}, now)
				if err != nil {
					return err
				}
				events = append(events, ev)
			case NodePending, NodeQueued:
				ev, err := makeEvent(EventNodeSkipped, map[string]any{
					"node_id": id,
					"attempt": view.Attempt,
					"reason":  "workflow_cancelled",
				}, now)
				if err != nil {
					return err
				}
				events = append(events, ev)
			}
		}
		ev, err := makeEvent(EventWorkflowCancelled, WorkflowTerminalPayload{Reason: reason}, now)
		if err != nil {
			return err
		}
		events = append(events, ev)
		if err := retry.observe(wf.Revision); err != nil {
			return err
		}
		if _, err := e.store.Append(writeCtx, wfID, wf.Revision, events...); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				retry.mark(wf.Revision)
				continue
			}
			return err
		}
		e.signal(wfID)
		return nil
	}
}

// Recover prepares unfinished workflows for a later Run. Running nodes restart
// with attempt+1; completed nodes are preserved; queued nodes stay queued.
func (e *Engine) Recover(ctx context.Context) error {
	if e.store == nil {
		return errors.New("dag: nil EventStore")
	}
	ids, err := e.store.ListUnfinished(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if e.isRunning(id) {
			continue
		}
		if err := e.recoverOne(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("dag: recover %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) recoverOne(ctx context.Context, wfID string) error {
	var retry staleRetry
	for {
		wf, err := e.loadView(ctx, wfID)
		if err != nil {
			return err
		}
		if wf.Terminal() {
			return nil
		}
		now := e.clock().UTC()
		if !wf.Deadline.IsZero() && !now.Before(wf.Deadline) {
			return e.cancelDurable(ctx, wfID, "deadline")
		}
		var events []Event
		for _, id := range wf.Order {
			view := wf.Nodes[id]
			if view.Status != NodeRunning {
				continue
			}
			nextAttempt := view.Attempt + 1
			ev, err := makeEvent(EventNodeRestarted, NodeRestartedPayload{
				NodeID:      id,
				Attempt:     nextAttempt,
				NextAttempt: nextAttempt,
				Reason:      "recovered",
			}, now)
			if err != nil {
				return err
			}
			events = append(events, ev)
		}
		if len(events) == 0 {
			return nil
		}
		if err := retry.observe(wf.Revision); err != nil {
			return err
		}
		if _, err := e.store.Append(ctx, wfID, wf.Revision, events...); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				retry.mark(wf.Revision)
				continue
			}
			return err
		}
		return nil
	}
}

func (e *Engine) loadView(ctx context.Context, wfID string) (WorkflowView, error) {
	events, err := e.store.Load(ctx, wfID)
	if err != nil {
		return WorkflowView{}, err
	}
	wf, err := Project(events)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return WorkflowView{}, fmt.Errorf("%w: %s", ErrNotFound, wfID)
		}
		return WorkflowView{}, fmt.Errorf("dag: project %s: %w", wfID, err)
	}
	if wf.ID == "" {
		wf.ID = wfID
	}
	return wf, nil
}

func (e *Engine) registerRun(wfID string) (*runHandle, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.runs[wfID]; exists {
		return nil, ErrAlreadyRunning
	}
	handle := &runHandle{
		active:      make(map[string]bool),
		nodeCancels: make(map[string]context.CancelCauseFunc),
		wake:        make(chan struct{}, 1),
	}
	e.runs[wfID] = handle
	return handle, nil
}

func (e *Engine) unregisterRun(wfID string) {
	e.mu.Lock()
	delete(e.runs, wfID)
	e.mu.Unlock()
}

func (e *Engine) isRunning(wfID string) bool {
	e.mu.Lock()
	_, ok := e.runs[wfID]
	e.mu.Unlock()
	return ok
}

func (e *Engine) cancelRun(wfID string) {
	e.mu.Lock()
	handle := e.runs[wfID]
	e.mu.Unlock()
	if handle != nil {
		handle.stop()
	}
}

// signal wakes the workflow's Run loop. A missing or full channel is fine:
// missing means no local owner, full means a wakeup is already pending.
func (e *Engine) signal(wfID string) {
	e.mu.Lock()
	handle := e.runs[wfID]
	e.mu.Unlock()
	if handle == nil {
		return
	}
	select {
	case handle.wake <- struct{}{}:
	default:
	}
}

func cloneView(wf WorkflowView) WorkflowView {
	out := wf
	out.Meta = cloneStrings(wf.Meta)
	out.Spec = cloneGraph(wf.Spec)
	out.Order = append([]string(nil), wf.Order...)
	out.Nodes = make(map[string]NodeView, len(wf.Nodes))
	for id, view := range wf.Nodes {
		view.Spec = cloneSpec(view.Spec)
		view.Meta = cloneStrings(view.Meta)
		view.Output = rawCopy(view.Output)
		out.Nodes[id] = view
	}
	return out
}
