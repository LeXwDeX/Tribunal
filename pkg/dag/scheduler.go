package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Run drives a workflow until it is terminal. Only one Run per workflow is
// allowed in this Engine. A standalone Cancel or a parent context cancellation
// aborts live executors and durably cancels the workflow.
func (e *Engine) Run(ctx context.Context, wfID string) error {
	if e.store == nil {
		return errors.New("dag: nil EventStore")
	}
	if e.exec == nil {
		return errors.New("dag: nil NodeExecutor")
	}
	handle, err := e.registerRun(wfID)
	if err != nil {
		return err
	}
	runCtx, runCancel := context.WithCancel(ctx)
	handle.setCancel(runCancel)
	var wg sync.WaitGroup
	defer func() {
		// A cancelled parent context must still leave a durable cancelled
		// workflow, even if Run stopped before its loop reached the check below.
		if ctx.Err() != nil && runCtx.Err() != nil {
			_ = e.cancelDurable(runCtx, wfID, "context_cancelled")
		}
		runCancel()
		wg.Wait()
		e.unregisterRun(wfID)
	}()

	if err := e.ensureStarted(runCtx, wfID); err != nil {
		return err
	}

	var scheduleRetry staleRetry
	for {
		if err := handle.getErr(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			cancelErr := e.cancelDurable(runCtx, wfID, "context_cancelled")
			return errors.Join(err, cancelErr)
		}
		if err := runCtx.Err(); err != nil {
			return err
		}
		wf, err := e.loadView(runCtx, wfID)
		if err != nil {
			return err
		}
		if wf.Terminal() {
			return nil
		}
		now := e.clock().UTC()
		if !wf.Deadline.IsZero() && !now.Before(wf.Deadline) {
			return e.cancelDurable(runCtx, wfID, "deadline")
		}
		if err := scheduleRetry.observe(wf.Revision); err != nil {
			return err
		}

		if wf.Spec.Budget.MaxTokens > 0 && wf.Usage.totalTokens() >= wf.Spec.Budget.MaxTokens {
			handle.cancelTokenBudgetNodes(wf)
			events := tokenBudgetEvents(wf, now)
			if len(events) > 0 {
				if err := e.appendBatch(runCtx, wfID, wf.Revision, events); err != nil {
					if errors.Is(err, ErrStaleRevision) {
						scheduleRetry.mark(wf.Revision)
						continue
					}
					return err
				}
				scheduleRetry.reset()
				continue
			}
		}

		events, starts := e.planSchedule(wf, now)
		if len(events) > 0 {
			if err := e.appendBatch(runCtx, wfID, wf.Revision, events); err != nil {
				if errors.Is(err, ErrStaleRevision) {
					scheduleRetry.mark(wf.Revision)
					continue
				}
				return err
			}
			scheduleRetry.reset()
			continue
		}

		launched := false
		for _, id := range starts {
			if !handle.claimNode(id) {
				continue
			}
			launched = true
			wg.Add(1)
			go func(nodeID string) {
				defer wg.Done()
				e.runNode(runCtx, handle, wfID, nodeID)
			}(id)
		}
		if launched {
			scheduleRetry.reset()
			continue
		}

		if allTerminal(wf) {
			// finalize can reload after a concurrent Extend added nodes; a nil
			// result then means the workflow is no longer all-terminal.
			if err := e.finalize(runCtx, wfID); err != nil {
				return err
			}
			continue
		}
		if handle.activeCount() == 0 {
			// A worker may have just committed and released its slot between
			// this snapshot and the check. Reload once before declaring a stall.
			if err := ctx.Err(); err != nil {
				cancelErr := e.cancelDurable(runCtx, wfID, "context_cancelled")
				return errors.Join(err, cancelErr)
			}
			if err := runCtx.Err(); err != nil {
				return err
			}
			latest, loadErr := e.loadView(runCtx, wfID)
			if loadErr != nil {
				if runCtx.Err() != nil {
					return runCtx.Err()
				}
				return loadErr
			}
			if latest.Terminal() {
				return nil
			}
			if latest.Revision != wf.Revision || handle.activeCount() > 0 {
				continue
			}
			if err := handle.getErr(); err != nil {
				return err
			}
			return fmt.Errorf("dag: workflow %s has no runnable node and no active worker", wfID)
		}

		e.wait(runCtx, handle, wf.Deadline)
	}
}

