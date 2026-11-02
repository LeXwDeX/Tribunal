package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type skillRawTrackingGitLab struct {
	*reviewGitLab
	refs []string
}

type repoSkillGrantTestStore struct {
	DispatchStore
	writes  int
	project *store.Project
}

func (s *repoSkillGrantTestStore) GetProject(ctx context.Context, id int64) (*store.Project, error) {
	if s.project == nil {
		return s.DispatchStore.GetProject(ctx, id)
	}
	if id != s.project.ID {
		return nil, store.ErrNotFound
	}
	return s.project, nil
}

func (s *repoSkillGrantTestStore) GetRepositorySkillAuthorization(_ context.Context, projectID int64, branch, block string) (*store.RepositorySkillAuthorization, error) {
	return &store.RepositorySkillAuthorization{ProjectID: projectID, TargetBranch: branch, Block: block, Skills: []string{}, Revision: store.BlockBindingAbsentRevision}, nil
}

func (s *repoSkillGrantTestStore) SaveRepositorySkillAuthorization(_ context.Context, grant store.RepositorySkillAuthorization, _ string, _ int64) (*store.RepositorySkillAuthorization, error) {
	s.writes++
	return &grant, nil
}

func (g *skillRawTrackingGitLab) GetRawFile(ctx context.Context, token string, projectID int64, name, ref string) ([]byte, error) {
	g.refs = append(g.refs, ref)
	return g.reviewGitLab.GetRawFile(ctx, token, projectID, name, ref)
}

