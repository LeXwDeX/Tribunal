package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func openLifecycleStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ConfigureMCPSecrets(context.Background(), newTestCipher(t)); err != nil {
		t.Fatal(err)
	}
	return st
}

type deleteProjectStore struct {
	ProjectStore
	deleteErr   error
	afterDelete func() error
}

func (s *deleteProjectStore) DeleteProject(ctx context.Context, id int64) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if err := s.ProjectStore.DeleteProject(ctx, id); err != nil {
		return err
	}
	if s.afterDelete != nil {
		return s.afterDelete()
	}
	return nil
}

func writeProjectCanary(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("canary"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireProjectCanary(t *testing.T, path string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != "canary" {
		t.Fatalf("canary %s = %q, %v", path, got, err)
	}
}

func requireAppCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != code {
		t.Fatalf("error = %#v, want %s", err, code)
	}
	return appErr
}

type lifecycleProjectCloser struct{ calls int }

func (c *lifecycleProjectCloser) CloseProject(int64) error {
	c.calls++
	return nil
}

func TestProjectDeleteDBFailurePreservesBothTargets(t *testing.T) {
	ctx := context.Background()
	st, cipher, root := openLifecycleStore(t), newTestCipher(t), t.TempDir()
	project := seedProject(t, st, cipher, 42, "secret", "", "default")
	hookID := int64(99)
	if err := st.UpdateProjectHook(ctx, 42, &hookID, project.WebhookSecretEnc); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "42", "canary"), filepath.Join(root, "skills", "project-42", "canary")}
	for _, path := range paths {
		writeProjectCanary(t, path)
	}
	service := newWorkspaceProjectService(t, &deleteProjectStore{ProjectStore: st, deleteErr: errors.New("synthetic DB failure")}, defaultFakeGitLab(), cipher, root)
	if err := service.Delete(ctx, 42); err == nil {
		t.Fatal("Delete succeeded with DB failure")
	}
	for _, path := range paths {
		requireProjectCanary(t, path)
	}
	if _, err := st.GetProject(ctx, 42); err != nil {
		t.Fatalf("project missing: %v", err)
	}
}

func TestProjectDeletePreflightsBothTargets(t *testing.T) {
	ctx := context.Background()
	st, cipher, root, outside := openLifecycleStore(t), newTestCipher(t), t.TempDir(), t.TempDir()
	project := seedProject(t, st, cipher, 42, "secret", "", "default")
	hookID := int64(99)
	if err := st.UpdateProjectHook(ctx, 42, &hookID, project.WebhookSecretEnc); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "42", "canary")
	second := filepath.Join(outside, "project-42", "canary")
	writeProjectCanary(t, first)
	writeProjectCanary(t, second)
	if err := os.Symlink(outside, filepath.Join(root, "skills")); err != nil {
		t.Fatal(err)
	}
	api := defaultFakeGitLab()
	service := newWorkspaceProjectService(t, st, api, cipher, root)
	closer := &lifecycleProjectCloser{}
	service.projectExtensions = closer
	if err := service.Delete(ctx, 42); err == nil {
		t.Fatal("Delete accepted unsafe second target")
	}
	if closer.calls != 0 || api.callCount("DeleteProjectHook") != 0 {
		t.Fatal("preflight failure reached extension or hook cleanup")
	}
	requireProjectCanary(t, first)
	requireProjectCanary(t, second)
	if _, err := st.GetProject(ctx, 42); err != nil {
		t.Fatalf("project missing: %v", err)
	}
}

func TestProjectDeletePartialCleanupIsReported(t *testing.T) {
	ctx := context.Background()
	st, cipher, root, outside := openLifecycleStore(t), newTestCipher(t), t.TempDir(), t.TempDir()
	seedProject(t, st, cipher, 42, "secret", "", "default")
	writeProjectCanary(t, filepath.Join(root, "42", "canary"))
	outsideCanary := filepath.Join(outside, "project-42", "canary")
	writeProjectCanary(t, outsideCanary)
	failing := &deleteProjectStore{ProjectStore: st, afterDelete: func() error { return os.Symlink(outside, filepath.Join(root, "skills")) }}
	service := newWorkspaceProjectService(t, failing, defaultFakeGitLab(), cipher, root)
	err := service.Delete(ctx, 42)
	appErr := requireAppCode(t, err, "internal")
	if !strings.Contains(appErr.Message, "registration") || !strings.Contains(appErr.Message, "cleanup") {
		t.Fatalf("partial completion not described: %q", appErr.Message)
	}
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DB deletion not committed: %v", err)
	}
	requireProjectCanary(t, outsideCanary)
	if err := service.Delete(ctx, 42); err == nil {
		t.Fatal("repeat Delete accepted unsafe residue")
	}
	restarted := newWorkspaceProjectService(t, st, defaultFakeGitLab(), cipher, root)
	if _, err := restarted.Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"}); err == nil {
		t.Fatal("registration after restart accepted unsafe residue")
	}
	if err := restarted.Delete(ctx, 42); err == nil {
		t.Fatal("Delete after restart accepted unsafe residue")
	}
	requireProjectCanary(t, outsideCanary)
}

