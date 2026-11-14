package server

import (
	"context"
	"net/http"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// reviewStatsStore is implemented by the SQLite store. The narrow interface
// keeps the endpoint independent of the broader Store surface.
type reviewStatsStore interface {
	ReviewStats(context.Context, store.ReviewStatsFilter) (store.ReviewStats, error)
}

// reviewStatsResponse adds the retention window to the review-scope counters.
// ModelDetailsPurged is computed by the store with the same data-driven rule
// as GET /api/v1/stats and is embedded in store.ReviewStats; the retention
// hours stay descriptive and never drive that boolean (#466).
type reviewStatsResponse struct {
	store.ReviewStats
	LLMCallRetentionHours int `json:"llm_call_retention_hours,omitempty"`
}

func (s *Server) handleReviewStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	if !s.requireAccess(w, r) {
		return
	}
	from, err := queryTime(r, "from")
	if err != nil {
		validationFailed(w, r, "from", err.Error())
		return
	}
	to, err := queryTime(r, "to")
	if err != nil {
		validationFailed(w, r, "to", err.Error())
		return
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		validationFailed(w, r, "from", "from must be before to")
		return
	}
	projectID, err := queryInt64(r, "project_id")
	if err != nil {
		validationFailed(w, r, "project_id", err.Error())
		return
	}
	if projectID != nil {
		if _, ok := s.requireProjectAccess(w, r, *projectID, false); !ok {
			return
		}
	}
	st, ok := s.store.(reviewStatsStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "review statistics are unavailable", nil)
		return
	}
	stats, err := st.ReviewStats(r.Context(), store.ReviewStatsFilter{
		From:         from,
		To:           to,
		ProjectID:    projectID,
		AccessUserID: s.projectScopeUserID(r),
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if stats.ByProject == nil {
		stats.ByProject = []store.ReviewProjectStats{}
	}
	if stats.Trend == nil {
		stats.Trend = []store.ReviewDailyStats{}
	}
	response := reviewStatsResponse{ReviewStats: stats}
	if s.platform != nil && s.platform.Storage.LLMCallRetention > 0 {
		response.LLMCallRetentionHours = int(s.platform.Storage.LLMCallRetention.Hours())
	}
	writeJSON(w, http.StatusOK, response)
}
