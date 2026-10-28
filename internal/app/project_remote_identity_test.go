package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type identityGit struct {
	*fakeGitOps
	remotes []string
	fetches int
}

type missingHeadIdentityGit struct{ *identityGit }

func (g *missingHeadIdentityGit) Resolve(ctx context.Context, dir, ref string) (string, error) {
	if ref == sha40 {
		return "", errors.New("missing object")
	}
	return g.fakeGitOps.Resolve(ctx, dir, ref)
}

func TestReviewAndFeedbackRequireCurrentProjectIdentity(t *testing.T) {
	for _, path := range []string{"review", "feedback"} {
		for _, tc := range []struct {
			name                                  string
			current                               string
			status, id                            int
			badURL, badOrigin, timeout, wantFetch bool
		}{
			{name: "rename", status: 200, id: 42, wantFetch: true},
			{name: "unchanged", current: "old/repo", status: 200, id: 42, wantFetch: true},
			{name: "lookup failure", status: 500},
			{name: "not found", status: 404},
			{name: "wrong ID", status: 200, id: 99},
			{name: "unsafe URL", status: 200, id: 42, badURL: true},
			{name: "unsafe stored origin", status: 200, id: 42, badOrigin: true},
			{name: "timeout", timeout: true},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				current := tc.current
				if current == "" {
					current = "new/repo"
				}
				var server *httptest.Server
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v4/projects/42" {
						t.Errorf("unexpected lookup %s", r.URL.Path)
					}
					if tc.timeout {
						<-r.Context().Done()
						return
					}
					w.WriteHeader(tc.status)
					if tc.status == 200 {
						clone := server.URL + "/" + current + ".git"
						if tc.badURL {
							clone = "https://other.invalid/new/repo.git"
						}
						fmt.Fprintf(w, `{"id":%d,"path_with_namespace":%q,"web_url":%q,"http_url_to_repo":%q}`, tc.id, current, server.URL+"/"+current, clone)
					}
				}))
				defer server.Close()
				client, err := gitlab.New(gitlab.Config{BaseURL: server.URL})
				if err != nil {
					t.Fatal(err)
				}
				manager := workspace.NewManager(t.TempDir(), nil)
				consumerReadLock(t, manager, 42)
				git := &missingHeadIdentityGit{&identityGit{fakeGitOps: newFakeGitOps(sha40)}}
				project := &store.Project{ID: 42, PathWithNamespace: "old/repo", WebURL: server.URL + "/old/repo"}
				if tc.badOrigin {
					project.WebURL += "?"
				}
				ctx := context.Background()
				if tc.timeout {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
					defer cancel()
				}
				var ok bool
				if path == "review" {
					s := &ReviewService{gitlab: client, git: git, workspace: manager, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
					_, cleanup, err := s.prepareRun(ctx, project, "fake-token", 7, "test-run", sha40)
					ok = err == nil
					if cleanup != nil {
						cleanup()
					}
				} else {
					s := &FeedbackService{gitlab: client, git: git}
					ok = s.ensureHead(ctx, "fake-token", project, manager.Get(42), 7, sha40)
				}
				if ok != tc.wantFetch {
					t.Fatalf("success=%v want=%v", ok, tc.wantFetch)
				}
				if tc.wantFetch {
					if git.fetches != 1 || len(git.remotes) != 1 || git.remotes[0] != server.URL+"/"+current+".git" {
						t.Fatalf("remotes=%v fetches=%d", git.remotes, git.fetches)
					}
				} else if git.fetches != 0 || len(git.remotes) != 0 {
					t.Fatalf("unexpected Git operations: remotes=%v fetches=%d", git.remotes, git.fetches)
				}
			})
		}
	}
}

type interleavingGit struct {
	*fakeGitOps
	mu            sync.Mutex
	remote        string
	events        []string
	firstEntered  chan struct{}
	secondEntered chan struct{}
	continueFirst chan struct{}
}

type missingConcurrentGit struct{ *interleavingGit }

func (g *missingConcurrentGit) Resolve(ctx context.Context, dir, ref string) (string, error) {
	if ref == sha40 {
		return "", errors.New("missing object")
	}
	return g.fakeGitOps.Resolve(ctx, dir, ref)
}

func (g *interleavingGit) EnsureMirror(_ context.Context, _, remote string, _ repo.Auth) error {
	g.mu.Lock()
	g.remote = remote
	g.events = append(g.events, "ensure "+remote)
	first := len(g.events) == 1
	g.mu.Unlock()
	if first {
		close(g.firstEntered)
		<-g.continueFirst
	} else {
		close(g.secondEntered)
	}
	return nil
}

func (g *interleavingGit) Fetch(context.Context, string, repo.Auth, ...string) error {
	g.mu.Lock()
	g.events = append(g.events, "fetch "+g.remote)
	g.mu.Unlock()
	return nil
}