func (e *Engine) ensureStarted(ctx context.Context, wfID string) error {
	var retry staleRetry
	for {
		wf, err := e.loadView(ctx, wfID)
		if err != nil {
			return err
		}
		switch wf.Status {
		case WorkflowRunning:
			return nil
		case WorkflowPending:
			if err := retry.observe(wf.Revision); err != nil {
				return err
			}
			ev, err := makeEvent(EventWorkflowStarted, struct{}{}, e.clock())
			if err != nil {
				return err
			}
			if _, err := e.store.Append(ctx, wfID, wf.Revision, ev); err != nil {
				if errors.Is(err, ErrStaleRevision) {
					retry.mark(wf.Revision)
					continue
				}
				return err
			}
			e.signal(wfID)
			return nil
		default:
			return nil
		}
	}
}

func (e *Engine) wait(ctx context.Context, handle *runHandle, deadline time.Time) {
	var timer *time.Timer
	var timerC <-chan time.Time
	if !deadline.IsZero() {
		remaining := deadline.Sub(e.clock())
		if remaining <= 0 {
			return
		}
		timer = time.NewTimer(remaining)
		timerC = timer.C
	}
	select {
	case <-handle.wake:
	case <-ctx.Done():
	case <-timerC:
	}
	if timer != nil {
		timer.Stop()
	}
}

func (e *Engine) appendBatch(ctx context.Context, wfID string, expect int64, events []Event) error {
	_, err := e.store.Append(ctx, wfID, expect, events...)
	return err
}

// planSchedule computes cascade transitions and queue events for nodes whose
// dependencies are all terminal. Queued nodes are returned as startable.
func (e *Engine) planSchedule(wf WorkflowView, now time.Time) ([]Event, []string) {
	var (
		events []Event
		starts []string
	)
	for _, id := range wf.Order {
		view := wf.Nodes[id]
		if view.Status != NodePending && view.Status != NodeQueued {
			continue
		}
		if !dependenciesTerminal(wf, view.Spec) {
			continue
		}
		if view.Status == NodeQueued {
			timeout := view.Spec.Timeout
			if timeout <= 0 {
				timeout = e.opts.DefaultNodeTimeout
			}
			if !view.QueuedAt.IsZero() && !now.Before(view.QueuedAt.Add(timeout)) {
				ev, err := makeEvent(EventNodeFailed, NodeFailedPayload{
					NodeID:    id,
					Attempt:   view.Attempt,
					Error:     "queue wait exceeded node timeout",
					ErrorKind: "queue_timeout",
					Reason:    "queue_timeout",
				}, now)
				if err == nil {
					events = append(events, ev)
				}
				continue
			}
			starts = append(starts, id)
			continue
		}
		if reason, terminal := cascadeStatus(wf, view.Spec); terminal {
			var ev Event
			var err error
			switch reason {
			case "deps_skipped":
				ev, err = makeEvent(EventNodeSkipped, NodeSkippedPayload{NodeID: id, Attempt: view.Attempt, Reason: reason}, now)
			case "condition_false":
				ev, err = makeEvent(EventNodeSkipped, NodeSkippedPayload{NodeID: id, Attempt: view.Attempt, Reason: reason}, now)
			default:
				ev, err = makeEvent(EventNodeFailed, NodeFailedPayload{
					NodeID:    id,
					Attempt:   view.Attempt,
					Error:     "required dependency did not complete",
					ErrorKind: reason,
					Reason:    reason,
				}, now)
			}
			if err == nil {
				events = append(events, ev)
			}
			continue
		}
		ev, err := makeEvent(EventNodeQueued, NodeQueuedPayload{NodeID: id, Attempt: view.Attempt, Reason: "ready"}, now)
		if err == nil {
			events = append(events, ev)
		}
	}
	return events, starts
}

func dependenciesTerminal(wf WorkflowView, spec NodeSpec) bool {
	for _, dep := range spec.DependsOn {
		if !wf.Nodes[dep].Status.Terminal() {
			return false
		}
	}
	return true
}

// cascadeStatus returns a terminal reason for a ready pending node.
func cascadeStatus(wf WorkflowView, spec NodeSpec) (string, bool) {
	allSkipped := len(spec.DependsOn) > 0
	for _, dep := range spec.DependsOn {
		switch wf.Nodes[dep].Status {
		case NodeSkipped:
		case NodeFailed, NodeAborted:
			if wf.Nodes[dep].Spec.Required {
				return "unsatisfied", true
			}
			allSkipped = false
		default:
			allSkipped = false
		}
	}
	if allSkipped {
		return "deps_skipped", true
	}
	if spec.Condition != "" && !evalCondition(spec.Condition, wf) {
		return "condition_false", true
	}
	return "", false
}

