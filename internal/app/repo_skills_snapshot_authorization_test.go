package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type repositorySnapshotSource struct {
	*fakeGitOps
	sha    string
	files  map[string][]byte
	config []byte
}

func (g *repositorySnapshotSource) ResolveCommitRef(context.Context, string, string) (string, error) {
	return g.sha, nil
}

func (g *repositorySnapshotSource) ReadRepositorySkillConfig(context.Context, string, string) ([]byte, bool, error) {
	if g.config == nil {
		return nil, false, nil
	}
	return append([]byte(nil), g.config...), true, nil
}

func (g *repositorySnapshotSource) ReadRepositorySkills(_ context.Context, _, _ string, deny func(string) bool) (map[string][]byte, error) {
	out := make(map[string][]byte, len(g.files))
	for name, data := range g.files {
		if !deny(".claude/skills/" + name) {
			out[name] = append([]byte(nil), data...)
		}
	}
	return out, nil
}

func repositorySnapshotFixture(t *testing.T) (*reviewFixture, *repositorySnapshotSource, *store.Project) {
	t.Helper()
	f := newReviewFixture(t, strings.Repeat("a", 40))
	source := &repositorySnapshotSource{
		fakeGitOps: f.git,
		sha:        f.head,
		files: map[string][]byte{
			"review/SKILL.md": []byte("---\nname: review\ndescription: visible\n---\nbody\n"),
			"review/empty":    {},
			"review/blob.bin": {0x00, 0xff, 0x01},
		},
	}
	settings := json.RawMessage(`{"allow_repo_skills":true,"ignore":[]}`)
	if _, err := f.db.UpdateProject(context.Background(), 42, store.ProjectUpdate{Settings: settings}); err != nil {
		t.Fatal(err)
	}
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	return f, source, project
}

