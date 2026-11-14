package store

import "time"

// ReviewStatsFilter selects the workflow-created window of review-scope
// statistics. The endpoint is intentionally review-only: onboarding runs share
// the workflows table but have no review verdict, so mixing them distorted the
// blocking ratio.
type ReviewStatsFilter struct {
	From      time.Time
	To        time.Time
	ProjectID *int64
	// AccessUserID restricts the aggregate to projects granted to that user.
	AccessUserID *int64
}

// ReviewStats counts review workflows. Runs, Blocking and Incomplete use the
// review denominator; AllRuns and AllBlocking keep the mixed denominator so
// the console can show both ratios without recomputing.
type ReviewStats struct {
	Runs             int64                `json:"runs"`
	Completed        int64                `json:"completed"`
	Incomplete       int64                `json:"incomplete"`
	Blocking         int64                `json:"blocking"`
	Failed           int64                `json:"failed"`
	Cancelled        int64                `json:"cancelled"`
	BlockingRatio    *float64             `json:"blocking_ratio"`
	IncompleteRatio  *float64             `json:"incomplete_ratio"`
	AllRuns          int64                `json:"all_runs"`
	AllBlocking      int64                `json:"all_blocking"`
	AllBlockingRatio *float64             `json:"all_blocking_ratio"`
	ByProject        []ReviewProjectStats `json:"by_project"`
	Trend            []ReviewDailyStats   `json:"trend"`
}

// ReviewProjectStats is the per-project review slice.
type ReviewProjectStats struct {
	ProjectID  int64 `json:"project_id"`
	Runs       int64 `json:"runs"`
	Incomplete int64 `json:"incomplete"`
	Blocking   int64 `json:"blocking"`
}

// ReviewDailyStats buckets review workflows by created_at (UTC day).
type ReviewDailyStats struct {
	Day        string `json:"day"`
	Runs       int64  `json:"runs"`
	Incomplete int64  `json:"incomplete"`
	Blocking   int64  `json:"blocking"`
}
