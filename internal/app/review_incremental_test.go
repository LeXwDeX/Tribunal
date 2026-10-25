package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/jobs"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

func findingRunner(severity review.Severity, path string) review.AgentRunner {
	return func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return &agent.RunResult{
			Output: &review.CodeReviewOut{
				Summary: "one issue found",
				Findings: []review.FindingOut{{
					Path:       path,
					Line:       2,
					Side:       "new",
					Severity:   severity,
					Category:   "correctness",
					Title:      "bad assignment",
					Body:       "the assignment is wrong",
					Evidence:   "bad := 1",
					Confidence: review.ConfidenceHigh,
				}},
			},
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}, nil
	}
}

func (f *reviewFixture) seedRunFor(t *testing.T, jobID, head, trigger string) *jobs.Job {
	t.Helper()
	result, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID:   42,
		MRIID:       7,
		HeadSHA:     head,
		Payload:     json.RawMessage(fmt.Sprintf(`{"trigger":%q,"profile":"default","full":false}`, trigger)),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"project_id": 42,
		"mr_iid":     7,
		"head_sha":   head,
		"generation": result.Generation,
		"profile":    "default",
		"trigger":    trigger,
		"full":       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The durable job ID is what the store maps workflows to; tests must use
	// it rather than a display label.
	return &jobs.Job{ID: result.JobID, Kind: "review.run", Payload: payload, MaxAttempts: 3}
}

func setIncrementalHead(f *reviewFixture, head string, changed ...string) {
	f.head = head
	f.git.head = head
	f.git.ancestor = true
	f.git.changed = append([]string(nil), changed...)
	f.gitlab.mr.SHA = head
	f.gitlab.version.HeadSHA = head
	f.gitlab.diffs = []gitlab.FileDiff{
		{NewPath: "main.go", Diff: "@@ -1,2 +1,3 @@\n a\n+bad := 1\n b\n"},
		{NewPath: "other.go", Diff: "@@ -1,2 +1,3 @@\n x\n+y := 2\n z\n"},
	}
}

func secondRoundDossier(t *testing.T, f *reviewFixture, job *jobs.Job) review.Dossier {
	t.Helper()
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier: %v", err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatalf("decode dossier: %v", err)
	}
	return dossier
}

// A previous published P1 in file A must not disappear when the next push
// only changes B. The incremental scope alone cannot speak for A, so the
// review falls back to the full MR.
func TestIncrementalReviewFallsBackForUnresolvedSevereFinding(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	profile := f.profiles["default"]
	profile.Challenge.Only = []string{"info"}
	f.profiles["default"] = profile
	f.service.agentRun = findingRunner(review.SeverityP1, "main.go")

	first := f.seedRunFor(t, "job-incremental-first", reviewTestHead, "mr_open")
	if err := f.service.Handle(ctx, first); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	firstWorkflow, err := f.db.GetWorkflowByJob(ctx, first.ID)
	if err != nil {
		t.Fatalf("first workflow: %v", err)
	}
	if firstWorkflow.Verdict != review.VerdictBlocking {
		t.Fatalf("first verdict = %q, want %s", firstWorkflow.Verdict, review.VerdictBlocking)
	}

	setIncrementalHead(f, strings.Repeat("c", 40), "other.go")
	second := f.seedRunFor(t, "job-incremental-second", f.head, "mr_update")
	if err := f.service.Handle(ctx, second); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	secondWorkflow, err := f.db.GetWorkflowByJob(ctx, second.ID)
	if err != nil {
		t.Fatalf("second workflow: %v", err)
	}
	if secondWorkflow.Verdict != review.VerdictBlocking {
		t.Fatalf("second verdict = %q, want %s (previous P1 was not rechecked)",
			secondWorkflow.Verdict, review.VerdictBlocking)
	}
	dossier := secondRoundDossier(t, f, second)
	if dossier.Incremental == nil || !dossier.Incremental.Full {
		t.Fatalf("second incremental = %+v, want a full review", dossier.Incremental)
	}
}

// With no previously published severe finding, a normal incremental review
// keeps its narrow file scope.
func TestIncrementalReviewStaysNarrowWithoutSevereHistory(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	profile := f.profiles["default"]
	profile.Challenge.Only = []string{"info"}
	f.profiles["default"] = profile
	f.service.agentRun = findingRunner(review.SeverityP2, "main.go")

	first := f.seedRunFor(t, "job-narrow-first", reviewTestHead, "mr_open")
	if err := f.service.Handle(ctx, first); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	setIncrementalHead(f, strings.Repeat("c", 40), "other.go")
	second := f.seedRunFor(t, "job-narrow-second", f.head, "mr_update")
	if err := f.service.Handle(ctx, second); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	dossier := secondRoundDossier(t, f, second)
	if dossier.Incremental == nil || dossier.Incremental.Full {
		t.Fatalf("incremental = %+v, want a narrow scope", dossier.Incremental)
	}
	if len(dossier.Incremental.Files) != 1 || dossier.Incremental.Files[0] != "other.go" {
		t.Fatalf("incremental files = %v, want [other.go]", dossier.Incremental.Files)
	}
}

// An empty changed-file intersection is not evidence that the whole MR is
// clean; it falls back to a full review.
func TestIncrementalReviewFallsBackForEmptyScope(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	profile := f.profiles["default"]
	profile.Challenge.Only = []string{"info"}
	f.profiles["default"] = profile
	f.service.agentRun = findingRunner(review.SeverityP2, "main.go")

	first := f.seedRunFor(t, "job-empty-first", reviewTestHead, "mr_open")
	if err := f.service.Handle(ctx, first); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	setIncrementalHead(f, strings.Repeat("c", 40), "elsewhere.go")
	second := f.seedRunFor(t, "job-empty-second", f.head, "mr_update")
	if err := f.service.Handle(ctx, second); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	dossier := secondRoundDossier(t, f, second)
	if dossier.Incremental == nil || !dossier.Incremental.Full {
		t.Fatalf("incremental = %+v, want a full review for an empty scope", dossier.Incremental)
	}
	if !strings.Contains(dossier.Incremental.Reason, "empty") {
		t.Fatalf("reason = %q, want an empty-scope explanation", dossier.Incremental.Reason)
	}
}

type severeLookupErrorStore struct {
	*sqlite.Store
}

func (s *severeLookupErrorStore) HasPublishedSevereFinding(context.Context, int64, int64) (bool, error) {
	return false, errors.New("injected finding lookup failure")
}

// When the previous severe findings cannot be read, the review must not claim
// a narrow clean result.
func TestIncrementalReviewFallsBackWhenHistoryIsUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	profile := f.profiles["default"]
	profile.Challenge.Only = []string{"info"}
	f.profiles["default"] = profile
	f.service.agentRun = findingRunner(review.SeverityP2, "main.go")

	first := f.seedRunFor(t, "job-history-first", reviewTestHead, "mr_open")
	if err := f.service.Handle(ctx, first); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	setIncrementalHead(f, strings.Repeat("c", 40), "other.go")
	f.service.store = &severeLookupErrorStore{Store: f.db}
	second := f.seedRunFor(t, "job-history-second", f.head, "mr_update")
	if err := f.service.Handle(ctx, second); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	dossier := secondRoundDossier(t, f, second)
	if dossier.Incremental == nil || !dossier.Incremental.Full {
		t.Fatalf("incremental = %+v, want a full review when history is unavailable", dossier.Incremental)
	}
	if !strings.Contains(dossier.Incremental.Reason, "lookup failed") {
		t.Fatalf("reason = %q, want the history lookup failure", dossier.Incremental.Reason)
	}
}