// This tests the components together, not runtime authorization or branch verification.
func TestRepositorySkillSnapshotUsesDossierIgnoreAndRenamePaths(t *testing.T) {
	dir := t.TempDir()
	baseSecurityGit(t, dir, "init", "-q")
	baseSecurityGit(t, dir, "config", "user.name", "Test")
	baseSecurityGit(t, dir, "config", "user.email", "test@example.invalid")
	files := map[string]string{
		".claude/skills/good/SKILL.md":      "---\nname: good\ndescription: visible\n---\nvisible body\n",
		".claude/skills/good/ok.txt":        "allowed resource",
		".claude/skills/good/private/a.txt": "ignored resource canary",
		".claude/skills/good/renamed.txt":   "ignored rename canary",
		".claude/skills/hidden/SKILL.md":    "---\nname: hidden\ndescription: ignored description canary\n---\nignored body canary\n",
	}
	for path, value := range files {
		name := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		baseSecurityWrite(t, name, value)
	}
	baseSecurityGit(t, dir, "add", "-A")
	baseSecurityGit(t, dir, "commit", "-qm", "target")
	sha := baseSecurityGit(t, dir, "rev-parse", "HEAD")
	patterns := []string{"**/private/**"}
	diffs := []gitlab.FileDiff{
		{OldPath: "private/secret.txt", NewPath: ".claude/skills/good/renamed.txt", RenamedFile: true},
		{OldPath: ".claude/skills/hidden/SKILL.md", NewPath: "private/hidden.md", RenamedFile: true},
	}
	_, ignored, _, _ := buildReviewFiles(diffs, patterns, 100, 100)
	dossier := &review.Dossier{Ignored: ignored, IgnorePolicy: &review.IgnorePolicy{Version: review.IgnorePolicyVersion, Patterns: patterns}}
	if dossier.IgnoresPath(".claude/skills/hidden/SKILL.md") {
		t.Fatal("fixture no longer proves the reverse-rename boundary")
	}
	deny, err := repositorySkillDeny(dossier, diffs)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&repo.Git{}).ReadRepositorySkills(context.Background(), dir, sha, deny)
	if err != nil {
		t.Fatal(err)
	}
	for path := range snapshot {
		if path != "good/SKILL.md" && path != "good/ok.txt" {
			t.Fatalf("ignored path in snapshot: %q", path)
		}
	}
	mount := t.TempDir()
	for path, data := range snapshot {
		file := filepath.Join(mount, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loader, err := skill.New(skill.Options{RepoSkillsDir: mount})
	if err != nil {
		t.Fatal(err)
	}
	catalogue := loader.Catalogue()
	if len(catalogue) != 1 || catalogue[0].Name != "repo:good" || catalogue[0].Description != "visible" {
		t.Fatalf("filtered catalogue = %+v", catalogue)
	}
	binding, err := loader.Bind([]string{"repo:good"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := binding.Tool().Call(context.Background(), json.RawMessage(`{"name":"repo:good"}`))
	if err != nil || !strings.Contains(loaded.Content, "visible body") || !strings.Contains(loaded.Content, "ok.txt") || strings.Contains(loaded.Content, "canary") || strings.Contains(loaded.Content, "renamed.txt") {
		t.Fatalf("filtered load = %q, %v", loaded.Content, err)
	}
}

func TestPinRepositorySkillTargetUsesAuthenticatedExactBranch(t *testing.T) {
	source := t.TempDir()
	baseSecurityGit(t, source, "init", "-q", "-b", "main")
	baseSecurityGit(t, source, "config", "user.name", "Test")
	baseSecurityGit(t, source, "config", "user.email", "test@example.invalid")
	skillFile := filepath.Join(source, ".claude", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := func(body string) string { return "---\nname: review\ndescription: " + body + "\n---\n" + body }
	baseSecurityWrite(t, skillFile, manifest("base"))
	configFile := filepath.Join(source, ".gitlab-review.yml")
	baseSecurityWrite(t, configFile, "version: 1\n")
	baseSecurityGit(t, source, "add", "-A")
	baseSecurityGit(t, source, "commit", "-qm", "base")
	base := baseSecurityGit(t, source, "rev-parse", "HEAD")
	baseSecurityGit(t, source, "branch", "feature")
	baseSecurityWrite(t, skillFile, manifest("target one"))
	baseSecurityWrite(t, configFile, "version: 1\nignore:\n  - .claude/skills/review/SKILL.md\n")
	baseSecurityGit(t, source, "commit", "-qam", "target one")
	one := baseSecurityGit(t, source, "rev-parse", "HEAD")
	baseSecurityGit(t, source, "branch", "release/one")
	baseSecurityGit(t, source, "checkout", "-q", "-b", "release/two", base)
	baseSecurityWrite(t, skillFile, manifest("target two"))
	baseSecurityWrite(t, configFile, "version: 1\n")
	baseSecurityGit(t, source, "commit", "-qam", "target two")
	two := baseSecurityGit(t, source, "rev-parse", "HEAD")
	baseSecurityGit(t, source, "checkout", "-q", "feature")
	baseSecurityWrite(t, skillFile, manifest("merge request head"))
	baseSecurityGit(t, source, "commit", "-qam", "head")
	head := baseSecurityGit(t, source, "rev-parse", "HEAD")

	serverRoot := t.TempDir()
	bare := filepath.Join(serverRoot, "repo.git")
	baseSecurityGit(t, source, "clone", "--bare", "-q", source, bare)
	baseSecurityGit(t, bare, "update-server-info")
	server := httptest.NewServer(http.StripPrefix("/group/", http.FileServer(http.Dir(serverRoot))))
	t.Cleanup(server.Close)
	project := &store.Project{ID: 42, PathWithNamespace: "group/repo", WebURL: server.URL + "/group/repo"}
	api := newReviewGitLab(head)
	api.project = &gitlab.Project{ID: 42, PathWithNamespace: project.PathWithNamespace, WebURL: project.WebURL, HTTPURLToRepo: project.WebURL + ".git"}
	git := repoGit{git: &repo.Git{}}
	manager := workspace.NewManager(t.TempDir(), git.git)
	ws := manager.Get(project.ID)
	unlock, err := ws.RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	for branch, want := range map[string]string{"release/one": one, "release/two": two} {
		got, err := pinRepositorySkillTarget(context.Background(), ws, git, api, project, "token", branch)
		if err != nil || got != want || got == head || got == base {
			t.Fatalf("branch %q tip = %q, %v; want %q", branch, got, err, want)
		}
	}
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		branch, want := "release/one", one
		if i%2 == 1 {
			branch, want = "release/two", two
		}
		go func() {
			unlock, err := ws.RLock(context.Background())
			if err == nil {
				defer unlock()
				var got string
				got, err = pinRepositorySkillTarget(context.Background(), ws, git, api, project, "token", branch)
				if err == nil && got != want {
					err = errors.New("concurrent branch fetched the wrong target")
				}
			}
			results <- err
		}()
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for sha, want := range map[string]string{one: "target one", two: "target two"} {
		files, err := git.git.ReadRepositorySkills(context.Background(), ws.MirrorDir(), sha, func(string) bool { return false })
		if err != nil || string(files["review/SKILL.md"]) != manifest(want) {
			t.Fatalf("pinned SHA %s = %q, %v; want %q", sha, files["review/SKILL.md"], err, want)
		}
	}
	tracker := &skillRawTrackingGitLab{reviewGitLab: api}
	api.docs[".gitlab-review.yml"] = "version: 1\n"
	discovery, err := discoverRepositorySkills(context.Background(), ws, git, tracker, project, "token", "release/one", config.ProjectSettings{AllowRepoSkills: true, AllowRepoConfig: true}, nil)
	if err != nil || !discovery.Enabled || discovery.TargetSHA != one || len(discovery.Skills) != 0 || len(tracker.refs) != 0 {
		t.Fatalf("ignored discovery = %+v, refs=%v, err=%v", discovery, tracker.refs, err)
	}
	oneHash := discovery.SnapshotSHA256
	api.docs[".gitlab-review.yml"] = "version: 1\nignore:\n  - .claude/skills/review/SKILL.md\n"
	discovery, err = discoverRepositorySkills(context.Background(), ws, git, tracker, project, "token", "release/two", config.ProjectSettings{AllowRepoSkills: true, AllowRepoConfig: true}, nil)
	if err != nil || discovery.TargetSHA != two || len(discovery.Skills) != 1 || discovery.Skills[0].Name != "repo:review" || discovery.Skills[0].Description != "target two" {
		t.Fatalf("target discovery = %+v, err=%v", discovery, err)
	}
	twoHash := discovery.SnapshotSHA256
	unlock()
	locked = false
	cipher := newTestCipher(t)
	project.TokenEnc, err = cipher.Seal([]byte("token"), secret.TokenAAD(project.ID))
	if err != nil {
		t.Fatal(err)
	}
	project.Enabled = true
	backend := &repoSkillGrantTestStore{project: project}
	dispatcher := &Dispatcher{store: backend, workspace: manager, cipher: cipher, onboarding: &WorkspaceService{
		git: git, gitlab: api, reviewDefaults: config.ProjectSettings{AllowRepoSkills: true, AllowRepoConfig: true},
	}}
	grant := store.RepositorySkillAuthorization{ProjectID: project.ID, TargetBranch: "release/one", Block: "review", Skills: []string{"repo:review"}}
	ctx := WithDispatchAuthorization(context.Background(), func(context.Context, int64) error { return nil })
	if _, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, one, oneHash, 7); err == nil || backend.writes != 0 {
		t.Fatalf("ignored manifest was authorized: %v, writes=%d", err, backend.writes)
	}
	grant.TargetBranch = "release/two"
	if _, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, "", "", 7); err == nil || backend.writes != 0 {
		t.Fatalf("grant without target SHA was accepted: %v, writes=%d", err, backend.writes)
	}
	if _, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, one, twoHash, 7); err == nil || backend.writes != 0 {
		t.Fatalf("stale discovered SHA was authorized: %v, writes=%d", err, backend.writes)
	}
	grant.TargetSHA = strings.Repeat("f", 40) // The separately verified SHA is authoritative.
	granted, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, strings.ToUpper(two), twoHash, 7)
	if err != nil || backend.writes != 1 {
		t.Fatalf("visible target skill grant: %v, writes=%d", err, backend.writes)
	}
	encoded, err := json.Marshal(granted)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil || fields["target_sha"] != two {
		t.Fatalf("reviewed target SHA missing from saved grant: %s, %v", encoded, err)
	}
	baseSecurityGit(t, source, "checkout", "-q", "release/two")
	baseSecurityWrite(t, skillFile, manifest("target changed"))
	baseSecurityGit(t, source, "commit", "-qam", "target changed")
	newTip := baseSecurityGit(t, source, "rev-parse", "HEAD")
	baseSecurityGit(t, source, "push", "-q", bare, "release/two")
	baseSecurityGit(t, bare, "update-server-info")
	if _, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, two, twoHash, 7); err == nil || backend.writes != 1 {
		t.Fatalf("grant for a moved branch was accepted: %v, writes=%d", err, backend.writes)
	}
	granted.Revision = strings.Repeat("a", 64)
	if _, _, err := applyRepositorySkillAuthorizations(nil, project.ID, "release/two", newTip, []store.RepositorySkillAuthorization{*granted}); err == nil {
		t.Fatal("new target inherited authorization for the old commit")
	}
	newDiscovery, err := discoverRepositorySkills(ctx, ws, git, api, project, "token", "release/two", config.ProjectSettings{AllowRepoSkills: true, AllowRepoConfig: true}, nil)
	if err != nil || newDiscovery.TargetSHA != newTip {
		t.Fatalf("new target discovery = %+v, %v", newDiscovery, err)
	}
	newHash := newDiscovery.SnapshotSHA256
	updated, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, newTip, newHash, 7)
	if err != nil || backend.writes != 2 || updated.TargetSHA != newTip {
		t.Fatalf("new target grant = %+v, %v, writes=%d", updated, err, backend.writes)
	}

	// Exercise the dispatcher and actual transaction together; runtime use and
	// external-send revocation remain outside this test's scope.
	db := openTestStore(t)
	dbProject := *project
	dbProject.DefaultBranch = "main"
	if _, err := db.RegisterProject(ctx, dbProject); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, store.Session{IDHash: []byte(strings.Repeat("x", 32)), UserID: 7, Username: "admin", Name: "admin", Role: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectGrant(ctx, project.ID, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	realDispatcher := &Dispatcher{store: db, workspace: manager, cipher: cipher, onboarding: dispatcher.onboarding}
	persisted, err := realDispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, newTip, newHash, 7)
	if err != nil || persisted.TargetSHA != newTip {
		t.Fatalf("persisted grant = %+v, %v", persisted, err)
	}
	reread, err := realDispatcher.RepositorySkillAuthorization(ctx, project.ID, grant.TargetBranch, grant.Block)
	if err != nil || reread.TargetSHA != newTip || reread.SnapshotSHA256 != newHash || reread.Revision != persisted.Revision {
		t.Fatalf("read persisted grant = %+v, %v", reread, err)
	}
	if _, err := realDispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, newTip, newHash, 7); err == nil {
		t.Fatal("stale revision replaced the grant")
	}
	revocation := grant
	revocation.Skills = []string{}
	revoked, err := realDispatcher.ReplaceRepositorySkillAuthorization(ctx, revocation, persisted.Revision, "", "", 7)
	if err != nil || revoked.TargetSHA != "" || revoked.Revision == persisted.Revision {
		t.Fatalf("persisted revocation = %+v, %v", revoked, err)
	}
	if _, err := realDispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, persisted.Revision, newTip, newHash, 7); err == nil {
		t.Fatal("stale revision undid the revocation")
	}
	reread, err = realDispatcher.RepositorySkillAuthorization(ctx, project.ID, grant.TargetBranch, grant.Block)
	if err != nil || len(reread.Skills) != 0 || reread.TargetSHA != "" || reread.SnapshotSHA256 != "" || reread.Revision != revoked.Revision {
		t.Fatalf("revocation not persisted: %+v, %v", reread, err)
	}
	for _, branch := range []string{"", "../main", "--upload-pack=x", "main:refs/heads/feature"} {
		if _, err := pinRepositorySkillTarget(context.Background(), ws, git, api, project, "token", branch); err == nil {
			t.Errorf("invalid branch %q accepted", branch)
		}
	}
	wrong := *project
	wrong.ID++
	if _, err := pinRepositorySkillTarget(context.Background(), ws, git, api, &wrong, "token", "release/one"); err == nil {
		t.Fatal("another project's workspace was accepted")
	}
	api.project.ID++
	if _, err := pinRepositorySkillTarget(context.Background(), ws, git, api, project, "token", "release/one"); err == nil {
		t.Fatal("unverified GitLab project was accepted")
	}
}

