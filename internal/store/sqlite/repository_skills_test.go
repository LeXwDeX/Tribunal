package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRepositorySkillAuthorizationRequiresAndPersistsTargetSHA(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
	missing := grant
	missing.Block = "missing-sha"
	if _, err := s.SaveRepositorySkillAuthorization(ctx, missing, store.BlockBindingAbsentRevision, 7); err == nil {
		t.Error("nonempty grant without a target SHA was accepted")
	}

	grant.TargetSHA = strings.Repeat("a", 40)
	created, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []*store.RepositorySkillAuthorization{created, got} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["target_sha"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Errorf("reviewed target SHA not persisted: %s", encoded)
		}
	}
}

func repositorySkillStore(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t, newFakeClock())
	for _, project := range []int64{42, 43} {
		mustProject(t, s, project)
	}
	mustAccessUser(t, s, 7, "alice", "admin")
	if err := s.SetProjectGrant(context.Background(), 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRepositorySkillAuthorizationMigrationAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustProject(t, s, 42)
	mustAccessUser(t, s, 7, "alice", "admin")
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	project := int64(42)
	legacy := store.BlockBinding{Scope: store.ScopeProject, ProjectID: &project, Block: "review", Skills: []string{"repo:legacy"}}
	if _, err := s.UpsertBlockBinding(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	// Simulate a database created before 0021, retaining all earlier data.
	if _, err := s.db.ExecContext(ctx, `DROP TABLE repository_skill_authorizations; DELETE FROM schema_migrations WHERE version IN (21, 23)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations WHERE version = 21`); got != 1 {
		t.Fatalf("migration 0021 recorded %d times", got)
	}
	legacyBindings, err := s.ListBlockBindings(ctx, store.ScopeProject, &project)
	if err != nil || len(legacyBindings) != 1 || !reflect.DeepEqual(legacyBindings[0].Skills, legacy.Skills) {
		t.Fatalf("migration changed legacy binding: %+v, %v", legacyBindings, err)
	}
	empty, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || len(empty.Skills) != 0 || empty.TargetSHA != "" || empty.Revision != store.BlockBindingAbsentRevision {
		t.Fatalf("legacy binding became authorization: %+v, %v", empty, err)
	}
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
	created, err := s.SaveRepositorySkillAuthorization(ctx, grant, empty.Revision, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || got.TargetSHA != grant.TargetSHA || got.SnapshotSHA256 != grant.SnapshotSHA256 || got.Revision != created.Revision || !reflect.DeepEqual(got.Skills, grant.Skills) {
		t.Fatalf("grant changed after reopen: %+v, %v", got, err)
	}
}

func TestRepositorySkillOldDraftSchemaFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	if _, err := s.db.ExecContext(ctx, `DROP TABLE repository_skill_authorizations;
		CREATE TABLE repository_skill_authorizations (project_id INTEGER, target_branch TEXT, block TEXT, skills TEXT, revision TEXT, PRIMARY KEY (project_id, target_branch, block));
		INSERT INTO repository_skill_authorizations VALUES (42, 'main', 'review', '["repo:safe"]', ?)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review"); err == nil {
		t.Fatal("old unpinned grant was read")
	}
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
	if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, strings.Repeat("a", 64), 7); err == nil {
		t.Fatal("old draft schema accepted a grant")
	}
}

func TestRepositorySkillAuthorizationIsScopedAndConditional(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	project := int64(42)
	if _, err := s.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopeProject, ProjectID: &project, Block: "review", Skills: []string{"repo:legacy"}}); err != nil {
		t.Fatal(err)
	}
	empty, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || empty.Revision != store.BlockBindingAbsentRevision || len(empty.Skills) != 0 {
		t.Fatalf("legacy binding granted repository skills: %+v, %v", empty, err)
	}
	sha := strings.Repeat("a", 40)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{"repo:b", "repo:a"}, SnapshotSHA256: strings.Repeat("b", 64)}
	created, err := s.SaveRepositorySkillAuthorization(ctx, grant, empty.Revision, 7)
	if err != nil || !reflect.DeepEqual(created.Skills, []string{"repo:a", "repo:b"}) {
		t.Fatalf("create = %+v, %v", created, err)
	}
	for _, key := range []struct {
		project       int64
		branch, block string
	}{
		{43, "main", "review"}, {42, "release", "review"}, {42, "main", "verify"},
	} {
		other, err := s.GetRepositorySkillAuthorization(ctx, key.project, key.branch, key.block)
		if err != nil || other.Revision != store.BlockBindingAbsentRevision || len(other.Skills) != 0 {
			t.Fatalf("grant leaked to %+v: %+v, %v", key, other, err)
		}
	}
	updated, err := s.SaveRepositorySkillAuthorization(ctx, grant, created.Revision, 7)
	if err != nil || updated.Revision == created.Revision {
		t.Fatalf("same-content write must advance revision: %+v %v", updated, err)
	}
	grant.TargetSHA = strings.Repeat("b", 40)
	if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, created.Revision, 7); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale authorization write: %v", err)
	}
	current, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || current.TargetSHA != sha || current.Revision != updated.Revision {
		t.Fatalf("stale write changed authorization: %+v, %v", current, err)
	}
	grant.Skills = []string{}
	revoked, err := s.SaveRepositorySkillAuthorization(ctx, grant, updated.Revision, 7)
	if err != nil || len(revoked.Skills) != 0 || revoked.TargetSHA != "" || revoked.Revision == updated.Revision {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	current, err = s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || current.TargetSHA != "" || len(current.Skills) != 0 || current.Revision != revoked.Revision {
		t.Fatalf("revocation did not persist: %+v, %v", current, err)
	}
	grant.Skills = []string{"repo:a", "repo:b"}
	grant.TargetSHA = sha
	if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, updated.Revision, 7); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale writer undid revocation: %v", err)
	}
	regranted, err := s.SaveRepositorySkillAuthorization(ctx, grant, revoked.Revision, 7)
	if err != nil || regranted.Revision == created.Revision || regranted.Revision == updated.Revision || regranted.Revision == revoked.Revision {
		t.Fatalf("regrant reused old identity: %+v, %v", regranted, err)
	}
}

func TestRepositorySkillAuthorizationValidatesTargetSHA(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
	for _, sha := range []string{"", "abc", strings.Repeat("a", 39), strings.Repeat("g", 40), " " + strings.Repeat("a", 40), strings.Repeat("a", 65)} {
		grant.TargetSHA = sha
		if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7); err == nil {
			t.Errorf("invalid target SHA %q accepted", sha)
		}
	}
	for _, sha := range []string{strings.Repeat("A", 40), strings.Repeat("B", 64)} {
		grant.Block = "review" + sha[:1]
		grant.Block = strings.ToLower(grant.Block)
		grant.TargetSHA = sha
		created, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7)
		if err != nil || created.TargetSHA != strings.ToLower(sha) {
			t.Fatalf("normalized grant = %+v, %v", created, err)
		}
		stored, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", grant.Block)
		if err != nil || stored.TargetSHA != strings.ToLower(sha) {
			t.Fatalf("normalized persisted SHA = %+v, %v", stored, err)
		}
	}
}

func TestRepositorySkillAuthorizationRejectsCorruptTargetState(t *testing.T) {
	validSnapshot := strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, skills, sha, snapshot string
	}{
		{"missing", `["repo:safe"]`, "", validSnapshot},
		{"invalid", `["repo:safe"]`, strings.Repeat("g", 40), validSnapshot},
		{"noncanonical", `["repo:safe"]`, strings.Repeat("A", 40), validSnapshot},
		{"revoked with SHA", `[]`, strings.Repeat("a", 40), ""},
		{"invalid snapshot", `["repo:safe"]`, strings.Repeat("a", 40), "invalid"},
		{"revoked with snapshot", `[]`, "", validSnapshot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := repositorySkillStore(t)
			grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
			created, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE repository_skill_authorizations SET skills = ?, target_sha = ?, snapshot_sha256 = ? WHERE project_id = 42`, tc.skills, tc.sha, tc.snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review"); err == nil {
				t.Fatal("corrupt authorization was read")
			}
			if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, created.Revision, 7); err == nil {
				t.Fatal("corrupt authorization was silently replaced")
			}
		})
	}
}

