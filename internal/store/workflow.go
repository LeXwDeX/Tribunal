// Package store defines persistence records shared by the SQLite implementation
// and its callers.
package store

import (
	"encoding/json"
	"time"
)

// Workflow is the projected workflow read model. Node specs and budget live in
// the event log.
type Workflow struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	ProjectID    int64  `json:"project_id"`
	MRIID        *int64 `json:"mr_iid,omitempty"`
	HeadSHA      string `json:"head_sha,omitempty"`
	BaseSHA      string `json:"base_sha,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	Profile      string `json:"profile,omitempty"`
	Trigger      string `json:"trigger"`
	TriggeredBy  *int64 `json:"triggered_by,omitempty"`
	Status       string `json:"status"`
	GraphRev     int64  `json:"graph_rev"`
	ExtendsUsed  int    `json:"extends_used"`
	Verdict      string `json:"verdict,omitempty"`
	CancelReason string `json:"cancel_reason,omitempty"`
	Error        string `json:"error,omitempty"`
	// PublicationState reports GitLab publication independently from workflow
	// execution: pending, published, skipped, or failed. It is empty for
	// workflows without a dossier.
	PublicationState string `json:"publication_state,omitempty"`
	// PublicationError is a bounded, sanitized reason. It is never a raw
	// upstream error.
	PublicationError string     `json:"publication_error,omitempty"`
	TokensIn         int64      `json:"tokens_in"`
	TokensOut        int64      `json:"tokens_out"`
	CostMicros       int64      `json:"cost_micros"`
	CreatedAt        time.Time  `json:"created_at"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

// WorkflowFilter narrows ListWorkflows. Zero values do not filter. Since is an
// inclusive created_at lower bound.
type WorkflowFilter struct {
	ProjectID *int64
	// AccessUserID restricts rows to projects granted to that user. Nil is the
	// trusted internal path; a non-positive value matches nothing.
	AccessUserID *int64
	MRIID        *int64
	Kind         string
	Status       string
	Verdict      string
	// PublicationState filters on the dossier publication state. Empty does
	// not filter; workflows without a dossier never match a non-empty value.
	PublicationState string
	Profile          string
	Trigger          string
	HeadSHA          string
	Since            time.Time
}

// WorkflowNode is one projected node row.
type WorkflowNode struct {
	WorkflowID string          `json:"workflow_id"`
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Required   bool            `json:"required"`
	DependsOn  []string        `json:"depends_on"`
	Params     json.RawMessage `json:"params"`
	Attempt    int             `json:"attempt"`
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	TokensIn   int64           `json:"tokens_in"`
	TokensOut  int64           `json:"tokens_out"`
	CostMicros int64           `json:"cost_micros"`
	AddedRev   int64           `json:"added_rev"`
	QueuedAt   *time.Time      `json:"queued_at,omitempty"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// Publication records one object published to GitLab. The unique key
// (ProjectID, MRIID, Kind, PublishKey) makes retries idempotent.
type Publication struct {
	ID          string    `json:"id"`
	ProjectID   int64     `json:"project_id"`
	MRIID       int64     `json:"mr_iid"`
	Kind        string    `json:"kind"`
	PublishKey  string    `json:"publish_key"`
	GitlabID    string    `json:"gitlab_id,omitempty"`
	WorkflowID  string    `json:"workflow_id"`
	Fingerprint string    `json:"fingerprint"`
	BodyHash    string    `json:"body_hash,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Finding is the normalized review finding and its challenge/decision fields.
// ChallengeEvidence is opaque JSON supplied by the challenger; the store never
// holds model transcripts here.
type Finding struct {
	ID                string          `json:"id"`
	WorkflowID        string          `json:"workflow_id"`
	OriginNode        string          `json:"origin_node"`
	OriginIndex       int             `json:"origin_index"`
	OriginProvider    string          `json:"origin_provider,omitempty"`
	Severity          string          `json:"severity"`
	Category          string          `json:"category"`
	Path              string          `json:"path,omitempty"`
	Line              *int            `json:"line,omitempty"`
	Side              string          `json:"side,omitempty"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	Evidence          string          `json:"evidence,omitempty"`
	Confidence        string          `json:"confidence,omitempty"`
	DedupeGroup       string          `json:"dedupe_group,omitempty"`
	Votes             float64         `json:"votes"`
	ChallengeStatus   string          `json:"challenge_status,omitempty"`
	ChallengeNode     string          `json:"challenge_node,omitempty"`
	ChallengeEvidence json.RawMessage `json:"challenge_evidence,omitempty"`
	Decision          string          `json:"decision,omitempty"`
	DecisionReason    string          `json:"decision_reason,omitempty"`
	FinalSeverity     string          `json:"final_severity,omitempty"`
	PositionOK        *bool           `json:"position_ok,omitempty"`
	PublishKey        string          `json:"publish_key,omitempty"`
	Annotation        string          `json:"annotation"`
	AnnotatedBy       string          `json:"annotated_by,omitempty"`
	AnnotatedAt       *time.Time      `json:"annotated_at,omitempty"`
}

// Manual finding annotation values. An empty value means the finding has not
// been reviewed by a human yet.
const (
	FindingAnnotationNone      = ""
	FindingAnnotationValid     = "valid"
	FindingAnnotationInvalid   = "invalid"
	FindingAnnotationUncertain = "uncertain"
)

// ValidFindingAnnotation reports whether value is a supported manual
// annotation. The store validates it again so a direct caller cannot persist an
// unknown label.
func ValidFindingAnnotation(value string) bool {
	switch value {
	case FindingAnnotationNone, FindingAnnotationValid, FindingAnnotationInvalid, FindingAnnotationUncertain:
		return true
	default:
		return false
	}
}

// FindingFilter narrows ListFindings. WorkflowID is required.
type FindingFilter struct {
	WorkflowID string
	Decision   string
	Severity   string
}