func TestRepositorySkillDiscoveryDisabledDoesNotReadRepository(t *testing.T) {
	project := &store.Project{ID: 42, Settings: json.RawMessage(`{"allow_repo_skills":false}`)}
	gitlab := newReviewGitLab(strings.Repeat("1", 40))
	gitlab.project = nil
	ws := workspace.NewManager(t.TempDir(), nil).Get(project.ID)
	got, err := discoverRepositorySkills(context.Background(), ws, &fakeGitOps{}, gitlab, project, "token", "main", config.ProjectSettings{}, nil)
	if err != nil || got.Enabled || got.TargetSHA != "" || got.SnapshotSHA256 != "" || len(got.Skills) != 0 {
		t.Fatalf("disabled discovery = %+v, %v", got, err)
	}
}

func TestRepositorySkillDenyPropagatesIgnoredAncestorsAcrossRenames(t *testing.T) {
	dossier := &review.Dossier{Ignored: []string{"protected"}, IgnorePolicy: &review.IgnorePolicy{Version: review.IgnorePolicyVersion, Patterns: []string{}}}
	diffs := []gitlab.FileDiff{
		{OldPath: "protected/a", NewPath: ".claude/skills/review/intermediate"},
		{OldPath: ".claude/skills/review/intermediate", NewPath: ".claude/skills/review/SKILL.md"},
	}
	deny, err := repositorySkillDeny(dossier, diffs)
	if err != nil || !deny(".claude/skills/review/SKILL.md") {
		t.Fatalf("ignored ancestor/rename chain was not denied: err=%v", err)
	}
	if _, err := repositorySkillDeny(&review.Dossier{}, diffs); err == nil {
		t.Fatal("missing ignore policy accepted")
	}
}

