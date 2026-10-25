package dag

import (
	"encoding/json"
	"fmt"
	"time"
)

// Project deterministically validates and folds events into a read model.
// Events without a Seq are assigned consecutive sequence numbers, which makes
// [Project] usable inside a transaction that appends and then validates a
// batch before commit. Stored events must carry consecutive sequence numbers.
// If a created payload omits workflow_id, a caller that knows the workflow ID
// may set WorkflowView.ID after Project.
func Project(events []Event) (WorkflowView, error) {
	var wf WorkflowView
	if len(events) == 0 {
		return wf, ErrNotFound
	}
	var lastSeq int64
	for i := range events {
		ev := events[i]
		if ev.Seq == 0 {
			ev.Seq = lastSeq + 1
		}
		if ev.Seq != lastSeq+1 {
			return WorkflowView{}, fmt.Errorf("dag: non-consecutive event seq %d, want %d", ev.Seq, lastSeq+1)
		}
		if i == 0 && ev.Type != EventWorkflowCreated {
			return WorkflowView{}, fmt.Errorf("dag: first event must be %s, got %s", EventWorkflowCreated, ev.Type)
		}
		if err := applyEvent(&wf, ev, i == 0); err != nil {
			return WorkflowView{}, err
		}
		lastSeq = ev.Seq
	}
	return wf, nil
}

func applyEvent(wf *WorkflowView, ev Event, first bool) error {
	switch ev.Type {
	case EventWorkflowCreated:
		if !first {
			return fmt.Errorf("%w: duplicate %s", ErrInvalidTransition, EventWorkflowCreated)
		}
		return applyCreated(wf, ev)
	case EventWorkflowStarted:
		if err := workflowTransition(wf.Status, WorkflowRunning); err != nil {
			return err
		}
		wf.Status = WorkflowRunning
		if wf.StartedAt.IsZero() {
			wf.StartedAt = ev.At.UTC()
		}
	case EventWorkflowExtended:
		return applyExtended(wf, ev)
	case EventWorkflowCompleted:
		return applyWorkflowTerminal(wf, ev, WorkflowCompleted)
	case EventWorkflowFailed:
		return applyWorkflowTerminal(wf, ev, WorkflowFailed)
	case EventWorkflowCancelled:
		return applyWorkflowTerminal(wf, ev, WorkflowCancelled)
	case EventNodeQueued:
		return applyNodeQueued(wf, ev)
	case EventNodeStarted:
		return applyNodeStarted(wf, ev)
	case EventNodeCompleted:
		return applyNodeCompleted(wf, ev)
	case EventNodeFailed:
		return applyNodeTerminal(wf, ev, NodeFailed)
	case EventNodeAborted:
		return applyNodeTerminal(wf, ev, NodeAborted)
	case EventNodeSkipped, EventNodeCancelled:
		return applyNodeTerminal(wf, ev, NodeSkipped)
	case EventNodeRestarted:
		return applyNodeRestarted(wf, ev)
	case EventNodeRegistered:
		return applyNodeRegistered(wf, ev)
	default:
		return fmt.Errorf("dag: unknown event type %q", ev.Type)
	}
	wf.Revision = ev.Seq
	return nil
}

func applyCreated(wf *WorkflowView, ev Event) error {
	payload, err := decodePayload[WorkflowCreatedPayload](ev)
	if err != nil {
		return err
	}
	spec := cloneGraph(payload.Spec)
	spec.Budget = NormalizeBudget(spec.Budget)
	if err := ValidateGraph(spec); err != nil {
		return err
	}
	wf.ID = payload.WorkflowID
	wf.Status = WorkflowPending
	wf.Spec = spec
	wf.Meta = cloneStrings(payload.Meta)
	wf.CreatedAt = ev.At.UTC()
	wf.Revision = ev.Seq
	wf.Nodes = make(map[string]NodeView, len(spec.Nodes))
	wf.Order = make([]string, 0, len(spec.Nodes))
	for _, node := range spec.Nodes {
		wf.Order = append(wf.Order, node.ID)
		wf.Nodes[node.ID] = NodeView{
			ID:       node.ID,
			Spec:     cloneSpec(node),
			Status:   NodePending,
			Revision: ev.Seq,
			AddedRev: ev.Seq,
			Attempt:  1,
			Meta:     cloneStrings(payload.Meta),
		}
	}
	if spec.Budget.Deadline > 0 {
		wf.Deadline = wf.CreatedAt.Add(spec.Budget.Deadline)
	}
	return nil
}

