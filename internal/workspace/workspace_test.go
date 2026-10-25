package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
)

type fixture struct {
	root       string
	source     string
	serverRoot string
	bare       string
	server     *httptest.Server
	git        *repo.Git
	mu         sync.Mutex
	authHeader string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		root:       filepath.Join(t.TempDir(), "workspaces"),
		source:     initRepo(t),
		serverRoot: t.TempDir(),
		git:        &repo.Git{},
	}
	f.bare = filepath.Join(f.serverRoot, "repo.git")
	gitCmd(t, "", "clone", "--bare", f.source, f.bare)
	gitCmd(t, f.bare, "update-server-info")

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authHeader = r.Header.Get("Authorization")
		f.mu.Unlock()
		http.FileServer(http.Dir(f.serverRoot)).ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) remoteURL() string { return f.server.URL + "/repo.git" }

func (f *fixture) publish(t *testing.T) {
	t.Helper()
	gitCmd(t, f.source, "push", "-q", f.bare, "refs/heads/main:refs/heads/main")
	gitCmd(t, f.bare, "update-server-info")
}

func (f *fixture) commit(t *testing.T, name, body, message string) string {
	t.Helper()
	return commitFile(t, f.source, name, body, message)
}

func (f *fixture) authorization() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authHeader
}

func TestRefreshBuildsAndUpdatesBase(t *testing.T) {
	f := newFixture(t)
	first := f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m := NewManager(f.root, f.git)
	ws := m.Get(42)
	if ws == nil {
		t.Fatal("Get(42) = nil")
	}
	unlock, err := ws.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	sha, err := ws.Refresh(ctx, f.remoteURL(), "main", repo.Auth{Token: "token-42"})
	if err != nil {
		unlock()
		t.Fatalf("Refresh() error = %v", err)
	}
	unlock()
	if sha != first {
		t.Fatalf("Refresh() = %s, want %s", sha, first)
	}
	if got := ws.BaseSHA(); got != first {
		t.Fatalf("BaseSHA() = %q, want %q", got, first)
	}
	if data, err := os.ReadFile(filepath.Join(ws.BaseDir(), "a.txt")); err != nil || string(data) != "one\n" {
		t.Fatalf("base content = %q, %v", data, err)
	}
	if got := f.authorization(); !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("fetch Authorization = %q", got)
	}
	if info, err := os.Stat(ws.MirrorDir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("mirror permissions = %v, %v", info, err)
	}

	second := f.commit(t, "b.txt", "two\n", "second")
	f.publish(t)
	unlock, err = ws.Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sha, err = ws.Refresh(ctx, f.remoteURL(), "refs/heads/main", repo.Auth{})
	unlock()
	if err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
	if sha != second || ws.BaseSHA() != second {
		t.Fatalf("second Refresh() = %s, BaseSHA = %s, want %s", sha, ws.BaseSHA(), second)
	}
	if data, err := os.ReadFile(filepath.Join(ws.BaseDir(), "b.txt")); err != nil || string(data) != "two\n" {
		t.Fatalf("updated base content = %q, %v", data, err)
	}
}