func TestRepositorySkillDiscoveryReturnsApprovedFilteredSnapshotHash(t *testing.T) {
	f, source, project := repositorySnapshotFixture(t)
	ws := f.manager.Get(42)
	defaults := config.ProjectSettings{AllowRepoSkills: true, AllowRepoConfig: false}
	discovery, err := discoverRepositorySkills(context.Background(), ws, source, f.gitlab, project, "token", "main", defaults, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := repo.RepositorySkillsSnapshotSHA256(source.files)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.TargetSHA != f.head || discovery.SnapshotSHA256 != want || len(discovery.Skills) != 1 || discovery.Skills[0].Name != "repo:review" {
		t.Fatalf("discovery = %+v, want hash %q", discovery, want)
	}

	ignored := json.RawMessage(`{"allow_repo_skills":true,"ignore":[".claude/skills/**"]}`)
	if _, err := f.db.UpdateProject(context.Background(), 42, store.ProjectUpdate{Settings: ignored}); err != nil {
		t.Fatal(err)
	}
	project, err = f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := discoverRepositorySkills(context.Background(), ws, source, f.gitlab, project, "token", "main", defaults, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := repo.RepositorySkillsSnapshotSHA256(nil)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.TargetSHA != f.head || filtered.SnapshotSHA256 != empty || len(filtered.Skills) != 0 {
		t.Fatalf("filtered discovery = %+v, want empty hash %q", filtered, empty)
	}
	if filtered.SnapshotSHA256 == discovery.SnapshotSHA256 {
		t.Fatal("widened ignore policy kept the approved snapshot hash")
	}
}

func TestRepositorySkillGrantBindsApprovedSnapshotAndRejectsStaleHash(t *testing.T) {
	f, source, project := repositorySnapshotFixture(t)
	ctx := WithDispatchAuthorization(context.Background(), func(context.Context, int64) error { return nil })
	now := time.Now().UTC()
	if err := f.db.CreateSession(ctx, store.Session{IDHash: []byte(strings.Repeat("x", 32)), UserID: 7, Username: "admin", Name: "admin", Role: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	defaults := config.ProjectSettings{AllowRepoSkills: true, AllowRepoConfig: false}
	dispatcher := &Dispatcher{
		store:     f.db,
		workspace: f.manager,
		cipher:    f.service.cipher,
		onboarding: &WorkspaceService{
			git:            source,
			gitlab:         f.gitlab,
			reviewDefaults: defaults,
			profiles:       f.profiles,
		},
	}
	discovery, err := discoverRepositorySkills(ctx, f.manager.Get(42), source, f.gitlab, project, "token", "main", defaults, nil)
	if err != nil {
		t.Fatal(err)
	}
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: f.head, Block: "review", Skills: []string{"repo:review"}}
	if _, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, f.head, "", 7); err == nil {
		t.Fatal("grant without a snapshot hash was accepted")
	}
	created, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, f.head, discovery.SnapshotSHA256, 7)
	if err != nil {
		t.Fatal(err)
	}
	if created.TargetSHA != f.head || created.SnapshotSHA256 != discovery.SnapshotSHA256 || len(created.Skills) != 1 {
		t.Fatalf("saved grant = %+v", created)
	}
	stale := strings.Repeat("c", 64)
	var conflict *Error
	if _, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, created.Revision, f.head, stale, 7); !errors.As(err, &conflict) || conflict.Code != "conflict" {
		t.Fatalf("stale snapshot hash = %v", err)
	}
	current, err := f.db.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || current.Revision != created.Revision || current.SnapshotSHA256 != discovery.SnapshotSHA256 {
		t.Fatalf("stale snapshot write changed grant: %+v, %v", current, err)
	}

	ignored := json.RawMessage(`{"allow_repo_skills":true,"ignore":[".claude/skills/**"]}`)
	if _, err := f.db.UpdateProject(ctx, 42, store.ProjectUpdate{Settings: ignored}); err != nil {
		t.Fatal(err)
	}
	project, err = f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := discoverRepositorySkills(ctx, f.manager.Get(42), source, f.gitlab, project, "token", "main", defaults, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.SnapshotSHA256 == created.SnapshotSHA256 {
		t.Fatal("ignore widening did not change the snapshot hash")
	}
	if _, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, created.Revision, f.head, discovery.SnapshotSHA256, 7); !errors.As(err, &conflict) || conflict.Code != "conflict" {
		t.Fatalf("widened ignore policy reused the approved snapshot: %v", err)
	}
	current, err = f.db.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || current.Revision != created.Revision || current.SnapshotSHA256 != created.SnapshotSHA256 || len(current.Skills) != 1 {
		t.Fatalf("widened ignore write changed grant: %+v, %v", current, err)
	}

	revocation := grant
	revocation.Skills = []string{}
	revoked, err := dispatcher.ReplaceRepositorySkillAuthorization(ctx, revocation, created.Revision, "", "", 7)
	if err != nil || len(revoked.Skills) != 0 || revoked.TargetSHA != "" || revoked.SnapshotSHA256 != "" {
		t.Fatalf("revocation = %+v, %v", revoked, err)
	}
}

type legacyRepositorySkillStore struct {
	DispatchStore
	grant *store.RepositorySkillAuthorization
}

func (s *legacyRepositorySkillStore) GetRepositorySkillAuthorization(_ context.Context, projectID int64, branch, block string) (*store.RepositorySkillAuthorization, error) {
	copy := *s.grant
	copy.ProjectID, copy.TargetBranch, copy.Block = projectID, branch, block
	return &copy, nil
}

func (s *legacyRepositorySkillStore) SaveRepositorySkillAuthorization(context.Context, store.RepositorySkillAuthorization, string, int64) (*store.RepositorySkillAuthorization, error) {
	return nil, errors.New("unexpected write")
}

func TestRepositorySkillAuthorizationReturnsLegacyGrantForAdminRevocation(t *testing.T) {
	f := newReviewFixture(t, strings.Repeat("a", 40))
	legacy := &store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:legacy"}, Revision: strings.Repeat("d", 64)}
	backend := &legacyRepositorySkillStore{DispatchStore: f.db, grant: legacy}
	dispatcher := &Dispatcher{store: backend, workspace: f.manager}
	ctx := WithDispatchAuthorization(context.Background(), func(context.Context, int64) error { return nil })
	grant, err := dispatcher.RepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || len(grant.Skills) != 1 || grant.SnapshotSHA256 != "" || grant.Revision != legacy.Revision {
		t.Fatalf("legacy authorization = %+v, %v", grant, err)
	}
	if _, _, err := applyRepositorySkillAuthorizations(nil, 42, "main", legacy.TargetSHA, []store.RepositorySkillAuthorization{*grant}); err == nil {
		t.Fatal("legacy grant without an approved snapshot was applied")
	}
}