func tokenBudgetEvents(wf WorkflowView, now time.Time) []Event {
	var events []Event
	for _, id := range wf.Order {
		view := wf.Nodes[id]
		if view.Status.Terminal() || view.Spec.TokenFree {
			continue
		}
		var (
			ev  Event
			err error
		)
		if view.Spec.Required {
			ev, err = makeEvent(EventNodeFailed, NodeFailedPayload{
				NodeID:    id,
				Attempt:   view.Attempt,
				Error:     "workflow token budget exhausted",
				ErrorKind: "budget_exhausted",
				Reason:    "budget_exhausted",
			}, now)
		} else {
			ev, err = makeEvent(EventNodeSkipped, NodeSkippedPayload{
				NodeID:  id,
				Attempt: view.Attempt,
				Reason:  "budget_exhausted",
			}, now)
		}
		if err == nil {
			events = append(events, ev)
		}
	}
	return events
}

func allTerminal(wf WorkflowView) bool {
	for _, view := range wf.Nodes {
		if !view.Status.Terminal() {
			return false
		}
	}
	return true
}

func (e *Engine) finalize(ctx context.Context, wfID string) error {
	var retry staleRetry
	for {
		wf, err := e.loadView(ctx, wfID)
		if err != nil {
			return err
		}
		if wf.Terminal() {
			return nil
		}
		if !allTerminal(wf) {
			return nil
		}
		if err := retry.observe(wf.Revision); err != nil {
			return err
		}
		failed := false
		failedNode := ""
		for _, id := range wf.Order {
			view := wf.Nodes[id]
			if view.Spec.Required && (view.Status == NodeFailed || view.Status == NodeAborted) {
				failed = true
				failedNode = id
				break
			}
		}
		now := e.clock()
		var ev Event
		if failed {
			ev, err = makeEvent(EventWorkflowFailed, WorkflowTerminalPayload{
				Reason: "required_node_failed",
				Error:  "required node failed",
				NodeID: failedNode,
			}, now)
		} else {
			ev, err = makeEvent(EventWorkflowCompleted, WorkflowTerminalPayload{}, now)
		}
		if err != nil {
			return err
		}
		if _, err := e.store.Append(ctx, wfID, wf.Revision, ev); err != nil {
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

func (e *Engine) runNode(runCtx context.Context, handle *runHandle, wfID, nodeID string) {
	defer e.signal(wfID)
	defer handle.releaseNode(nodeID)

	wf, err := e.loadView(runCtx, wfID)
	if err != nil {
		if runCtx.Err() == nil {
			handle.setErr(err)
		}
		return
	}
	if wf.Terminal() {
		return
	}
	view, ok := wf.Nodes[nodeID]
	if !ok || view.Status != NodeQueued {
		return
	}
	timeout := view.Spec.Timeout
	if timeout <= 0 {
		timeout = e.opts.DefaultNodeTimeout
	}
	queuedAt := view.QueuedAt
	if queuedAt.IsZero() {
		queuedAt = e.clock()
	}
	queueDeadline := queuedAt.Add(timeout)

	// Queue wait is measured with the engine clock, not wall time. Mixing
	// context.WithDeadline (wall clock) with a custom engine clock makes a
	// fixed test clock appear already expired.
	var timeoutCh <-chan time.Time
	if remaining := queueDeadline.Sub(e.clock()); remaining <= 0 {
		if err := e.commitNodeFailure(runCtx, wfID, nodeID, view.Attempt, "queue_timeout", "queue wait exceeded node timeout"); err != nil {
			handle.setErr(err)
		}
		return
	} else {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	select {
	case e.sem <- struct{}{}:
	case <-timeoutCh:
		if runCtx.Err() != nil {
			return
		}
		if err := e.commitNodeFailure(runCtx, wfID, nodeID, view.Attempt, "queue_timeout", "queue wait exceeded node timeout"); err != nil {
			handle.setErr(err)
		}
		return
	case <-runCtx.Done():
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			<-e.sem
		}
	}
	defer release()

	wf, err = e.loadView(runCtx, wfID)
	if err != nil {
		if runCtx.Err() == nil {
			handle.setErr(err)
		}
		return
	}
	if wf.Terminal() {
		return
	}
	view, ok = wf.Nodes[nodeID]
	if !ok || view.Status != NodeQueued {
		return
	}
	if !view.Spec.TokenFree && wf.Spec.Budget.MaxTokens > 0 && wf.Usage.totalTokens() >= wf.Spec.Budget.MaxTokens {
		if view.Spec.Required {
			if err := e.commitNodeFailure(runCtx, wfID, nodeID, view.Attempt, "budget_exhausted", "workflow token budget exhausted"); err != nil {
				handle.setErr(err)
			}
		} else if err := e.commitNodeSkipped(runCtx, wfID, nodeID, view.Attempt, "budget_exhausted"); err != nil {
			handle.setErr(err)
		}
		return
	}

	remaining := queueDeadline.Sub(e.clock())
	if remaining <= 0 {
		release()
		if err := e.commitNodeFailure(runCtx, wfID, nodeID, view.Attempt, "queue_timeout", "queue wait exceeded node timeout"); err != nil {
			handle.setErr(err)
		}
		return
	}
	nodeCtx, nodeCancel := context.WithCancelCause(runCtx)
	handle.setNodeCancel(nodeID, nodeCancel)
	defer handle.clearNodeCancel(nodeID)
	defer nodeCancel(nil)

	input := buildNodeInput(wf, view.Spec, view.Attempt)
	started, err := e.commitNodeStarted(runCtx, wfID, nodeID, view.Attempt, nodeMetadata(e.exec, input))
	if err != nil {
		if runCtx.Err() == nil {
			handle.setErr(err)
		}
		return
	}
	if !started {
		return
	}
	execCtx, execCancel := context.WithTimeout(nodeCtx, remaining)
	out, execErr := executeSafely(execCtx, e.exec, input)
	timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded) || errors.Is(execErr, context.DeadlineExceeded)
	execCancel()
	release()

	if errors.Is(context.Cause(nodeCtx), errTokenBudget) {
		// The scheduler owns the durable budget event for this node.
		return
	}
	if runCtx.Err() != nil {
		// Cancel or the caller owns the durable abort/skip path.
		return
	}
	if execErr != nil {
		reason := "executor_error"
		if timedOut {
			reason = "timeout"
		}
		if err := e.commitNodeFailureUsage(runCtx, wfID, nodeID, view.Attempt, reason, execErr.Error(), out.Usage); err != nil {
			handle.setErr(err)
		}
		return
	}
	if err := e.commitNodeSuccess(runCtx, wfID, nodeID, view.Spec, view.Attempt, out); err != nil {
		handle.setErr(err)
	}
}

func executeSafely(ctx context.Context, exec NodeExecutor, in NodeInput) (out NodeOutput, err error) {
	if err := ctx.Err(); err != nil {
		return NodeOutput{}, err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("executor panic: %v", recovered)
		}
	}()
	return exec.Execute(ctx, in)
}

// nodeMetadata asks an executor for optional attribution. Metadata is
// advisory: a legacy executor, an unresolved provider, or a panicking lookup
// leaves the node unattributed and never changes scheduling.
func nodeMetadata(exec NodeExecutor, in NodeInput) (meta NodeMetadata) {
	provider, ok := exec.(NodeMetadataProvider)
	if !ok {
		return NodeMetadata{}
	}
	defer func() {
		if recover() != nil {
			meta = NodeMetadata{}
		}
	}()
	return provider.NodeMetadata(in)
}

func (e *Engine) commitNodeStarted(ctx context.Context, wfID, nodeID string, attempt int, meta NodeMetadata) (bool, error) {
	var retry staleRetry
	for {
		wf, err := e.loadView(ctx, wfID)
		if err != nil {
			return false, err
		}
		if wf.Terminal() {
			return false, nil
		}
		view, ok := wf.Nodes[nodeID]
		if !ok || view.Attempt != attempt || view.Status != NodeQueued {
			return false, nil
		}
		if err := retry.observe(wf.Revision); err != nil {
			return false, err
		}
		ev, err := makeEvent(EventNodeStarted, NodeStartedPayload{
			NodeID:   nodeID,
			Attempt:  attempt,
			Provider: meta.Provider,
			Model:    meta.Model,
		}, e.clock())
		if err != nil {
			return false, err
		}
		if _, err := e.store.Append(ctx, wfID, wf.Revision, ev); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				retry.mark(wf.Revision)
				continue
			}
			return false, err
		}
		e.signal(wfID)
		return true, nil
	}
}

