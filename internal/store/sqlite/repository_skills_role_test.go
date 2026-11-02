package sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRepositorySkillWriteRequiresCurrentPlatformAdmin(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "grant"
		if revoke {
			name = "revoke"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := repositorySkillStore(t)
			grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), Block: "review", Skills: []string{"repo:safe"}, SnapshotSHA256: strings.Repeat("b", 64)}
			expected := store.BlockBindingAbsentRevision
			if revoke {
				created, err := s.SaveRepositorySkillAuthorization(ctx, grant, expected, 7)
				if err != nil {
					t.Fatal(err)
				}
				expected = created.Revision
				grant.Skills = []string{}
			}
			before, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
			if err != nil {
				t.Fatal(err)
			}
			// The actor retains project manage access but loses the platform role
			// after the HTTP request's initial authorization check.
			if err := s.SetPlatformRole(ctx, 7, "viewer"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveRepositorySkillAuthorization(ctx, grant, expected, 7); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("write by former platform admin = %v", err)
			}
			after, err := s.GetRepositorySkillAuthorization(ctx, 42, "main", "review")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("authorization changed: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}
