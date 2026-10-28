package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// waitContext reports when a lock actually enters its context wait.
type waitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *waitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

type observingConsumerStore struct {
	ReviewStore
	projectReads atomic.Int32
	dossierReads atomic.Int32
}

func (s *observingConsumerStore) GetWorkflowDossier(ctx context.Context, id string) (*store.WorkflowDossier, error) {
	s.dossierReads.Add(1)
	return s.ReviewStore.GetWorkflowDossier(ctx, id)
}

func (s *observingConsumerStore) GetProject(ctx context.Context, id int64) (*store.Project, error) {
	s.projectReads.Add(1)
	return s.ReviewStore.GetProject(ctx, id)
}

func waitConsumer(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not reach barrier")
	}
}

func consumerReadLock(t *testing.T, manager *workspace.Manager, id int64) {
	t.Helper()
	unlock, err := manager.Get(id).RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
}

func TestConsumerWorkspaceReadsProjectOnlyAfterLock(t *testing.T) {
	for _, operation := range []string{"onboard", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			r := newLifecycleRuntime(t)
			observed := &observingConsumerStore{ReviewStore: r.store}
			r.workspaces.store = observed
			unlock, err := r.workspace.Get(42).Lock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				if operation == "onboard" {
					done <- r.workspaces.Onboard(wait, 42)
				} else {
					done <- r.workspaces.Refresh(wait, 42, "main")
				}
			}()
			waitConsumer(t, wait.waiting)
			if got := observed.projectReads.Load(); got != 0 {
				t.Errorf("project reads while lock held = %d", got)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("operation = %v, want canceled", err)
			}
		})
	}
}

func TestConsumerOldWorkspaceJobsDoNotCrossRegistration(t *testing.T) {
	for _, kind := range []string{"workspace.onboard", "workspace.refresh"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r := newLifecycleRuntime(t)
			r.workspaces.git = newFakeGitOps(sha40)
			r.workspaces.gitlab = newReviewGitLab(sha40)
			r.workspaces.mcpFactory = nil
			id, _, err := r.store.Enqueue(ctx, store.Job{ID: "old-workspace-job", Kind: kind, Key: "ws:42:old", Payload: json.RawMessage(`{"project_id":42,"ref":"main"}`)})
			if err != nil {
				t.Fatal(err)
			}
			job, err := r.store.GetJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Projects().Delete(ctx, 42); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Projects().Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"}); err != nil {
				t.Fatal(err)
			}
			if err := r.workspaces.Handle(ctx, job); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			state, err := r.store.GetWorkspace(ctx, 42)
			if err != nil || state.State != "new" {
				t.Fatalf("old job changed new workspace state: %+v, %v", state, err)
			}
			if _, err := os.Lstat(filepath.Join(r.workspace.Root(), "42")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old job touched project directory: %v", err)
			}
			newID, _, err := r.store.Enqueue(ctx, store.Job{ID: "new-workspace-job", Kind: "workspace.refresh", Key: "ws:42:new", Payload: json.RawMessage(`{"project_id":42,"ref":"main","regenerate_context":false,"reindex":false}`)})
			if err != nil {
				t.Fatal(err)
			}
			newJob, err := r.store.GetJob(ctx, newID)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.workspaces.Handle(ctx, newJob); err != nil {
				t.Fatalf("new workspace job: %v", err)
			}
			state, err = r.store.GetWorkspace(ctx, 42)
			if err != nil || state.State != "ready" {
				t.Fatalf("new workspace job state: %+v, %v", state, err)
			}
		})
	}
}

