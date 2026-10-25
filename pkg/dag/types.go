// Package dag provides a scene-independent workflow engine. It does not know
// about reviews, GitLab, agents, or persistence backends.
package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// NodeStatus is the lifecycle state of one node.
type NodeStatus string

const (
	NodePending   NodeStatus = "pending"
	NodeQueued    NodeStatus = "queued"
	NodeRunning   NodeStatus = "running"
	NodeCompleted NodeStatus = "completed"
	NodeFailed    NodeStatus = "failed"
	NodeAborted   NodeStatus = "aborted"
	NodeSkipped   NodeStatus = "skipped"
)

// WorkflowStatus is the lifecycle state of a workflow.
type WorkflowStatus string

const (
	WorkflowPending   WorkflowStatus = "pending"
	WorkflowRunning   WorkflowStatus = "running"
	WorkflowCompleted WorkflowStatus = "completed"
	WorkflowFailed    WorkflowStatus = "failed"
	WorkflowCancelled WorkflowStatus = "cancelled"
)

var (
	// ErrNotFound means the workflow does not exist.
	ErrNotFound = errors.New("dag: workflow not found")
	// ErrStaleRevision is returned by EventStore.Append when expectRev does not
	// match the current workflow revision.
	ErrStaleRevision = errors.New("dag: stale revision")
	// ErrStaleAttempt fences a late result from an old node attempt.
	ErrStaleAttempt = errors.New("dag: stale attempt")
	// ErrInvalidGraph is the base error for graph validation failures.
	ErrInvalidGraph = errors.New("dag: invalid graph")
	// ErrInvalidTransition is the base error for rejected state transitions.
	ErrInvalidTransition = errors.New("dag: invalid transition")
	// ErrTerminalViolation means an event tried to change a terminal state.
	ErrTerminalViolation = errors.New("dag: terminal state is immutable")
	// ErrAlreadyRunning means another Run owns the workflow in this process.
	ErrAlreadyRunning = errors.New("dag: workflow already running")
	// ErrBudgetExceeded means a hard graph or token budget was exceeded.
	ErrBudgetExceeded = errors.New("dag: budget exceeded")
)

// GraphError describes a validation failure and unwraps to ErrInvalidGraph.
type GraphError struct {
	Field   string
	Problem string
}

func (e *GraphError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%v: %s", ErrInvalidGraph, e.Problem)
	}
	return fmt.Sprintf("%v: %s: %s", ErrInvalidGraph, e.Field, e.Problem)
}

func (e *GraphError) Unwrap() error { return ErrInvalidGraph }

// TransitionError describes a rejected node or workflow transition.
type TransitionError struct {
	Entity   string
	From     string
	To       string
	Terminal bool
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("%s %q -> %q is not allowed", e.Entity, e.From, e.To)
}

func (e *TransitionError) Unwrap() error {
	if e.Terminal {
		return ErrTerminalViolation
	}
	return ErrInvalidTransition
}

type attemptError struct {
	entity string
	want   int
	got    int
}

func (e *attemptError) Error() string {
	return fmt.Sprintf("dag: stale attempt for %s: current %d, event %d", e.entity, e.want, e.got)
}

func (e *attemptError) Unwrap() error { return ErrStaleAttempt }

// NodeSpec is the static specification of one node. Kind is interpreted by the
// caller; the engine treats all kinds alike.
type NodeSpec struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	DependsOn    []string          `json:"depends_on,omitempty"`
	Required     bool              `json:"required"`
	Condition    string            `json:"condition,omitempty"`
	InputMapping map[string]string `json:"input_mapping,omitempty"`
	Params       json.RawMessage   `json:"params,omitempty"`
	Timeout      time.Duration     `json:"timeout,omitempty"`
	Expands      bool              `json:"expands,omitempty"`
	// TokenFree marks a node that may still run after the workflow token
	// budget is exhausted. Its executor must stop starting model calls once
	// the budget is exhausted and settle with deterministic code instead.
	TokenFree bool `json:"token_free,omitempty"`
}

// Budget bounds one workflow. Zero values are normalized by Create: MaxNodes
// defaults to 30 and MaxExtends to 1. Negative values are invalid.
//
// MaxTokens is a shared workflow budget. The scheduler and node executors
// check committed usage before each request. A model request already in
// flight can finish after the check and overshoot the limit; this is a bounded
// best-effort cap, not an exact upstream billing guarantee.
type Budget struct {
	MaxNodes   int           `json:"max_nodes"`
	MaxExtends int           `json:"max_extends"`
	Deadline   time.Duration `json:"deadline"`
	MaxTokens  int64         `json:"max_tokens"`
}

// GraphSpec is the full static graph and its budget.
type GraphSpec struct {
	Nodes  []NodeSpec `json:"nodes"`
	Budget Budget     `json:"budget"`
}

// NodeInput is passed to a NodeExecutor.
type NodeInput struct {
	WorkflowID string
	Node       NodeSpec
	Attempt    int
	Inputs     map[string]json.RawMessage
	Meta       map[string]string
}

// NodeOutput is a successful node result.
type NodeOutput struct {
	Data  json.RawMessage
	Usage Usage
}

// Usage accounts for node work.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CostMicros       int64 `json:"cost_micros"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
}

