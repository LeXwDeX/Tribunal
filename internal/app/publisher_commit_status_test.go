package app

import (
	"context"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func commitStatusPublications(t *testing.T, f *reviewFixture, workflowID string) []store.Publication {
	t.Helper()
	publications, err := f.db.ListPublications(context.Background(), workflowID)
	if err != nil {
		t.Fatalf("ListPublications: %v", err)
	}
	out := make([]store.Publication, 0, 1)
	for _, publication := range publications {
		if publication.Kind == "commit_status" {
			out = append(out, publication)
		}
	}
	return out
}

// A successful commit status publication must record the state that was sent
// to GitLab so the admin API can report it without guessing from the verdict.
func TestPublisherRecordsCommitStatusState(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults

	job := f.seedReview(t, "job-commit-status")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	first := commitStatusPublications(t, f, workflow.ID)
	if len(first) != 1 {
		t.Fatalf("commit status publications = %d, want 1", len(first))
	}
	if len(f.gitlab.statuses) != 1 {
		t.Fatalf("commit statuses = %d, want 1", len(f.gitlab.statuses))
	}
	state := f.gitlab.statuses[0].State
	if state != "success" {
		t.Fatalf("commit status state = %q, want success for a non-blocking review", state)
	}
	wantKey := "status:" + f.head + ":" + state
	if first[0].PublishKey != wantKey {
		t.Fatalf("status publish key = %q, want %q", first[0].PublishKey, wantKey)
	}

	// Republishing the same verdict must update the existing row rather than
	// creating a second commit_status publication.
	if err := f.publisher.Publish(context.Background(), workflow.ID); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	second := commitStatusPublications(t, f, workflow.ID)
	if len(second) != 1 {
		t.Fatalf("commit status publications after retry = %d, want 1", len(second))
	}
	if second[0].ID != first[0].ID || second[0].PublishKey != wantKey {
		t.Fatalf("retry changed publication: first=%+v second=%+v", first[0], second[0])
	}
}

// A blocking verdict must reach GitLab as failed and be recorded once with
// the state in the publication key.
func TestPublisherRecordsFailedCommitStatusForBlockingVerdict(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)

	// Keep P1 findings outside challenge_only so the deterministic code
	// synthesis yields BLOCKING without a real verifier.
	profile := f.profiles["default"]
	profile.Challenge.Only = []string{"info"}
	f.profiles["default"] = profile

	run := f.runner.Run
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		result, err := run(ctx, spec)
		if err != nil {
			return nil, err
		}
		if out, ok := result.Output.(*review.CodeReviewOut); ok && len(out.Findings) > 0 {
			out.Findings[0].Severity = review.SeverityP1
		}
		return result, nil
	}

	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults

	job := f.seedReview(t, "job-blocking-commit-status")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Verdict != review.VerdictBlocking {
		t.Fatalf("workflow verdict = %q, want %s", workflow.Verdict, review.VerdictBlocking)
	}
	if len(f.gitlab.statuses) != 1 {
		t.Fatalf("commit statuses = %d, want 1", len(f.gitlab.statuses))
	}
	if state := f.gitlab.statuses[0].State; state != "failed" {
		t.Fatalf("commit status state = %q, want failed for a blocking review", state)
	}
	wantKey := "status:" + f.head + ":failed"
	publications := commitStatusPublications(t, f, workflow.ID)
	if len(publications) != 1 {
		t.Fatalf("commit status publications = %d, want 1", len(publications))
	}
	if publications[0].PublishKey != wantKey {
		t.Fatalf("status publish key = %q, want %q", publications[0].PublishKey, wantKey)
	}
}
