// Package store defines persistence records shared by the SQLite implementation
// and its callers.
package store

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrInvalidCursor means an opaque list cursor could not be decoded.
	ErrInvalidCursor = errors.New("store: invalid cursor")
	// ErrContextLocked is retained for callers that still map the old
	// generated-refresh conflict. Generated saves now append an inactive
	// revision instead of returning this error.
	ErrContextLocked = errors.New("store: context document is locked")
)

// LLMCall is one audited provider attempt. Request and Response are
// decompressed JSON only when IncludePayload was requested.
type LLMCall struct {
	ID           string          `json:"id"`
	WorkflowID   string          `json:"workflow_id"`
	NodeID       string          `json:"node_id"`
	Turn         int             `json:"turn"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Request      json.RawMessage `json:"request,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	CacheRead    int64           `json:"cache_read"`
	CacheWrite   int64           `json:"cache_write"`
	LatencyMS    int64           `json:"latency_ms"`
	StopReason   string          `json:"stop_reason,omitempty"`
	ErrorKind    string          `json:"error_kind,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// LLMCallFilter narrows ListLLMCalls.
type LLMCallFilter struct {
	WorkflowID     string
	NodeID         string
	Cursor         string
	Limit          int
	IncludePayload bool
}

// Workspace is the project workspace state row.
type Workspace struct {
	ProjectID     int64      `json:"project_id"`
	State         string     `json:"state"`
	BaseSHA       string     `json:"base_sha,omitempty"`
	IndexSHA      string     `json:"index_sha,omitempty"`
	ContextSHA    string     `json:"context_sha,omitempty"`
	LastRefreshAt *time.Time `json:"last_refresh_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ContextDoc is one generated or manually maintained context document
// revision. Active is stored per (project, kind); Stale is computed from the
// workspace base.
type ContextDoc struct {
	ID         string    `json:"id"`
	ProjectID  int64     `json:"project_id"`
	BaseSHA    string    `json:"base_sha"`
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	Content    string    `json:"content"`
	Locked     bool      `json:"locked"`
	Model      string    `json:"model,omitempty"`
	WorkflowID string    `json:"workflow_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedBy  *int64    `json:"updated_by,omitempty"`
	Active     bool      `json:"active"`
	Stale      bool      `json:"stale"`
}

// ContextDocFilter narrows ListContextDocs.
type ContextDocFilter struct {
	ProjectID int64
	Kind      string
	Cursor    string
	Limit     int
}

// Feedback is one GitLab feedback signal. A nil user is anonymous and must
// still be unique per publication and signal.
type Feedback struct {
	ID            string    `json:"id"`
	ProjectID     int64     `json:"project_id"`
	MRIID         int64     `json:"mr_iid"`
	PublicationID string    `json:"publication_id"`
	FindingID     string    `json:"finding_id,omitempty"`
	Signal        string    `json:"signal"`
	GitlabUserID  *int64    `json:"gitlab_user_id,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// FeedbackFilter narrows ListFeedback and FeedbackTotals. Time filters apply
// to Feedback.ObservedAt; From is inclusive and To is exclusive.
type FeedbackFilter struct {
	ProjectID     *int64
	AccessUserID  *int64
	MRIID         *int64
	PublicationID string
	Signal        string
	From          time.Time
	To            time.Time
	Cursor        string
	Limit         int
}

// FeedbackProviderStats counts observed signals attributed to one provider in
// one review role. A signal can appear for both roles, so these totals are
// attribution counts, not a correctness or accuracy rate.
type FeedbackProviderStats struct {
	Provider string           `json:"provider"`
	Role     string           `json:"role"`
	Total    int64            `json:"total"`
	BySignal map[string]int64 `json:"by_signal"`
}

// FeedbackTotals aggregates feedback signals. ByProvider is additive and
// optional: it is empty when no stored signal resolves to a provider. Total
// remains the raw signal count and is never multiplied by role attribution.
type FeedbackTotals struct {
	Total      int64                   `json:"total"`
	BySignal   map[string]int64        `json:"by_signal"`
	ByProvider []FeedbackProviderStats `json:"by_provider"`
}

// StatsFilter bounds workflow and audit aggregates. To is exclusive.
type StatsFilter struct {
	From      time.Time
	To        time.Time
	ProjectID *int64
	// AccessUserID restricts every aggregate branch to projects granted to that
	// user. Nil is the trusted internal path; a non-positive value matches
	// nothing.
	AccessUserID *int64
}

// ProjectStats is the per-project part of Stats.
type ProjectStats struct {
	ProjectID       int64        `json:"project_id"`
	Runs            int64        `json:"runs"`
	Blocking        int64        `json:"blocking"`
	Published       int64        `json:"published"`
	CostMicros      int64        `json:"cost_micros"`
	DurationSamples int64        `json:"duration_samples"`
	AvgDurationMS   *float64     `json:"avg_duration_ms"`
	P95DurationMS   *float64     `json:"p95_duration_ms"`
	Trend           []DailyStats `json:"trend"`
}

// DailyStats is one UTC day of workflow or started-node activity. A workflow
// is bucketed by created_at, never by completion time. AvgDurationMS and
// P95DurationMS are nil when no paired timestamps yielded a duration.
type DailyStats struct {
	Day             string   `json:"day"`
	Runs            int64    `json:"runs"`
	CostMicros      int64    `json:"cost_micros"`
	DurationSamples int64    `json:"duration_samples"`
	AvgDurationMS   *float64 `json:"avg_duration_ms"`
	P95DurationMS   *float64 `json:"p95_duration_ms"`
}

// ProviderStats aggregates started workflow nodes by persisted provider. Runs
// counts distinct persisted node rows, not LLM turns or retry attempts.
type ProviderStats struct {
	Provider        string       `json:"provider"`
	Runs            int64        `json:"runs"`
	CostMicros      int64        `json:"cost_micros"`
	DurationSamples int64        `json:"duration_samples"`
	AvgDurationMS   *float64     `json:"avg_duration_ms"`
	P95DurationMS   *float64     `json:"p95_duration_ms"`
	Trend           []DailyStats `json:"trend"`
}

// BlockStats aggregates started workflow nodes by node kind. Code nodes
// without a provider still count here.
type BlockStats struct {
	Block           string       `json:"block"`
	Runs            int64        `json:"runs"`
	CostMicros      int64        `json:"cost_micros"`
	DurationSamples int64        `json:"duration_samples"`
	AvgDurationMS   *float64     `json:"avg_duration_ms"`
	P95DurationMS   *float64     `json:"p95_duration_ms"`
	Trend           []DailyStats `json:"trend"`
}

// ModelStats is the per-provider/model part of Stats.
type ModelStats struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Calls        int64    `json:"calls"`
	Errors       int64    `json:"errors"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CostMicros   int64    `json:"cost_micros"`
	AvgLatencyMS *float64 `json:"avg_latency_ms"`
	P50LatencyMS *float64 `json:"p50_latency_ms"`
	ErrorRate    *float64 `json:"error_rate"`
}

// ProviderQualityStats combines a provider's proposer and challenger
// performance. Proposed, Valid, Invalid and Reviewed count findings proposed by
// the provider; Refuted, RefutedReviewed and FalseRefuted count findings
// challenged by the provider. Only human "valid" and "invalid" labels enter
// Reviewed and RefutedReviewed; unannotated and "uncertain" never do. Ratios
// stay nil when their denominator is zero.
type ProviderQualityStats struct {
	Provider        string   `json:"provider"`
	Proposed        int64    `json:"proposed"`
	Reviewed        int64    `json:"reviewed"`
	Valid           int64    `json:"valid"`
	Invalid         int64    `json:"invalid"`
	Accuracy        *float64 `json:"accuracy"`
	Overturned      int64    `json:"overturned"`
	OverturnedRatio *float64 `json:"overturned_ratio"`
	Refuted         int64    `json:"refuted"`
	RefutedReviewed int64    `json:"refuted_reviewed"`
	FalseRefuted    int64    `json:"false_refuted"`
	FalseKillRate   *float64 `json:"false_kill_rate"`
}

// Stats is the M6 aggregate view. Ratios and durations stay nil when there is
// no denominator instead of pretending to be zero.
type Stats struct {
	Runs              int64                  `json:"runs"`
	Blocking          int64                  `json:"blocking"`
	BlockingRatio     *float64               `json:"blocking_ratio"`
	Published         int64                  `json:"published"`
	OverturnedRatio   *float64               `json:"overturned_ratio"`
	AvgDurationMS     *float64               `json:"avg_duration_ms"`
	P95DurationMS     *float64               `json:"p95_duration_ms"`
	DurationSamples   int64                  `json:"duration_samples"`
	CostMicros        int64                  `json:"cost_micros"`
	ByProject         []ProjectStats         `json:"by_project"`
	ByModel           []ModelStats           `json:"by_model"`
	ByProvider        []ProviderStats        `json:"by_provider"`
	ByBlock           []BlockStats           `json:"by_block"`
	QualityByProvider []ProviderQualityStats `json:"quality_by_provider"`
	Trend             []DailyStats           `json:"trend"`
	Decisions         map[string]int64       `json:"decisions"`
	// Feedback is a pointer so an older response without the field stays
	// distinguishable from a counted empty range.
	Feedback *FeedbackTotals `json:"feedback,omitempty"`
}