func (e *Engine) commitNodeFailure(ctx context.Context, wfID, nodeID string, attempt int, reason, message string) error {
	return e.commitNodeFailureUsage(ctx, wfID, nodeID, attempt, reason, message, Usage{})
}

// commitNodeFailureUsage persists a failed attempt together with any usage the
// executor already knows. Retries and recovery therefore see the spend even
// though the node did not complete.
func (e *Engine) commitNodeFailureUsage(ctx context.Context, wfID, nodeID string, attempt int, reason, message string, usage Usage) error {
	if ctx.Err() != nil {
		return nil
	}
	var retry staleRetry
	for {
		if ctx.Err() != nil {
			return nil
		}
		wf, err := e.loadView(ctx, wfID)
		if err != nil {
			return err
		}
		if wf.Terminal() {
			return nil
		}
		view, ok := wf.Nodes[nodeID]
		if !ok || view.Attempt != attempt || view.Status.Terminal() {
			return nil
		}
		if err := retry.observe(wf.Revision); err != nil {
			return err
		}
		ev, err := makeEvent(EventNodeFailed, NodeFailedPayload{
			NodeID:    nodeID,
			Attempt:   attempt,
			Error:     message,
			ErrorKind: reason,
			Reason:    reason,
			Usage:     usage,
		}, e.clock())
		if err != nil {
			return err
		}
		if _, err := e.store.Append(ctx, wfID, wf.Revision, ev); err != nil {
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

func (e *Engine) commitNodeSkipped(ctx context.Context, wfID, nodeID string, attempt int, reason string) error {
	if ctx.Err() != nil {
		return nil
	}
	var retry staleRetry
	for {
		if ctx.Err() != nil {
			return nil
		}
		wf, err := e.loadView(ctx, wfID)
		if err != nil {
			return err
		}
		if wf.Terminal() {
			return nil
		}
		view, ok := wf.Nodes[nodeID]
		if !ok || view.Attempt != attempt || view.Status.Terminal() {
			return nil
		}
		if err := retry.observe(wf.Revision); err != nil {
			return err
		}
		ev, err := makeEvent(EventNodeSkipped, NodeSkippedPayload{
			NodeID:  nodeID,
			Attempt: attempt,
			Reason:  reason,
		}, e.clock())
		if err != nil {
			return err
		}
		if _, err := e.store.Append(ctx, wfID, wf.Revision, ev); err != nil {
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

func (e *Engine) commitNodeSuccess(ctx context.Context, wfID, nodeID string, spec NodeSpec, attempt int, out NodeOutput) error {
	if ctx.Err() != nil {
		return nil
	}
	if len(out.Data) > 0 && !json.Valid(out.Data) {
		return e.commitNodeFailure(ctx, wfID, nodeID, attempt, "invalid_output", "executor returned invalid JSON")
	}
	var (
		extension *Extension
		expandErr error
	)
	if spec.Expands {
		wf, err := e.loadView(ctx, wfID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if wf.Terminal() {
			return nil
		}
		if e.exp == nil {
			expandErr = errors.New("dag: node expands but no Expander is configured")
		} else {
			ext, err := e.exp.Expand(ctx, cloneView(wf), cloneSpec(spec), rawCopy(out.Data))
			if err != nil {
				expandErr = err
			} else if ext.FromNode != "" && ext.FromNode != nodeID {
				expandErr = fmt.Errorf("dag: extension from_node %q does not match %q", ext.FromNode, nodeID)
			} else {
				ext.FromNode = nodeID
				extension = &ext
			}
		}
	}

	var retry staleRetry
	for {
		if ctx.Err() != nil {
			return nil
		}
		wf, err := e.loadView(ctx, wfID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if wf.Terminal() {
			return nil
		}
		view, ok := wf.Nodes[nodeID]
		if !ok || view.Attempt != attempt || view.Status != NodeRunning {
			return nil
		}
		if err := retry.observe(wf.Revision); err != nil {
			return err
		}
		if expandErr != nil {
			return e.commitNodeFailure(ctx, wfID, nodeID, attempt, "expand_failed", expandErr.Error())
		}
		events, err := e.resultEvents(wf, nodeID, spec, attempt, out, extension)
		if err != nil {
			return e.commitNodeFailure(ctx, wfID, nodeID, attempt, "expand_failed", err.Error())
		}
		if _, err := e.store.Append(ctx, wfID, wf.Revision, events...); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				retry.mark(wf.Revision)
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		e.signal(wfID)
		return nil
	}
}

func (e *Engine) resultEvents(wf WorkflowView, nodeID string, spec NodeSpec, attempt int, out NodeOutput, ext *Extension) ([]Event, error) {
	now := e.clock()
	completed, err := makeEvent(EventNodeCompleted, NodeCompletedPayload{
		NodeID:  nodeID,
		Attempt: attempt,
		Output:  rawCopy(out.Data),
		Usage:   out.Usage,
	}, now)
	if err != nil {
		return nil, err
	}
	if ext == nil || (len(ext.Nodes) == 0 && len(ext.DepPatches) == 0) {
		return []Event{completed}, nil
	}
	payload, err := validateExtension(wf, ext)
	if err != nil {
		return nil, err
	}
	extended, err := makeEvent(EventWorkflowExtended, payload, now)
	if err != nil {
		return nil, err
	}
	return []Event{completed, extended}, nil
}

func validateExtension(wf WorkflowView, ext *Extension) (WorkflowExtendedPayload, error) {
	source, err := normalizeExtensionSource(ext.Source)
	if err != nil {
		return WorkflowExtendedPayload{}, err
	}
	if source == ExtensionSourceExtend && wf.ExtendsUsed >= wf.Spec.Budget.MaxExtends {
		return WorkflowExtendedPayload{}, fmt.Errorf("%w: max_extends %d reached", ErrBudgetExceeded, wf.Spec.Budget.MaxExtends)
	}
	candidate := cloneGraph(wf.Spec)
	candidate.Nodes = append(candidate.Nodes, cloneSpecs(ext.Nodes)...)
	for _, patch := range ext.DepPatches {
		view, ok := wf.Nodes[patch.Node]
		if !ok {
			return WorkflowExtendedPayload{}, &GraphError{Field: "dep_patches", Problem: fmt.Sprintf("unknown node %q", patch.Node)}
		}
		if view.Status != NodePending {
			return WorkflowExtendedPayload{}, &GraphError{Field: "dep_patches", Problem: fmt.Sprintf("node %q is %s, patches require pending", patch.Node, view.Status)}
		}
		for i := range candidate.Nodes {
			if candidate.Nodes[i].ID == patch.Node {
				candidate.Nodes[i].DependsOn = append(candidate.Nodes[i].DependsOn, append([]string(nil), patch.Add...)...)
				break
			}
		}
	}
	if err := ValidateGraph(candidate); err != nil {
		return WorkflowExtendedPayload{}, err
	}
	return WorkflowExtendedPayload{
		Nodes:      cloneSpecs(ext.Nodes),
		DepPatches: clonePatches(ext.DepPatches),
		Source:     source,
		FromNode:   ext.FromNode,
	}, nil
}

func clonePatches(in []DepPatch) []DepPatch {
	out := make([]DepPatch, len(in))
	for i, patch := range in {
		out[i] = DepPatch{Node: patch.Node, Add: append([]string(nil), patch.Add...)}
	}
	return out
}

func buildNodeInput(wf WorkflowView, spec NodeSpec, attempt int) NodeInput {
	inputs := make(map[string]json.RawMessage, len(spec.InputMapping))
	for name, ref := range spec.InputMapping {
		inputs[name] = resolveInput(wf, ref)
	}
	return NodeInput{
		WorkflowID: wf.ID,
		Node:       cloneSpec(spec),
		Attempt:    attempt,
		Inputs:     inputs,
		Meta:       cloneStrings(wf.Meta),
	}
}

func resolveInput(wf WorkflowView, ref string) json.RawMessage {
	ids := make(map[string]bool, len(wf.Nodes))
	for id := range wf.Nodes {
		ids[id] = true
	}
	nodeID, path, err := splitOutputRef(ref, ids)
	if err != nil {
		return unavailable("invalid_reference")
	}
	view, ok := wf.Nodes[nodeID]
	if !ok {
		return unavailable("missing_dependency")
	}
	switch view.Status {
	case NodeCompleted:
	case NodeFailed:
		return unavailable("dependency_failed")
	case NodeAborted:
		return unavailable("dependency_aborted")
	case NodeSkipped:
		return unavailable("dependency_skipped")
	default:
		return unavailable("dependency_pending")
	}
	value, present := outputPath(view.Output, path)
	if !present {
		return unavailable("output_missing")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return unavailable("output_invalid")
	}
	return data
}

func unavailable(reason string) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"_unavailable": true, "reason": reason})
	return data
}

func (h *runHandle) claimNode(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active[id] {
		return false
	}
	h.active[id] = true
	return true
}

func (h *runHandle) releaseNode(id string) {
	h.mu.Lock()
	delete(h.active, id)
	h.mu.Unlock()
}

func (h *runHandle) activeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.active)
}