// NodeExecutor is implemented by the caller. Any returned error fails the node.
type NodeExecutor interface {
	Execute(ctx context.Context, in NodeInput) (NodeOutput, error)
}

// NodeMetadata is the provider and model selected for one node. The engine
// records it on node.started before Execute runs.
type NodeMetadata struct {
	Provider string
	Model    string
}

// NodeMetadataProvider is an optional NodeExecutor extension. The engine asks
// it for attribution immediately before node.started. The lookup must be
// synchronous, local, and non-blocking (for example an in-memory provider
// lookup); it must not perform network work. The result is advisory: legacy
// executors stay valid, empty values leave the node unattributed, and a panic
// degrades to empty attribution instead of failing the node.
type NodeMetadataProvider interface {
	NodeMetadata(in NodeInput) NodeMetadata
}

// DepPatch appends dependencies to an existing pending node. It is applied
// atomically with the graph extension that carries it.
type DepPatch struct {
	Node string   `json:"node"`
	Add  []string `json:"add"`
}

// Extension is returned by an Expander. Source is either "fan_out" or
// "extend"; only "extend" consumes Budget.MaxExtends.
type Extension struct {
	Nodes      []NodeSpec `json:"nodes"`
	DepPatches []DepPatch `json:"dep_patches,omitempty"`
	Source     string     `json:"source,omitempty"`
	FromNode   string     `json:"from_node,omitempty"`
}

const (
	ExtensionSourceFanOut = "fan_out"
	ExtensionSourceExtend = "extend"
)

// Expander derives new nodes from a completed expanding node. out is the
// node's output Data.
type Expander interface {
	Expand(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error)
}

// EventStore is the durable event boundary.
//
// Append is transactional and strictly compares expectRev with the current
// revision (zero for a new workflow). Events are stored with consecutive
// sequence numbers starting at expectRev+1, and the returned revision is the
// last stored sequence number. If any event is invalid, nothing is stored and
// ErrStaleRevision or a validation error is returned. Load returns events in
// sequence order. ListUnfinished returns IDs whose projected workflow is not
// terminal.
type EventStore interface {
	Append(ctx context.Context, wfID string, expectRev int64, evs ...Event) (int64, error)
	Load(ctx context.Context, wfID string) ([]Event, error)
	ListUnfinished(ctx context.Context) ([]string, error)
}

// Options configures an Engine. Clock timestamps events and derives logical
// workflow deadlines; queue and node timeouts are measured from those logical
// deadlines. When no Clock is supplied, time.Now is used.
type Options struct {
	MaxConcurrentNodes int
	DefaultNodeTimeout time.Duration
	Clock              func() time.Time
}

// NodeView is the projected read model of one node. Meta is a copy of the
// workflow metadata for convenience; it is not node-specific configuration.
type NodeView struct {
	ID         string            `json:"id"`
	Spec       NodeSpec          `json:"spec"`
	Status     NodeStatus        `json:"status"`
	Revision   int64             `json:"revision"`
	AddedRev   int64             `json:"added_rev"`
	Attempt    int               `json:"attempt"`
	Meta       map[string]string `json:"meta,omitempty"`
	Output     json.RawMessage   `json:"output,omitempty"`
	Usage      Usage             `json:"usage"`
	Provider   string            `json:"provider,omitempty"`
	Model      string            `json:"model,omitempty"`
	Error      string            `json:"error,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	QueuedAt   time.Time         `json:"queued_at,omitempty"`
	StartedAt  time.Time         `json:"started_at,omitempty"`
	FinishedAt time.Time         `json:"finished_at,omitempty"`
}

// WorkflowView is the deterministic projection of a workflow's events.
type WorkflowView struct {
	ID           string              `json:"id"`
	Status       WorkflowStatus      `json:"status"`
	Revision     int64               `json:"revision"`
	Spec         GraphSpec           `json:"spec"`
	Meta         map[string]string   `json:"meta,omitempty"`
	Nodes        map[string]NodeView `json:"nodes"`
	Order        []string            `json:"order"`
	ExtendsUsed  int                 `json:"extends_used"`
	Usage        Usage               `json:"usage"`
	CreatedAt    time.Time           `json:"created_at"`
	StartedAt    time.Time           `json:"started_at,omitempty"`
	FinishedAt   time.Time           `json:"finished_at,omitempty"`
	Deadline     time.Time           `json:"deadline,omitempty"`
	Error        string              `json:"error,omitempty"`
	CancelReason string              `json:"cancel_reason,omitempty"`
}

// Terminal reports whether the workflow can no longer change.
func (w WorkflowView) Terminal() bool {
	return w.Status == WorkflowCompleted || w.Status == WorkflowFailed || w.Status == WorkflowCancelled
}

// Node returns a deep copy of one projected node.
func (w WorkflowView) Node(id string) (NodeView, bool) {
	n, ok := w.Nodes[id]
	if !ok {
		return NodeView{}, false
	}
	n.Spec = cloneSpec(n.Spec)
	n.Meta = cloneStrings(n.Meta)
	n.Output = rawCopy(n.Output)
	return n, true
}

// Terminal reports whether the node can no longer change.
func (s NodeStatus) Terminal() bool {
	switch s {
	case NodeCompleted, NodeFailed, NodeAborted, NodeSkipped:
		return true
	default:
		return false
	}
}
