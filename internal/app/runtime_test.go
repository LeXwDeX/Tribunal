package app

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type blockingFeedbackStore struct {
	ReviewStore
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (s *blockingFeedbackStore) ListWorkflows(context.Context, store.WorkflowFilter, string, int) ([]store.Workflow, string, error) {
	s.startOnce.Do(func() { close(s.started) })
	<-s.release
	return nil, "", nil
}

type blockingRetentionStore struct {
	*retentionStub
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (s *blockingRetentionStore) DeleteLLMCallsBefore(context.Context, time.Time) (int64, error) {
	s.startOnce.Do(func() { close(s.started) })
	<-s.release
	return 0, nil
}

type emptyRunCleanupStore struct{ RunCleanupStore }

func (emptyRunCleanupStore) ListWorkspaceProjectIDs(context.Context) ([]int64, error) {
	return nil, nil
}

func TestStartBackgroundDoneAfterAllTasksExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		feedback := &blockingFeedbackStore{started: make(chan struct{}), release: make(chan struct{})}
		retention := &blockingRetentionStore{retentionStub: newRetentionStub(), started: make(chan struct{}), release: make(chan struct{})}
		var releaseFeedback, releaseRetention sync.Once
		defer func() {
			releaseFeedback.Do(func() { close(feedback.release) })
			releaseRetention.Do(func() { close(retention.release) })
		}()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		runtime := &Runtime{
			feedback:   &FeedbackService{store: feedback, now: time.Now, logger: logger},
			retention:  &RetentionService{store: retention, now: time.Now, logger: logger, interval: time.Hour},
			runCleanup: &RunCleanupService{store: emptyRunCleanupStore{}, now: time.Now, logger: logger},
			startDone:  make(chan struct{}),
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop, done := runtime.StartBackground(ctx)
		synctest.Wait()
		select {
		case <-feedback.started:
		default:
			t.Fatal("feedback did not start")
		}
		select {
		case <-retention.started:
		default:
			t.Fatal("retention did not start")
		}
		stopped := make(chan struct{})
		go func() { defer close(stopped); stop() }()
		<-stopped
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("done closed while tasks were blocked")
		default:
		}
		releaseFeedback.Do(func() { close(feedback.release) })
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("done closed while retention was blocked")
		default:
		}
		releaseRetention.Do(func() { close(retention.release) })
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("background tasks did not finish")
		}
	})
}

type runtimeTestClients struct{}

func (runtimeTestClients) Client(string) (llm.Client, error) {
	return nil, errors.New("runtime test clients are not configured")
}

// runtimeTestGitLab completes the review fake with the administration methods
// needed by RuntimeDeps.
type runtimeTestGitLab struct {
	*reviewGitLab
	admin *fakeGitLab
}

func newRuntimeTestGitLab() *runtimeTestGitLab {
	return &runtimeTestGitLab{reviewGitLab: newReviewGitLab(sha40), admin: defaultFakeGitLab()}
}

func (g *runtimeTestGitLab) GetProject(ctx context.Context, token, ref string) (*gitlab.Project, error) {
	return g.admin.GetProject(ctx, token, ref)
}

func (g *runtimeTestGitLab) GetTokenInfo(ctx context.Context, token string) (*gitlab.TokenInfo, error) {
	return g.admin.GetTokenInfo(ctx, token)
}

func (g *runtimeTestGitLab) GetMemberAccess(ctx context.Context, token string, projectID, userID int64) (int, error) {
	return g.admin.GetMemberAccess(ctx, token, projectID, userID)
}

func (g *runtimeTestGitLab) ListProjectHooks(ctx context.Context, token string, projectID int64) ([]gitlab.Hook, error) {
	return g.admin.ListProjectHooks(ctx, token, projectID)
}

func (g *runtimeTestGitLab) CreateProjectHook(ctx context.Context, token string, projectID int64, spec gitlab.HookSpec) (int64, error) {
	return g.admin.CreateProjectHook(ctx, token, projectID, spec)
}

func (g *runtimeTestGitLab) UpdateProjectHook(ctx context.Context, token string, projectID, hookID int64, spec gitlab.HookSpec) error {
	return g.admin.UpdateProjectHook(ctx, token, projectID, hookID, spec)
}

func (g *runtimeTestGitLab) DeleteProjectHook(ctx context.Context, token string, projectID, hookID int64) error {
	return g.admin.DeleteProjectHook(ctx, token, projectID, hookID)
}

func TestRuntimeProjectDeleteUsesWorkspaceManagerRoot(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	root := t.TempDir() + "/nested/../workspaces"
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{
			Root:            root,
			RunCleanupAfter: time.Hour,
		},
		Storage: config.StorageConfig{
			LLMCallRetention:  time.Hour,
			DeliveryRetention: time.Hour,
			JobRetention:      time.Hour,
			ContextVersions:   1,
		},
		Engine: config.EngineConfig{
			MaxConcurrentNodes: 1,
			DefaultNodeTimeout: time.Minute,
		},
		Profiles: testProfiles(),
	}
	runtime, err := NewRuntime(ctx, cfg, RuntimeDeps{
		Store:  st,
		Cipher: cipher,
		GitLab: newRuntimeTestGitLab(),
		LLM:    runtimeTestClients{},
		Git:    repoGit{git: &repo.Git{}},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	managerRoot := runtime.WorkspaceManager().Root()
	if managerRoot == "" {
		t.Fatal("workspace manager root is empty")
	}
	if managerRoot == root {
		t.Fatalf("workspace manager root was not normalized: %q", managerRoot)
	}
	if got := runtime.Projects().workspace; got != managerRoot {
		t.Fatalf("project service workspace = %q, want manager root %q", got, managerRoot)
	}

	seedProject(t, st, cipher, 42, "webhook-secret", "", "default")
	paths := []string{
		filepath.Join(managerRoot, "42"),
		filepath.Join(managerRoot, "skills", "project-42"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Projects().Delete(ctx, 42); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still exists: %v", path, err)
		}
	}
}