func TestRepositorySkillAuthorizationChecksCurrentManageGrantAndNames(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
	otherProject := grant
	otherProject.ProjectID = 43
	if _, err := s.SaveRepositorySkillAuthorization(ctx, otherProject, store.BlockBindingAbsentRevision, 7); err == nil {
		t.Fatal("manage grant for another project authorized repository skills")
	}
	for _, name := range []string{"safe", "repo:", "repo:../bad", "repo:Bad", "repo:a__b"} {
		invalid := grant
		invalid.Skills = []string{name}
		if _, err := s.SaveRepositorySkillAuthorization(ctx, invalid, store.BlockBindingAbsentRevision, 7); err == nil {
			t.Errorf("invalid source-qualified name %q accepted", name)
		}
	}
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessRead); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7); err == nil {
		t.Fatal("project reader authorized repository skills")
	}
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserDisabled(ctx, 7, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7); err == nil {
		t.Fatal("disabled user authorized repository skills")
	}
}

func TestRepositorySkillAuthorizationDeletedWithProject(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
	if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	mustProject(t, s, 42)
	got, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || got.Revision != store.BlockBindingAbsentRevision || len(got.Skills) != 0 {
		t.Fatalf("authorization survived project deletion: %+v, %v", got, err)
	}
}

func TestRepositorySkillAuthorizationConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("1", 40), Block: "review", Skills: []string{"repo:one"}, SnapshotSHA256: strings.Repeat("b", 64)}
	created, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for _, name := range []string{"repo:a", "repo:b"} {
		go func() {
			<-start
			write := grant
			write.Skills = []string{name}
			write.TargetSHA = strings.Repeat(strings.TrimPrefix(name, "repo:"), 40)
			_, err := s.SaveRepositorySkillAuthorization(ctx, write, created.Revision, 7)
			results <- err
		}()
	}
	close(start)
	var succeeded, stale int
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrStaleRevision):
			stale++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatalf("concurrent writes success=%d stale=%d", succeeded, stale)
	}
	current, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || len(current.Skills) != 1 || current.TargetSHA != strings.Repeat(strings.TrimPrefix(current.Skills[0], "repo:"), 40) {
		t.Fatalf("concurrent write mixed skills and target SHA: %+v, %v", current, err)
	}
}

