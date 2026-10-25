package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestOnboardingFailureLeavesReviewAvailable(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	executors := newExecutorRegistry()
	engine := dag.NewEngine(f.db, executors, executors, dag.Options{})
	service, err := NewWorkspaceService(WorkspaceServiceConfig{
		Store:          f.db,
		Queue:          f.db,
		GitLab:         f.gitlab,
		Cipher:         newTestCipher(t),
		Workspace:      workspace.NewManager(t.TempDir(), &repo.Git{}),
		Git:            f.git,
		Engine:         engine,
		Executors:      executors,
		Profiles:       map[string]config.ProfileConfig{"default": testReviewProfile()},
		ReviewDefaults: f.defaults,
		Prompts:        f.bundle,
		AgentRun:       f.runner.Run,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:            time.Now,
	})
	if err != nil {
		t.Fatalf("NewWorkspaceService: %v", err)
	}
	if err := service.Onboard(context.Background(), 42); err == nil {
		t.Fatal("expected onboarding failure without an onboarding profile")
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.State != "error" {
		t.Fatalf("workspace state = %q, want error", state.State)
	}

	// A failed onboarding must not block reviews.
	job := f.seedReview(t, "job-run-onboard")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("review after failed onboarding: %v", err)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("review after failed onboarding did not publish")
	}
}

func TestFeedbackCollectionIsIdempotent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-feedback")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	f.gitlab.mu.Lock()
	summaryID := f.gitlab.notes[0].ID
	if len(f.gitlab.discussions) != 1 {
		f.gitlab.mu.Unlock()
		t.Fatalf("discussions = %d", len(f.gitlab.discussions))
	}
	discussionID := f.gitlab.discussions[0].ID
	f.gitlab.discussions[0].Notes[0].Resolved = true
	discussionNoteID := f.gitlab.discussions[0].Notes[0].ID
	f.gitlab.awards[summaryID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	f.gitlab.awards[discussionNoteID] = []gitlab.Award{{ID: 2, Name: "thumbsdown", UserID: 6}}
	f.gitlab.mr.SHA = "cccccccccccccccccccccccccccccccccccccccc"
	f.gitlab.mu.Unlock()
	_ = discussionID
	f.git.changed = []string{"main.go"}

	service, err := NewFeedbackService(FeedbackServiceConfig{
		Store:     f.db,
		GitLab:    f.gitlab,
		Cipher:    newTestCipher(t),
		Git:       f.git,
		Workspace: workspace.NewManager(t.TempDir(), &repo.Git{}),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("NewFeedbackService: %v", err)
	}
	service.RunOnce(context.Background())
	service.RunOnce(context.Background())

	projectID := int64(42)
	feedback, _, err := f.db.ListFeedback(context.Background(), store.FeedbackFilter{ProjectID: &projectID})
	if err != nil {
		t.Fatalf("ListFeedback: %v", err)
	}
	signals := map[string]int{}
	for _, item := range feedback {
		signals[item.Signal]++
	}
	if signals["thumbsup"] != 1 || signals["thumbsdown"] != 1 || signals["resolved_changed"] != 1 {
		t.Fatalf("feedback signals = %+v", signals)
	}
	if len(feedback) != 3 {
		t.Fatalf("feedback rows = %d, want 3 (idempotent)", len(feedback))
	}
}
