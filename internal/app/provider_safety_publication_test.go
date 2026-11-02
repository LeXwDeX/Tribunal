package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func safetyStartReview(t *testing.T, f *reviewFixture) *store.Workflow {
	t.Helper()
	ctx := context.Background()
	dispatcher, err := NewDispatcher(DispatcherConfig{
		Store: f.db, Workspace: f.manager, GitLab: f.gitlab,
		Cipher: f.publisher.cipher, Profiles: f.profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := dispatcher.StartReview(ctx, 42, 7, "default", true)
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.db.Claim(ctx, "safety-review", []string{"review.run"}, time.Minute)
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("claim review = %v, %v", job, err)
	}
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Complete(ctx, *job); err != nil {
		t.Fatal(err)
	}
	return wf
}

func TestSafetyFailedReviewReplacesGreenCommitStatus(t *testing.T) {
	for _, postNote := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure_note_%t", postNote), func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.defaults.CommitStatus, f.defaults.PostFailureNote = true, postNote
			f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
			f.publisher.publicURL = "https://console.invalid"
			api := &mrSerialGitLab{reviewGitLab: f.gitlab}
			f.publisher.gitlab = api
			first := f.seedReview(t, "safety-green")
			if err := f.service.RunReview(ctx, first); err != nil {
				t.Fatal(err)
			}
			f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
				return nil, fmt.Errorf("upstream token=%s: %w", publicationSecret, agent.ErrUnsafeResponse)
			}
			failed := safetyStartReview(t, f)
			if failed.Status != "failed" || failed.PublicationState != store.WorkflowPublicationPublished {
				t.Fatalf("failed workflow = %+v", failed)
			}
			writes := api.snapshot()
			var statuses []mrSerialWrite
			for _, write := range writes {
				if write.kind == "commit-status" {
					statuses = append(statuses, write)
				}
			}
			if len(statuses) != 2 || statuses[0].state != "success" || statuses[1].state != "failed" ||
				statuses[0].sha != f.head || statuses[1].sha != f.head || !strings.Contains(statuses[1].target, failed.ID) {
				t.Fatalf("status writes = %+v", statuses)
			}
			pubs := commitStatusPublications(t, f, failed.ID)
			if len(pubs) != 1 || pubs[0].PublishKey != commitStatusPublishKey(f.head, "failed") {
				t.Fatalf("failure status publications = %+v", pubs)
			}
			firstID := pubs[0].ID
			for i := 0; i < 2; i++ {
				if err := f.publisher.Republish(ctx, failed.ID); err != nil {
					t.Fatalf("Republish: %v", err)
				}
			}
			pubs = commitStatusPublications(t, f, failed.ID)
			if len(pubs) != 1 || pubs[0].ID != firstID {
				t.Fatalf("failure publications after retry = %+v", pubs)
			}
			if f.gitlab.noteCount() != 1 {
				t.Fatalf("summary count = %d", f.gitlab.noteCount())
			}
			body := f.gitlab.notes[0].Body
			if strings.Contains(body, "评审未完成") != postNote {
				t.Fatalf("failure note setting %t, summary = %q", postNote, body)
			}
			assertNoSecret(t, body)
			for _, status := range f.gitlab.statuses {
				assertNoSecret(t, status.Description)
			}
		})
	}
}

func TestSafetyFailurePublicationSwitches(t *testing.T) {
	for _, note := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure_note_%t", note), func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			f.defaults.CommitStatus, f.defaults.PostFailureNote = false, note
			f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
			f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
				return nil, agent.ErrUnsafeResponse
			}
			job := f.seedReview(t, "safety-disabled")
			if err := f.service.RunReview(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantNotes := store.WorkflowPublicationSkipped, 0
			if note {
				wantState, wantNotes = store.WorkflowPublicationPublished, 1
			}
			if wf.PublicationState != wantState || f.gitlab.noteCount() != wantNotes || len(f.gitlab.statuses) != 0 {
				t.Fatalf("publication=%q notes=%d statuses=%+v", wf.PublicationState, f.gitlab.noteCount(), f.gitlab.statuses)
			}
		})
	}
}