func applyExtended(wf *WorkflowView, ev Event) error {
	if wf.Terminal() {
		return &TransitionError{Entity: "workflow", From: string(wf.Status), To: string(wf.Status), Terminal: true}
	}
	payload, err := decodePayload[WorkflowExtendedPayload](ev)
	if err != nil {
		return err
	}
	source, err := normalizeExtensionSource(payload.Source)
	if err != nil {
		return err
	}
	if source == ExtensionSourceExtend && wf.ExtendsUsed >= wf.Spec.Budget.MaxExtends {
		return fmt.Errorf("%w: max_extends %d reached", ErrBudgetExceeded, wf.Spec.Budget.MaxExtends)
	}
	if payload.FromNode != "" {
		from, ok := wf.Nodes[payload.FromNode]
		if !ok {
			return &GraphError{Field: "from_node", Problem: fmt.Sprintf("unknown node %q", payload.FromNode)}
		}
		if from.Status != NodeCompleted {
			return &GraphError{Field: "from_node", Problem: fmt.Sprintf("node %q is %s, not completed", payload.FromNode, from.Status)}
		}
		if !from.Spec.Expands {
			return &GraphError{Field: "from_node", Problem: fmt.Sprintf("node %q does not expand", payload.FromNode)}
		}
	}

	candidate := cloneGraph(wf.Spec)
	existing := make(map[string]NodeStatus, len(wf.Nodes))
	for id, view := range wf.Nodes {
		existing[id] = view.Status
	}
	patched := make(map[string]bool, len(payload.DepPatches))
	candidate.Nodes = append(candidate.Nodes, cloneSpecs(payload.Nodes)...)
	for _, patch := range payload.DepPatches {
		patched[patch.Node] = true
		status, ok := existing[patch.Node]
		if !ok {
			// A patch can target a node introduced by this same extension.
			for _, node := range candidate.Nodes {
				if node.ID == patch.Node {
					status, ok = NodePending, true
					break
				}
			}
		}
		if !ok {
			return &GraphError{Field: "dep_patches", Problem: fmt.Sprintf("unknown node %q", patch.Node)}
		}
		if status != NodePending {
			return &GraphError{Field: "dep_patches", Problem: fmt.Sprintf("node %q is %s, patches require pending", patch.Node, status)}
		}
		for i := range candidate.Nodes {
			if candidate.Nodes[i].ID != patch.Node {
				continue
			}
			candidate.Nodes[i].DependsOn = append(candidate.Nodes[i].DependsOn, append([]string(nil), patch.Add...)...)
			break
		}
	}
	if err := ValidateGraph(candidate); err != nil {
		return err
	}
	for _, node := range candidate.Nodes {
		if view, exists := wf.Nodes[node.ID]; exists {
			view.Spec = cloneSpec(node)
			if patched[node.ID] {
				view.Revision = ev.Seq
			}
			wf.Nodes[node.ID] = view
			continue
		}
		wf.Order = append(wf.Order, node.ID)
		wf.Nodes[node.ID] = NodeView{
			ID:       node.ID,
			Spec:     cloneSpec(node),
			Status:   NodePending,
			Revision: ev.Seq,
			AddedRev: ev.Seq,
			Attempt:  1,
			Meta:     cloneStrings(wf.Meta),
		}
	}
	wf.Spec = candidate
	if source == ExtensionSourceExtend {
		wf.ExtendsUsed++
	}
	wf.Revision = ev.Seq
	return nil
}

func applyNodeRegistered(wf *WorkflowView, ev Event) error {
	if wf.Terminal() {
		return &TransitionError{Entity: "workflow", From: string(wf.Status), To: string(wf.Status), Terminal: true}
	}
	payload, err := decodePayload[NodeRegisteredPayload](ev)
	if err != nil {
		return err
	}
	id := payload.Spec.ID
	if id == "" {
		id = payload.NodeID
	}
	if id == "" {
		return &GraphError{Field: "node.registered", Problem: "missing node id"}
	}
	if _, exists := wf.Nodes[id]; exists {
		if payload.Spec.ID == "" {
			// A bare node_id registration that accompanies created/extended is
			// idempotent; the spec is already present from the batch event.
			wf.Revision = ev.Seq
			return nil
		}
		return &GraphError{Field: "node.registered", Problem: fmt.Sprintf("duplicate node %q", id)}
	}
	spec := cloneSpec(payload.Spec)
	spec.ID = id
	candidate := cloneGraph(wf.Spec)
	candidate.Nodes = append(candidate.Nodes, spec)
	if err := ValidateGraph(candidate); err != nil {
		return err
	}
	wf.Spec = candidate
	wf.Order = append(wf.Order, id)
	wf.Nodes[id] = NodeView{
		ID:       id,
		Spec:     cloneSpec(spec),
		Status:   NodePending,
		Revision: ev.Seq,
		AddedRev: ev.Seq,
		Attempt:  1,
		Meta:     cloneStrings(wf.Meta),
	}
	wf.Revision = ev.Seq
	return nil
}

