package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// A retrigger of the same head starts a new generation. If repository
// configuration cannot be parsed before a workflow exists, the previous
// generation's green status must not remain the head status.
func TestPreWorkflowConfigFailureReplacesGreenCommitStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults

	first := f.seedReview(t, "preworkflow-green")
	if err := f.service.Handle(ctx, first); err != nil {
		t.Fatalf("first review: %v", err)
	}
	if len(f.gitlab.statuses) != 1 || f.gitlab.statuses[0].State != "success" {
		t.Fatalf("first statuses = %+v", f.gitlab.statuses)
	}

	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [" + publicationSecret + "\n"
	second := f.seedReview(t, "preworkflow-invalid")
	err := f.service.RunReview(ctx, second)
	if err == nil || !IsPermanentJob(err) || !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("second RunReview error = %v, want permanent ErrInvalidRepoConfig", err)
	}
	if jobGeneration(t, second) <= jobGeneration(t, first) {
		t.Fatalf("second review did not start a new generation")
	}
	if len(f.gitlab.statuses) != 2 {
		t.Fatalf("statuses = %+v, want success then failed", f.gitlab.statuses)
	}
	if f.gitlab.statuses[0].State != "success" || f.gitlab.statuses[1].State != "failed" {
		t.Fatalf("status transitions = %+v", f.gitlab.statuses)
	}
	if f.gitlab.statuses[1].Name != "ai-review" || f.gitlab.statuses[1].TargetURL != "" {
		t.Fatalf("failure status = %+v", f.gitlab.statuses[1])
	}
	assertNoSecret(t, f.gitlab.statuses[1].Description)
	if strings.Contains(f.gitlab.statuses[1].Description, ".gitlab-review.yml") ||
		strings.Contains(f.gitlab.statuses[1].Description, "ignore") {
		t.Fatalf("failure description leaked configuration: %q", f.gitlab.statuses[1].Description)
	}
}

// The configuration-failure status must never land after a newer generation
// has already written its own result.
func TestPreWorkflowConfigFailureFencesNewerResult(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults

	first := f.seedReview(t, "preworkflow-race-green")
	if err := f.service.Handle(ctx, first); err != nil {
		t.Fatalf("first review: %v", err)
	}
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [\n"

	api := &preWorkflowRaceGitLab{
		reviewGitLab: f.gitlab,
		bump:         mrSerialBumpControl(t, f),
	}
	f.service.gitlab = api
	f.publisher.gitlab = api

	second := f.seedReview(t, "preworkflow-race-invalid")
	err := f.service.RunReview(ctx, second)
	if err == nil || !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("RunReview error = %v, want ErrInvalidRepoConfig", err)
	}
	if len(f.gitlab.statuses) != 2 ||
		f.gitlab.statuses[0].State != "success" || f.gitlab.statuses[1].State != "success" {
		t.Fatalf("stale failure wrote a status: %+v", f.gitlab.statuses)
	}
}

// A transient GitLab failure while writing the pre-workflow failure status
// must not settle the permanent configuration error. The job keeps retrying
// so the previous green status is eventually replaced.
func TestPreWorkflowConfigFailureKeepsTransientStatusFailureRetryable(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	api := usePartialGitLab(f)
	api.fail(nil, &gitlab.Error{Op: "commit_status", Status: 503})
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [\n"

	job := f.seedReview(t, "preworkflow-transient")
	err := f.service.RunReview(ctx, job)
	if err == nil || IsPermanentJob(err) {
		t.Fatalf("transient status failure RunReview error = %v, want a retryable error", err)
	}
	if len(f.gitlab.statuses) != 0 {
		t.Fatalf("transient status failure wrote %+v", f.gitlab.statuses)
	}

	api.fail(nil, nil)
	err = f.service.RunReview(ctx, job)
	if err == nil || !IsPermanentJob(err) || !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("retried RunReview error = %v, want permanent ErrInvalidRepoConfig", err)
	}
	if len(f.gitlab.statuses) != 1 || f.gitlab.statuses[0].State != "failed" {
		t.Fatalf("retried statuses = %+v, want one failed status", f.gitlab.statuses)
	}
}

// A terminal status failure still settles the permanent configuration error.
// Only transient upstream failures may keep the job retryable.
func TestPreWorkflowConfigFailureKeepsPermanentStatusFailureTerminal(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	api := usePartialGitLab(f)
	api.fail(nil, &gitlab.Error{Op: "commit_status", Status: 403})
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [\n"

	job := f.seedReview(t, "preworkflow-permanent-status")
	err := f.service.RunReview(ctx, job)
	if err == nil || !IsPermanentJob(err) || !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("RunReview error = %v, want permanent ErrInvalidRepoConfig", err)
	}
	if len(f.gitlab.statuses) != 0 {
		t.Fatalf("permanent status failure wrote %+v", f.gitlab.statuses)
	}
}

func TestPreWorkflowConfigFailureHonorsCommitStatusSwitch(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [\n"

	job := f.seedReview(t, "preworkflow-disabled")
	err := f.service.RunReview(ctx, job)
	if err == nil || !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("RunReview error = %v, want ErrInvalidRepoConfig", err)
	}
	if len(f.gitlab.statuses) != 0 {
		t.Fatalf("disabled commit status wrote %+v", f.gitlab.statuses)
	}
}

// When stored project settings cannot be read, the commit-status switch is
// unknown. The failure must block visibly without writing a guessed status.
func TestPreWorkflowConfigFailureSkipsUnknownCommitStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if _, err := f.db.UpdateProject(ctx, 42, store.ProjectUpdate{Settings: []byte(`{"unknown":true}`)}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	job := f.seedReview(t, "preworkflow-unsafe-settings")
	err := f.service.RunReview(ctx, job)
	if err == nil || !IsPermanentJob(err) || !strings.Contains(err.Error(), "settings cannot be read safely") {
		t.Fatalf("RunReview error = %v, want a permanent, explicit settings error", err)
	}
	if errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("unsafe settings were misclassified as ErrInvalidRepoConfig")
	}
	if len(f.gitlab.statuses) != 0 {
		t.Fatalf("unknown switch wrote %+v", f.gitlab.statuses)
	}
	if _, err := f.db.GetWorkflowByJob(ctx, job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("workflow was created for unsafe settings: %v", err)
	}
}

func TestPreWorkflowFailureRejectsMissingGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)

	err := f.publisher.PublishPreWorkflowFailure(ctx, 42, 7, 1, reviewTestHead, "token")
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("error = %v, want ErrStalePublication", err)
	}
	if len(f.gitlab.statuses) != 0 {
		t.Fatalf("missing control wrote %+v", f.gitlab.statuses)
	}
}

type preWorkflowRaceGitLab struct {
	*reviewGitLab
	once sync.Once
	bump func()
}

func (g *preWorkflowRaceGitLab) GetRawFile(ctx context.Context, token string, projectID int64, path, ref string) ([]byte, error) {
	if path == ".gitlab-review.yml" {
		g.once.Do(func() {
			g.bump()
			_ = g.reviewGitLab.SetCommitStatus(ctx, token, projectID, g.reviewGitLab.version.HeadSHA,
				gitlab.CommitStatus{State: "success", Name: "ai-review"})
		})
	}
	return g.reviewGitLab.GetRawFile(ctx, token, projectID, path, ref)
}
