package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
	"github.com/LeXwDeX/tribunal/prompts"
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

func TestPublisherReusesConfirmedDiscussionButKeepsMarkedEvidence(t *testing.T) {
	const evidence = "bad := 1"
	key := review.PublishKeyFor("main.go", "Injected failing assignment", evidence)
	previousBody, err := review.RenderFinding(review.Group{
		Path: "main.go", Line: 2, Severity: review.SeverityP1, Title: "Injected failing assignment",
		Body: "Confirmed finding.", Evidence: evidence, PublishKey: key, PositionOK: true,
		Decision: &review.FindingDecision{Action: review.ActionPublish},
	}, review.FindingRenderOptions{})
	if err != nil || strings.Contains(previousBody, evidence) {
		t.Fatalf("confirmed discussion unexpectedly includes evidence: %q, err = %v", previousBody, err)
	}
	f, _, workflow, _ := runDeepReview(t, "job-marked-reuse", func(f *reviewFixture, runner *scriptedAgentRunner) {
		runner.fail[prompts.Verify] = errors.New("synthetic verifier failure")
		runner.outputs[prompts.Synthesize] = &review.SynthesizeOut{
			Verdict: review.VerdictBlocking, Summary: "uncertain finding",
			Decisions: []review.Decision{}, NewFindings: []review.FindingOut{}, ExtendRequests: []review.ExtendRequest{},
		}
		f.gitlab.discussions = []gitlab.Discussion{{ID: "confirmed-discussion", Notes: []gitlab.Note{{Body: previousBody}}}}
		_, err := f.db.UpsertPublication(context.Background(), store.Publication{
			ProjectID: 42, MRIID: 7, Kind: discussionPublishKind, PublishKey: key,
			GitlabID: "confirmed-discussion", WorkflowID: "previous-workflow", Fingerprint: "previous",
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := f.publisher.Publish(context.Background(), workflow.ID); err != nil {
				t.Fatalf("retry Publish: %v", err)
			}
		}
		if f.gitlab.discussionCreates != 0 || len(f.gitlab.discussions) != 1 || f.gitlab.discussions[0].Notes[0].Body != previousBody {
			t.Fatalf("reused discussion was modified or duplicated: %+v", f.gitlab.discussions)
		}
		if len(f.gitlab.notes) != 1 || !strings.HasPrefix(f.gitlab.notes[0].Body, review.SummaryMarkerPrefix) ||
			!strings.Contains(f.gitlab.notes[0].Body, "<details><summary>证据</summary>") || !strings.Contains(f.gitlab.notes[0].Body, evidence) {
			t.Fatalf("marked evidence is absent from the summary: %+v", f.gitlab.notes)
		}
		finding, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
		if err != nil || len(finding) != 1 || finding[0].PositionOK == nil || !*finding[0].PositionOK ||
			finding[0].Decision != string(review.ActionMark) || finding[0].Evidence != evidence || finding[0].PublishKey != key {
			t.Fatalf("positioned finding = %+v, err = %v", finding, err)
		}
		publication, err := f.db.GetPublication(context.Background(), 42, 7, discussionPublishKind, key)
		if err != nil || publication.GitlabID != "confirmed-discussion" || publication.WorkflowID != "previous-workflow" {
			t.Fatalf("existing publication changed: %+v, err = %v", publication, err)
		}
	}
}
