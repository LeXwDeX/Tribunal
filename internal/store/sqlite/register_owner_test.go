package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func ownerTestProject(id int64) store.Project {
	return store.Project{
		ID: id, PathWithNamespace: "group/owned", WebURL: "https://gitlab.example.com/group/owned",
		DefaultBranch: "main", TokenEnc: []byte{1}, WebhookSecretEnc: []byte{2}, TokenScopes: []string{"api"},
	}
}

func TestRegisterProjectWithOwnerGrantsManage(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustAccessUser(t, s, 7, "alice", "admin")
	if _, err := s.RegisterProjectWithOwner(ctx, ownerTestProject(42), 7); err != nil {
		t.Fatalf("RegisterProjectWithOwner: %v", err)
	}
	role, err := s.GetProjectAccess(ctx, 7, 42)
	if err != nil || role != store.ProjectAccessManage {
		t.Fatalf("owner role = %q, %v; want manage", role, err)
	}
}

func TestRegisterProjectWithZeroOwnerWritesNoGrant(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if _, err := s.RegisterProjectWithOwner(ctx, ownerTestProject(42), 0); err != nil {
		t.Fatalf("RegisterProjectWithOwner: %v", err)
	}
	if n := rowCount(t, s, `SELECT COUNT(*) FROM project_grants WHERE project_id = 42`); n != 0 {
		t.Fatalf("grants = %d, want 0", n)
	}
}

// A grant that cannot be written must not leave a half-registered project.
func TestRegisterProjectWithMissingOwnerRollsBack(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if _, err := s.RegisterProjectWithOwner(ctx, ownerTestProject(42), 99); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing owner err = %v, want ErrNotFound", err)
	}
	for table, query := range map[string]string{
		"projects":       `SELECT COUNT(*) FROM projects WHERE id = 42`,
		"workspaces":     `SELECT COUNT(*) FROM workspaces WHERE project_id = 42`,
		"jobs":           `SELECT COUNT(*) FROM jobs`,
		"project_grants": `SELECT COUNT(*) FROM project_grants`,
	} {
		if n := rowCount(t, s, query); n != 0 {
			t.Fatalf("%s rows = %d after rollback, want 0", table, n)
		}
	}
	mustAccessUser(t, s, 99, "late", "admin")
	if _, err := s.RegisterProjectWithOwner(ctx, ownerTestProject(42), 99); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

func TestRegisterProjectWithNegativeOwnerIsRejected(t *testing.T) {
	s := openTestStore(t, newFakeClock())
	if _, err := s.RegisterProjectWithOwner(context.Background(), ownerTestProject(42), -1); err == nil {
		t.Fatal("negative owner accepted")
	}
}

func TestReregisterWithOwnerAfterDeleteGrantsManage(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustAccessUser(t, s, 7, "alice", "admin")
	mustAccessUser(t, s, 8, "bob", "viewer")
	if _, err := s.RegisterProjectWithOwner(ctx, ownerTestProject(42), 7); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := s.SetProjectGrant(ctx, 42, 8, store.ProjectAccessRead); err != nil {
		t.Fatalf("SetProjectGrant: %v", err)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if n := rowCount(t, s, `SELECT COUNT(*) FROM project_grants WHERE project_id = 42`); n != 0 {
		t.Fatalf("grants after delete = %d, want 0", n)
	}
	if _, err := s.RegisterProjectWithOwner(ctx, ownerTestProject(42), 7); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if role, err := s.GetProjectAccess(ctx, 7, 42); err != nil || role != store.ProjectAccessManage {
		t.Fatalf("owner role after re-register = %q, %v", role, err)
	}
	if _, err := s.GetProjectAccess(ctx, 8, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old viewer grant reappeared: %v", err)
	}
}
