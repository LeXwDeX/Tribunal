package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func baseSecurityGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func baseSecurityWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func baseSecurityRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRefreshBaseOwnershipRealGit(t *testing.T) {
	parent := t.TempDir()
	baseSecurityGit(t, parent, "init", "-q")
	baseSecurityGit(t, parent, "config", "user.name", "Test")
	baseSecurityGit(t, parent, "config", "user.email", "test@example.invalid")
	victim := filepath.Join(parent, "victim.txt")
	baseSecurityWrite(t, victim, "target\n")
	baseSecurityGit(t, parent, "add", "victim.txt")
	baseSecurityGit(t, parent, "commit", "-qm", "target")
	target := baseSecurityGit(t, parent, "rev-parse", "HEAD")
	baseSecurityGit(t, parent, "branch", "target", target)
	baseSecurityWrite(t, victim, "current\n")
	baseSecurityGit(t, parent, "commit", "-qam", "current")
	baseSecurityGit(t, parent, "branch", "current")
	baseSecurityWrite(t, victim, "dirty\n")

	serverRoot := t.TempDir()
	bare := filepath.Join(serverRoot, "repo.git")
	baseSecurityGit(t, parent, "clone", "--bare", "-q", parent, bare)
	baseSecurityGit(t, bare, "update-server-info")
	server := httptest.NewServer(http.StripPrefix("/group/", http.FileServer(http.Dir(serverRoot))))
	t.Cleanup(server.Close)

	git := &repo.Git{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}}
	project := &store.Project{ID: 42, PathWithNamespace: "group/repo", WebURL: server.URL + "/group/repo", DefaultBranch: "target"}
	api := newReviewGitLab(target)
	api.project = &gitlab.Project{ID: 42, PathWithNamespace: "group/repo", WebURL: project.WebURL, HTTPURLToRepo: project.WebURL + ".git"}
	service := &WorkspaceService{git: repoGit{git: git}, gitlab: api}

	t.Run("parent repository", func(t *testing.T) {
		ws := workspace.NewManager(filepath.Join(parent, "workspaces"), git).Get(1)
		unlock, err := ws.Lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := os.MkdirAll(ws.BaseDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(ws.BaseDir(), "keep.txt")
		baseSecurityWrite(t, keep, "keep")
		head := baseSecurityGit(t, parent, "rev-parse", "HEAD")
		index := baseSecurityRead(t, filepath.Join(parent, ".git", "index"))
		_, err = service.refreshBase(context.Background(), ws, project, "", "", "target")
		if !errors.Is(err, workspace.ErrBaseConflict) {
			t.Errorf("refresh error = %v, want ErrBaseConflict", err)
		}
		if got := baseSecurityGit(t, parent, "rev-parse", "HEAD"); got != head {
			t.Errorf("parent HEAD changed: %s != %s", got, head)
		}
		if got := string(baseSecurityRead(t, victim)); got != "dirty\n" {
			t.Errorf("parent dirty content changed: %q", got)
		}
		if got := baseSecurityRead(t, filepath.Join(parent, ".git", "index")); !bytes.Equal(got, index) {
			t.Error("parent index changed")
		}
		if got := string(baseSecurityRead(t, keep)); got != "keep" {
			t.Errorf("base content changed: %q", got)
		}
	})

	for _, kind := range []string{"independent repository", "other mirror worktree"} {
		t.Run(kind, func(t *testing.T) {
			ws := workspace.NewManager(t.TempDir(), git).Get(2)
			unlock, err := ws.Lock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if err := os.MkdirAll(ws.Root, 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "independent repository" {
				baseSecurityGit(t, ws.Root, "clone", "-q", parent, ws.BaseDir())
			} else {
				other := filepath.Join(t.TempDir(), "other.git")
				baseSecurityGit(t, parent, "clone", "--bare", "-q", parent, other)
				baseSecurityGit(t, other, "worktree", "add", "--detach", ws.BaseDir(), target)
			}
			keep := filepath.Join(ws.BaseDir(), "keep.txt")
			baseSecurityWrite(t, keep, "keep")
			baseSecurityWrite(t, filepath.Join(ws.BaseDir(), "victim.txt"), "foreign dirty\n")
			head := baseSecurityGit(t, ws.BaseDir(), "rev-parse", "HEAD")
			indexPath := baseSecurityGit(t, ws.BaseDir(), "rev-parse", "--git-path", "index")
			if !filepath.IsAbs(indexPath) {
				indexPath = filepath.Join(ws.BaseDir(), indexPath)
			}
			index := baseSecurityRead(t, indexPath)
			_, err = service.refreshBase(context.Background(), ws, project, "", "", "target")
			if !errors.Is(err, workspace.ErrBaseConflict) {
				t.Errorf("refresh error = %v, want ErrBaseConflict", err)
			}
			if got := baseSecurityGit(t, ws.BaseDir(), "rev-parse", "HEAD"); got != head {
				t.Errorf("foreign HEAD changed: %s != %s", got, head)
			}
			if got := string(baseSecurityRead(t, filepath.Join(ws.BaseDir(), "victim.txt"))); got != "foreign dirty\n" {
				t.Errorf("foreign content changed: %q", got)
			}
			if got := baseSecurityRead(t, indexPath); !bytes.Equal(got, index) {
				t.Error("foreign index changed")
			}
			if got := string(baseSecurityRead(t, keep)); got != "keep" {
				t.Errorf("base content changed: %q", got)
			}
		})
	}

	for _, kind := range []string{"missing", "empty", "owned"} {
		t.Run(kind, func(t *testing.T) {
			// The manager and service can receive different Git instances.
			ws := workspace.NewManager(t.TempDir(), &repo.Git{Bin: filepath.Join(t.TempDir(), "unused-git")}).Get(3)
			unlock, err := ws.Lock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if kind == "empty" {
				if err := os.MkdirAll(ws.BaseDir(), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "owned" {
				if _, err := service.refreshBase(context.Background(), ws, project, "", "", "current"); err != nil {
					t.Fatal(err)
				}
				baseSecurityWrite(t, filepath.Join(ws.BaseDir(), "victim.txt"), "dirty\n")
			}
			ref := "target"
			if kind == "missing" {
				ref = ""
			} else if kind == "empty" {
				ref = "refs/heads/target"
			}
			sha, err := service.refreshBase(context.Background(), ws, project, "", "", ref)
			if err != nil || sha != target || ws.BaseSHA() != target {
				t.Fatalf("refresh = %q, base = %q, err = %v", sha, ws.BaseSHA(), err)
			}
			if got := string(baseSecurityRead(t, filepath.Join(ws.BaseDir(), "victim.txt"))); got != "target\n" {
				t.Errorf("base content = %q", got)
			}
		})
	}
}