func TestConcurrentReviewAndFeedbackDoNotInterleaveMirrorOrigin(t *testing.T) {
	var calls atomic.Int32
	secondLookup := make(chan struct{})
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/42" {
			t.Errorf("unexpected lookup %s", r.URL.Path)
		}
		name := "first"
		if calls.Add(1) > 1 {
			name = "second"
			close(secondLookup)
		}
		fmt.Fprintf(w, `{"id":42,"path_with_namespace":%q,"web_url":%q,"http_url_to_repo":%q}`, name+"/repo", server.URL+"/"+name+"/repo", server.URL+"/"+name+"/repo.git")
	}))
	defer server.Close()
	client, err := gitlab.New(gitlab.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	manager := workspace.NewManager(t.TempDir(), nil)
	git := &interleavingGit{fakeGitOps: newFakeGitOps(sha40), firstEntered: make(chan struct{}), secondEntered: make(chan struct{}), continueFirst: make(chan struct{})}
	project := &store.Project{ID: 42, PathWithNamespace: "old/repo", WebURL: server.URL + "/old/repo"}
	service := &ReviewService{gitlab: client, git: git, workspace: manager, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() {
		unlock, err := manager.Get(42).RLock(ctx)
		if err != nil {
			done <- err
			return
		}
		defer unlock()
		_, cleanup, err := service.prepareRun(ctx, project, "fake-token", 7, "run-1", sha40)
		if cleanup != nil {
			cleanup()
		}
		done <- err
	}()
	<-git.firstEntered
	go func() {
		unlock, err := manager.Get(42).RLock(ctx)
		if err != nil {
			done <- err
			return
		}
		defer unlock()
		feedback := &FeedbackService{gitlab: client, git: &missingConcurrentGit{git}}
		if !feedback.ensureHead(ctx, "fake-token", project, manager.Get(42), 7, sha40) {
			done <- errors.New("feedback fetch failed")
			return
		}
		done <- nil
	}()
	select {
	case <-secondLookup:
	case <-time.After(5 * time.Second):
		close(git.continueFirst)
		<-done
		<-done
		t.Fatal("second identity lookup did not start")
	}
	select {
	case <-git.secondEntered:
		close(git.continueFirst)
		<-done
		<-done
		t.Fatal("a second mirror update interleaved before the first fetch")
	case <-time.After(200 * time.Millisecond):
	}
	close(git.continueFirst)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	want := []string{"ensure " + server.URL + "/first/repo.git", "fetch " + server.URL + "/first/repo.git", "ensure " + server.URL + "/second/repo.git", "fetch " + server.URL + "/second/repo.git"}
	if fmt.Sprint(git.events) != fmt.Sprint(want) {
		t.Fatalf("events=%v want=%v", git.events, want)
	}
}

func TestRefreshBaseRenameFetchesCurrentRepository(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"old", "new"} {
		source := t.TempDir()
		baseSecurityGit(t, source, "init", "-q", "-b", "main")
		baseSecurityGit(t, source, "config", "user.name", "Test")
		baseSecurityGit(t, source, "config", "user.email", "test@example.invalid")
		baseSecurityWrite(t, filepath.Join(source, "identity.txt"), name)
		baseSecurityGit(t, source, "add", "identity.txt")
		baseSecurityGit(t, source, "commit", "-qm", name)
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		bare := filepath.Join(root, name, "repo.git")
		baseSecurityGit(t, source, "clone", "--bare", "-q", source, bare)
		baseSecurityGit(t, bare, "update-server-info")
	}
	var oldRequests, newRequests atomic.Int32
	var server *httptest.Server
	files := http.FileServer(http.Dir(root))
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/projects/42" {
			fmt.Fprintf(w, `{"id":42,"path_with_namespace":"new/repo","web_url":%q,"http_url_to_repo":%q}`, server.URL+"/new/repo", server.URL+"/new/repo.git")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/old/") {
			oldRequests.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/new/") {
			newRequests.Add(1)
		}
		files.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := gitlab.New(gitlab.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	git := &repo.Git{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}}
	ws := workspace.NewManager(t.TempDir(), git).Get(42)
	unlock, err := ws.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	service := &WorkspaceService{gitlab: client, git: repoGit{git: git}}
	project := &store.Project{ID: 42, PathWithNamespace: "old/repo", WebURL: server.URL + "/old/repo", DefaultBranch: "main"}
	if _, err := service.refreshBase(context.Background(), ws, project, "fake-token", "", ""); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(ws.BaseDir(), "identity.txt"))
	if err != nil || string(content) != "new" || oldRequests.Load() != 0 || newRequests.Load() == 0 {
		t.Fatalf("content=%q err=%v old requests=%d new requests=%d", content, err, oldRequests.Load(), newRequests.Load())
	}
}

func (g *identityGit) EnsureMirror(_ context.Context, _, remote string, _ repo.Auth) error {
	g.remotes = append(g.remotes, remote)
	return nil
}

func (g *identityGit) Fetch(context.Context, string, repo.Auth, ...string) error {
	g.fetches++
	return nil
}

