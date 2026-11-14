package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestWebhookDeliveriesEndpointGroupsByMR(t *testing.T) {
	fake := newFakeReviewStore()
	fake.deliverySummaries = []store.WebhookDeliverySummary{{
		ProjectID: 42, MRIID: 7,
		Deliveries: 4, Ignored: 3, Rejected: 1, RejectedInvalidSecret: 1,
		LastReceivedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		Reasons:        map[string]int64{"unsupported_action": 2, "branch_filtered": 1},
	}}
	env := newM6Env(t, m6Options{store: fake})
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/webhook-deliveries", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	rows, ok := body["deliveries"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("deliveries = %v", body["deliveries"])
	}
	row := rows[0].(map[string]any)
	if row["project_id"] != float64(42) || row["mr_iid"] != float64(7) {
		t.Fatalf("row identity = %v", row)
	}
	if row["ignored"] != float64(3) || row["rejected"] != float64(1) ||
		row["rejected_invalid_secret"] != float64(1) {
		t.Fatalf("row counts = %v", row)
	}
	reason := row["reasons"].(map[string]any)
	if reason["unsupported_action"] != float64(2) || reason["branch_filtered"] != float64(1) {
		t.Fatalf("reasons = %v", reason)
	}
	if _, present := body["next_cursor"]; !present {
		t.Fatalf("next_cursor missing: %v", body)
	}
	// The platform administrator keeps the trusted unscoped path.
	if fake.lastDeliveryFilter.AccessUserID != nil {
		t.Fatalf("admin access filter = %+v", fake.lastDeliveryFilter)
	}
}

func TestWebhookDeliveriesEndpointScopesViewersAndRejectsForeignProject(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	viewer := env.loginAs("code-viewer")

	response := viewer.request(http.MethodGet, "/api/v1/webhook-deliveries", nil, nil)
	assertStatus(t, response, http.StatusOK)
	if fake.lastDeliveryFilter.AccessUserID == nil || *fake.lastDeliveryFilter.AccessUserID <= 0 {
		t.Fatalf("viewer access filter missing: %+v", fake.lastDeliveryFilter)
	}

	foreign := viewer.request(http.MethodGet, "/api/v1/webhook-deliveries?project_id=42", nil, nil)
	assertStatus(t, foreign, http.StatusNotFound)
}

func TestWebhookDeliveriesEndpointMapsInvalidCursor(t *testing.T) {
	fake := newFakeReviewStore()
	fake.deliveryErr = store.ErrInvalidCursor
	env := newM6Env(t, m6Options{store: fake})
	response := env.businessClient().request(http.MethodGet, "/api/v1/webhook-deliveries?cursor=bogus", nil, nil)
	assertStatus(t, response, http.StatusBadRequest)
}

func TestWebhookDeliveriesEndpointValidatesFilters(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	client := env.businessClient()
	for _, path := range []string{
		"/api/v1/webhook-deliveries?project_id=abc",
		"/api/v1/webhook-deliveries?mr_iid=abc",
		"/api/v1/webhook-deliveries?limit=0",
	} {
		response := client.request(http.MethodGet, path, nil, nil)
		assertStatus(t, response, http.StatusBadRequest)
	}
}
