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

// reviewStatsResponse adds the retention state to the review-scope counters.
// An older range no longer has per-call audit rows, so model statistics in the
// same view may read zero without meaning the models were never used.
type reviewStatsResponse struct {
	store.ReviewStats
	LLMCallRetentionHours int  `json:"llm_call_retention_hours,omitempty"`
	ModelDetailsPurged    bool `json:"model_details_purged"`
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
		retention := s.platform.Storage.LLMCallRetention
		response.LLMCallRetentionHours = int(retention.Hours())
		if !from.IsZero() && from.Before(s.now().UTC().Add(-retention)) {
			response.ModelDetailsPurged = true
		}
	}
	writeJSON(w, http.StatusOK, response)
}
