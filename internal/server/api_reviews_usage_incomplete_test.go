package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func markUsageIncomplete(t *testing.T, fake *fakeReviewStore, wfID string, nodeIDs ...string) {
	t.Helper()
	wanted := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		wanted[id] = true
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	found := false
	for i := range fake.workflows {
		if fake.workflows[i].ID == wfID {
			fake.workflows[i].UsageIncomplete = true
			found = true
		}
	}
	if !found {
		t.Fatalf("workflow %s not seeded", wfID)
	}
	for i := range fake.nodes[wfID] {
		fake.nodes[wfID][i].UsageIncomplete = wanted[fake.nodes[wfID][i].ID]
	}
}

func nodesByID(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := body["nodes"].([]any)
	if !ok {
		data, _ := json.Marshal(body)
		t.Fatalf("nodes missing: %s", data)
	}
	out := make(map[string]map[string]any, len(raw))
	for _, value := range raw {
		node := value.(map[string]any)
		out[node["id"].(string)] = node
	}
	return out
}

// assertUsageThreeNumbers keeps the existing usage object contract: exactly
// the three numeric fields, with no nested completeness flag. The top-level
// usage_incomplete field is the single source of that state.
func assertUsageThreeNumbers(t *testing.T, label string, usage map[string]any) {
	t.Helper()
	if _, present := usage["incomplete"]; present {
		t.Fatalf("%s usage has a nested incomplete field: %v", label, usage)
	}
	for _, key := range []string{"tokens_in", "tokens_out", "cost_micros"} {
		if _, present := usage[key]; !present {
			t.Fatalf("%s usage missing %s: %v", label, key, usage)
		}
	}
}

func TestReviewUsageIncompleteAPIExposure(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	markUsageIncomplete(t, fake, "wf-1", "review.std.p1")
	client := env.businessClient()

	detail := client.request(http.MethodGet, "/api/v1/reviews/wf-1", nil, nil)
	assertStatus(t, detail, http.StatusOK)
	body := env.decode(t, detail)
	if body["usage_incomplete"] != true {
		t.Fatalf("detail usage_incomplete = %v, want true", body["usage_incomplete"])
	}
	usage := body["usage"].(map[string]any)
	assertUsageThreeNumbers(t, "detail", usage)
	if usage["tokens_in"] != float64(1000) || usage["cost_micros"] != float64(5000) {
		t.Fatalf("detail usage values = %v", usage)
	}
	detailNodes := nodesByID(t, body)
	if detailNodes["review.std.p1"]["usage_incomplete"] != true {
		t.Fatalf("detail node flag = %v", detailNodes["review.std.p1"])
	}
	if detailNodes["plan"]["usage_incomplete"] != false {
		t.Fatalf("detail clean node flag = %v", detailNodes["plan"])
	}

	list := client.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, list, http.StatusOK)
	item := listItemsByID(t, env.decode(t, list))["wf-1"]
	if item["usage_incomplete"] != true {
		t.Fatalf("list usage_incomplete = %v, want true", item["usage_incomplete"])
	}
	if nodesByID(t, item)["review.std.p1"]["usage_incomplete"] != true {
		t.Fatalf("list node flag = %v", item["nodes"])
	}

	graph := client.request(http.MethodGet, "/api/v1/reviews/wf-1/graph", nil, nil)
	assertStatus(t, graph, http.StatusOK)
	graphNodes := nodesByID(t, env.decode(t, graph))
	if graphNodes["review.std.p1"]["usage_incomplete"] != true || graphNodes["plan"]["usage_incomplete"] != false {
		t.Fatalf("graph node flags = %v", graphNodes)
	}

	node := client.request(http.MethodGet, "/api/v1/reviews/wf-1/nodes/review.std.p1", nil, nil)
	assertStatus(t, node, http.StatusOK)
	nodeBody := env.decode(t, node)
	if nodeBody["usage_incomplete"] != true {
		t.Fatalf("node usage_incomplete = %v, want true", nodeBody["usage_incomplete"])
	}
	nodeUsage := nodeBody["usage"].(map[string]any)
	assertUsageThreeNumbers(t, "node", nodeUsage)
	if nodeUsage["tokens_in"] != float64(200) {
		t.Fatalf("node usage values = %v", nodeUsage)
	}
}

