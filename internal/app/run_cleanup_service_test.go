package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

type cleanupTestStore struct {
	projects         []int64
	protected        map[string]struct{}
	protectedContext map[int64]map[string]struct{}
	contextErr       map[int64]error
	projectsErr      error
	protectedErr     error
}

func (s *cleanupTestStore) ListWorkspaceProjectIDs(context.Context) ([]int64, error) {
	return s.projects, s.projectsErr
}

func (s *cleanupTestStore) ProtectedRunRefs(context.Context) (map[string]struct{}, error) {
	return s.protected, s.protectedErr
}

func (s *cleanupTestStore) ProtectedContextSHAs(_ context.Context, projectID int64) (map[string]struct{}, error) {
	if err := s.contextErr[projectID]; err != nil {
		return nil, err
	}
	return s.protectedContext[projectID], nil
}

type cleanupTestGit struct {
	mu      sync.Mutex
	fail    map[string]error
	keep    bool
	removed []string
}

func (g *cleanupTestGit) RemoveWorktree(_ context.Context, _, dir string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.fail[dir]; err != nil {
		return err
	}
	g.removed = append(g.removed, dir)
	if g.keep {
		return nil
	}
	return os.RemoveAll(dir)
}

func (g *cleanupTestGit) removedPaths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.removed...)
}

type cleanupFixture struct {
	manager    *workspace.Manager
	ws         *workspace.Workspace
	runsDir    string
	contextDir string
	store      *cleanupTestStore
	git        *cleanupTestGit
	now        time.Time
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	t.Helper()
	root := t.TempDir()
	manager := workspace.NewManager(root, nil)
	ws := manager.Get(7)
	runsDir := filepath.Join(ws.Root, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contextDir := filepath.Join(ws.Root, "context")
	if err := os.MkdirAll(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &cleanupFixture{
		manager:    manager,
		ws:         ws,
		runsDir:    runsDir,
		contextDir: contextDir,
		store: &cleanupTestStore{
			projects:         []int64{7},
			protected:        map[string]struct{}{},
			protectedContext: map[int64]map[string]struct{}{},
			contextErr:       map[int64]error{},
		},
		git: &cleanupTestGit{fail: map[string]error{}},
		now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
	}
}

func (f *cleanupFixture) makeContext(t *testing.T, sha string, mtime time.Time) string {
	t.Helper()
	if err := f.ws.WriteContext(sha, map[string][]byte{"AGENTS.md": []byte("context")}, workspace.ContextMeta{
		SourceSHA: sha,
	}); err != nil {
		t.Fatalf("WriteContext(%s): %v", sha, err)
	}
	dir := filepath.Join(f.contextDir, strings.ToLower(sha))
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *cleanupFixture) makeRun(t *testing.T, id string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(f.runsDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *cleanupFixture) service(t *testing.T) *RunCleanupService {
	t.Helper()
	service, err := NewRunCleanupService(RunCleanupConfig{
		Store:     f.store,
		Workspace: f.manager,
		Git:       f.git,
		After:     time.Hour,
		Now:       func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatalf("NewRunCleanupService: %v", err)
	}
	return service
}

func (f *cleanupFixture) old() time.Time { return f.now.Add(-2 * time.Hour) }

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s does not exist: %v", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists: %v", path, err)
	}
}

func TestRunCleanupDeletesOnlyExpiredUnprotectedRuns(t *testing.T) {
	f := newCleanupFixture(t)
	orphan := f.makeRun(t, "orphan-old", f.old())
	terminal := f.makeRun(t, "terminal-old", f.old())
	fresh := f.makeRun(t, "fresh", f.now.Add(-time.Minute))
	runningJob := f.makeRun(t, "job-running", f.old())
	runningWorkflow := f.makeRun(t, "wf-running", f.old())
	pendingPublish := f.makeRun(t, "pub-pending", f.old())
	invalid := f.makeRun(t, "bad name", f.old())

	file := filepath.Join(f.runsDir, "file-old")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.runsDir, "linked-old")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	unregisteredProject := filepath.Join(filepath.Dir(f.ws.Root), "8", "runs", "orphan-old")
	if err := os.MkdirAll(unregisteredProject, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unregisteredProject, f.old(), f.old()); err != nil {
		t.Fatal(err)
	}

	f.store.protected = map[string]struct{}{
		"job-running": {},
		"wf-running":  {},
		"pub-pending": {},
	}
	service := f.service(t)
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	assertMissing(t, orphan)
	assertMissing(t, terminal)
	assertExists(t, fresh)
	assertExists(t, runningJob)
	assertExists(t, runningWorkflow)
	assertExists(t, pendingPublish)
	assertExists(t, invalid)
	assertExists(t, file)
	assertExists(t, filepath.Join(outside, "keep"))
	assertExists(t, unregisteredProject)

	removed := f.git.removedPaths()
	if len(removed) != 2 || removed[0] != orphan || removed[1] != terminal {
		t.Fatalf("removed = %v, want [%s %s]", removed, orphan, terminal)
	}

	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if got := f.git.removedPaths(); len(got) != 2 {
		t.Fatalf("second RunOnce removed more directories: %v", got)
	}
}

