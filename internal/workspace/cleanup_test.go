package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
)

func TestValidRunID(t *testing.T) {
	for _, id := range []string{"workflow-1", "A0", "a_b-c", strings.Repeat("a", 128)} {
		if !ValidRunID(id) {
			t.Errorf("ValidRunID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "../evil", "a/b", "a b", "-leading", strings.Repeat("a", 129)} {
		if ValidRunID(id) {
			t.Errorf("ValidRunID(%q) = true", id)
		}
	}
}

func TestRegisteredRunsReturnsSnapshot(t *testing.T) {
	f := newFixture(t)
	sha := f.commit(t, "a.txt", "one\n", "first")
	f.publish(t)

	ctx := context.Background()
	ws := NewManager(f.root, f.git).Get(31)
	if _, err := ws.Refresh(ctx, f.remoteURL(), "main", repo.Auth{}); err != nil {
		t.Fatal(err)
	}
	if runs := ws.RegisteredRuns(); len(runs) != 0 {
		t.Fatalf("initial runs = %v, want empty", runs)
	}
	dir, cleanup, err := ws.NewRun(ctx, "workflow-31", sha)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	runs := ws.RegisteredRuns()
	if runs["workflow-31"] != dir {
		t.Fatalf("runs = %v, want workflow-31 -> %s", runs, dir)
	}
	runs["workflow-31"] = "mutated"
	if got := ws.RegisteredRuns()["workflow-31"]; got != dir {
		t.Fatalf("snapshot mutation reached the registry: %q", got)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if runs := ws.RegisteredRuns(); len(runs) != 0 {
		t.Fatalf("runs after cleanup = %v, want empty", runs)
	}
}

func TestNewRunDoesNotRegisterFailedAdd(t *testing.T) {
	ctx := context.Background()
	ws := NewManager(t.TempDir(), &repo.Git{}).Get(32)
	sha := strings.Repeat("a", 40)
	if _, _, err := ws.NewRun(ctx, "workflow-missing", sha); err == nil {
		t.Fatal("NewRun with a missing mirror returned nil error")
	}
	if runs := ws.RegisteredRuns(); len(runs) != 0 {
		t.Fatalf("failed NewRun left registrations: %v", runs)
	}
}
