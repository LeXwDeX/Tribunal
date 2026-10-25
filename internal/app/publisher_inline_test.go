package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// inlineGitLab delegates every call to the in-memory fake except discussion
// creation, which goes through the real GitLab client and an HTTP test server.
type inlineGitLab struct {
	*reviewGitLab
	client *gitlab.Client
}

func (h *inlineGitLab) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	return h.client.CreateDiscussion(ctx, token, projectID, iid, d)
}

func useInlineGitLab(t *testing.T, f *reviewFixture, handler http.HandlerFunc) *inlineGitLab {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := gitlab.New(gitlab.Config{
		BaseURL:        server.URL,
		RequestTimeout: 2 * time.Second,
		MaxConcurrency: 4,
	})
	if err != nil {
		t.Fatalf("gitlab.New: %v", err)
	}
	api := &inlineGitLab{reviewGitLab: f.gitlab, client: client}
	f.service.gitlab = api
	f.publisher.gitlab = api
	return api
}

func ensureDiscussionPath(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/api/v4/projects/42/merge_requests/7/discussions" {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}
}

type capturedDiscussionRequest struct {
	mu        sync.Mutex
	requests  int
	body      string
	position  map[string]json.RawMessage
	decodeErr error
}

func (c *capturedDiscussionRequest) capture(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	var payload struct {
		Body     string                     `json:"body"`
		Position map[string]json.RawMessage `json:"position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		c.decodeErr = err
		return
	}
	c.body = payload.Body
	c.position = payload.Position
}

func (c *capturedDiscussionRequest) snapshot() (int, string, map[string]json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests, c.body, c.position, c.decodeErr
}

func publicationKindCounts(t *testing.T, f *reviewFixture, workflowID string) map[string]int {
	t.Helper()
	publications, err := f.db.ListPublications(context.Background(), workflowID)
	if err != nil {
		t.Fatalf("ListPublications: %v", err)
	}
	kinds := map[string]int{}
	for _, publication := range publications {
		kinds[publication.Kind]++
	}
	return kinds
}

func TestPublisherPublishesAddedLineDiscussionInline(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	captured := &capturedDiscussionRequest{}
	useInlineGitLab(t, f, func(w http.ResponseWriter, r *http.Request) {
		ensureDiscussionPath(t, r)
		captured.capture(r)
		_, _, position, _ := captured.snapshot()
		if raw, ok := position["old_line"]; ok && string(raw) == "0" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"message":"line_code invalid"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"inline-disc-1","notes":[]}`)
	})

	job := f.seedReview(t, "job-inline-added")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowCompleted) {
		t.Fatalf("workflow status = %q, want completed", workflow.Status)
	}

	requests, body, position, decodeErr := captured.snapshot()
	if decodeErr != nil {
		t.Fatalf("decode discussion request: %v", decodeErr)
	}
	if requests != 1 {
		t.Fatalf("discussion requests = %d, want 1", requests)
	}
	if body == "" {
		t.Fatalf("discussion body is empty")
	}
	if _, ok := position["old_line"]; ok {
		t.Fatalf("added-line request sent old_line: %v", position)
	}
	if got := string(position["new_line"]); got != "2" {
		t.Fatalf("new_line = %s, want 2", got)
	}
	if f.gitlab.discussionCreates != 0 {
		t.Fatalf("fake discussion creates = %d, want real client only", f.gitlab.discussionCreates)
	}
	if f.gitlab.summaryUpdates != 0 {
		t.Fatalf("summary was updated %d times, want no fallback", f.gitlab.summaryUpdates)
	}

	if kinds := publicationKindCounts(t, f, workflow.ID); kinds["summary"] != 1 || kinds["discussion"] != 1 {
		t.Fatalf("publications = %+v, want one summary and one discussion", kinds)
	}
	publications, err := f.db.ListPublications(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("ListPublications: %v", err)
	}
	for _, publication := range publications {
		if publication.Kind == discussionPublishKind && publication.GitlabID != "inline-disc-1" {
			t.Fatalf("discussion publication = %+v, want the real client response ID", publication)
		}
	}
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings = %d, err = %v", len(findings), err)
	}
	if findings[0].PositionOK == nil || !*findings[0].PositionOK {
		t.Fatalf("finding position_ok = %v, want true", findings[0].PositionOK)
	}
}

func TestPublisherFallsBackToSummaryOnInlineRejection(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	captured := &capturedDiscussionRequest{}
	useInlineGitLab(t, f, func(w http.ResponseWriter, r *http.Request) {
		ensureDiscussionPath(t, r)
		captured.capture(r)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"line_code invalid"}`)
	})

	job := f.seedReview(t, "job-inline-rejected")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	requests, _, _, decodeErr := captured.snapshot()
	if decodeErr != nil {
		t.Fatalf("decode discussion request: %v", decodeErr)
	}
	if requests != 1 {
		t.Fatalf("discussion requests = %d, want one non-retried 400", requests)
	}
	if kinds := publicationKindCounts(t, f, workflow.ID); kinds["discussion"] != 0 {
		t.Fatalf("publications = %+v, want no inline discussion", kinds)
	}
	if f.gitlab.summaryUpdates == 0 {
		t.Fatalf("summary was not updated after the 400 fallback")
	}
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings = %d, err = %v", len(findings), err)
	}
	if findings[0].PositionOK == nil || *findings[0].PositionOK {
		t.Fatalf("finding position_ok = %v, want false", findings[0].PositionOK)
	}
}