func TestRunCleanupRemovesOnlyAgedUnprotectedContextDirs(t *testing.T) {
	f := newCleanupFixture(t)
	orphan := f.makeContext(t, strings.Repeat("a", 40), f.old())
	protected := f.makeContext(t, strings.Repeat("b", 40), f.old())
	locked := f.makeContext(t, strings.Repeat("c", 40), f.old())
	if err := f.ws.SetContextLocked(strings.Repeat("c", 40), true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(locked, f.old(), f.old()); err != nil {
		t.Fatal(err)
	}
	malformed := f.makeContext(t, strings.Repeat("d", 40), f.old())
	if err := os.WriteFile(filepath.Join(malformed, "meta.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(malformed, f.old(), f.old()); err != nil {
		t.Fatal(err)
	}
	missingMeta := f.makeContext(t, strings.Repeat("e", 40), f.old())
	if err := os.Remove(filepath.Join(missingMeta, "meta.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(missingMeta, f.old(), f.old()); err != nil {
		t.Fatal(err)
	}
	fresh := f.makeContext(t, strings.Repeat("f", 40), f.now.Add(-time.Minute))

	invalid := filepath.Join(f.contextDir, "not-a-sha")
	if err := os.MkdirAll(invalid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(invalid, f.old(), f.old()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.contextDir, "note.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.contextDir, strings.Repeat("9", 40))
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	f.store.protectedContext[7] = map[string]struct{}{strings.Repeat("b", 40): {}}
	err := f.service(t).RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce accepted malformed context metadata")
	}
	assertMissing(t, orphan)
	for _, path := range []string{protected, locked, malformed, missingMeta, fresh, invalid, file, link} {
		assertExists(t, path)
	}
	assertExists(t, filepath.Join(outside, "keep"))

	if err := f.service(t).RunOnce(context.Background()); err == nil {
		t.Fatal("second RunOnce accepted malformed context metadata")
	}
	assertMissing(t, orphan)
	assertExists(t, malformed)
}

func TestRunCleanupKeepsContextOnProtectionQueryFailure(t *testing.T) {
	f := newCleanupFixture(t)
	run := f.makeRun(t, "orphan-old", f.old())
	contextDir := f.makeContext(t, strings.Repeat("a", 40), f.old())
	f.store.contextErr[7] = errors.New("db down")

	err := f.service(t).RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("RunOnce = %v, want protection query error", err)
	}
	assertExists(t, run)
	assertExists(t, contextDir)
	if got := f.git.removedPaths(); len(got) != 0 {
		t.Fatalf("removal attempted after query failure: %v", got)
	}
}

func TestRunCleanupIsolatesProjectErrors(t *testing.T) {
	f := newCleanupFixture(t)
	f.store.projects = []int64{7, 8}
	f.store.contextErr[7] = errors.New("project 7 down")

	failedRun := f.makeRun(t, "failed-project-run", f.old())
	failedContext := f.makeContext(t, strings.Repeat("a", 40), f.old())

	ws := f.manager.Get(8)
	runsDir := filepath.Join(ws.Root, "runs")
	contextDir := filepath.Join(ws.Root, "context", strings.Repeat("b", 40))
	for _, dir := range []string{runsDir, contextDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, f.old(), f.old()); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.WriteContext(strings.Repeat("b", 40), map[string][]byte{"AGENTS.md": []byte("context")}, workspace.ContextMeta{
		SourceSHA: strings.Repeat("b", 40),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(contextDir, f.old(), f.old()); err != nil {
		t.Fatal(err)
	}
	otherRun := filepath.Join(runsDir, "other-project-run")
	if err := os.MkdirAll(otherRun, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(otherRun, f.old(), f.old()); err != nil {
		t.Fatal(err)
	}

	err := f.service(t).RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "project 7 down") {
		t.Fatalf("RunOnce = %v, want project 7 error", err)
	}
	assertExists(t, failedRun)
	assertExists(t, failedContext)
	assertMissing(t, otherRun)
	assertMissing(t, contextDir)
}

func TestRunCleanupProtectsRegisteredRun(t *testing.T) {
	root := t.TempDir()
	manager := workspace.NewManager(root, &repo.Git{})
	ws := manager.Get(7)
	sha := initCleanupMirror(t, ws.MirrorDir())
	runDir, cleanup, err := ws.NewRun(context.Background(), "job-registered", sha)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	defer cleanup()
	old := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(runDir, old, old); err != nil {
		t.Fatal(err)
	}

	git := &cleanupTestGit{fail: map[string]error{}}
	service, err := NewRunCleanupService(RunCleanupConfig{
		Store:     &cleanupTestStore{projects: []int64{7}, protected: map[string]struct{}{}},
		Workspace: manager,
		Git:       git,
		After:     time.Hour,
		Now:       func() time.Time { return old.Add(2 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	assertExists(t, runDir)
	if got := git.removedPaths(); len(got) != 0 {
		t.Fatalf("registered run was removed: %v", got)
	}
}

func TestRunCleanupRemovesGitWorktreeRegistration(t *testing.T) {
	root := t.TempDir()
	creator := workspace.NewManager(root, &repo.Git{})
	ws := creator.Get(7)
	sha := initCleanupMirror(t, ws.MirrorDir())
	runDir, _, err := ws.NewRun(context.Background(), "job-old", sha)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	old := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(runDir, old, old); err != nil {
		t.Fatal(err)
	}

	fresh := workspace.NewManager(root, &repo.Git{})
	service, err := NewRunCleanupService(RunCleanupConfig{
		Store:     &cleanupTestStore{projects: []int64{7}, protected: map[string]struct{}{}},
		Workspace: fresh,
		Git:       &repo.Git{},
		After:     time.Hour,
		Now:       func() time.Time { return old.Add(2 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	assertMissing(t, runDir)
	if _, err := os.Lstat(filepath.Join(ws.MirrorDir(), "worktrees", "job-old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git worktree registration remains: %v", err)
	}
}

func TestRunCleanupRemovesEligibleRunWhenMirrorIsMissing(t *testing.T) {
	root := t.TempDir()
	creator := workspace.NewManager(root, &repo.Git{})
	ws := creator.Get(7)
	sha := initCleanupMirror(t, ws.MirrorDir())
	runDir, _, err := ws.NewRun(context.Background(), "job-old", sha)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	old := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(runDir, old, old); err != nil {
		t.Fatal(err)
	}
	// No repository remains, so there is no worktree registration to remove.
	if err := os.RemoveAll(ws.MirrorDir()); err != nil {
		t.Fatal(err)
	}

	service, err := NewRunCleanupService(RunCleanupConfig{
		Store:     &cleanupTestStore{projects: []int64{7}, protected: map[string]struct{}{}},
		Workspace: workspace.NewManager(root, &repo.Git{}),
		Git:       &repo.Git{},
		After:     time.Hour,
		Now:       func() time.Time { return old.Add(2 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	assertMissing(t, runDir)
}

func TestRunCleanupFailsClosedWhenQueriesFail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *cleanupTestStore
	}{
		{name: "projects", store: &cleanupTestStore{projectsErr: errors.New("db down")}},
		{name: "protected refs", store: &cleanupTestStore{
			projects:     []int64{7},
			protectedErr: errors.New("db down"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCleanupFixture(t)
			dir := f.makeRun(t, "orphan-old", f.old())
			service, err := NewRunCleanupService(RunCleanupConfig{
				Store:     tc.store,
				Workspace: f.manager,
				Git:       f.git,
				After:     time.Hour,
				Now:       func() time.Time { return f.now },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.RunOnce(context.Background()); err == nil {
				t.Fatal("RunOnce returned nil error")
			}
			assertExists(t, dir)
			if got := f.git.removedPaths(); len(got) != 0 {
				t.Fatalf("removal attempted after query failure: %v", got)
			}
		})
	}
}

func TestRunCleanupKeepsDirectoryWhenGitRemovalFails(t *testing.T) {
	f := newCleanupFixture(t)
	failed := f.makeRun(t, "first-fail", f.old())
	ok := f.makeRun(t, "second-ok", f.old())
	f.git.fail[failed] = errors.New("worktree remove failed")

	service := f.service(t)
	err := service.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "worktree remove failed") {
		t.Fatalf("RunOnce = %v, want git removal error", err)
	}
	assertExists(t, failed)
	assertMissing(t, ok)
	if got := f.git.removedPaths(); len(got) != 1 || got[0] != ok {
		t.Fatalf("removed = %v, want [%s]", got, ok)
	}
}

func TestRunCleanupRemovesDirectoryLeftByGit(t *testing.T) {
	f := newCleanupFixture(t)
	dir := f.makeRun(t, "left-behind", f.old())
	f.git.keep = true
	if err := f.service(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	assertMissing(t, dir)
}

func TestRunCleanupHonorsCancelledContext(t *testing.T) {
	f := newCleanupFixture(t)
	dir := f.makeRun(t, "orphan-old", f.old())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.service(t).RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want context.Canceled", err)
	}
	assertExists(t, dir)
	if got := f.git.removedPaths(); len(got) != 0 {
		t.Fatalf("removal attempted with a cancelled context: %v", got)
	}
}

func TestRunCleanupWaitsForWorkspaceLock(t *testing.T) {
	f := newCleanupFixture(t)
	dir := f.makeRun(t, "orphan-old", f.old())
	contextDir := f.makeContext(t, strings.Repeat("a", 40), f.old())
	service := f.service(t)

	unlock, err := f.ws.RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := service.RunOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		unlock()
		t.Fatalf("RunOnce while read-locked = %v, want deadline", err)
	}
	assertExists(t, dir)
	assertExists(t, contextDir)
	unlock()

	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce after unlock: %v", err)
	}
	assertMissing(t, dir)
	assertMissing(t, contextDir)
}

func TestRunCleanupRejectsSymlinkedProjectRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	runsDir := filepath.Join(outside, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(runsDir, "orphan-old")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "7")); err != nil {
		t.Fatal(err)
	}
	manager := workspace.NewManager(root, nil)
	git := &cleanupTestGit{fail: map[string]error{}}
	service, err := NewRunCleanupService(RunCleanupConfig{
		Store:     &cleanupTestStore{projects: []int64{7}, protected: map[string]struct{}{}},
		Workspace: manager,
		Git:       git,
		After:     time.Hour,
		Now:       func() time.Time { return old.Add(2 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce accepted a symlinked project root")
	}
	assertExists(t, dir)
	if got := git.removedPaths(); len(got) != 0 {
		t.Fatalf("removal escaped the project root: %v", got)
	}
}

func TestRunCleanupStartStopsOnCancel(t *testing.T) {
	f := newCleanupFixture(t)
	service := f.service(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.Start(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
}

func TestNewRunCleanupServiceValidation(t *testing.T) {
	manager := workspace.NewManager(t.TempDir(), nil)
	git := &cleanupTestGit{fail: map[string]error{}}
	store := &cleanupTestStore{}
	valid := RunCleanupConfig{Store: store, Workspace: manager, Git: git, After: time.Hour}
	if _, err := NewRunCleanupService(valid); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, mutate := range map[string]func(*RunCleanupConfig){
		"store":     func(c *RunCleanupConfig) { c.Store = nil },
		"workspace": func(c *RunCleanupConfig) { c.Workspace = nil },
		"git":       func(c *RunCleanupConfig) { c.Git = nil },
		"after":     func(c *RunCleanupConfig) { c.After = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if _, err := NewRunCleanupService(cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func initCleanupMirror(t *testing.T, mirror string) string {
	t.Helper()
	work := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		runCleanupGit(t, work, args...)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, work, "add", ".")
	runCleanupGit(t, work, "commit", "-q", "-m", "first")
	sha := strings.TrimSpace(runCleanupGit(t, work, "rev-parse", "HEAD"))
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, "", "init", "--bare", "-q", mirror)
	runCleanupGit(t, work, "push", "-q", mirror, "HEAD:refs/heads/main")
	return sha
}

func runCleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
