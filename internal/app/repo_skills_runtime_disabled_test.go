package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// Storage-level writes deliberately bypass the disabled HTTP routes. Once the
// runtime accepts grants, an unverifiable grant must fail closed before a
// worktree, model call or publication.
func TestDisabledRepositorySkillsDoNotEscapeAfterPolicyWrites(t *testing.T) {
	for _, change := range []string{"ignore", "revoke", "cancel"} {
		t.Run(change, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			projects, err := NewProjectService(ProjectServiceConfig{Store: f.db, GitLab: defaultFakeGitLab(), Cipher: f.service.cipher, Profiles: f.profiles, Workspace: f.manager})
			if err != nil {
				t.Fatal(err)
			}
			initial := json.RawMessage(`{"allow_repo_skills":true,"ignore":[]}`)
			if _, err := projects.Update(ctx, 42, UpdateProjectInput{Settings: &initial}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if err := f.db.CreateSession(ctx, store.Session{IDHash: []byte(strings.Repeat("x", 32)), UserID: 7, Username: "admin", Name: "admin", Role: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, store.SessionLimits{}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
				t.Fatal(err)
			}
			grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "code-review", Skills: []string{"repo:demo"}, SnapshotSHA256: strings.Repeat("b", 64)}
			granted, err := f.db.SaveRepositorySkillAuthorization(ctx, grant, store.BlockBindingAbsentRevision, 7)
			if err != nil {
				t.Fatal(err)
			}
			job := f.seedReview(t, "repo-skills-disabled")
			if err := f.service.RunReview(ctx, job); err == nil {
				t.Fatal("storage-level repository grant reached execution")
			}
			switch change {
			case "ignore":
				updated := json.RawMessage(`{"allow_repo_skills":false,"ignore":[".claude/skills/**"]}`)
				if _, err := projects.Update(ctx, 42, UpdateProjectInput{Settings: &updated}); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				grant.Skills = []string{}
				if revoked, err := f.db.SaveRepositorySkillAuthorization(ctx, grant, granted.Revision, 7); err != nil || len(revoked.Skills) != 0 {
					t.Fatalf("storage revocation = %+v, %v", revoked, err)
				}
			case "cancel":
				cancel()
			}
			if f.runner.count() != 0 {
				t.Fatal("repository content reached a model")
			}
			f.gitlab.mu.Lock()
			published, err := json.Marshal([]any{f.gitlab.notes, f.gitlab.discussions, f.gitlab.statuses})
			f.gitlab.mu.Unlock()
			if err != nil || strings.Contains(string(published), "CANARY_") {
				t.Fatalf("repository content reached publication: %s, %v", published, err)
			}
			f.git.mu.Lock()
			remaining := len(f.git.worktrees)
			f.git.mu.Unlock()
			if remaining != 0 {
				t.Fatalf("review worktree cleanup: remaining=%d", remaining)
			}
		})
	}
}