func TestRepositorySkillAuthorizationConcurrentGrantAndRevocation(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:one"}, SnapshotSHA256: strings.Repeat("b", 64)}
	created, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for _, revoke := range []bool{true, false} {
		go func() {
			<-start
			write := grant
			if revoke {
				write.Skills = []string{}
			} else {
				write.Skills = []string{"repo:two"}
				write.TargetSHA = strings.Repeat("b", 40)
			}
			_, err := s.SaveRepositorySkillAuthorization(ctx, write, created.Revision, 7)
			results <- err
		}()
	}
	close(start)
	var succeeded, stale int
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrStaleRevision):
			stale++
		default:
			t.Fatal(err)
		}
	}
	stored, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || succeeded != 1 || stale != 1 || stored.Revision == created.Revision {
		t.Fatalf("concurrent grant/revoke success=%d stale=%d stored=%+v err=%v", succeeded, stale, stored, err)
	}
	if !((len(stored.Skills) == 0 && stored.TargetSHA == "") || (reflect.DeepEqual(stored.Skills, []string{"repo:two"}) && stored.TargetSHA == strings.Repeat("b", 40))) {
		t.Fatalf("concurrent grant/revoke mixed state: %+v", stored)
	}
}

func TestRepositorySkillAuthorizationsListByBranch(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	empty, err := s.ListRepositorySkillAuthorizations(ctx, 42, "main")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty list = %+v, %v", empty, err)
	}
	sha := strings.Repeat("a", 40)
	snapshot := strings.Repeat("b", 64)
	reviewGrant, err := s.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{
		ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review",
		Skills: []string{"repo:review"}, SnapshotSHA256: snapshot,
	}, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	verifyGrant, err := s.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{
		ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "verify",
		Skills: []string{"repo:verify"}, SnapshotSHA256: snapshot,
	}, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{
		ProjectID: 42, TargetBranch: "release", TargetSHA: sha, Block: "review",
		Skills: []string{"repo:release"}, SnapshotSHA256: snapshot,
	}, store.BlockBindingAbsentRevision, 7); err != nil {
		t.Fatal(err)
	}
	grants, err := s.ListRepositorySkillAuthorizations(ctx, 42, "main")
	if err != nil || len(grants) != 2 || grants[0].Block != "review" || grants[1].Block != "verify" {
		t.Fatalf("listed grants = %+v, %v", grants, err)
	}
	if grants[0].Revision != reviewGrant.Revision || grants[1].Revision != verifyGrant.Revision || grants[0].TargetSHA != sha || grants[0].SnapshotSHA256 != snapshot {
		t.Fatalf("listed grant identity = %+v", grants)
	}
	for _, branch := range []string{"", " main", "main\x00"} {
		if _, err := s.ListRepositorySkillAuthorizations(ctx, 42, branch); err == nil {
			t.Errorf("invalid branch %q was accepted", branch)
		}
	}
}