func TestRefreshRejectsForeignBaseAndPreservesContent(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ctx := context.Background()
	ws := NewManager(f.root, f.git).Get(7)
	if err := os.MkdirAll(ws.BaseDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(ws.BaseDir(), "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Refresh(ctx, f.remoteURL(), "main", repo.Auth{}); !errors.Is(err, ErrBaseConflict) {
		t.Fatalf("Refresh() = %v, want ErrBaseConflict", err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("foreign base content changed: %q, %v", data, err)
	}
}

func TestRefreshReplacesEmptyBaseDirectory(t *testing.T) {
	f := newFixture(t)
	sha := f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ctx := context.Background()
	ws := NewManager(f.root, f.git).Get(8)
	if err := os.MkdirAll(ws.BaseDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ws.Refresh(ctx, f.remoteURL(), "main", repo.Auth{})
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got != sha || ws.BaseSHA() != sha {
		t.Fatalf("Refresh() = %q, BaseSHA = %q, want %q", got, ws.BaseSHA(), sha)
	}
}

func TestNewRunAndCleanup(t *testing.T) {
	f := newFixture(t)
	sha := f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ctx := context.Background()
	ws := NewManager(f.root, f.git).Get(9)
	if _, err := ws.Refresh(ctx, f.remoteURL(), "main", repo.Auth{}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ws.NewRun(ctx, "../evil", sha); !errors.Is(err, ErrInvalidRunID) {
		t.Fatalf("NewRun(../evil) = %v, want ErrInvalidRunID", err)
	}
	if _, _, err := ws.NewRun(ctx, "workflow-1", "not-a-sha"); !errors.Is(err, ErrInvalidSHA) {
		t.Fatalf("NewRun(bad sha) = %v, want ErrInvalidSHA", err)
	}

	dir, cleanup, err := ws.NewRun(ctx, "workflow-1", sha)
	if err != nil {
		t.Fatalf("NewRun() error = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(data) != "one\n" {
		t.Fatalf("run content = %q, %v", data, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("run permissions = %v, %v", info, err)
	}
	if _, _, err := ws.NewRun(ctx, "workflow-1", sha); !errors.Is(err, ErrRunExists) {
		t.Fatalf("second NewRun() = %v, want ErrRunExists", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory still exists")
	}
	if err := cleanup(); !errors.Is(err, ErrRunNotRegistered) {
		t.Fatalf("second cleanup() = %v, want ErrRunNotRegistered", err)
	}
}

func TestNewRunRejectsSymlinkedProjectRoot(t *testing.T) {
	f := newFixture(t)
	sha := f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ctx := context.Background()
	ws := NewManager(f.root, f.git).Get(10)
	if _, err := ws.Refresh(ctx, f.remoteURL(), "main", repo.Auth{}); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(ws.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ws.Root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.NewRun(ctx, "workflow-2", sha); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("NewRun() = %v, want ErrUnsafePath", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("symlink target was modified")
	}
}

func TestLockHonorsContext(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(1)
	unlock, err := ws.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ws.RLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RLock() = %v, want deadline", err)
	}
	unlock()

	rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
	defer rcancel()
	runlock, err := ws.RLock(rctx)
	if err != nil {
		t.Fatalf("RLock() after unlock = %v", err)
	}
	if _, err := ws.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock() with reader = %v, want deadline", err)
	}
	runlock()
}

func TestLockWriterPreference(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(2)
	readUnlock, err := ws.RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	writerAcquired := make(chan func(), 1)
	go func() {
		unlock, err := ws.Lock(context.Background())
		if err != nil {
			close(writerAcquired)
			return
		}
		writerAcquired <- unlock
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		ws.lock.mu.Lock()
		waiting := ws.lock.writers
		ws.lock.mu.Unlock()
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer did not start waiting")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ws.RLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RLock() while writer waits = %v, want deadline", err)
	}
	readUnlock()

	select {
	case unlock, ok := <-writerAcquired:
		if !ok {
			t.Fatal("writer failed to acquire lock")
		}
		unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not acquire the lock")
	}
}

func TestLockCanceledWriterDoesNotBlockReaders(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(3)
	readUnlock, err := ws.RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := ws.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock() = %v, want deadline", err)
	}
	ws.lock.mu.Lock()
	waiting := ws.lock.writers
	ws.lock.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("writers = %d after cancellation", waiting)
	}
	second, err := ws.RLock(context.Background())
	if err != nil {
		t.Fatalf("second RLock() error = %v", err)
	}
	second()
	readUnlock()
}

func TestBaseSHAEmptyWithoutCheckout(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(4)
	if got := ws.BaseSHA(); got != "" {
		t.Fatalf("BaseSHA() = %q, want empty", got)
	}
}

func TestContextLockedDocumentIsNotOverwritten(t *testing.T) {
	sha := strings.Repeat("a", 40)
	ws := NewManager(t.TempDir(), nil).Get(5)
	meta := ContextMeta{SourceSHA: sha, Model: "test", GeneratedAt: time.Now().UTC()}
	if err := ws.WriteContext(sha, map[string][]byte{"AGENTS.md": []byte("original")}, meta); err != nil {
		t.Fatalf("WriteContext() error = %v", err)
	}
	if err := ws.SetContextLocked(sha, true); err != nil {
		t.Fatalf("SetContextLocked() error = %v", err)
	}
	if err := ws.WriteContext(sha, map[string][]byte{"AGENTS.md": []byte("replacement")}, meta); !errors.Is(err, ErrContextLocked) {
		t.Fatalf("WriteContext(locked) = %v, want ErrContextLocked", err)
	}
	data, err := ws.ReadContext(sha, "AGENTS.md")
	if err != nil || string(data) != "original" {
		t.Fatalf("document = %q, %v", data, err)
	}
	if err := ws.SetContextStale(sha, true); err != nil {
		t.Fatalf("SetContextStale() error = %v", err)
	}
	got, err := ws.ContextMeta(sha)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Locked || !got.Stale {
		t.Fatalf("metadata = %+v, want locked and stale", got)
	}
	if err := ws.SetContextLocked(sha, false); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteContext(sha, map[string][]byte{"AGENTS.md": []byte("replacement")}, meta); err != nil {
		t.Fatalf("WriteContext(unlocked) error = %v", err)
	}
}

func TestWriteActiveContextReplacesLockedProjection(t *testing.T) {
	shaA := strings.Repeat("c", 40)
	shaB := strings.Repeat("d", 40)
	ws := NewManager(t.TempDir(), nil).Get(9)
	meta := ContextMeta{SourceSHA: shaA, Model: "test", GeneratedAt: time.Now().UTC()}
	if err := ws.WriteContext(shaA, map[string][]byte{"AGENTS.md": []byte("original")}, meta); err != nil {
		t.Fatalf("WriteContext() error = %v", err)
	}
	if err := ws.SetContextLocked(shaA, true); err != nil {
		t.Fatalf("SetContextLocked() error = %v", err)
	}
	activeMeta := ContextMeta{SourceSHA: shaB, Model: "test", GeneratedAt: time.Now().UTC(), Locked: true}
	if err := ws.WriteActiveContext(shaB, map[string][]byte{"AGENTS.md": []byte("database active")}, activeMeta); err != nil {
		t.Fatalf("WriteActiveContext() error = %v", err)
	}
	data, err := ws.ReadContext(shaB, "AGENTS.md")
	if err != nil || string(data) != "database active" {
		t.Fatalf("active document = %q, %v", data, err)
	}
	got, err := ws.ContextMeta(shaB)
	if err != nil || !got.Locked {
		t.Fatalf("active metadata = %+v, %v", got, err)
	}
	if err := ws.WriteContext(shaB, map[string][]byte{"AGENTS.md": []byte("file writer")}, activeMeta); !errors.Is(err, ErrContextLocked) {
		t.Fatalf("WriteContext(locked) = %v, want ErrContextLocked", err)
	}
	if data, err := ws.ReadContext(shaB, "AGENTS.md"); err != nil || string(data) != "database active" {
		t.Fatalf("locked projection changed: %q, %v", data, err)
	}
}

func TestContextRejectsEscapeAndSymlinks(t *testing.T) {
	sha := strings.Repeat("b", 40)
	ws := NewManager(t.TempDir(), nil).Get(6)
	if _, err := ws.ReadContext("../escape", "AGENTS.md"); !errors.Is(err, ErrInvalidSHA) {
		t.Fatalf("ReadContext(escape) = %v, want ErrInvalidSHA", err)
	}
	if err := ws.WriteContext(sha, map[string][]byte{"../meta.json": []byte("x")}, ContextMeta{SourceSHA: sha}); !errors.Is(err, ErrInvalidContextName) {
		t.Fatalf("WriteContext(name escape) = %v, want ErrInvalidContextName", err)
	}
	if err := ws.WriteContext(sha, nil, ContextMeta{SourceSHA: sha}); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ContextMeta(sha); err != nil {
		t.Fatal(err)
	}
	contextDir := filepath.Join(ws.Root, "context")
	outside := t.TempDir()
	link := filepath.Join(contextDir, strings.Repeat("c", 40))
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ReadContext(strings.Repeat("c", 40), "AGENTS.md"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ReadContext(symlink) = %v, want ErrUnsafePath", err)
	}
}

func TestGetRejectsNonPositiveIDs(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	if m.Get(0) != nil || m.Get(-1) != nil {
		t.Fatal("Get accepted a non-positive project ID")
	}
	if m.Get(1) != m.Get(1) {
		t.Fatal("Get returned different workspaces for one project")
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q", "-b", "main")
	gitCmd(t, dir, "config", "user.email", "test@example.com")
	gitCmd(t, dir, "config", "user.name", "Test")
	return dir
}

func commitFile(t *testing.T, dir, name, body, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", name)
	gitCmd(t, dir, "commit", "-q", "-m", message)
	return strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = fixtureEnv()
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func fixtureEnv() []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
	)
}

func TestRefreshRejectsSymlinkedMirror(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ws := NewManager(f.root, f.git).Get(11)
	if err := os.MkdirAll(ws.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, ws.MirrorDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Refresh(context.Background(), f.remoteURL(), "main", repo.Auth{}); !errors.Is(err, repo.ErrUnsafePath) {
		t.Fatalf("Refresh(symlinked mirror) = %v, want repo.ErrUnsafePath", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("symlink target changed: %v, %v", entries, err)
	}
}

func TestBaseSHARejectsSymlinkedBase(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(17)
	if err := os.MkdirAll(ws.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, ws.BaseDir()); err != nil {
		t.Fatal(err)
	}
	if got := ws.BaseSHA(); got != "" {
		t.Fatalf("BaseSHA(symlinked base) = %q, want empty", got)
	}
}

func TestRefreshRejectsForeignGitdir(t *testing.T) {
	f := newFixture(t)
	sha := f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ctx := context.Background()
	ws := NewManager(f.root, f.git).Get(18)
	if _, err := ws.Refresh(ctx, f.remoteURL(), "main", repo.Auth{}); err != nil {
		t.Fatal(err)
	}
	runDir, cleanup, err := ws.NewRun(ctx, "workflow-foreign", sha)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	runGitFile, err := os.ReadFile(filepath.Join(runDir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.BaseDir(), ".git"), runGitFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Refresh(ctx, f.remoteURL(), "main", repo.Auth{}); !errors.Is(err, ErrBaseConflict) {
		t.Fatalf("Refresh(foreign gitdir) = %v, want ErrBaseConflict", err)
	}
	if got := ws.BaseSHA(); got != "" {
		t.Fatalf("BaseSHA(foreign gitdir) = %q, want empty", got)
	}
}

func TestBaseSHAEmptyWhenNotDetached(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ws := NewManager(f.root, f.git).Get(12)
	if _, err := ws.Refresh(context.Background(), f.remoteURL(), "main", repo.Auth{}); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ws.BaseDir(), "checkout", "-q", "-b", "local")
	if got := ws.BaseSHA(); got != "" {
		t.Fatalf("BaseSHA() = %q, want empty for a branch checkout", got)
	}
}

func TestConcurrentReaders(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(13)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := ws.RLock(context.Background())
			if err != nil {
				t.Errorf("RLock() error = %v", err)
				return
			}
			started <- struct{}{}
			<-release
			unlock()
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent readers did not both acquire the lock")
		}
	}
	close(release)
	wg.Wait()
}

func TestCleanupUsesDetachedContext(t *testing.T) {
	f := newFixture(t)
	sha := f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ws := NewManager(f.root, f.git).Get(14)
	if _, err := ws.Refresh(context.Background(), f.remoteURL(), "main", repo.Auth{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	dir, cleanup, err := ws.NewRun(ctx, "workflow-3", sha)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() after cancel error = %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory still exists")
	}
}

func TestWriteContextRejectsMismatchedSource(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(15)
	sha := strings.Repeat("d", 40)
	other := strings.Repeat("e", 40)
	if err := ws.WriteContext(sha, map[string][]byte{"AGENTS.md": []byte("x")}, ContextMeta{SourceSHA: other}); !errors.Is(err, ErrInvalidSHA) {
		t.Fatalf("WriteContext(mismatched source) = %v, want ErrInvalidSHA", err)
	}
	if _, err := ws.ReadContext(sha, "AGENTS.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("document was partially written: %v", err)
	}
}

func TestContextLeavesNoTemporaryFiles(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(16)
	sha := strings.Repeat("f", 40)
	if err := ws.WriteContext(sha, map[string][]byte{"AGENTS.md": []byte("x"), "CLAUDE.md": []byte("y")}, ContextMeta{SourceSHA: sha}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(ws.Root, "context", sha))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}