func TestProjectDeleteCancellationAfterCommitStillCleans(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st, cipher, root := openLifecycleStore(t), newTestCipher(t), t.TempDir()
			seedProject(t, st, cipher, 42, "secret", "", "default")
			canary := filepath.Join(root, "42", "canary")
			writeProjectCanary(t, canary)
			outside := t.TempDir()
			failing := &deleteProjectStore{ProjectStore: st, afterDelete: func() error {
				cancel()
				if partial {
					return os.Symlink(outside, filepath.Join(root, "skills"))
				}
				return nil
			}}
			service := newWorkspaceProjectService(t, failing, defaultFakeGitLab(), cipher, root)
			err := service.Delete(ctx, 42)
			if partial {
				appErr := requireAppCode(t, err, "internal")
				if !strings.Contains(appErr.Message, "cleanup") {
					t.Fatalf("partial completion not described: %v", err)
				}
			} else if err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := os.Lstat(canary); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workspace cleanup skipped: %v", err)
			}
			if _, err := st.GetProject(context.Background(), 42); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("project still present: %v", err)
			}
		})
	}
}

func TestProjectDeleteSkillsSymlinkDoesNotTouchOtherData(t *testing.T) {
	ctx := context.Background()
	st, cipher, root, outside := openLifecycleStore(t), newTestCipher(t), t.TempDir(), t.TempDir()
	seedProject(t, st, cipher, 42, "secret", "", "default")
	outsideCanary := filepath.Join(outside, "canary")
	otherProject := filepath.Join(root, "43", "canary")
	otherSkill := filepath.Join(root, "skills", "project-43", "canary")
	platformSkill := filepath.Join(root, "skills", "platform", "canary")
	for _, path := range []string{outsideCanary, otherProject, otherSkill, platformSkill} {
		writeProjectCanary(t, path)
	}
	link := filepath.Join(root, "skills", "project-42")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	service := newWorkspaceProjectService(t, st, defaultFakeGitLab(), cipher, root)
	if err := service.Delete(ctx, 42); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link still exists: %v", err)
	}
	for _, path := range []string{outsideCanary, otherProject, otherSkill, platformSkill} {
		requireProjectCanary(t, path)
	}
}

func TestProjectResidualPathsBlockRegisterAndDelete(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink", "skill"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st, cipher, root := openLifecycleStore(t), newTestCipher(t), t.TempDir()
			path := filepath.Join(root, "42")
			switch kind {
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "file":
				writeProjectCanary(t, path)
			case "symlink":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			case "skill":
				path = filepath.Join(root, "skills", "project-42")
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			service := newWorkspaceProjectService(t, st, defaultFakeGitLab(), cipher, root)
			requireAppCode(t, service.Delete(ctx, 42), "conflict")
			_, err := service.Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"})
			requireAppCode(t, err, "conflict")
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("residue removed: %v", err)
			}
			if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unexpected project: %v", err)
			}
		})
	}
	// No registration and no residue is still a 404.
	service := newWorkspaceProjectService(t, openLifecycleStore(t), defaultFakeGitLab(), newTestCipher(t), t.TempDir())
	requireAppCode(t, service.Delete(context.Background(), 42), "not_found")
}

func newLifecycleRuntime(t *testing.T) *Runtime {
	t.Helper()
	st, cipher := openLifecycleStore(t), newTestCipher(t)
	manager := workspace.NewManager(t.TempDir(), nil)
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: t.TempDir(), RunCleanupAfter: time.Hour},
		Storage:   config.StorageConfig{LLMCallRetention: time.Hour, DeliveryRetention: time.Hour, JobRetention: time.Hour, ContextVersions: 1},
		Engine:    config.EngineConfig{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Minute},
		Profiles:  testProfiles(),
	}
	runtime, err := NewRuntime(context.Background(), cfg, RuntimeDeps{Store: st, Cipher: cipher, GitLab: newRuntimeTestGitLab(), LLM: runtimeTestClients{}, Git: repoGit{git: &repo.Git{}}, Workspace: manager})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	seedProject(t, st, cipher, 42, "secret", "", "default")
	return runtime
}