func TestFetchProjectRefCancelledWhileWaitingForMirror(t *testing.T) {
	ws := workspace.NewManager(t.TempDir(), nil).Get(42)
	unlock, err := ws.LockMirror(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	git := &identityGit{fakeGitOps: newFakeGitOps(sha40)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = fetchProjectRef(ctx, ws, git, "https://example.invalid/group/repo.git", repo.Auth{}, "+refs/heads/main:refs/workspace/base", "refs/workspace/base")
	if !errors.Is(err, context.DeadlineExceeded) || len(git.remotes) != 0 || git.fetches != 0 {
		unlock()
		t.Fatalf("err=%v remotes=%v fetches=%d", err, git.remotes, git.fetches)
	}
	unlock()
	if _, err := fetchProjectRef(context.Background(), ws, git, "https://example.invalid/group/repo.git", repo.Auth{}, "+refs/heads/main:refs/workspace/base", "refs/workspace/base"); err != nil {
		t.Fatal(err)
	}
}

type faultingIdentityGit struct {
	*identityGit
	fail string
}

func (g *faultingIdentityGit) EnsureMirror(ctx context.Context, dir, remote string, auth repo.Auth) error {
	if g.fail == "ensure" {
		return errors.New("test failure")
	}
	return g.identityGit.EnsureMirror(ctx, dir, remote, auth)
}
func (g *faultingIdentityGit) Fetch(ctx context.Context, dir string, auth repo.Auth, refs ...string) error {
	if g.fail == "fetch" {
		return errors.New("test failure")
	}
	return g.identityGit.Fetch(ctx, dir, auth, refs...)
}
func (g *faultingIdentityGit) Resolve(ctx context.Context, dir, ref string) (string, error) {
	if g.fail == "resolve" {
		return "", errors.New("test failure")
	}
	return g.fakeGitOps.Resolve(ctx, dir, ref)
}

func TestFetchProjectRefReleasesMirrorOnFailure(t *testing.T) {
	for _, failure := range []string{"ensure", "fetch", "resolve"} {
		t.Run(failure, func(t *testing.T) {
			ws := workspace.NewManager(t.TempDir(), nil).Get(42)
			git := &faultingIdentityGit{identityGit: &identityGit{fakeGitOps: newFakeGitOps(sha40)}, fail: failure}
			if _, err := fetchProjectRef(context.Background(), ws, git, "https://example.invalid/group/repo.git", repo.Auth{}, "refspec", "ref"); err == nil {
				t.Fatal("expected failure")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			git.fail = ""
			if _, err := fetchProjectRef(ctx, ws, git, "https://example.invalid/group/repo.git", repo.Auth{}, "refspec", "ref"); err != nil {
				t.Fatalf("lock not released: %v", err)
			}
		})
	}
}

func TestPrepareRunRejectsChangedHeadAfterIdentityLookup(t *testing.T) {
	f := newReviewFixture(t, sha40)
	consumerReadLock(t, f.manager, 42)
	f.git.head = strings.Repeat("b", 40)
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := f.service.prepareRun(context.Background(), project, "fake-token", 7, "head-mismatch", sha40)
	if cleanup != nil {
		cleanup()
	}
	if err == nil || f.git.addCalls != 0 {
		t.Fatalf("err=%v worktrees=%d", err, f.git.addCalls)
	}
}

func TestRefreshBaseUsesCurrentProjectIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		id        int
		status    int
		wantFetch bool
		badURL    bool
		timeout   bool
	}{
		{name: "renamed", id: 42, status: 200, wantFetch: true},
		{name: "mismatch", id: 99, status: 200},
		{name: "not found", status: 404},
		{name: "invalid URL", id: 42, status: 200, badURL: true},
		{name: "timeout", timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/projects/42" {
					t.Errorf("unexpected project lookup %s", r.URL.Path)
				}
				if tc.timeout {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					clone := server.URL + "/new/repo.git"
					if tc.badURL {
						clone = "https://attacker.invalid/new/repo.git"
					}
					fmt.Fprintf(w, `{"id":%d,"path_with_namespace":"new/repo","web_url":%q,"http_url_to_repo":%q}`, tc.id, server.URL+"/new/repo", clone)
				}
			}))
			defer server.Close()
			client, err := gitlab.New(gitlab.Config{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			git := &identityGit{fakeGitOps: newFakeGitOps(sha40)}
			s := &WorkspaceService{gitlab: client, git: git}
			ws := workspace.NewManager(t.TempDir(), nil).Get(42)
			ctx := context.Background()
			if tc.timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			_, err = s.refreshBase(ctx, ws, &store.Project{ID: 42, WebURL: server.URL + "/old/repo", DefaultBranch: "main"}, "fake-token", "", "")
			if tc.wantFetch {
				if err != nil || git.fetches != 1 || len(git.remotes) != 1 || git.remotes[0] != server.URL+"/new/repo.git" {
					t.Fatalf("err=%v remotes=%v fetches=%d", err, git.remotes, git.fetches)
				}
			} else if err == nil || len(git.remotes) != 0 || git.fetches != 0 {
				t.Fatalf("err=%v remotes=%v fetches=%d", err, git.remotes, git.fetches)
			}
			if err != nil && strings.Contains(err.Error(), "fake-token") {
				t.Fatal("error exposed token")
			}
		})
	}
}
