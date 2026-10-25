package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

const annotationURL = "/api/v1/reviews/wf-1/findings/f1/annotation"

func TestFindingAnnotationAPIAdminCSRFAndViewer(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	env.grant(8, 42, store.ProjectAccessRead)

	viewer := env.loginAs("code-viewer")
	read := viewer.request(http.MethodGet, "/api/v1/reviews/wf-1/findings", nil, nil)
	assertStatus(t, read, http.StatusOK)
	viewerWrite := viewer.json(http.MethodPut, annotationURL, map[string]any{"annotation": "valid"})
	assertStatus(t, viewerWrite, http.StatusForbidden)

	admin := env.businessClient()
	noCSRF := admin.withoutCSRF().json(http.MethodPut, annotationURL, map[string]any{"annotation": "valid"})
	assertStatus(t, noCSRF, http.StatusForbidden)

	response := admin.json(http.MethodPut, annotationURL, map[string]any{"annotation": "valid"})
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if body["id"] != "f1" || body["workflow_id"] != "wf-1" || body["origin_node"] != "review.std.p1" ||
		body["severity"] != "P1" || body["category"] != "bug" || body["title"] != "one" ||
		body["body"] != "body" || body["decision"] != "publish" {
		t.Fatalf("response is not a full finding: %v", body)
	}
	if body["annotation"] != "valid" {
		t.Fatalf("annotation = %v", body["annotation"])
	}
	if body["annotated_by"] != "alice" {
		t.Fatalf("annotated_by = %v, want the session username", body["annotated_by"])
	}
	rawAt, _ := body["annotated_at"].(string)
	if _, err := time.Parse(time.RFC3339, rawAt); err != nil {
		t.Fatalf("annotated_at = %q, not RFC3339: %v", rawAt, err)
	}

	listed := admin.request(http.MethodGet, "/api/v1/reviews/wf-1/findings", nil, nil)
	assertStatus(t, listed, http.StatusOK)
	var found map[string]any
	for _, item := range env.decode(t, listed)["findings"].([]any) {
		finding := item.(map[string]any)
		if finding["id"] == "f1" {
			found = finding
		}
	}
	if found == nil || found["annotation"] != "valid" || found["annotated_by"] != "alice" {
		t.Fatalf("listed finding = %v", found)
	}

	// The server, not the body, decides the actor and the time.
	again := env.businessClient()
	againUpdate := again.json(http.MethodPut, annotationURL, map[string]any{"annotation": "valid"})
	assertStatus(t, againUpdate, http.StatusOK)
	if got := env.decode(t, againUpdate)["annotated_by"]; got != "alice" {
		t.Fatalf("annotated_by = %v, want alice", got)
	}

	clear := admin.json(http.MethodPut, annotationURL, map[string]any{"annotation": ""})
	assertStatus(t, clear, http.StatusOK)
	cleared := env.decode(t, clear)
	if cleared["annotation"] != "" {
		t.Fatalf("cleared annotation = %v", cleared["annotation"])
	}
	if _, present := cleared["annotated_by"]; present {
		t.Fatalf("cleared response kept annotated_by: %v", cleared)
	}
	if _, present := cleared["annotated_at"]; present {
		t.Fatalf("cleared response kept annotated_at: %v", cleared)
	}
}

func TestFindingAnnotationAPIRejectsInvalidRequests(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	admin := env.businessClient()

	for _, testCase := range []struct {
		name string
		body []byte
	}{
		{name: "missing", body: []byte(`{}`)},
		{name: "null", body: []byte(`{"annotation":null}`)},
		{name: "unsupported", body: []byte(`{"annotation":"maybe"}`)},
		{name: "malformed", body: []byte(`{"annotation":`)},
		{name: "client actor", body: []byte(`{"annotation":"valid","annotated_by":"eve"}`)},
		{name: "client time", body: []byte(`{"annotation":"valid","annotated_at":"2020-01-01T00:00:00Z"}`)},
	} {
		response := admin.request(http.MethodPut, annotationURL, testCase.body,
			map[string]string{"Content-Type": "application/json"})
		if response.status != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400, body=%s", testCase.name, response.status, response.body)
		}
	}
	findings := fake.findings["wf-1"]
	for _, finding := range findings {
		if finding.ID == "f1" && finding.Annotation != "" {
			t.Fatalf("rejected request changed the finding: %+v", finding)
		}
	}

	missing := admin.json(http.MethodPut, "/api/v1/reviews/wf-1/findings/missing/annotation",
		map[string]any{"annotation": "valid"})
	assertStatus(t, missing, http.StatusNotFound)
	wrongWorkflow := admin.json(http.MethodPut, "/api/v1/reviews/wf-other/findings/f1/annotation",
		map[string]any{"annotation": "valid"})
	assertStatus(t, wrongWorkflow, http.StatusNotFound)
	if response := admin.request(http.MethodPut, annotationURL, nil, nil); response.status != http.StatusBadRequest {
		t.Fatalf("empty body status = %d, want 400", response.status)
	}
}

func TestFindingAnnotationAPIErrorEnvelope(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedWorkflow(env)
	admin := env.businessClient()

	response := admin.json(http.MethodPut, annotationURL, map[string]any{"annotation": "unsupported"})
	assertStatus(t, response, http.StatusBadRequest)
	if !strings.Contains(string(response.body), `"validation_failed"`) {
		t.Fatalf("validation envelope = %s", response.body)
	}
	if !strings.Contains(string(response.body), `"annotation"`) {
		t.Fatalf("validation field missing: %s", response.body)
	}

	missing := admin.json(http.MethodPut, "/api/v1/reviews/wf-1/findings/missing/annotation",
		map[string]any{"annotation": "valid"})
	assertStatus(t, missing, http.StatusNotFound)
	if !strings.Contains(string(missing.body), `"not_found"`) {
		t.Fatalf("not-found envelope = %s", missing.body)
	}
}

// Compile-time check that the fake satisfies the extended read/write surface.
var _ Store = (*fakeReviewStore)(nil)