func TestConsumerOldReviewJobDoesNotCrossRegistration(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	old, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(ctx, old.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Projects().Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"}); err != nil {
		t.Fatal(err)
	}
	newResult, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`)})
	if err != nil {
		t.Fatal(err)
	}
	api := newReviewGitLab(sha40)
	r.reviews.gitlab = api
	r.reviews.git = newFakeGitOps(sha40)
	r.reviews.extensions = nil
	r.reviews.testIdentity = "synthetic"
	observed := &observingConsumerStore{ReviewStore: r.store}
	r.reviews.store = observed
	if err := r.reviews.RunReview(ctx, job); !IsPermanentJob(err) {
		t.Fatalf("old review job error = %v, want permanent", err)
	}
	if observed.projectReads.Load() != 0 {
		t.Fatal("old review job reached the new registration")
	}
	if _, err := os.Lstat(filepath.Join(r.workspace.Root(), "42")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old job touched project directory: %v", err)
	}
	r.reviews.profiles = map[string]config.ProfileConfig{"default": testReviewProfile()}
	r.reviews.providerNames = []string{"fake"}
	r.reviews.agentRun = (&fakeAgentRunner{}).Run
	r.reviews.clients, r.reviews.tools, r.reviews.skills, r.reviews.blocks = nil, nil, nil, nil
	r.reviews.reviewDefaults = testDefaults()
	r.publisher.gitlab, r.publisher.profiles = api, r.reviews.profiles
	r.publisher.reviewDefaults = r.reviews.reviewDefaults
	newJob, err := r.store.GetJob(ctx, newResult.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reviews.RunReview(ctx, newJob); err != nil {
		t.Fatalf("new review job: %v", err)
	}
	workflow, err := r.store.GetWorkflowByJob(ctx, newJob.ID)
	if err != nil || workflow.Status != "completed" {
		t.Fatalf("new review workflow = %+v, %v", workflow, err)
	}
}

func seedConsumerWorkflow(t *testing.T, r *Runtime) (*store.Job, *store.Workflow) {
	t.Helper()
	ctx := context.Background()
	job, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open"}`)})
	if err != nil {
		t.Fatal(err)
	}
	dossier := testDossier(t, sha40)
	data, err := review.MarshalDossier(dossier)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := review.DossierHash(dossier)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := testGraph(t, dossier)
	id, _, err := r.store.CreateReviewWorkflow(ctx, sqlite.ReviewWorkflowRequest{
		JobID: job.JobID, Spec: spec, Meta: map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA, "fingerprint": "synthetic", "config_hash": "synthetic", "prompt_hash": "synthetic", "profile": "default"},
		DossierVersion: review.DossierSchemaVersion, DossierHash: hash, DossierData: data,
		ExpectedGeneration: job.Generation, ProjectID: 42, MRIID: 7, HeadSHA: sha40,
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := r.store.GetWorkflow(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	storedJob, err := r.store.GetJob(ctx, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	return storedJob, workflow
}

func TestConsumerOldWorkflowDoesNotCrossRegistration(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	_, workflow := seedConsumerWorkflow(t, r)
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Projects().Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"}); err != nil {
		t.Fatal(err)
	}
	observed := &observingConsumerStore{ReviewStore: r.store}
	r.reviews.store = observed
	r.feedback.store = observed
	_ = r.reviews.recoverWorkflow(ctx, *workflow)
	r.feedback.collectWorkflow(ctx, *workflow)
	if observed.dossierReads.Load() != 0 || observed.projectReads.Load() != 0 {
		t.Fatalf("old workflow reached deleted dossier or new project: dossier=%d project=%d", observed.dossierReads.Load(), observed.projectReads.Load())
	}
}

func TestConsumerDeleteFirstBlocksAllStaleConsumers(t *testing.T) {
	r := newLifecycleRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, workflow := seedConsumerWorkflow(t, r)
	wsID, _, err := r.store.Enqueue(ctx, store.Job{ID: "old-refresh", Kind: "workspace.refresh", Key: "ws:42:old-refresh", Payload: json.RawMessage(`{"project_id":42,"ref":"main"}`)})
	if err != nil {
		t.Fatal(err)
	}
	wsJob, err := r.store.GetJob(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(r.workspace.Root(), "42", "canary")
	writeProjectCanary(t, canary)
	committed, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	r.Projects().store = &deleteProjectStore{ProjectStore: r.store, afterDelete: func() error {
		close(committed)
		<-release
		return nil
	}}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- r.Projects().Delete(ctx, 42) }()
	waitConsumer(t, committed)
	observed := &observingConsumerStore{ReviewStore: r.store}
	r.workspaces.store, r.reviews.store, r.feedback.store = observed, observed, observed
	type outcome struct {
		name string
		err  error
	}
	done := make(chan outcome, 4)
	for _, operation := range []string{"workspace", "review", "recovery", "feedback"} {
		wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
		go func(name string) {
			var err error
			switch name {
			case "workspace":
				err = r.workspaces.Handle(wait, wsJob)
			case "review":
				err = r.reviews.RunReview(wait, job)
			case "recovery":
				err = r.reviews.recoverWorkflow(wait, *workflow)
			case "feedback":
				r.feedback.collectWorkflow(wait, *workflow)
			}
			done <- outcome{name, err}
		}(operation)
		waitConsumer(t, wait.waiting)
	}
	if observed.projectReads.Load() != 0 || observed.dossierReads.Load() != 0 {
		t.Fatal("consumer read project data while Delete held the lock")
	}
	requireProjectCanary(t, canary)
	releaseOnce.Do(func() { close(release) })
	if err := <-deleteDone; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for range 4 {
		result := <-done
		if result.name == "review" {
			if !IsPermanentJob(result.err) {
				t.Fatalf("review error = %v, want permanent", result.err)
			}
		} else if result.err != nil {
			t.Fatalf("%s error = %v", result.name, result.err)
		}
	}
	if observed.projectReads.Load() != 0 || observed.dossierReads.Load() != 0 {
		t.Fatal("stale consumer reached the deleted project or dossier")
	}
	if _, err := os.Lstat(filepath.Join(r.workspace.Root(), "42")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumer recreated deleted workspace: %v", err)
	}
}

type barrierReviewGitLab struct {
	*reviewGitLab
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *barrierReviewGitLab) GetMergeRequest(ctx context.Context, token string, projectID, iid int64) (*gitlab.MergeRequest, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return g.reviewGitLab.GetMergeRequest(ctx, token, projectID, iid)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConsumerReviewCompletesWithDeleteWriterQueued(t *testing.T) {
	r := newLifecycleRuntime(t)
	api := &barrierReviewGitLab{reviewGitLab: newReviewGitLab(sha40), entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var release sync.Once
	defer release.Do(func() { close(api.release) })
	r.reviews.gitlab = api
	r.reviews.git = newFakeGitOps(sha40)
	r.reviews.profiles = map[string]config.ProfileConfig{"default": testReviewProfile()}
	r.reviews.providerNames = []string{"fake"}
	r.reviews.extensions = nil
	r.reviews.testIdentity = "synthetic"
	runner := &fakeAgentRunner{}
	r.reviews.agentRun = runner.Run
	r.reviews.clients, r.reviews.tools, r.reviews.skills, r.reviews.blocks = nil, nil, nil, nil
	r.reviews.reviewDefaults = testDefaults()
	r.publisher.gitlab = api
	r.publisher.profiles = r.reviews.profiles
	r.publisher.reviewDefaults = r.reviews.reviewDefaults
	result, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(ctx, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(r.workspace.Root(), "42", "canary")
	writeProjectCanary(t, canary)
	reviewDone := make(chan error, 1)
	go func() { reviewDone <- r.reviews.RunReview(ctx, job) }()
	waitConsumer(t, api.entered)
	deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- r.Projects().Delete(deleteCtx, 42) }()
	waitConsumer(t, deleteCtx.waiting)
	requireProjectCanary(t, canary)
	if _, err := r.store.GetProject(ctx, 42); err != nil {
		t.Fatalf("project deleted while review active: %v", err)
	}
	release.Do(func() { close(api.release) })
	select {
	case err := <-reviewDone:
		if err != nil {
			t.Fatalf("review with waiting writer: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("review deadlocked with waiting writer")
	}
	if runner.count() == 0 {
		t.Fatal("review did not execute its agent")
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(r.workspace.Root(), "42")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace remains after Delete: %v", err)
	}
}

func TestConsumerReviewCancellationReleasesDelete(t *testing.T) {
	r := newLifecycleRuntime(t)
	guard, stopGuard := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopGuard()
	ctx, cancel := context.WithCancel(guard)
	defer cancel()
	api := &barrierReviewGitLab{reviewGitLab: newReviewGitLab(sha40), entered: make(chan struct{}), release: make(chan struct{})}
	defer close(api.release)
	r.reviews.gitlab = api
	result, err := r.store.EnqueueReviewLatest(guard, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(guard, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(r.workspace.Root(), "42", "canary")
	writeProjectCanary(t, canary)
	done := make(chan error, 1)
	go func() { done <- r.reviews.RunReview(ctx, job) }()
	waitConsumer(t, api.entered)
	deleteCtx := &waitContext{Context: guard, waiting: make(chan struct{})}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- r.Projects().Delete(deleteCtx, 42) }()
	waitConsumer(t, deleteCtx.waiting)
	requireProjectCanary(t, canary)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("review = %v, want cancellation", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("Delete after cancellation: %v", err)
	}
}

type failingConsumerJobStore struct{ ReviewStore }

var errConsumerLookup = errors.New("synthetic job lookup failure")

func (s *failingConsumerJobStore) GetJob(context.Context, string) (*store.Job, error) {
	return nil, errConsumerLookup
}

func TestConsumerLookupFailureReleasesProjectLock(t *testing.T) {
	for _, operation := range []string{"workspace", "review"} {
		t.Run(operation, func(t *testing.T) {
			r := newLifecycleRuntime(t)
			ctx := context.Background()
			failing := &failingConsumerJobStore{ReviewStore: r.store}
			var err error
			if operation == "workspace" {
				r.workspaces.store = failing
				err = r.workspaces.Handle(ctx, &store.Job{ID: "synthetic", Kind: "workspace.refresh", Payload: json.RawMessage(`{"project_id":42}`)})
			} else {
				r.reviews.store = failing
				result, enqueueErr := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open"}`)})
				if enqueueErr != nil {
					t.Fatal(enqueueErr)
				}
				job, lookupErr := r.store.GetJob(ctx, result.JobID)
				if lookupErr != nil {
					t.Fatal(lookupErr)
				}
				err = r.reviews.RunReview(ctx, job)
			}
			if !errors.Is(err, errConsumerLookup) {
				t.Fatalf("consumer error = %v", err)
			}
			deleteCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := r.Projects().Delete(deleteCtx, 42); err != nil {
				t.Fatalf("Delete after lookup error: %v", err)
			}
		})
	}
}

func TestConsumerTerminalFallbackCompletesWithDeleteWriterQueued(t *testing.T) {
	r := newLifecycleRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base, git := newReviewGitLab(sha40), newFakeGitOps(sha40)
	r.reviews.gitlab, r.reviews.git = base, git
	r.reviews.profiles = map[string]config.ProfileConfig{"default": testReviewProfile()}
	r.reviews.providerNames = []string{"fake"}
	r.reviews.extensions = nil
	r.reviews.testIdentity = "initial"
	r.reviews.agentRun = (&fakeAgentRunner{}).Run
	r.reviews.clients, r.reviews.tools, r.reviews.skills, r.reviews.blocks = nil, nil, nil, nil
	r.reviews.reviewDefaults = testDefaults()
	r.publisher.gitlab, r.publisher.profiles = base, r.reviews.profiles
	r.publisher.reviewDefaults = r.reviews.reviewDefaults
	result, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(ctx, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reviews.RunReview(ctx, job); err != nil {
		t.Fatalf("initial review: %v", err)
	}
	workflow, err := r.store.GetWorkflowByJob(ctx, job.ID)
	if err != nil || workflow.Status != "completed" {
		t.Fatalf("initial workflow = %+v, %v", workflow, err)
	}
	git.mu.Lock()
	initialAdds := git.addCalls
	git.mu.Unlock()
	r.reviews.testIdentity = "changed"
	api := &barrierReviewGitLab{reviewGitLab: base, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(api.release) })
	r.reviews.gitlab, r.publisher.gitlab = api, api
	canary := filepath.Join(r.workspace.Root(), "42", "canary")
	writeProjectCanary(t, canary)
	done := make(chan error, 1)
	go func() { done <- r.reviews.RunReview(ctx, job) }()
	waitConsumer(t, api.entered)
	deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- r.Projects().Delete(deleteCtx, 42) }()
	waitConsumer(t, deleteCtx.waiting)
	requireProjectCanary(t, canary)
	release.Do(func() { close(api.release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("terminal fallback: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("terminal fallback deadlocked with waiting writer")
	}
	git.mu.Lock()
	adds := git.addCalls
	git.mu.Unlock()
	if adds != initialAdds+1 {
		t.Fatalf("fallback worktree additions = %d, want %d", adds, initialAdds+1)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestConsumerRecoveryFallbackCompletesWithDeleteWriterQueued(t *testing.T) {
	r := newLifecycleRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base, git, runner := newReviewGitLab(sha40), newFakeGitOps(sha40), &fakeAgentRunner{}
	r.reviews.gitlab, r.reviews.git = base, git
	r.reviews.profiles = map[string]config.ProfileConfig{"default": testReviewProfile()}
	r.reviews.providerNames = []string{"fake"}
	r.reviews.extensions = nil
	r.reviews.testIdentity = "synthetic"
	r.reviews.agentRun = runner.Run
	r.reviews.clients, r.reviews.tools, r.reviews.skills, r.reviews.blocks = nil, nil, nil, nil
	r.reviews.reviewDefaults = testDefaults()
	r.publisher.gitlab, r.publisher.profiles = base, r.reviews.profiles
	r.publisher.reviewDefaults = r.reviews.reviewDefaults
	result, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(ctx, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	f := &reviewFixture{db: r.store, gitlab: base, git: git, runner: runner, service: r.reviews, engine: r.engine}
	wfID := interruptReview(t, f, job)
	if err := r.engine.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	workflow, err := r.store.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	git.mu.Lock()
	initialAdds := git.addCalls
	git.mu.Unlock()
	api := &barrierReviewGitLab{reviewGitLab: base, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(api.release) })
	r.reviews.gitlab, r.publisher.gitlab = api, api
	canary := filepath.Join(r.workspace.Root(), "42", "canary")
	writeProjectCanary(t, canary)
	done := make(chan error, 1)
	go func() { done <- r.reviews.recoverWorkflow(ctx, *workflow) }()
	waitConsumer(t, api.entered)
	deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- r.Projects().Delete(deleteCtx, 42) }()
	waitConsumer(t, deleteCtx.waiting)
	requireProjectCanary(t, canary)
	release.Do(func() { close(api.release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recovery fallback: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("recovery fallback deadlocked with waiting writer")
	}
	git.mu.Lock()
	adds := git.addCalls
	git.mu.Unlock()
	if adds != initialAdds+1 {
		t.Fatalf("recovery worktree additions = %d, want %d", adds, initialAdds+1)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

type barrierWorkspaceGit struct {
	*fakeGitOps
	entered chan struct{}
	release chan struct{}
}

func (g *barrierWorkspaceGit) EnsureMirror(ctx context.Context, dir, remote string, auth repo.Auth) error {
	close(g.entered)
	select {
	case <-g.release:
		return g.fakeGitOps.EnsureMirror(ctx, dir, remote, auth)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestConsumerWorkspaceDeleteWaitsAndReleasesOnCancel(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "canceled"}[canceled], func(t *testing.T) {
			r := newLifecycleRuntime(t)
			guard, stopGuard := context.WithTimeout(context.Background(), 10*time.Second)
			defer stopGuard()
			ctx, cancel := context.WithCancel(guard)
			defer cancel()
			git := &barrierWorkspaceGit{fakeGitOps: newFakeGitOps(sha40), entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(git.release) })
			r.workspaces.git = git
			r.workspaces.gitlab = newReviewGitLab(sha40)
			r.workspaces.mcpFactory = nil
			canary := filepath.Join(r.workspace.Root(), "42", "canary")
			writeProjectCanary(t, canary)
			refreshDone := make(chan error, 1)
			go func() { refreshDone <- r.workspaces.Refresh(ctx, 42, "main") }()
			waitConsumer(t, git.entered)
			deleteCtx := &waitContext{Context: guard, waiting: make(chan struct{})}
			deleteDone := make(chan error, 1)
			go func() { deleteDone <- r.Projects().Delete(deleteCtx, 42) }()
			waitConsumer(t, deleteCtx.waiting)
			requireProjectCanary(t, canary)
			if canceled {
				cancel()
			} else {
				release.Do(func() { close(git.release) })
			}
			err := <-refreshDone
			if canceled && !errors.Is(err, context.Canceled) || !canceled && err != nil {
				t.Fatalf("Refresh = %v", err)
			}
			if err := <-deleteDone; err != nil {
				t.Fatalf("Delete: %v", err)
			}
		})
	}
}

type barrierFeedbackDiff struct {
	entered chan struct{}
	release chan struct{}
}

func (g *barrierFeedbackDiff) FileDiff(ctx context.Context, _, _, _, path string) (*repo.FileDiff, error) {
	close(g.entered)
	select {
	case <-g.release:
		return &repo.FileDiff{OldPath: path, Deleted: true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConsumerFeedbackHoldsLockThroughFileDiff(t *testing.T) {
	r := newLifecycleRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	api, git := newReviewGitLab(sha40), newFakeGitOps(sha40)
	r.reviews.gitlab, r.reviews.git = api, git
	r.reviews.profiles = map[string]config.ProfileConfig{"default": testReviewProfile()}
	r.reviews.providerNames = []string{"fake"}
	r.reviews.extensions = nil
	r.reviews.testIdentity = "synthetic"
	r.reviews.agentRun = (&fakeAgentRunner{}).Run
	r.reviews.clients, r.reviews.tools, r.reviews.skills, r.reviews.blocks = nil, nil, nil, nil
	r.reviews.reviewDefaults = testDefaults()
	r.publisher.gitlab, r.publisher.profiles = api, r.reviews.profiles
	r.publisher.reviewDefaults = r.reviews.reviewDefaults
	result, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(ctx, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reviews.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	workflow, err := r.store.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	if len(api.discussions) == 0 {
		api.mu.Unlock()
		publications, _ := r.store.ListPublications(ctx, workflow.ID)
		t.Fatalf("synthetic review published no discussion: workflow=%+v publications=%+v", workflow, publications)
	}
	api.discussions[0].Notes[0].Resolved = true
	api.mr.SHA = "cccccccccccccccccccccccccccccccccccccccc"
	api.mu.Unlock()
	diff := &barrierFeedbackDiff{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(diff.release) })
	r.feedback.gitlab, r.feedback.git, r.feedback.diff = api, git, diff
	canary := filepath.Join(r.workspace.Root(), "42", "canary")
	writeProjectCanary(t, canary)
	feedbackDone := make(chan struct{})
	go func() { r.feedback.collectWorkflow(ctx, *workflow); close(feedbackDone) }()
	waitConsumer(t, diff.entered)
	deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- r.Projects().Delete(deleteCtx, 42) }()
	waitConsumer(t, deleteCtx.waiting)
	requireProjectCanary(t, canary)
	release.Do(func() { close(diff.release) })
	waitConsumer(t, feedbackDone)
	if err := <-deleteDone; err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
