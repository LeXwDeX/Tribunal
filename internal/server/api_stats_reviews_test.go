package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func (f *fakeReviewStore) ReviewStats(ctx context.Context, filter store.ReviewStatsFilter) (store.ReviewStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReviewStats = filter
	return f.reviewStats, nil
}

func TestReviewStatsEndpointUsesTheReviewScope(t *testing.T) {
	fake := newFakeReviewStore()
	blocking := 1.0 / 3.0
	incomplete := 2.0 / 3.0
	all := 0.25
	fake.reviewStats = store.ReviewStats{
		Runs: 3, Completed: 2, Incomplete: 2, Blocking: 1, Failed: 1,
		BlockingRatio: &blocking, IncompleteRatio: &incomplete,
		AllRuns: 4, AllBlocking: 1, AllBlockingRatio: &all,
		ByProject: []store.ReviewProjectStats{{ProjectID: 42, Runs: 3, Incomplete: 2, Blocking: 1}},
		Trend:     []store.ReviewDailyStats{{Day: "2026-10-20", Runs: 3, Incomplete: 2, Blocking: 1}},
	}
	platform := &config.Config{}
	platform.Storage.LLMCallRetention = 720 * time.Hour
	env := newM6Env(t, m6Options{store: fake, platform: platform})
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet,
		"/api/v1/stats/reviews?project_id=42&from=2026-09-01T00:00:00Z&to=2026-11-01T00:00:00Z", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if body["runs"] != float64(3) || body["incomplete"] != float64(2) || body["blocking"] != float64(1) {
		t.Fatalf("review stats = %v", body)
	}
	if body["blocking_ratio"] != blocking || body["incomplete_ratio"] != incomplete {
		t.Fatalf("review ratios = %v / %v", body["blocking_ratio"], body["incomplete_ratio"])
	}
	if body["all_runs"] != float64(4) || body["all_blocking"] != float64(1) || body["all_blocking_ratio"] != all {
		t.Fatalf("mixed denominator = %v", body)
	}
	if body["llm_call_retention_hours"] != float64(720) {
		t.Fatalf("retention = %v", body["llm_call_retention_hours"])
	}
	if body["model_details_purged"] != true {
		t.Fatalf("model_details_purged = %v, want true for a range older than 720h", body["model_details_purged"])
	}
	if fake.lastReviewStats.ProjectID == nil || *fake.lastReviewStats.ProjectID != 42 {
		t.Fatalf("filter = %+v", fake.lastReviewStats)
	}
	if fake.lastReviewStats.From.IsZero() || fake.lastReviewStats.To.IsZero() {
		t.Fatalf("time filter = %+v", fake.lastReviewStats)
	}
	if fake.lastReviewStats.AccessUserID != nil {
		t.Fatalf("platform admin filter should use the trusted path: %+v", fake.lastReviewStats)
	}

	viewer := env.loginAs("code-viewer")
	viewerResponse := viewer.request(http.MethodGet, "/api/v1/stats/reviews", nil, nil)
	assertStatus(t, viewerResponse, http.StatusOK)
	if fake.lastReviewStats.AccessUserID == nil || *fake.lastReviewStats.AccessUserID <= 0 {
		t.Fatalf("viewer access filter missing: %+v", fake.lastReviewStats)
	}

	recent := client.request(http.MethodGet,
		"/api/v1/stats/reviews?from="+time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339)+
			"&to="+time.Now().UTC().Format(time.RFC3339), nil, nil)
	assertStatus(t, recent, http.StatusOK)
	if env.decode(t, recent)["model_details_purged"] != false {
		t.Fatalf("recent range reported purged details: %s", recent.body)
	}

	invalid := client.request(http.MethodGet, "/api/v1/stats/reviews?from=2026-11-01T00:00:00Z&to=2026-10-01T00:00:00Z", nil, nil)
	assertStatus(t, invalid, http.StatusBadRequest)
}
