package store

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrSuperseded means a review workflow request was built for a generation or
// head that is no longer current. It is distinct from ErrConflict so callers
// can treat "a newer push won" as an expected outcome.
var ErrSuperseded = errors.New("store: review superseded")

// ReviewControl is the durable per-MR review intent. Generation increments on
// every accepted review.run or review.cancel enqueue, including a replacement
// of a queued job. A duplicate delivery never increments it.
//
// CandidateSeq is the high-water mark of allocated deferred review events.
// BarrierSeq is the highest candidate sequence already consumed or invalidated
// by an immediate run/cancel enqueue. A pending candidate below the barrier is
// stale; a candidate above it may still activate.
type ReviewControl struct {
	ProjectID    int64     `json:"project_id"`
	MRIID        int64     `json:"mr_iid"`
	Generation   int64     `json:"generation"`
	HeadSHA      string    `json:"head_sha,omitempty"`
	Cancelled    bool      `json:"cancelled"`
	CandidateSeq int64     `json:"candidate_seq"`
	BarrierSeq   int64     `json:"barrier_seq"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ReviewCandidate is one deferred automatic review event. Ingress allocates
// its CandidateSeq inside the same transaction as the delivery and job write;
// activating it later owns the generation bump and review.run enqueue.
type ReviewCandidate struct {
	ProjectID   int64
	MRIID       int64
	HeadSHA     string
	Payload     json.RawMessage
	MaxAttempts int
}

// ReviewCandidateActivation identifies the claimed candidate job and the
// durable event it must activate. The job lease and payload are verified by
// the store, so a stale handler cannot bump the generation.
type ReviewCandidateActivation struct {
	JobID        string
	LeaseOwner   string
	Attempts     int
	ProjectID    int64
	MRIID        int64
	HeadSHA      string
	CandidateSeq int64
}

// ReviewCommand is one authenticated review.run or review.cancel enqueue.
// The store derives the job kind and key, and assigns the generation inside
// the same transaction that writes the job. Payload must be a JSON object with
// job-specific fields (for example trigger, profile, full); it must not carry
// a generation because the store owns that field. ProjectID, MRIID and HeadSHA
// are always written by the store and override identical payload keys.
//
// HeadSHA is required for run and must be empty for cancel. Cancel preserves
// the previously recorded head so publication can still detect a stale MR.
type ReviewCommand struct {
	ProjectID   int64
	MRIID       int64
	HeadSHA     string
	Cancel      bool
	Payload     json.RawMessage
	MaxAttempts int
}

// ReviewEnqueueResult reports the durable outcome of an ingress call.
// Generation is zero when no review command was written (ignored delivery).
type ReviewEnqueueResult struct {
	JobID      string
	Duplicate  bool
	Generation int64
}

const (
	WorkflowPublicationPending   = "pending"
	WorkflowPublicationPublished = "published"
	WorkflowPublicationSkipped   = "skipped"
	// WorkflowPublicationFailed is a terminal publication failure. It stays
	// separate from workflow execution and needs an explicit republish.
	WorkflowPublicationFailed = "failed"
	// WorkflowPublicationUnknownReason explains why an unresolved discussion
	// must stay pending even when its workflow becomes stale or is cancelled.
	WorkflowPublicationUnknownReason = "discussion outcome unknown"
)

// WorkflowDossier is the sanitized input snapshot needed to resume or publish
// a workflow. Data is decompressed JSON on read and compressed at rest.
//
// The store validates bounded size and JSON syntax only. It cannot recognize
// every credential, so the caller must sanitize Data (no project tokens,
// provider keys, MCP secrets, signed URLs, or absolute temporary paths) before
// calling SaveWorkflowDossier. Events and workflow metadata should carry only
// small hashes/pointers, not a second copy of this payload.
type WorkflowDossier struct {
	WorkflowID       string          `json:"workflow_id"`
	ProjectID        int64           `json:"-"`
	JobID            string          `json:"job_id,omitempty"`
	Generation       int64           `json:"generation"`
	Version          int             `json:"version"`
	Hash             string          `json:"hash"`
	Data             json.RawMessage `json:"data"`
	PublicationState string          `json:"publication_state"`
	// PublicationError is a bounded, caller-sanitized reason code or generic
	// text. It must never contain upstream response bodies or credentials.
	PublicationError string    `json:"publication_error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// PendingPublication is one terminal workflow whose dossier has not been
// published yet. It deliberately omits the dossier body. Status selects the
// publisher path: completed uses the normal review, failed uses the failure
// summary.
type PendingPublication struct {
	WorkflowID string    `json:"workflow_id"`
	JobID      string    `json:"job_id,omitempty"`
	ProjectID  int64     `json:"project_id"`
	MRIID      int64     `json:"mr_iid"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	Generation int64     `json:"generation"`
	Status     string    `json:"status"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Review status and reaction states are durable side-effect goals. They are
// separate from publications because a queued status exists before any
// workflow, and reactions are keyed by source note rather than workflow.
const (
	ReviewStatusQueued     = "queued"
	ReviewStatusRunning    = "running"
	ReviewStatusPublishing = "publishing"
	ReviewStatusPass       = "PASS"
	ReviewStatusBlocking   = "BLOCKING"
	ReviewStatusIncomplete = "INCOMPLETE"
	ReviewStatusFailed     = "failed"
	ReviewStatusCancelled  = "cancelled"
	ReviewStatusStale      = "stale"

	ReviewReactionEyes = "eyes"
	ReviewReactionPass = "white_check_mark"
	ReviewReactionFail = "x"
	ReviewReactionNone = "none"
)

// ReviewStatus is the desired status note for one merge request. Generation
// and HeadSHA fence a late worker; Version/AppliedVersion let a retry detect
// that a newer desired state replaced the one it started to publish.
type ReviewStatus struct {
	ProjectID      int64     `json:"project_id"`
	MRIID          int64     `json:"mr_iid"`
	Generation     int64     `json:"generation"`
	HeadSHA        string    `json:"head_sha,omitempty"`
	Attempt        int       `json:"attempt"`
	Profile        string    `json:"profile,omitempty"`
	State          string    `json:"state"`
	Detail         string    `json:"detail,omitempty"`
	NoteID         int64     `json:"note_id,omitempty"`
	BodyHash       string    `json:"body_hash,omitempty"`
	Version        int       `json:"version"`
	AppliedVersion int       `json:"applied_version"`
	LastError      string    `json:"last_error,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ReviewStatusGoal is one desired status write. Attempt zero means "the
// current attempt"; the store uses the stored attempt for monotonic ordering.
type ReviewStatusGoal struct {
	ProjectID  int64
	MRIID      int64
	Generation int64
	Attempt    int
	HeadSHA    string
	Profile    string
	State      string
	Detail     string
}

// ReviewReaction is the desired reaction on one source note. Desired none
// removes only the bot's own working/terminal awards.
type ReviewReaction struct {
	ProjectID      int64     `json:"project_id"`
	MRIID          int64     `json:"mr_iid"`
	NoteID         int64     `json:"note_id"`
	Generation     int64     `json:"generation"`
	Desired        string    `json:"desired"`
	Version        int       `json:"version"`
	AppliedVersion int       `json:"applied_version"`
	LastError      string    `json:"last_error,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ReviewReactionGoal is one desired reaction write.
type ReviewReactionGoal struct {
	ProjectID  int64
	MRIID      int64
	NoteID     int64
	Generation int64
	Desired    string
}
