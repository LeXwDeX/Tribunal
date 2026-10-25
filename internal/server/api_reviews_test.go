package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func TestListReviewsAppliesFiltersBeforePaging(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	client := env.businessClient()

	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for i, projectID := range []int64{42, 42, 42, 43, 42} {
		mrIID := int64(7)
		fake.workflows = append(fake.workflows, store.Workflow{
			ID:        "wf-" + string(rune('a'+i)),
			Kind:      "review",
			ProjectID: projectID,
			MRIID:     &mrIID,
			Trigger:   "mr_update",
			Status:    "completed",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	// An onboarding workflow must not appear in the review list.
	fake.workflows = append(fake.workflows, store.Workflow{
		ID: "onboard", Kind: "onboarding", ProjectID: 42, Trigger: "register",
		Status: "completed", CreatedAt: base.Add(time.Hour),
	})

	first := client.request(http.MethodGet, "/api/v1/reviews?project_id=42&status=completed&limit=1", nil, nil)
	assertStatus(t, first, http.StatusOK)
	body := env.decode(t, first)
	reviews := body["reviews"].([]any)
	if len(reviews) != 1 {
		t.Fatalf("first page size = %d, body=%s", len(reviews), first.body)
	}
	if reviews[0].(map[string]any)["id"] != "wf-e" {
		t.Fatalf("first page item = %v", reviews[0])
	}
	next, _ := body["next_cursor"].(string)
	if next == "" {
		t.Fatal("missing next_cursor")
	}
	if fake.lastFilter.ProjectID == nil || *fake.lastFilter.ProjectID != 42 ||
		fake.lastFilter.Kind != "review" || fake.lastFilter.Status != "completed" {
		t.Fatalf("filter passed to store = %+v", fake.lastFilter)
	}
	if fake.lastLimit != 1 {
		t.Fatalf("limit passed to store = %d", fake.lastLimit)
	}

	second := client.request(http.MethodGet, "/api/v1/reviews?project_id=42&status=completed&limit=1&cursor="+next, nil, nil)
	assertStatus(t, second, http.StatusOK)
	secondBody := env.decode(t, second)
	secondReviews := secondBody["reviews"].([]any)
	if len(secondReviews) != 1 || secondReviews[0].(map[string]any)["id"] != "wf-c" {
		t.Fatalf("second page = %v", secondReviews)
	}
	if secondBody["next_cursor"] == "" {
		t.Fatal("expected a third page cursor")
	}
	third := client.request(http.MethodGet, "/api/v1/reviews?project_id=42&status=completed&limit=1&cursor="+secondBody["next_cursor"].(string), nil, nil)
	assertStatus(t, third, http.StatusOK)
	thirdBody := env.decode(t, third)
	if id := thirdBody["reviews"].([]any)[0].(map[string]any)["id"]; id != "wf-b" {
		t.Fatalf("third page item = %v", id)
	}

	final := client.request(http.MethodGet, "/api/v1/reviews?project_id=42&status=completed&limit=1&cursor="+thirdBody["next_cursor"].(string), nil, nil)
	assertStatus(t, final, http.StatusOK)
	finalBody := env.decode(t, final)
	if id := finalBody["reviews"].([]any)[0].(map[string]any)["id"]; id != "wf-a" {
		t.Fatalf("final page item = %v", id)
	}
	if finalBody["next_cursor"] != "" {
		t.Fatalf("unexpected final next_cursor = %v", finalBody["next_cursor"])
	}

	byProfile := client.request(http.MethodGet, "/api/v1/reviews?profile=deep", nil, nil)
	assertStatus(t, byProfile, http.StatusOK)
	if n := len(env.decode(t, byProfile)["reviews"].([]any)); n != 0 {
		t.Fatalf("profile filter ignored: %d", n)
	}
	if fake.lastFilter.Profile != "deep" {
		t.Fatalf("profile filter = %q", fake.lastFilter.Profile)
	}
}

func TestListReviewsRejectsInvalidQueries(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	client := env.businessClient()

	for _, path := range []string{
		"/api/v1/reviews?cursor=not-base64",
		"/api/v1/reviews?project_id=abc",
		"/api/v1/reviews?since=yesterday",
		"/api/v1/reviews?limit=0",
		"/api/v1/reviews?limit=500",
	} {
		response := client.request(http.MethodGet, path, nil, nil)
		if response.status != http.StatusBadRequest {
			t.Fatalf("%s status = %d, body=%s", path, response.status, response.body)
		}
	}
}

func TestReviewDetailGraphNodeAndCalls(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	client := env.businessClient()

	detail := client.request(http.MethodGet, "/api/v1/reviews/wf-1", nil, nil)
	assertStatus(t, detail, http.StatusOK)
	body := env.decode(t, detail)
	if body["verdict"] != "BLOCKING" || body["status"] != "completed" {
		t.Fatalf("workflow fields = %v", body)
	}
	if body["duration_ms"] != float64(120000) {
		t.Fatalf("duration_ms = %v", body["duration_ms"])
	}
	summary := body["findings_summary"].(map[string]any)
	if summary["published"] != float64(1) || summary["marked"] != float64(1) || summary["dropped"] != float64(1) {
		t.Fatalf("findings summary = %v", summary)
	}
	bySeverity := summary["by_severity"].(map[string]any)
	if bySeverity["P1"] != float64(1) || bySeverity["P2"] != float64(2) {
		t.Fatalf("by_severity = %v", bySeverity)
	}
	usage := body["usage"].(map[string]any)
	if usage["tokens_in"] != float64(1000) || usage["cost_micros"] != float64(5000) {
		t.Fatalf("usage = %v", usage)
	}
	publications := body["publications"].(map[string]any)
	if publications["summary_note_id"] != float64(99102) || publications["discussions"] != float64(1) {
		t.Fatalf("publications = %v", publications)
	}
	nodes := body["nodes"].([]any)
	if len(nodes) != 3 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	firstNode := nodes[0].(map[string]any)
	if firstNode["id"] != "plan" || firstNode["duration_ms"] != float64(8000) {
		t.Fatalf("plan node = %v", firstNode)
	}

	graph := client.request(http.MethodGet, "/api/v1/reviews/wf-1/graph", nil, nil)
	assertStatus(t, graph, http.StatusOK)
	graphBody := env.decode(t, graph)
	edges := graphBody["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("edges = %v", edges)
	}
	if edges[0].(map[string]any)["from"] != "plan" || edges[0].(map[string]any)["to"] != "review.std.p1" {
		t.Fatalf("edge = %v", edges[0])
	}

	node := client.request(http.MethodGet, "/api/v1/reviews/wf-1/nodes/review.std.p1", nil, nil)
	assertStatus(t, node, http.StatusOK)
	nodeBody := env.decode(t, node)
	if nodeBody["id"] != "review.std.p1" || nodeBody["kind"] != "review.standards" {
		t.Fatalf("dotted node id was not preserved: %v", nodeBody)
	}
	if nodeBody["usage"].(map[string]any)["tokens_in"] != float64(200) {
		t.Fatalf("node usage = %v", nodeBody["usage"])
	}

	missing := client.request(http.MethodGet, "/api/v1/reviews/wf-1/nodes/review", nil, nil)
	assertStatus(t, missing, http.StatusNotFound)

	calls := client.request(http.MethodGet, "/api/v1/reviews/wf-1/nodes/plan/calls", nil, nil)
	assertStatus(t, calls, http.StatusOK)
	callsBody := env.decode(t, calls)
	if len(callsBody["calls"].([]any)) != 1 {
		t.Fatalf("calls = %v", callsBody)
	}
	call := callsBody["calls"].([]any)[0].(map[string]any)
	if _, present := call["request"]; present {
		t.Fatalf("payload expanded without include_payload: %v", call)
	}
	expanded := client.request(http.MethodGet, "/api/v1/reviews/wf-1/nodes/plan/calls?include_payload=true", nil, nil)
	expandedBody := env.decode(t, expanded)
	expandedCall := expandedBody["calls"].([]any)[0].(map[string]any)
	if expandedCall["request"] == nil {
		t.Fatalf("payload missing with include_payload: %v", expandedCall)
	}
	if !fake.lastCallFilter.IncludePayload || fake.lastCallFilter.NodeID != "plan" {
		t.Fatalf("call filter = %+v", fake.lastCallFilter)
	}
}

func TestReviewFindingsFiltersAndCursor(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	client := env.businessClient()

	drop := client.request(http.MethodGet, "/api/v1/reviews/wf-1/findings?decision=drop", nil, nil)
	assertStatus(t, drop, http.StatusOK)
	dropBody := env.decode(t, drop)
	items := dropBody["findings"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "f3" {
		t.Fatalf("decision filter = %v", items)
	}

	p1 := client.request(http.MethodGet, "/api/v1/reviews/wf-1/findings?severity=P1", nil, nil)
	assertStatus(t, p1, http.StatusOK)
	if n := len(env.decode(t, p1)["findings"].([]any)); n != 1 {
		t.Fatalf("severity filter returned %d", n)
	}

	first := client.request(http.MethodGet, "/api/v1/reviews/wf-1/findings?limit=2", nil, nil)
	assertStatus(t, first, http.StatusOK)
	firstBody := env.decode(t, first)
	if len(firstBody["findings"].([]any)) != 2 {
		t.Fatalf("first page = %v", firstBody)
	}
	next, _ := firstBody["next_cursor"].(string)
	if next == "" {
		t.Fatal("missing findings cursor")
	}
	second := client.request(http.MethodGet, "/api/v1/reviews/wf-1/findings?limit=2&cursor="+next, nil, nil)
	assertStatus(t, second, http.StatusOK)
	secondItems := env.decode(t, second)["findings"].([]any)
	if len(secondItems) != 1 || secondItems[0].(map[string]any)["id"] != "f3" {
		t.Fatalf("second page = %v", secondItems)
	}
}

func TestTriggerCancelAndRepublish(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	client := env.businessClient()

	trigger := client.json(http.MethodPost, "/api/v1/projects/42/merge_requests/7/reviews",
		map[string]any{"profile": "deep", "full": true})
	assertStatus(t, trigger, http.StatusAccepted)
	if got := env.decode(t, trigger)["job_id"]; got != "job-start" {
		t.Fatalf("trigger job = %v", got)
	}
	if len(env.reviews.startCalls) != 1 {
		t.Fatalf("start calls = %v", env.reviews.startCalls)
	}
	start := env.reviews.startCalls[0]
	if start.projectID != 42 || start.mrIID != 7 || start.profile != "deep" || !start.full {
		t.Fatalf("start call = %+v", start)
	}

	empty := client.request(http.MethodPost, "/api/v1/projects/42/merge_requests/7/reviews", nil,
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, empty, http.StatusAccepted)

	cancel := client.request(http.MethodPost, "/api/v1/reviews/wf-1/cancel", nil, nil)
	assertStatus(t, cancel, http.StatusAccepted)
	if env.decode(t, cancel)["job_id"] != "job-cancel" {
		t.Fatalf("cancel body = %s", cancel.body)
	}
	if len(env.reviews.cancelIDs) != 1 || env.reviews.cancelIDs[0] != "wf-1" {
		t.Fatalf("cancel ids = %v", env.reviews.cancelIDs)
	}

	republish := client.request(http.MethodPost, "/api/v1/reviews/wf-1/republish", nil, nil)
	assertStatus(t, republish, http.StatusAccepted)
	if env.decode(t, republish)["job_id"] != "job-republish" {
		t.Fatalf("republish body = %s", republish.body)
	}
	if len(env.reviews.republishID) != 1 || env.reviews.republishID[0] != "wf-1" {
		t.Fatalf("republish ids = %v", env.reviews.republishID)
	}
}

func TestReviewReadIsOpenToViewersAndWritesRequireCSRF(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	// The viewer has a read grant, so a write is a 403 rather than the 404
	// used for an absent grant.
	env.grant(8, 42, store.ProjectAccessRead)

	viewer := env.loginAs("code-viewer")
	read := viewer.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, read, http.StatusOK)
	write := viewer.request(http.MethodPost, "/api/v1/reviews/wf-1/cancel", nil, nil)
	assertStatus(t, write, http.StatusForbidden)

	admin := env.businessClient()
	noCSRF := admin.withoutCSRF().request(http.MethodPost, "/api/v1/reviews/wf-1/cancel", nil, nil)
	assertStatus(t, noCSRF, http.StatusForbidden)
	withCSRF := admin.request(http.MethodPost, "/api/v1/reviews/wf-1/cancel", nil, nil)
	assertStatus(t, withCSRF, http.StatusAccepted)

	unauthorized := env.anonymousClient().request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, unauthorized, http.StatusUnauthorized)
}

func TestReviewServiceErrorEnvelope(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	env.reviews.err = store.ErrNotFound
	client := env.loginAs("code-admin")
	response := client.request(http.MethodPost, "/api/v1/reviews/wf-1/cancel", nil, nil)
	assertStatus(t, response, http.StatusNotFound)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.body, &body); err != nil || body.Error.Code != "not_found" {
		t.Fatalf("error = %s", response.body)
	}
}