func TestLifecycleRuntimeDeleteWaitsForWorkspace(t *testing.T) {
	runtime := newLifecycleRuntime(t)
	canary := filepath.Join(runtime.workspace.Root(), "42", "canary")
	writeProjectCanary(t, canary)
	unlock, err := runtime.workspace.Get(42).RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = runtime.Projects().Delete(ctx, 42)
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Delete while read locked = %v", err)
	}
	requireProjectCanary(t, canary)
	if _, err := runtime.store.GetProject(context.Background(), 42); err != nil {
		t.Fatalf("project missing: %v", err)
	}
	if err := runtime.Projects().Delete(context.Background(), 42); err != nil {
		t.Fatalf("Delete after unlock: %v", err)
	}
}

func TestLifecycleRuntimeRegisterWaitsForWorkspace(t *testing.T) {
	runtime := newLifecycleRuntime(t)
	if err := runtime.Projects().Delete(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	unlock, err := runtime.workspace.Get(42).RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = runtime.Projects().Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"})
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Register while read locked = %v", err)
	}
	if _, err := runtime.store.GetProject(context.Background(), 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project unexpectedly registered: %v", err)
	}
}

func TestLifecycleRuntimeDeleteRebuildsProjectExtensions(t *testing.T) {
	ctx := context.Background()
	runtime := newLifecycleRuntime(t)
	id := int64(42)
	if _, err := runtime.store.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopeProject, ProjectID: &id, Block: "code-review", Skills: []string{"old-skill"}}); err != nil {
		t.Fatal(err)
	}
	old, err := runtime.projectExts.ForProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runtime.workspace.Root(), "42")); err != nil {
		t.Fatalf("resolver did not use shared manager root: %v", err)
	}
	if err := runtime.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Projects().Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.store.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopeProject, ProjectID: &id, Block: "code-review", Skills: []string{"new-skill"}}); err != nil {
		t.Fatal(err)
	}
	current, err := runtime.projectExts.ForProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if current == old || current.mcp == old.mcp {
		t.Fatal("project extension cache reused after re-registration")
	}
	if got := current.blocks["code-review"].Skills; len(got) != 1 || got[0] != "new-skill" {
		t.Fatalf("rebuilt project binding = %v", got)
	}
}

func TestLifecycleDeleteDBFailureAllowsFreshExtensions(t *testing.T) {
	ctx := context.Background()
	runtime := newLifecycleRuntime(t)
	old, err := runtime.projectExts.ForProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Projects().store = &deleteProjectStore{ProjectStore: runtime.store, deleteErr: errors.New("synthetic DB failure")}
	if err := runtime.Projects().Delete(ctx, 42); err == nil {
		t.Fatal("Delete succeeded despite DB failure")
	}
	current, err := runtime.projectExts.ForProject(ctx, 42)
	if err != nil {
		t.Fatalf("rebuild after DB failure: %v", err)
	}
	if current == old || current.mcp == old.mcp {
		t.Fatal("closed set was reused after DB failure")
	}
	if _, err := runtime.store.GetProject(ctx, 42); err != nil {
		t.Fatalf("project missing: %v", err)
	}
}

type lifecycleHookGitLab struct {
	*fakeGitLab
	called chan context.Context
	block  bool
}

func (g *lifecycleHookGitLab) DeleteProjectHook(ctx context.Context, _ string, _, _ int64) error {
	g.called <- ctx
	if g.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return errors.New("synthetic hook failure")
}

var _ gitlab.API = (*lifecycleHookGitLab)(nil)

func TestLifecycleDeleteHookIsBoundedAndBestEffort(t *testing.T) {
	ctx := context.Background()
	st, cipher, root := openLifecycleStore(t), newTestCipher(t), t.TempDir()
	project := seedProject(t, st, cipher, 42, "secret", "", "default")
	hookID := int64(99)
	if err := st.UpdateProjectHook(ctx, 42, &hookID, project.WebhookSecretEnc); err != nil {
		t.Fatal(err)
	}
	api := &lifecycleHookGitLab{fakeGitLab: defaultFakeGitLab(), called: make(chan context.Context, 1)}
	service := newWorkspaceProjectService(t, st, api, cipher, root)
	if err := service.Delete(ctx, 42); err != nil {
		t.Fatalf("Delete after hook failure: %v", err)
	}
	hookCtx := <-api.called
	if _, ok := hookCtx.Deadline(); !ok {
		t.Fatal("hook deletion has no deadline")
	}
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project remains: %v", err)
	}
}

