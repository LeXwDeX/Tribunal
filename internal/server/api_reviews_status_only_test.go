package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func seedStatusOnly(fake *fakeReviewStore, project, mr int64, at time.Time) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.statusOnly = append(fake.statusOnly, store.StatusOnlyReview{
		ProjectID: project, MRIID: mr, HeadSHA: "abc123", State: store.ReviewStatusIncomplete,
		Detail: store.StatusDetailEmptyDiff, UpdatedAt: at,
	})
}

func reviewItems(t *testing.T, env *m6Env, resp apiResponse) ([]map[string]any, string) {
	t.Helper()
	assertStatus(t, resp, http.StatusOK)
	body := env.decode(t, resp)
	raw, _ := body["reviews"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		items = append(items, item.(map[string]any))
	}
	next, _ := body["next_cursor"].(string)
	return items, next
}

func TestListReviewsIncludesStatusOnlyRows(t *testing.T) {
	env, fake := isolationEnv(t)
	seedIsolationWorkflow(env, fake, "wf-42", 42)
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	seedStatusOnly(fake, 42, 9, base.Add(time.Hour))
	env.grant(7, 42, store.ProjectAccessRead)
	alice := env.loginAs("code-admin")

	items, next := reviewItems(t, env, alice.request(http.MethodGet, "/api/v1/reviews", nil, nil))
	if len(items) != 2 || next != "" {
		t.Fatalf("items = %v, next %q", items, next)
	}
	only := items[0]
	if only["kind"] != "status_only" || only["verdict"] != "INCOMPLETE" || only["reason"] != "empty diff" ||
		only["mr_iid"] != float64(9) || only["head_sha"] != "abc123" || only["project_id"] != float64(42) {
		t.Fatalf("status-only item = %v", only)
	}
	if _, hasID := only["id"]; hasID {
		t.Fatalf("status-only item must not carry a workflow id: %v", only)
	}
	if items[1]["id"] != "wf-42" || items[1]["kind"] != "review" {
		t.Fatalf("workflow item changed: %v", items[1])
	}
}

func TestListReviewsStatusOnlyKeepsAccessFiltering(t *testing.T) {
	env, fake := isolationEnv(t)
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	seedStatusOnly(fake, 42, 1, base)
	seedStatusOnly(fake, 43, 2, base.Add(time.Minute))
	env.grant(7, 42, store.ProjectAccessRead)
	env.grant(8, 43, store.ProjectAccessRead)
	alice := env.loginAs("code-admin")

	items, _ := reviewItems(t, env, alice.request(http.MethodGet, "/api/v1/reviews", nil, nil))
	if len(items) != 1 || items[0]["project_id"] != float64(42) {
		t.Fatalf("alice sees %v", items)
	}
	// A project the caller cannot read stays indistinguishable from a missing one.
	denied := alice.request(http.MethodGet, "/api/v1/reviews?project_id=43", nil, nil)
	assertStatus(t, denied, http.StatusNotFound)
}

func TestListReviewsPaginatesMixedWorkflowAndStatusOnly(t *testing.T) {
	env, fake := isolationEnv(t)
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	// Newest first: s5 w4 s3 w2 s1 plus a workflow w0 older than all.
	for i, id := range []string{"w0", "", "w2", "", "w4"} {
		if id == "" {
			continue
		}
		mr := int64(i)
		fake.setWorkflow(store.Workflow{
			ID: id, Kind: "review", ProjectID: 42, MRIID: &mr, Trigger: "api", Status: "completed",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	for _, mr := range []int64{1, 3, 5} {
		seedStatusOnly(fake, 42, mr, base.Add(time.Duration(mr)*time.Minute))
	}
	env.grant(7, 42, store.ProjectAccessRead)
	alice := env.loginAs("code-admin")

	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "/api/v1/reviews?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		items, next := reviewItems(t, env, alice.request(http.MethodGet, path, nil, nil))
		if len(items) > 2 {
			t.Fatalf("page %d has %d items", page, len(items))
		}
		for _, item := range items {
			if item["kind"] == "status_only" {
				got = append(got, "s"+string(rune('0'+int(item["mr_iid"].(float64)))))
			} else {
				got = append(got, item["id"].(string))
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	want := []string{"s5", "w4", "s3", "w2", "s1", "w0"}
	if len(got) != len(want) {
		t.Fatalf("paged = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paged = %v, want %v", got, want)
		}
	}
}

func TestListReviewsWorkflowOnlyFiltersExcludeStatusOnly(t *testing.T) {
	env, fake := isolationEnv(t)
	seedIsolationWorkflow(env, fake, "wf-42", 42)
	seedStatusOnly(fake, 42, 9, time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC))
	env.grant(7, 42, store.ProjectAccessRead)
	alice := env.loginAs("code-admin")

	for _, query := range []string{"status=completed", "profile=default", "trigger=api", "publication_state=published"} {
		items, _ := reviewItems(t, env, alice.request(http.MethodGet, "/api/v1/reviews?"+query, nil, nil))
		for _, item := range items {
			if item["kind"] == "status_only" {
				t.Fatalf("%s returned a status-only row: %v", query, item)
			}
		}
	}
	items, _ := reviewItems(t, env, alice.request(http.MethodGet, "/api/v1/reviews?verdict=INCOMPLETE", nil, nil))
	if len(items) != 1 || items[0]["kind"] != "status_only" {
		t.Fatalf("verdict filter = %v", items)
	}
}
