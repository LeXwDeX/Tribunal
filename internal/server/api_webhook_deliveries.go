package server

import (
	"context"
	"net/http"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// webhookDeliveryStore is implemented by the SQLite store. The narrow
// interface keeps the read-only delivery view independent of the full Store
// surface and of older test doubles.
type webhookDeliveryStore interface {
	ListWebhookDeliverySummaries(ctx context.Context, filter store.WebhookDeliveryFilter, cursor string, limit int) ([]store.WebhookDeliverySummary, string, error)
}

// handleListWebhookDeliveries exposes why merge requests were or were not
// reviewed: one row per project and MR, with the fixed ignore reasons, the last
// delivery time and the rejected/invalid_secret count. It never returns the
// upstream request body or credentials, and the access filter runs before
// grouping, so a caller sees only projects they may read (#466).
func (s *Server) handleListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	if !s.requireAccess(w, r) {
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
	mrIID, err := queryInt64(r, "mr_iid")
	if err != nil {
		validationFailed(w, r, "mr_iid", err.Error())
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	st, ok := s.store.(webhookDeliveryStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "webhook deliveries are unavailable", nil)
		return
	}
	deliveries, next, err := st.ListWebhookDeliverySummaries(r.Context(), store.WebhookDeliveryFilter{
		ProjectID:    projectID,
		MRIID:        mrIID,
		AccessUserID: s.projectScopeUserID(r),
	}, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if deliveries == nil {
		deliveries = []store.WebhookDeliverySummary{}
	}
	for i := range deliveries {
		if deliveries[i].Reasons == nil {
			deliveries[i].Reasons = map[string]int64{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deliveries":  deliveries,
		"next_cursor": next,
	})
}