func TestRepositorySkillDiscoveryRechecksAccessAndFailsClosedWithoutVerifiedSource(t *testing.T) {
	f := newReviewFixture(t, strings.Repeat("a", 40))
	workspaceService := newOnboardingContextService(t, f, f.runner.Run)
	dispatcher := &Dispatcher{store: f.db, workspace: f.manager, onboarding: workspaceService, cipher: f.service.cipher}
	checks := 0
	ctx := WithDispatchAuthorization(context.Background(), func(context.Context, int64) error {
		checks++
		if checks == 2 {
			return store.ErrNotFound
		}
		return nil
	})
	result, err := dispatcher.DiscoverRepositorySkills(ctx, 42, "main")
	if err == nil || result != nil || checks != 2 {
		t.Fatalf("revoked discovery = %+v, %v, checks=%d", result, err, checks)
	}
	if _, err := f.db.UpdateProject(context.Background(), 42, store.ProjectUpdate{Settings: json.RawMessage(`{"allow_repo_skills":true}`)}); err != nil {
		t.Fatal(err)
	}
	if result, err := dispatcher.DiscoverRepositorySkills(context.Background(), 42, "main"); err == nil || result != nil {
		t.Fatalf("unverified source discovery = %+v, %v", result, err)
	}
}

func TestRepositorySkillGrantsCannotBeCreatedFromDisabledSource(t *testing.T) {
	f := newReviewFixture(t, strings.Repeat("a", 40))
	backend := &repoSkillGrantTestStore{DispatchStore: f.db}
	dispatcher := &Dispatcher{store: backend, workspace: f.manager, onboarding: newOnboardingContextService(t, f, f.runner.Run), cipher: f.service.cipher}
	ctx := WithDispatchAuthorization(context.Background(), func(context.Context, int64) error { return nil })
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{"repo:unreviewed"}}
	if _, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, "", "", 7); err == nil || backend.writes != 0 {
		t.Fatalf("disabled source granted repository skill: %v, writes=%d", err, backend.writes)
	}
	grant.Skills = []string{}
	grant.TargetSHA = strings.Repeat("a", 40)
	revoked, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, "", "", 7)
	if err != nil || backend.writes != 1 || revoked.TargetSHA != "" || len(revoked.Skills) != 0 {
		t.Fatalf("revocation while disabled failed: %v, writes=%d", err, backend.writes)
	}
}

