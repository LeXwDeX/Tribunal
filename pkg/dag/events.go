package dag

import (
	"encoding/json"
	"fmt"
	"time"
)

// Event types. Only the types emitted by Engine are listed first. The two
// compatibility event types (node.registered and node.cancelled) are accepted
// by the projector but are not emitted.
const (
	EventWorkflowCreated   = "dag.workflow.created"
	EventWorkflowStarted   = "dag.workflow.started"
	EventWorkflowExtended  = "dag.workflow.extended"
	EventWorkflowCompleted = "dag.workflow.completed"
	EventWorkflowFailed    = "dag.workflow.failed"
	EventWorkflowCancelled = "dag.workflow.cancelled"

	EventNodeQueued    = "dag.node.queued"
	EventNodeStarted   = "dag.node.started"
	EventNodeCompleted = "dag.node.completed"
	EventNodeFailed    = "dag.node.failed"
	EventNodeAborted   = "dag.node.aborted"
	EventNodeSkipped   = "dag.node.skipped"
	EventNodeRestarted = "dag.node.restarted"

	EventNodeRegistered = "dag.node.registered"
	EventNodeCancelled  = "dag.node.cancelled"
)

// Event is one immutable append-only fact. Seq is assigned by the store; the
// store revision is the sequence number of the last event.
type Event struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	At      time.Time       `json:"at"`
	Seq     int64           `json:"seq,omitempty"`
}

// WorkflowCreatedPayload is the payload of workflow.created.
type WorkflowCreatedPayload struct {
	WorkflowID string            `json:"workflow_id,omitempty"`
	Spec       GraphSpec         `json:"spec"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// WorkflowExtendedPayload is the payload of workflow.extended. Nodes and
// DepPatches are applied atomically.
type WorkflowExtendedPayload struct {
	Nodes      []NodeSpec `json:"nodes"`
	DepPatches []DepPatch `json:"dep_patches,omitempty"`
	Source     string     `json:"source,omitempty"`
	FromNode   string     `json:"from_node,omitempty"`
}

// WorkflowTerminalPayload is used by workflow.completed, failed, and cancelled.
type WorkflowTerminalPayload struct {
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

// NodeQueuedPayload is the payload of node.queued.
type NodeQueuedPayload struct {
	NodeID  string `json:"node_id"`
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason,omitempty"`
}

// NodeStartedPayload is the payload of node.started.
type NodeStartedPayload struct {
	NodeID   string `json:"node_id"`
	Attempt  int    `json:"attempt"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// NodeCompletedPayload is the payload of node.completed.
type NodeCompletedPayload struct {
	NodeID  string          `json:"node_id"`
	Attempt int             `json:"attempt"`
	Output  json.RawMessage `json:"output,omitempty"`
	Usage   Usage           `json:"usage"`
}

// NodeFailedPayload is the payload of node.failed.
type NodeFailedPayload struct {
	NodeID      string `json:"node_id"`
	Attempt     int    `json:"attempt"`
	Error       string `json:"error,omitempty"`
	ErrorKind   string `json:"error_kind,omitempty"`
	ErrorStatus int    `json:"error_status,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Usage       Usage  `json:"usage"`
}

// NodeAbortedPayload is the payload of node.aborted.
type NodeAbortedPayload struct {
	NodeID  string `json:"node_id"`
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason,omitempty"`
}

// NodeSkippedPayload is the payload of node.skipped.
type NodeSkippedPayload struct {
	NodeID  string `json:"node_id"`
	Attempt int    `json:"attempt,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// NodeRestartedPayload is the payload of node.restarted.
type NodeRestartedPayload struct {
	NodeID      string `json:"node_id"`
	Attempt     int    `json:"attempt,omitempty"`
	NextAttempt int    `json:"next_attempt,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// NodeRegisteredPayload is the compatibility payload of node.registered.
type NodeRegisteredPayload struct {
	NodeID string   `json:"node_id,omitempty"`
	Spec   NodeSpec `json:"spec"`
}

func makeEvent(typ string, payload any, at time.Time) (Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("dag: marshal %s payload: %w", typ, err)
	}
	return Event{Type: typ, Payload: data, At: at.UTC()}, nil
}

func decodePayload[P any](ev Event) (P, error) {
	var payload P
	if len(ev.Payload) == 0 {
		return payload, nil
	}
	if err := strictUnmarshal(ev.Payload, &payload); err != nil {
		return payload, fmt.Errorf("dag: decode %s payload: %w", ev.Type, err)
	}
	return payload, nil
}