func TestSafetyHistoricalFailureDoesNotSuppressNewFailureStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus, f.defaults.PostFailureNote = true, false
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	fail := func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrUnsafeResponse }
	f.service.agentRun = fail
	first := f.seedReview(t, "safety-first-failure")
	if err := f.service.RunReview(ctx, first); err != nil {
		t.Fatal(err)
	}
	old, err := f.db.GetWorkflowByJob(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldPub := commitStatusPublications(t, f, old.ID)
	if len(oldPub) != 1 {
		t.Fatalf("first failure publications = %+v", oldPub)
	}
	f.service.agentRun = f.runner.Run
	safetyStartReview(t, f)
	f.service.agentRun = fail
	last := safetyStartReview(t, f)
	if len(f.gitlab.statuses) != 3 || f.gitlab.statuses[0].State != "failed" ||
		f.gitlab.statuses[1].State != "success" || f.gitlab.statuses[2].State != "failed" {
		t.Fatalf("status transitions = %+v", f.gitlab.statuses)
	}
	newPub := commitStatusPublications(t, f, last.ID)
	if len(newPub) != 1 || newPub[0].ID != oldPub[0].ID {
		t.Fatalf("latest failure publications = %+v; first = %+v", newPub, oldPub)
	}
}

func TestSafetyFailureStatusPrecedesBrokenSummary(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	f.publisher.retries = 1
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrUnsafeResponse
	}
	f.gitlab.noteErr = errors.New("note token=" + publicationSecret)
	job := f.seedReview(t, "safety-summary-error")
	if err := f.service.RunReview(ctx, job); err == nil || IsPermanentJob(err) {
		t.Fatalf("RunReview error = %v, want retryable", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.gitlab.statuses) != 1 || f.gitlab.statuses[0].State != "failed" || wf.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("statuses=%+v workflow=%+v", f.gitlab.statuses, wf)
	}
	assertNoSecret(t, wf.PublicationError)
	f.gitlab.noteErr = nil
	if err := f.service.PublishPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := commitStatusPublications(t, f, wf.ID); len(got) != 1 {
		t.Fatalf("failure status publications = %+v", got)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("summary count = %d", f.gitlab.noteCount())
	}
}

func TestSafetyPendingFailureStatusWithoutSummary(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus, f.defaults.PostFailureNote = true, false
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrUnsafeResponse }
	api := usePartialGitLab(f)
	api.fail(nil, &gitlab.Error{Op: "status", Status: 503})
	job := f.seedReview(t, "safety-status-retry")
	if err := f.service.RunReview(ctx, job); err == nil || IsPermanentJob(err) {
		t.Fatalf("RunReview error = %v, want retryable", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("workflow=%+v err=%v", wf, err)
	}
	api.fail(nil, nil)
	if err := f.service.PublishPending(ctx); err != nil {
		t.Fatal(err)
	}
	wf, err = f.db.GetWorkflow(ctx, wf.ID)
	if err != nil || wf.PublicationState != store.WorkflowPublicationPublished || len(f.gitlab.statuses) != 1 || f.gitlab.statuses[0].State != "failed" || f.gitlab.noteCount() != 0 {
		t.Fatalf("workflow=%+v statuses=%+v notes=%d err=%v", wf, f.gitlab.statuses, f.gitlab.noteCount(), err)
	}
}

func interruptSafetyReview(t *testing.T, f *reviewFixture, job *jobs.Job) string {
	t.Helper()
	started := make(chan struct{})
	var once sync.Once
	f.service.agentRun = func(ctx context.Context, _ agent.RunSpec) (*agent.RunResult, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.service.Handle(ctx, job) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("review did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted Handle error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handle did not stop after shutdown")
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.engine.Snapshot(context.Background(), workflow.ID)
	if err != nil || snapshot.Terminal() {
		t.Fatalf("interrupted snapshot = %+v, err = %v", snapshot, err)
	}
	return workflow.ID
}

func TestSafetyRecoveredFailureHonorsFailureOutputs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, note bool
		wantState    string
		wantStatus   int
		wantNotes    int
	}{
		{name: "status_only", status: true, wantState: store.WorkflowPublicationPublished, wantStatus: 1},
		{name: "both_disabled", wantState: store.WorkflowPublicationSkipped},
		{name: "note_only", note: true, wantState: store.WorkflowPublicationPublished, wantNotes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.defaults.CommitStatus, f.defaults.PostFailureNote = tc.status, tc.note
			f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
			job := f.seedReview(t, "safety-recovered-failure")
			id := interruptSafetyReview(t, f, job)
			f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
				return nil, agent.ErrUnsafeResponse
			}
			if err := f.service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflow(ctx, id)
			if err != nil || wf.Status != string(dag.WorkflowFailed) || wf.PublicationState != tc.wantState {
				t.Fatalf("workflow=%+v err=%v", wf, err)
			}
			if len(f.gitlab.statuses) != tc.wantStatus || f.gitlab.noteCount() != tc.wantNotes {
				t.Fatalf("statuses=%+v notes=%d", f.gitlab.statuses, f.gitlab.noteCount())
			}
			if tc.wantStatus == 1 && f.gitlab.statuses[0].State != "failed" {
				t.Fatalf("statuses=%+v", f.gitlab.statuses)
			}
		})
	}
}

