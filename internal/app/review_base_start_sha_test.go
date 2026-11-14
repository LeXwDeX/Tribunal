package app

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/repo"
)

// reviewBaseGitOps resolves the MR base only after a fetch proves it exists in
// the mirror, like a target-branch fetch that brings the merge base along.
type reviewBaseGitOps struct {
	*fakeGitOps
	mu         sync.Mutex
	commits    map[string]bool
	fetchCalls int
	bringBase  bool
	knownRefs  map[string]bool
	addedHeads map[string]string
	fetchRefs  []string
}

func (g *reviewBaseGitOps) Resolve(ctx context.Context, dir, ref string) (string, error) {
	g.mu.Lock()
	head, known := g.dirHeads[dir]
	hasCommit := g.commits[ref] || g.knownRefs[ref]
	g.mu.Unlock()
	if known {
		return head, nil
	}
	if hasCommit {
		return ref, nil
	}
	return g.fakeGitOps.Resolve(ctx, dir, ref)
}

func (g *reviewBaseGitOps) Fetch(ctx context.Context, mirror string, auth repo.Auth, refspecs ...string) error {
	g.mu.Lock()
	g.fetchCalls++
	g.fetchRefs = append(g.fetchRefs, refspecs...)
	for _, ref := range refspecs {
		if strings.HasPrefix(ref, "+") {
			if g.bringBase {
				if idx := strings.Index(ref, ":"); idx > 1 && strings.HasPrefix(ref[1:idx], "refs/heads/") {
					// Fetching the target branch also brings its merge base.
					g.commits[strings.Repeat("2", 40)] = true
				}
			}
		}
	}
	g.mu.Unlock()
	return nil
}

func (g *reviewBaseGitOps) AddWorktree(ctx context.Context, mirror, dir, head string) error {
	g.mu.Lock()
	if g.addedHeads == nil {
		g.addedHeads = map[string]string{}
	}
	g.addedHeads[dir] = head
	g.mu.Unlock()
	return g.fakeGitOps.AddWorktree(ctx, mirror, dir, head)
}

func (g *reviewBaseGitOps) fetchCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fetchCalls
}

func TestReviewPreparesBaseAtStartSHA(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	git := &reviewBaseGitOps{fakeGitOps: f.git, commits: map[string]bool{}, bringBase: true}
	f.service.git = git
	startSHA := strings.Repeat("2", 40)

	job := f.seedReview(t, "base-at-start")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	dossier := readReviewDossier(t, f, job.ID)
	if !strings.HasSuffix(dossier.BaseDir, "-base") || !strings.HasPrefix(dossier.BaseDir, f.manager.Get(42).Root) {
		t.Fatalf("base dir = %q, want a per-run base worktree", dossier.BaseDir)
	}
	if git.addedHeads[dossier.BaseDir] != startSHA {
		t.Fatalf("base worktree head = %q, want %q", git.addedHeads[dossier.BaseDir], startSHA)
	}
	if git.fetchCount() < 2 {
		t.Fatalf("fetch calls = %d, want the MR ref and the target branch", git.fetchCount())
	}
	if got, want := git.fetchRefs[git.fetchCount()-1], "+refs/heads/main:refs/heads/main"; got != want {
		t.Fatalf("last fetch = %q, want the target branch %q", got, want)
	}
	if dossier.Metadata["base_identity"] == "" || dossier.Metadata["base_identity"] == "absent" {
		t.Fatalf("base identity = %q", dossier.Metadata["base_identity"])
	}
	if git.removeCalls < 2 {
		t.Fatalf("remove calls = %d, want the run and base worktrees cleaned up", git.removeCalls)
	}
}

func TestReviewDisclosesUnavailableBase(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	git := &reviewBaseGitOps{fakeGitOps: f.git, commits: map[string]bool{}} // fetch never brings the base
	f.service.git = git
	job := f.seedReview(t, "base-unavailable")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	dossier := readReviewDossier(t, f, job.ID)
	if dossier.BaseDir != "" || dossier.Metadata["base_identity"] != "absent" {
		t.Fatalf("base dir = %q identity = %q", dossier.BaseDir, dossier.Metadata["base_identity"])
	}
	if !dossierWarningContains(dossier, "base commit is unavailable") {
		t.Fatalf("dossier warnings = %v", dossier.Warnings)
	}
	found := false
	for _, body := range f.gitlab.noteBodies() {
		if strings.Contains(body, "未能取得 MR 的 base 提交") {
			found = true
		}
	}
	if !found {
		t.Fatalf("published notes do not disclose the missing base: %v", f.gitlab.noteBodies())
	}
}

// ensure the test fake still satisfies the interface after embedding.
var _ GitOps = (*reviewBaseGitOps)(nil)
