package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
)

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