func TestSafetyFailureStatusFences(t *testing.T) {
	for _, change := range []string{"head", "base", "config", "prompt", "generation"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.defaults.CommitStatus, f.defaults.PostFailureNote = true, false
			f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
			f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrUnsafeResponse }
			job := f.seedReview(t, "safety-stale")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "head":
				setReviewHeadForTest(f, strings.Repeat("9", 40))
			case "base":
				f.gitlab.version.BaseSHA = strings.Repeat("9", 40)
			case "config":
				f.gitlab.docs["AGENTS.md"] = "changed"
			case "prompt":
				f.publisher.promptsHash = "changed"
			case "generation":
				mrSerialBumpControl(t, f)()
			}
			before := len(f.gitlab.statuses)
			if err := f.publisher.PublishFailure(ctx, wf.ID, "review execution failed"); !errors.Is(err, ErrStalePublication) {
				t.Fatalf("PublishFailure error = %v, want stale", err)
			}
			if len(f.gitlab.statuses) != before {
				t.Fatalf("stale failure wrote status: %+v", f.gitlab.statuses)
			}
		})
	}
}

func TestSafetyFailureStatusRetryRechecksGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus, f.defaults.PostFailureNote = true, false
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrUnsafeResponse }
	job := f.seedReview(t, "safety-stale-retry")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	api := &statusRetryGitLab{reviewGitLab: f.gitlab, bump: mrSerialBumpControl(t, f)}
	f.publisher.gitlab = api
	before := len(f.gitlab.statuses)
	if err := f.publisher.PublishFailure(ctx, wf.ID, "review execution failed"); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("PublishFailure error = %v, want stale", err)
	}
	if api.attempts() != 1 || len(f.gitlab.statuses) != before {
		t.Fatalf("status attempts=%d statuses=%+v", api.attempts(), f.gitlab.statuses)
	}
}

func TestSafetyWorkspaceUnsafeResponseClass(t *testing.T) {
	const safeClass = "unsafe_model_response"
	if got := onboardingErrorClass(safeClass); got != safeClass {
		t.Fatalf("onboarding class = %q", got)
	}
	if got := onboardingErrorClass(safeClass + "_" + publicationSecret); got != errclass.Internal {
		t.Fatalf("unknown onboarding class = %q", got)
	}
	for _, cause := range []error{
		&onboardingFailure{class: onboardingErrorClass(safeClass)},
		fmt.Errorf("token=%s: %w", publicationSecret, agent.ErrUnsafeResponse),
		&llm.Error{Kind: safeClass, Err: errors.New("token=" + publicationSecret)},
	} {
		if got := workspaceErrorClass(cause); got != safeClass {
			t.Fatalf("workspace class = %q", got)
		}
	}
	f := newReviewFixture(t, reviewTestHead)
	var log bytes.Buffer
	s := &WorkspaceService{store: f.db, logger: slog.New(slog.NewTextHandler(&log, nil))}
	s.markWorkspaceError(context.Background(), 42, fmt.Errorf("token=%s: %w", publicationSecret, agent.ErrUnsafeResponse))
	if !strings.Contains(log.String(), "error_class="+safeClass) {
		t.Fatalf("workspace log = %q", log.String())
	}
	assertNoSecret(t, log.String())
	ws, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil || ws.LastError != store.WorkspaceOperationFailed {
		t.Fatalf("workspace=%+v err=%v", ws, err)
	}
	assertNoSecret(t, ws.LastError)
}

func TestSafetyOnboardingFailureClassAndRedaction(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	s := newOnboardingContextService(t, f, func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, fmt.Errorf("token=%s: %w", publicationSecret, agent.ErrUnsafeResponse)
	})
	var log bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&log, nil))
	err := s.Onboard(context.Background(), 42)
	if err == nil || errclass.Of(err) != "unsafe_model_response" {
		t.Fatalf("onboarding error class = %q, error = %v", errclass.Of(err), err)
	}
	if !strings.Contains(log.String(), "error_class=unsafe_model_response") {
		t.Fatalf("onboarding log = %q", log.String())
	}
	assertNoSecret(t, log.String())
	assertNoSecret(t, err.Error())
	ws, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil || ws.LastError != store.WorkspaceOperationFailed {
		t.Fatalf("workspace=%+v err=%v", ws, err)
	}
	assertNoSecret(t, ws.LastError)
}
