package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// TestListReviewsIncludesFindingsSummaryAndNodes checks that every list item
// carries the same findings_summary and nodes as the detail endpoint, and that
// the page is summarized with one batched read per kind of data.
func TestListReviewsIncludesFindingsSummaryAndNodes(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	// A queued review without nodes or findings yet.
	fake.setWorkflow(store.Workflow{
		ID: "wf-2", Kind: "review", ProjectID: 42, Trigger: "api", Status: "queued",
		CreatedAt: time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC),
	})
	client := env.businessClient()

	detail := client.request(http.MethodGet, "/api/v1/reviews/wf-1", nil, nil)
	assertStatus(t, detail, http.StatusOK)
	detailBody := env.decode(t, detail)

	fake.resetReadCounters()
	list := client.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, list, http.StatusOK)
	items := listItemsByID(t, env.decode(t, list))

	wf1, ok := items["wf-1"]
	if !ok {
		t.Fatalf("wf-1 missing from list: %s", list.body)
	}
	if !reflect.DeepEqual(wf1["findings_summary"], detailBody["findings_summary"]) {
		t.Fatalf("list findings_summary = %v, detail = %v", wf1["findings_summary"], detailBody["findings_summary"])
	}
	if !reflect.DeepEqual(wf1["nodes"], detailBody["nodes"]) {
		t.Fatalf("list nodes = %v, detail = %v", wf1["nodes"], detailBody["nodes"])
	}
	summary := wf1["findings_summary"].(map[string]any)
	if summary["published"] != float64(1) || summary["marked"] != float64(1) || summary["dropped"] != float64(1) {
		t.Fatalf("findings_summary = %v", summary)
	}
	nodes := wf1["nodes"].([]any)
	if len(nodes) != 3 || nodes[1].(map[string]any)["depends_on"].([]any)[0] != "plan" {
		t.Fatalf("nodes = %v", nodes)
	}

	wf2, ok := items["wf-2"]
	if !ok {
		t.Fatalf("wf-2 missing from list: %s", list.body)
	}
	emptyNodes, ok := wf2["nodes"].([]any)
	if !ok || len(emptyNodes) != 0 {
		t.Fatalf("wf-2 nodes = %#v, want []", wf2["nodes"])
	}
	wantEmpty := map[string]any{
		"published": float64(0), "marked": float64(0), "dropped": float64(0),
		"by_severity": map[string]any{"P0": float64(0), "P1": float64(0), "P2": float64(0), "info": float64(0)},
	}
	if !reflect.DeepEqual(wf2["findings_summary"], wantEmpty) {
		t.Fatalf("wf-2 findings_summary = %v", wf2["findings_summary"])
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.nodeReads != 0 || fake.findingReads != 0 {
		t.Fatalf("per-row reads: nodes=%d findings=%d", fake.nodeReads, fake.findingReads)
	}
	wantIDs := []string{"wf-2", "wf-1"}
	if len(fake.nodeBatchReads) != 1 || !reflect.DeepEqual(fake.nodeBatchReads[0], wantIDs) {
		t.Fatalf("node batch reads = %v", fake.nodeBatchReads)
	}
	if len(fake.findingCountReads) != 1 || !reflect.DeepEqual(fake.findingCountReads[0], wantIDs) {
		t.Fatalf("finding count reads = %v", fake.findingCountReads)
	}
}

func TestListReviewsEmptyPageSkipsSummaryReads(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	client := env.businessClient()

	list := client.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if reviews := env.decode(t, list)["reviews"].([]any); len(reviews) != 0 {
		t.Fatalf("reviews = %v", reviews)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.nodeBatchReads) != 0 || len(fake.findingCountReads) != 0 {
		t.Fatalf("empty page read summaries: nodes=%v findings=%v", fake.nodeBatchReads, fake.findingCountReads)
	}
}

// TestListReviewsSummaryKeepsAccessFiltering checks that summary reads only
// cover workflows the caller may see.
func TestListReviewsSummaryKeepsAccessFiltering(t *testing.T) {
	env, fake := isolationEnv(t)
	seedIsolationWorkflow(env, fake, "wf-42", 42)
	seedIsolationWorkflow(env, fake, "wf-43", 43)
	fake.setFindings("wf-43", store.Finding{ID: "secret", WorkflowID: "wf-43", Severity: "P0", Decision: "publish"})
	env.grant(7, 42, store.ProjectAccessManage)
	env.grant(8, 43, store.ProjectAccessRead)
	alice := env.loginAs("code-admin")

	fake.resetReadCounters()
	list := alice.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, list, http.StatusOK)
	items := listItemsByID(t, env.decode(t, list))
	if len(items) != 1 {
		t.Fatalf("alice sees %d reviews: %s", len(items), list.body)
	}
	item, ok := items["wf-42"]
	if !ok {
		t.Fatalf("wf-42 missing: %s", list.body)
	}
	if nodes := item["nodes"].([]any); len(nodes) != 1 {
		t.Fatalf("wf-42 nodes = %v", nodes)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, ids := range append(append([][]string{}, fake.nodeBatchReads...), fake.findingCountReads...) {
		if !reflect.DeepEqual(ids, []string{"wf-42"}) {
			t.Fatalf("summary read covered %v", ids)
		}
	}
}

func listItemsByID(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	reviews, ok := body["reviews"].([]any)
	if !ok {
		raw, _ := json.Marshal(body)
		t.Fatalf("reviews missing: %s", raw)
	}
	out := make(map[string]map[string]any, len(reviews))
	for _, raw := range reviews {
		item := raw.(map[string]any)
		out[item["id"].(string)] = item
	}
	return out
}

// TestListReviewsIncludesMRTitle checks that list items carry the persisted
// MR title, read once for the whole page, and omit it when none is stored.
func TestListReviewsIncludesMRTitle(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	fake.setWorkflow(store.Workflow{
		ID: "wf-2", Kind: "review", ProjectID: 42, Trigger: "api", Status: "queued",
		CreatedAt: time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC),
	})
	fake.mrTitles["wf-1"] = "feat: 支持批量导入"
	client := env.businessClient()

	list := client.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, list, http.StatusOK)
	items := listItemsByID(t, env.decode(t, list))
	if items["wf-1"]["mr_title"] != "feat: 支持批量导入" {
		t.Fatalf("wf-1 mr_title = %v", items["wf-1"]["mr_title"])
	}
	if title, present := items["wf-2"]["mr_title"]; present {
		t.Fatalf("wf-2 mr_title present without a stored title: %v", title)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.titleBatchReads) != 1 || !reflect.DeepEqual(fake.titleBatchReads[0], []string{"wf-2", "wf-1"}) {
		t.Fatalf("title batch reads = %v", fake.titleBatchReads)
	}
}
