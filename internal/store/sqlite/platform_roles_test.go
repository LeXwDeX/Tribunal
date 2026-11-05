package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func TestPlatformRolesUpgradeFromVersion24(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if version > 24 {
			continue
		}
		body, err := migrations.FS.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?, ?, ?)`, version, entry.Name(), "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, name, role) VALUES (7, 'alice', 'Alice', 'admin')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SyncPlatformRoles(context.Background(), []int64{7}, nil); err != nil {
		t.Fatal(err)
	}
	users, _, err := s.ListPlatformUsers(context.Background(), "", 20, []int64{7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Role != "admin" || users[0].RoleSource != "config" || users[0].Revision != 1 {
		t.Fatalf("upgraded users = %+v", users)
	}
	if _, err := s.db.Exec(`UPDATE users SET granted_role='viewer' WHERE id=7`); err == nil {
		t.Fatal("invalid granted role accepted")
	}
}

func TestGrantedRoleSurvivesRenameAndTokenCreationDoesNotRestoreDemotion(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	if err := s.CreateSession(ctx, sessionFor("first", 8, "before", clock.Now()), store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	role, rev := "admin", int64(1)
	if _, _, err := s.UpdatePlatformUser(ctx, 8, store.PlatformUserUpdate{Role: &role, Revision: &rev}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, sessionFor("renamed", 8, "after", clock.Now()), store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.GrantedPlatformAdmin(ctx, 8); err != nil || !ok {
		t.Fatalf("grant after rename: %v %v", ok, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM users WHERE id=8 AND role='admin' AND username='after'`); got != 1 {
		t.Fatalf("renamed admin count=%d", got)
	}
	// A token request carrying a role computed before demotion must not rewrite
	// the current role mirror after the update commits.
	if _, err := s.db.Exec(`UPDATE users SET granted_role=NULL, role='viewer' WHERE id=8`); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("test-token"))
	if _, err := s.CreateAPIToken(ctx, store.TokenUser{ID: 8, Username: "after", Role: "admin"}, store.APIToken{Name: "test", TokenHash: hash[:], CreatedAt: clock.Now().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM users WHERE id=8 AND role='viewer'`); got != 1 {
		t.Fatalf("token creation restored admin role: %d", got)
	}
}

func TestConcurrentDemotionsKeepAnEffectiveAdmin(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	for _, id := range []int64{7, 8} {
		if err := s.CreateSession(ctx, sessionFor("concurrent-"+string(rune('0'+id)), id, "user", clock.Now()), store.SessionLimits{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE users SET granted_role='admin', role='admin' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	role := "viewer"
	revision := int64(1)
	errs := make(chan error, 2)
	for _, id := range []int64{7, 8} {
		go func(id int64) {
			_, _, err := s.UpdatePlatformUser(ctx, id, store.PlatformUserUpdate{Role: &role, Revision: &revision})
			errs <- err
		}(id)
	}
	var succeeded, protected int
	for range 2 {
		err := <-errs
		if err == nil {
			succeeded++
		} else if errors.Is(err, store.ErrPlatformUserProtected) {
			protected++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || protected != 1 {
		t.Fatalf("success=%d protected=%d", succeeded, protected)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM users WHERE role='admin' AND disabled=0`); got != 1 {
		t.Fatalf("effective admins=%d", got)
	}
}

func TestPlatformUserRevisionAndActorAreCheckedInsideTransaction(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	for _, id := range []int64{7, 8} {
		if err := s.CreateSession(ctx, sessionFor("revision-"+string(rune('0'+id)), id, "user", clock.Now()), store.SessionLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	role, revision := "admin", int64(1)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := s.UpdatePlatformUser(ctx, 7, store.PlatformUserUpdate{Role: &role, Revision: &revision})
			errs <- err
		}()
	}
	var successes, stale int
	for range 2 {
		err := <-errs
		if err == nil {
			successes++
		} else if errors.Is(err, store.ErrStaleRevision) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("success=%d stale=%d", successes, stale)
	}
	newRevision := int64(2)
	viewer := "viewer"
	_, _, err := s.UpdatePlatformUser(ctx, 7, store.PlatformUserUpdate{Role: &viewer, Revision: &newRevision, ActorID: 8})
	if !errors.Is(err, store.ErrPlatformUserProtected) {
		t.Fatalf("demoted actor: error=%v", err)
	}
}

func TestConfiguredAdminRoleWritesAreProtected(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if err := s.CreateSession(ctx, sessionFor("configured-role", 7, "user", newFakeClock().Now()), store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncPlatformRoles(ctx, []int64{7}, nil); err != nil {
		t.Fatal(err)
	}
	role := "admin"
	user, err := s.GetUser(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	revision := user.Revision
	_, _, err = s.UpdatePlatformUser(ctx, 7, store.PlatformUserUpdate{Role: &role, Revision: &revision, AdminIDs: []int64{7}})
	if !errors.Is(err, store.ErrPlatformUserProtected) {
		t.Fatalf("configured admin role write: error=%v, want protected", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM users WHERE id=7 AND granted_role IS NOT NULL`); got != 0 {
		t.Fatalf("configured admin received a durable grant: %d", got)
	}
}
