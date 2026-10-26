package sqlite

import (
	"context"
	"testing"
)

func TestBindAdminUsernameKeepsFirstOwner(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)

	owner, err := s.BindAdminUsername(ctx, "Alice", 7, clock.Now())
	if err != nil {
		t.Fatalf("BindAdminUsername(alice, 7): %v", err)
	}
	if owner != 7 {
		t.Fatalf("first binding owner = %d, want 7", owner)
	}
	// A different GitLab account that later takes the same username must not
	// replace the binding. Usernames compare case-insensitively.
	owner, err = s.BindAdminUsername(ctx, " alice ", 99, clock.Now())
	if err != nil {
		t.Fatalf("BindAdminUsername(alice, 99): %v", err)
	}
	if owner != 7 {
		t.Fatalf("binding owner after takeover attempt = %d, want 7", owner)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM admin_bindings`); got != 1 {
		t.Fatalf("admin_bindings rows = %d, want 1", got)
	}
	owner, err = s.BindAdminUsername(ctx, "alice", 7, clock.Now())
	if err != nil || owner != 7 {
		t.Fatalf("rebinding the owner = %d, %v", owner, err)
	}
}

func TestBindAdminUsernameValidation(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	if _, err := s.BindAdminUsername(ctx, "  ", 7, clock.Now()); err == nil {
		t.Error("empty username was accepted")
	}
	if _, err := s.BindAdminUsername(ctx, "alice", 0, clock.Now()); err == nil {
		t.Error("non-positive user id was accepted")
	}
}