// TestReviewUsageIncompleteAPILegacyValuesAreFalseNotAbsent checks that old
// rows and events still expose the field as false. False is not proof that the
// historical totals are exact.
func TestReviewUsageIncompleteAPILegacyValuesAreFalseNotAbsent(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	client := env.businessClient()

	detail := client.request(http.MethodGet, "/api/v1/reviews/wf-1", nil, nil)
	assertStatus(t, detail, http.StatusOK)
	body := env.decode(t, detail)
	value, present := body["usage_incomplete"]
	if !present || value != false {
		t.Fatalf("detail usage_incomplete = %v (present=%t), want present false", value, present)
	}
	usage := body["usage"].(map[string]any)
	assertUsageThreeNumbers(t, "legacy detail", usage)
	for id, node := range nodesByID(t, body) {
		value, present := node["usage_incomplete"]
		if !present || value != false {
			t.Fatalf("detail node %s usage_incomplete = %v (present=%t)", id, value, present)
		}
	}

	list := client.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, list, http.StatusOK)
	item := listItemsByID(t, env.decode(t, list))["wf-1"]
	value, present = item["usage_incomplete"]
	if !present || value != false {
		t.Fatalf("list usage_incomplete = %v (present=%t), want present false", value, present)
	}
	for id, node := range nodesByID(t, item) {
		value, present := node["usage_incomplete"]
		if !present || value != false {
			t.Fatalf("list node %s usage_incomplete = %v (present=%t)", id, value, present)
		}
	}

	graph := client.request(http.MethodGet, "/api/v1/reviews/wf-1/graph", nil, nil)
	assertStatus(t, graph, http.StatusOK)
	for id, node := range nodesByID(t, env.decode(t, graph)) {
		value, present := node["usage_incomplete"]
		if !present || value != false {
			t.Fatalf("graph node %s usage_incomplete = %v (present=%t)", id, value, present)
		}
	}

	node := client.request(http.MethodGet, "/api/v1/reviews/wf-1/nodes/plan", nil, nil)
	assertStatus(t, node, http.StatusOK)
	nodeBody := env.decode(t, node)
	value, present = nodeBody["usage_incomplete"]
	if !present || value != false {
		t.Fatalf("node usage_incomplete = %v (present=%t), want present false", value, present)
	}
	nodeUsage := nodeBody["usage"].(map[string]any)
	assertUsageThreeNumbers(t, "legacy node", nodeUsage)
}

func TestReviewUsageIncompleteAPIRespectsProjectAccess(t *testing.T) {
	env, fake := isolationEnv(t)
	seedIsolationWorkflow(env, fake, "wf-42", 42)
	markUsageIncomplete(t, fake, "wf-42", "plan")
	env.grant(7, 42, store.ProjectAccessManage)
	env.grant(8, 43, store.ProjectAccessRead)
	alice := env.loginAs("code-admin")
	bob := env.loginAs("code-viewer")

	for _, path := range []string{
		"/api/v1/reviews/wf-42",
		"/api/v1/reviews/wf-42/graph",
		"/api/v1/reviews/wf-42/nodes/plan",
	} {
		assertStatus(t, bob.request(http.MethodGet, path, nil, nil), http.StatusNotFound)
		allowed := alice.request(http.MethodGet, path, nil, nil)
		assertStatus(t, allowed, http.StatusOK)
		if path != "/api/v1/reviews/wf-42" {
			body := env.decode(t, allowed)
			if path == "/api/v1/reviews/wf-42/graph" {
				if nodesByID(t, body)["plan"]["usage_incomplete"] != true {
					t.Fatalf("%s node flag = %v", path, body)
				}
				continue
			}
			if body["usage_incomplete"] != true {
				t.Fatalf("%s usage_incomplete = %v", path, body["usage_incomplete"])
			}
		}
	}

	bobList := bob.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, bobList, http.StatusOK)
	if items := listItemsByID(t, env.decode(t, bobList)); len(items) != 0 {
		t.Fatalf("bob sees foreign reviews: %v", items)
	}
	aliceList := alice.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, aliceList, http.StatusOK)
	item := listItemsByID(t, env.decode(t, aliceList))["wf-42"]
	if item["usage_incomplete"] != true || nodesByID(t, item)["plan"]["usage_incomplete"] != true {
		t.Fatalf("alice list item = %v", item)
	}
}

func TestReviewUsageIncompleteAPIConcurrentReads(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	markUsageIncomplete(t, fake, "wf-1", "review.std.p1")
	// businessClient also creates the project grant the request guard needs.
	session := env.businessClient()

	const graphPath = "/api/v1/reviews/wf-1/graph"
	paths := []string{
		"/api/v1/reviews/wf-1",
		graphPath,
		"/api/v1/reviews/wf-1/nodes/review.std.p1",
	}
	const readers = 6
	errs := make(chan error, readers*len(paths))
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		for _, path := range paths {
			path := path
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, err := http.NewRequest(http.MethodGet, env.server.URL+path, nil)
				if err != nil {
					errs <- err
					return
				}
				resp, err := session.client.Do(req)
				if err != nil {
					errs <- err
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("%s status %d", path, resp.StatusCode)
					return
				}
				var body struct {
					UsageIncomplete bool `json:"usage_incomplete"`
					Nodes           []struct {
						ID              string `json:"id"`
						UsageIncomplete bool   `json:"usage_incomplete"`
					} `json:"nodes"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					errs <- err
					return
				}
				incomplete := body.UsageIncomplete
				if path == graphPath {
					incomplete = false
					for _, node := range body.Nodes {
						if node.ID == "review.std.p1" && node.UsageIncomplete {
							incomplete = true
						}
					}
				}
				if !incomplete {
					errs <- fmt.Errorf("%s lost usage_incomplete", path)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent API read: %v", err)
	}
}