func TestRepositorySkillRevocationWaitHonorsCancellation(t *testing.T) {
	f := newReviewFixture(t, strings.Repeat("a", 40))
	backend := &repoSkillGrantTestStore{DispatchStore: f.db}
	dispatcher := &Dispatcher{store: backend, workspace: f.manager, onboarding: newOnboardingContextService(t, f, f.runner.Run), cipher: f.service.cipher}
	ctx := WithDispatchAuthorization(context.Background(), func(context.Context, int64) error { return nil })
	ws := f.manager.Get(42)
	unlock, err := ws.RLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = dispatcher.ReplaceRepositorySkillAuthorization(blocked, store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{}}, store.BlockBindingAbsentRevision, "", "", 7)
	cancel()
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) || backend.writes != 0 {
		t.Fatalf("blocked revocation = %v, writes=%d", err, backend.writes)
	}
	_, err = dispatcher.ReplaceRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{}}, store.BlockBindingAbsentRevision, "", "", 7)
	if err != nil || backend.writes != 1 {
		t.Fatalf("revocation after release = %v, writes=%d", err, backend.writes)
	}
}

// Replace waits for the lifecycle and content locks under the caller's
// context. Only the outbound drain is bounded, so these cases outlive the
// removed internal 5 s lock deadline to catch a regression.
func TestRepositorySkillReplaceLockWaitsUseCallerContext(t *testing.T) {
	for _, lock := range []struct {
		name    string
		acquire func(*workspace.Workspace, context.Context) (func(), error)
		probe   func(*workspace.Workspace, context.Context) (func(), error)
	}{
		{"content", (*workspace.Workspace).Lock, (*workspace.Workspace).RLock},
		{"lifecycle", (*workspace.Workspace).LifecycleLock, (*workspace.Workspace).LifecycleRLock},
	} {
		t.Run(lock.name, func(t *testing.T) {
			f := newReviewFixture(t, strings.Repeat("a", 40))
			backend := &repoSkillGrantTestStore{DispatchStore: f.db}
			dispatcher := &Dispatcher{store: backend, workspace: f.manager, onboarding: newOnboardingContextService(t, f, f.runner.Run), cipher: f.service.cipher}
			ctx := WithDispatchAuthorization(context.Background(), func(context.Context, int64) error { return nil })
			ws := f.manager.Get(42)
			unlock, err := lock.acquire(ws, ctx)
			if err != nil {
				t.Fatal(err)
			}
			released := false
			defer func() {
				if !released {
					unlock()
				}
			}()
			done := make(chan error, 1)
			go func() {
				_, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{}}, store.BlockBindingAbsentRevision, "", "", 7)
				done <- err
			}()
			waitLockWriter(t, ctx, func(probeCtx context.Context) (func(), error) { return lock.probe(ws, probeCtx) })
			select {
			case err := <-done:
				t.Fatalf("replace returned while the %s lock was held: %v", lock.name, err)
			case <-time.After(legacyLockWait + 500*time.Millisecond):
			}
			released = true
			unlock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("replace after the %s lock was released: %v", lock.name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("replace did not resume after the %s lock was released", lock.name)
			}
			if backend.writes != 1 {
				t.Fatalf("writes = %d", backend.writes)
			}
		})
	}
}
