package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRepositorySkillAuthorizationRequiresCanonicalSnapshotHash(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	sha := strings.Repeat("a", 40)
	hash := strings.Repeat("b", 64)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{"repo:safe"}}
	valid := grant
	valid.SnapshotSHA256 = hash
	for _, snapshot := range []string{"", "bad", strings.Repeat("B", 64), strings.Repeat("b", 63), strings.Repeat("b", 65), " " + hash, strings.Repeat("g", 64)} {
		invalid := valid
		invalid.SnapshotSHA256 = snapshot
		if _, err := s.SaveRepositorySkillAuthorization(ctx, invalid, store.BlockBindingAbsentRevision, 7); err == nil {
			t.Errorf("invalid snapshot hash %q accepted", snapshot)
		}
	}
	created, err := s.SaveRepositorySkillAuthorization(ctx, valid, store.BlockBindingAbsentRevision, 7)
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
		if fields["snapshot_sha256"] != hash {
			t.Errorf("snapshot hash not persisted: %s", encoded)
		}
	}
}

func TestRepositorySkillAuthorizationLegacySnapshotReadAndRevoke(t *testing.T) {
	ctx := context.Background()
	s := repositorySkillStore(t)
	sha := strings.Repeat("a", 40)
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
	created, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	// A database written before 0023 retains its grant, target and revision but
	// has no approved snapshot. It must stay readable so an administrator can
	// inspect and revoke it.
	if _, err := s.db.ExecContext(ctx, `UPDATE repository_skill_authorizations SET snapshot_sha256 = '' WHERE project_id = 42`); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.TargetSHA != sha || legacy.Revision != created.Revision || legacy.SnapshotSHA256 != "" || len(legacy.Skills) != 1 {
		t.Fatalf("legacy grant = %+v", legacy)
	}
	replacement := grant
	replacement.SnapshotSHA256 = ""
	if _, err := s.SaveRepositorySkillAuthorization(ctx, replacement, legacy.Revision, 7); err == nil {
		t.Fatal("legacy grant without a snapshot was re-authorized for execution")
	}
	revoked, err := s.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{}}, legacy.Revision, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked.Skills) != 0 || revoked.TargetSHA != "" || revoked.SnapshotSHA256 != "" || revoked.Revision == legacy.Revision {
		t.Fatalf("legacy revoke = %+v", revoked)
	}
	stored, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || len(stored.Skills) != 0 || stored.TargetSHA != "" || stored.SnapshotSHA256 != "" || stored.Revision != revoked.Revision {
		t.Fatalf("legacy revoke not persisted: %+v, %v", stored, err)
	}
}

func TestRepositorySkillSnapshotMigrationPreservesAndInvalidatesLegacyGrant(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-0021.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustProject(t, s, 42)
	mustAccessUser(t, s, 7, "alice", "admin")
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
	created, err := s.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`ALTER TABLE repository_skill_authorizations DROP COLUMN snapshot_sha256;
		DELETE FROM schema_migrations WHERE version = 23`); err != nil {
		t.Fatalf("recreate 0021 schema: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations WHERE version = 23`); got != 1 {
		t.Fatalf("migration 0023 recorded %d times", got)
	}
	migrated, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil {
		t.Fatal(err)
	}
	if migrated.TargetSHA != grant.TargetSHA || migrated.Revision != created.Revision || migrated.SnapshotSHA256 != "" || len(migrated.Skills) != 1 {
		t.Fatalf("migrated legacy grant = %+v", migrated)
	}
	unauthorized := grant
	unauthorized.SnapshotSHA256 = ""
	if _, err := s.SaveRepositorySkillAuthorization(ctx, unauthorized, migrated.Revision, 7); err == nil {
		t.Fatal("migrated legacy grant reused without a snapshot")
	}
	revoked, err := s.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{}}, migrated.Revision, 7)
	if err != nil || len(revoked.Skills) != 0 || revoked.SnapshotSHA256 != "" {
		t.Fatalf("migrated legacy revoke = %+v, %v", revoked, err)
	}
	stored, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
	if err != nil || len(stored.Skills) != 0 || stored.TargetSHA != "" || stored.SnapshotSHA256 != "" || stored.Revision != revoked.Revision {
		t.Fatalf("migrated legacy revoke not persisted: %+v, %v", stored, err)
	}
}

func TestRepositorySkillLegacyAndCorruptSnapshotStates(t *testing.T) {
	for _, tc := range []struct {
		name, skills, sha, snapshot string
		ok                          bool
	}{
		{"legacy-empty", `["repo:safe"]`, strings.Repeat("a", 40), "", true},
		{"invalid-snapshot", `["repo:safe"]`, strings.Repeat("a", 40), "bad", false},
		{"uppercase-snapshot", `["repo:safe"]`, strings.Repeat("a", 40), strings.Repeat("B", 64), false},
		{"revoked-with-snapshot", `[]`, "", strings.Repeat("b", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := repositorySkillStore(t)
			if _, err := s.db.ExecContext(ctx, `INSERT INTO repository_skill_authorizations
				(project_id, target_branch, block, skills, target_sha, snapshot_sha256, revision)
				VALUES (42, 'main', 'review', ?, ?, ?, ?)`, tc.skills, tc.sha, tc.snapshot, strings.Repeat("c", 64)); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
			if (err == nil) != tc.ok {
				t.Fatalf("read = %+v, %v; want ok=%v", got, err, tc.ok)
			}
			if !tc.ok {
				return
			}
			if got.SnapshotSHA256 != tc.snapshot || got.TargetSHA != tc.sha {
				t.Fatalf("legacy read changed fields: %+v", got)
			}
			if _, err := s.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:safe"}}, got.Revision, 7); err == nil {
				t.Fatal("legacy grant was replaced without an approved snapshot")
			}
		})
	}
}