func TestLifecycleDeleteHookCancellationDoesNotDelete(t *testing.T) {
	st, cipher, root := openLifecycleStore(t), newTestCipher(t), t.TempDir()
	project := seedProject(t, st, cipher, 42, "secret", "", "default")
	hookID := int64(99)
	if err := st.UpdateProjectHook(context.Background(), 42, &hookID, project.WebhookSecretEnc); err != nil {
		t.Fatal(err)
	}
	api := &lifecycleHookGitLab{fakeGitLab: defaultFakeGitLab(), called: make(chan context.Context, 1), block: true}
	service := newWorkspaceProjectService(t, st, api, cipher, root)
	canary := filepath.Join(root, "42", "canary")
	writeProjectCanary(t, canary)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Delete(ctx, 42) }()
	select {
	case <-api.called:
	case <-time.After(time.Second):
		t.Fatal("hook deletion not entered")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete = %v", err)
	}
	requireProjectCanary(t, canary)
	if _, err := st.GetProject(context.Background(), 42); err != nil {
		t.Fatalf("project missing: %v", err)
	}
}

type lifecycleCloser struct {
	entered chan struct{}
	release chan struct{}
	err     error
	calls   atomic.Int32
	once    sync.Once
}

func (c *lifecycleCloser) Close() error {
	c.calls.Add(1)
	if c.entered != nil {
		c.once.Do(func() { close(c.entered) })
	}
	if c.release != nil {
		<-c.release
	}
	return c.err
}

func TestLifecycleDeleteCloseFailureRetainsUnavailableCache(t *testing.T) {
	ctx := context.Background()
	runtime := newLifecycleRuntime(t)
	set, err := runtime.projectExts.ForProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.mcp.Close() })
	closer := &lifecycleCloser{err: errors.New("synthetic close failure")}
	set.closer = closer
	canary := filepath.Join(runtime.workspace.Root(), "42", "canary")
	writeProjectCanary(t, canary)
	if err := runtime.Projects().Delete(ctx, 42); err == nil {
		t.Fatal("Delete succeeded despite close failure")
	}
	if closer.calls.Load() != 1 {
		t.Fatalf("close calls = %d", closer.calls.Load())
	}
	requireProjectCanary(t, canary)
	if _, err := runtime.store.GetProject(ctx, 42); err != nil {
		t.Fatalf("project missing: %v", err)
	}
	if reused, err := runtime.projectExts.ForProject(ctx, 42); err == nil || reused != nil {
		t.Fatal("failed closed set was reissued")
	}
	if runtime.projectExts.sets[42] != set {
		t.Fatal("failed close lost the manager reference")
	}
}

func TestLifecycleDeleteWaitsForCloseWithoutHoldingResolverMutex(t *testing.T) {
	ctx := context.Background()
	runtime := newLifecycleRuntime(t)
	set, err := runtime.projectExts.ForProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.mcp.Close() })
	closer := &lifecycleCloser{entered: make(chan struct{}), release: make(chan struct{})}
	set.closer = closer
	other := &projectExtensionSet{}
	runtime.projectExts.mu.Lock()
	runtime.projectExts.sets[43] = other
	runtime.projectExts.mu.Unlock()
	var release sync.Once
	defer release.Do(func() { close(closer.release) })
	done := make(chan error, 1)
	go func() { done <- runtime.Projects().Delete(ctx, 42) }()
	select {
	case <-closer.entered:
	case err := <-done:
		t.Fatalf("Delete did not wait for Close: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Close not entered")
	}
	if _, err := runtime.store.GetProject(ctx, 42); err != nil {
		t.Fatalf("project deleted before Close: %v", err)
	}
	resolved := make(chan *projectExtensionSet, 1)
	go func() { got, _ := runtime.projectExts.ForProject(ctx, 43); resolved <- got }()
	select {
	case got := <-resolved:
		if got != other {
			t.Fatal("other project's cache unavailable")
		}
	case <-time.After(time.Second):
		t.Fatal("resolver mutex held while Close waited")
	}
	if got, err := runtime.projectExts.ForProject(ctx, 42); err == nil || got != nil {
		t.Fatal("closing cache was reissued")
	}
	release.Do(func() { close(closer.release) })
	if err := <-done; err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