func nodePayload(wf *WorkflowView, ev Event, id string, attempt int, requireRunning bool) (*NodeView, error) {
	if wf.Terminal() {
		return nil, &TransitionError{Entity: "workflow", From: string(wf.Status), To: string(wf.Status), Terminal: true}
	}
	view, ok := wf.Nodes[id]
	if !ok {
		return nil, fmt.Errorf("%w: node %q", ErrNotFound, id)
	}
	if view.Attempt != attempt {
		return nil, &attemptError{entity: id, want: view.Attempt, got: attempt}
	}
	if requireRunning && view.Status != NodeRunning {
		return nil, fmt.Errorf("%w: node %q is %s, want running", ErrInvalidTransition, id, view.Status)
	}
	return &view, nil
}

func nodeEventID(ev Event) (string, error) {
	var meta struct {
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(ev.Payload, &meta); err != nil {
		return "", fmt.Errorf("dag: decode %s payload: %w", ev.Type, err)
	}
	if meta.NodeID == "" {
		return "", fmt.Errorf("dag: %s payload is missing node_id", ev.Type)
	}
	return meta.NodeID, nil
}

func finishNode(wf *WorkflowView, view NodeView, status NodeStatus, ev Event) {
	view.Status = status
	view.Revision = ev.Seq
	view.FinishedAt = ev.At.UTC()
	wf.Nodes[view.ID] = view
	wf.Revision = ev.Seq
}

func applyNodeQueued(wf *WorkflowView, ev Event) error {
	id, err := nodeEventID(ev)
	if err != nil {
		return err
	}
	payload, err := decodePayload[NodeQueuedPayload](ev)
	if err != nil {
		return err
	}
	view, err := nodePayload(wf, ev, id, payload.Attempt, false)
	if err != nil {
		return err
	}
	if err := nodeTransition(view.Status, NodeQueued); err != nil {
		return err
	}
	if view.Status == NodePending {
		view.QueuedAt = ev.At.UTC()
	}
	view.Status = NodeQueued
	view.Reason = payload.Reason
	view.Revision = ev.Seq
	wf.Nodes[id] = *view
	wf.Revision = ev.Seq
	return nil
}

func applyNodeStarted(wf *WorkflowView, ev Event) error {
	id, err := nodeEventID(ev)
	if err != nil {
		return err
	}
	payload, err := decodePayload[NodeStartedPayload](ev)
	if err != nil {
		return err
	}
	view, err := nodePayload(wf, ev, id, payload.Attempt, false)
	if err != nil {
		return err
	}
	if err := nodeTransition(view.Status, NodeRunning); err != nil {
		return err
	}
	view.Status = NodeRunning
	view.Reason = ""
	view.Provider = payload.Provider
	view.Model = payload.Model
	view.StartedAt = ev.At.UTC()
	view.FinishedAt = time.Time{}
	view.Revision = ev.Seq
	wf.Nodes[id] = *view
	wf.Revision = ev.Seq
	return nil
}

func applyNodeCompleted(wf *WorkflowView, ev Event) error {
	id, err := nodeEventID(ev)
	if err != nil {
		return err
	}
	payload, err := decodePayload[NodeCompletedPayload](ev)
	if err != nil {
		return err
	}
	view, err := nodePayload(wf, ev, id, payload.Attempt, true)
	if err != nil {
		return err
	}
	if len(payload.Output) > 0 && !json.Valid(payload.Output) {
		return fmt.Errorf("dag: node %q output is not valid JSON", id)
	}
	view.Output = rawCopy(payload.Output)
	view.Usage = payload.Usage
	view.Error = ""
	view.Reason = ""
	finishNode(wf, *view, NodeCompleted, ev)
	wf.Usage.add(payload.Usage)
	return nil
}

func applyNodeTerminal(wf *WorkflowView, ev Event, status NodeStatus) error {
	id, err := nodeEventID(ev)
	if err != nil {
		return err
	}
	switch ev.Type {
	case EventNodeFailed:
		payload, err := decodePayload[NodeFailedPayload](ev)
		if err != nil {
			return err
		}
		view, err := nodePayload(wf, ev, id, payload.Attempt, false)
		if err != nil {
			return err
		}
		if err := nodeTransition(view.Status, NodeFailed); err != nil {
			return err
		}
		view.Usage = payload.Usage
		view.Error = payload.Error
		view.Reason = payload.Reason
		if view.Reason == "" {
			view.Reason = payload.ErrorKind
		}
		finishNode(wf, *view, NodeFailed, ev)
		wf.Usage.add(payload.Usage)
		return nil
	case EventNodeAborted:
		payload, err := decodePayload[NodeAbortedPayload](ev)
		if err != nil {
			return err
		}
		view, err := nodePayload(wf, ev, id, payload.Attempt, true)
		if err != nil {
			return err
		}
		if err := nodeTransition(view.Status, NodeAborted); err != nil {
			return err
		}
		view.Reason = payload.Reason
		finishNode(wf, *view, NodeAborted, ev)
		return nil
	default:
		payload, err := decodePayload[NodeSkippedPayload](ev)
		if err != nil {
			return err
		}
		attempt := payload.Attempt
		if attempt == 0 {
			if current, ok := wf.Nodes[id]; ok {
				attempt = current.Attempt
			}
		}
		view, err := nodePayload(wf, ev, id, attempt, false)
		if err != nil {
			return err
		}
		if err := nodeTransition(view.Status, NodeSkipped); err != nil {
			return err
		}
		view.Reason = payload.Reason
		finishNode(wf, *view, status, ev)
		return nil
	}
}

func applyNodeRestarted(wf *WorkflowView, ev Event) error {
	id, err := nodeEventID(ev)
	if err != nil {
		return err
	}
	payload, err := decodePayload[NodeRestartedPayload](ev)
	if err != nil {
		return err
	}
	if wf.Terminal() {
		return &TransitionError{Entity: "workflow", From: string(wf.Status), To: string(wf.Status), Terminal: true}
	}
	view, ok := wf.Nodes[id]
	if !ok {
		return fmt.Errorf("%w: node %q", ErrNotFound, id)
	}
	if view.Status != NodeRunning && view.Status != NodeQueued {
		return fmt.Errorf("%w: node %q is %s, restart requires running or queued", ErrInvalidTransition, id, view.Status)
	}
	nextAttempt := payload.NextAttempt
	if nextAttempt == 0 {
		nextAttempt = payload.Attempt
	}
	if nextAttempt != view.Attempt+1 {
		return &attemptError{entity: id, want: view.Attempt + 1, got: nextAttempt}
	}
	if err := nodeTransition(view.Status, NodePending); err != nil {
		return err
	}
	view.Status = NodePending
	view.Attempt++
	view.Output = nil
	view.Usage = Usage{}
	view.Provider = ""
	view.Model = ""
	view.Error = ""
	view.Reason = payload.Reason
	view.StartedAt = time.Time{}
	view.FinishedAt = time.Time{}
	view.Revision = ev.Seq
	wf.Nodes[id] = view
	wf.Revision = ev.Seq
	return nil
}

func applyWorkflowTerminal(wf *WorkflowView, ev Event, status WorkflowStatus) error {
	if err := workflowTransition(wf.Status, status); err != nil {
		return err
	}
	for _, id := range wf.Order {
		view := wf.Nodes[id]
		if !view.Status.Terminal() {
			return fmt.Errorf("dag: cannot finish workflow while node %q is %s", view.ID, view.Status)
		}
	}
	payload, err := decodePayload[WorkflowTerminalPayload](ev)
	if err != nil {
		return err
	}
	wf.Status = status
	wf.CancelReason = ""
	if status == WorkflowCancelled {
		wf.CancelReason = payload.Reason
	}
	wf.Error = payload.Error
	if wf.Error == "" && status == WorkflowFailed {
		wf.Error = payload.Reason
	}
	wf.FinishedAt = ev.At.UTC()
	wf.Revision = ev.Seq
	return nil
}

func normalizeExtensionSource(source string) (string, error) {
	switch source {
	case "":
		return ExtensionSourceFanOut, nil
	case ExtensionSourceFanOut, ExtensionSourceExtend:
		return source, nil
	default:
		return "", &GraphError{Field: "workflow.extended.source", Problem: fmt.Sprintf("unknown source %q", source)}
	}
}

func cloneSpecs(in []NodeSpec) []NodeSpec {
	out := make([]NodeSpec, len(in))
	for i, spec := range in {
		out[i] = cloneSpec(spec)
	}
	return out
}

func (u *Usage) add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CostMicros += other.CostMicros
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.ReasoningTokens += other.ReasoningTokens
}

func (u Usage) totalTokens() int64 {
	return u.InputTokens + u.OutputTokens
}
