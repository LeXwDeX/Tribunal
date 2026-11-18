package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// mirrorCreatingGitOps mimics repo.EnsureMirror: it creates the mirror
// directory, and with it the project root, through os.MkdirAll before any
// workspace operation runs.
type mirrorCreatingGitOps struct {
	*fakeGitOps
}

func (g *mirrorCreatingGitOps) EnsureMirror(_ context.Context, dir, _ string, _ repo.Auth) error {
	return os.MkdirAll(dir, 0o700)
}

func assertManagedMarker(t *testing.T, manager *workspace.Manager, projectID int64) {
	t.Helper()
	ws := manager.Get(projectID)
	if ws == nil {
		t.Fatalf("workspace %d is unavailable", projectID)
	}
	data, err := os.ReadFile(filepath.Join(ws.Root, workspace.ManagedMarker))
	if err != nil {
		t.Fatalf("managed marker missing for project %d: %v", projectID, err)
	}
	if got, want := string(data), workspace.ManagedMarkerValue(projectID)+"\n"; got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
}

// TestOnboardWritesManagedWorkspaceMarker is the #547 regression: onboarding
// created the project root through the Git mirror first, so ensureDir never
// wrote the managed marker and reclaim/residue cleanup could not attribute
// the directory.
func TestOnboardWritesManagedWorkspaceMarker(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	svc := newOnboardingContextService(t, f, validOnboardingContext)
	svc.git = &mirrorCreatingGitOps{fakeGitOps: f.git}
	if err := svc.Onboard(ctx, 42); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	assertManagedMarker(t, f.manager, 42)
}

// TestPrepareRunWritesManagedWorkspaceMarker covers the second creation path:
// a review preparing a run before any onboarding created the root itself.
func TestPrepareRunWritesManagedWorkspaceMarker(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	consumerReadLock(t, f.manager, 42)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "job-managed-marker", f.head)
	if err != nil {
		t.Fatalf("prepareRun: %v", err)
	}
	defer cleanup()
	if dir == "" {
		t.Fatal("prepareRun returned an empty run directory")
	}
	assertManagedMarker(t, f.manager, 42)
}
