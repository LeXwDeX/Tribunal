package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func newResidueTestService(t *testing.T, root string) (*ProjectService, *sqlite.Store) {
	t.Helper()
	st := openTestStore(t)
	service, err := NewProjectService(ProjectServiceConfig{
		Store:     st,
		GitLab:    defaultFakeGitLab(),
		Cipher:    newTestCipher(t),
		Profiles:  testProfiles(),
		Workspace: workspace.NewManager(root, nil),
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service, st
}

func TestListProjectResiduesReportsOrphansWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	service, st := newResidueTestService(t, root)
	seedProject(t, st, newTestCipher(t), 7, "secret", "{}", "default")

	orphanWorkspace := filepath.Join(root, "42", "mirror.git")
	orphanSkills := filepath.Join(root, "skills", "project-42")
	unknown := filepath.Join(root, "unowned")
	active := filepath.Join(root, "7", "mirror.git")
	platformSkills := filepath.Join(root, "skills", "platform")
	for _, dir := range []string{orphanWorkspace, orphanSkills, unknown, active, platformSkills} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(orphanWorkspace, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	residues, err := service.ListProjectResidues(ctx)
	if err != nil {
		t.Fatalf("ListProjectResidues: %v", err)
	}
	if len(residues) != 3 {
		t.Fatalf("residues = %+v, want 3 entries", residues)
	}
	want := map[string]struct {
		id        int64
		kind      string
		removable bool
	}{
		filepath.Join(root, "42"):                   {42, ProjectResidueWorkspace, true},
		filepath.Join(root, "skills", "project-42"): {42, ProjectResidueManagedSkills, false},
		unknown: {0, ProjectResidueUnknown, false},
	}
	for _, residue := range residues {
		expected, ok := want[residue.Path]
		if !ok {
			t.Errorf("unexpected residue %+v", residue)
			continue
		}
		if residue.ProjectID != expected.id || residue.Kind != expected.kind || residue.Removable != expected.removable {
			t.Errorf("residue %s = %+v, want id=%d kind=%s removable=%v",
				residue.Path, residue, expected.id, expected.kind, expected.removable)
		}
	}
	for _, dir := range []string{orphanWorkspace, orphanSkills, unknown, active, platformSkills} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("list removed %s: %v", dir, err)
		}
	}
}

func TestCleanupProjectResiduesKeepsUnknownOwnership(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	service, st := newResidueTestService(t, root)
	seedProject(t, st, newTestCipher(t), 7, "secret", "{}", "default")

	orphanWorkspace := filepath.Join(root, "42", "mirror.git")
	orphanSkills := filepath.Join(root, "skills", "project-42")
	unknown := filepath.Join(root, "unowned")
	active := filepath.Join(root, "7", "mirror.git")
	platformSkills := filepath.Join(root, "skills", "platform")
	for _, dir := range []string{orphanWorkspace, orphanSkills, unknown, active, platformSkills} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(orphanWorkspace, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := service.CleanupProjectResidues(ctx)
	if err != nil {
		t.Fatalf("CleanupProjectResidues: %v", err)
	}
	if len(result.Removed) != 1 || len(result.Kept) != 2 || len(result.Failed) != 0 {
		t.Fatalf("result = %+v, want 1 removed, 2 kept, 0 failed", result)
	}
	if result.Removed[0].Path != filepath.Join(root, "42") {
		t.Fatalf("removed = %+v", result.Removed)
	}
	for _, path := range []string{unknown, filepath.Join(root, "skills", "project-42")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unverified residue was removed: %s: %v", path, err)
		}
	}
	for _, path := range []string{unknown, filepath.Join(root, "7"), platformSkills, filepath.Join(root, "skills", "project-42")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}

	again, err := service.CleanupProjectResidues(ctx)
	if err != nil {
		t.Fatalf("second CleanupProjectResidues: %v", err)
	}
	if len(again.Removed) != 0 || len(again.Kept) != 2 || len(again.Failed) != 0 {
		t.Fatalf("second result = %+v", again)
	}
}

func TestProjectResiduesLeaveSymlinksAlone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	service, _ := newResidueTestService(t, root)
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "42")); err != nil {
		t.Fatal(err)
	}

	residues, err := service.ListProjectResidues(ctx)
	if err != nil {
		t.Fatalf("ListProjectResidues: %v", err)
	}
	if len(residues) != 1 || residues[0].Kind != ProjectResidueUnknown || residues[0].Removable {
		t.Fatalf("residues = %+v", residues)
	}
	if _, err := service.CleanupProjectResidues(ctx); err != nil {
		t.Fatalf("CleanupProjectResidues: %v", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("symlink target was removed: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(root, "42")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was changed: %v", err)
	}
}

func TestListProjectResiduesKeepsUnverifiedWorkspace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	service, st := newResidueTestService(t, root)
	seedProject(t, st, newTestCipher(t), 7, "secret", "{}", "default")
	manual := filepath.Join(root, "43", "manual-backup")
	if err := os.MkdirAll(manual, 0o700); err != nil {
		t.Fatal(err)
	}

	residues, err := service.ListProjectResidues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(residues) != 1 || residues[0].Path != filepath.Join(root, "43") || residues[0].Removable || residues[0].Reason != projectResidueUnknownOwner {
		t.Fatalf("residues = %+v, want an unverified workspace residue", residues)
	}
	result, err := service.CleanupProjectResidues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 0 || len(result.Kept) != 1 {
		t.Fatalf("cleanup = %+v, want the unverified directory kept", result)
	}
	if _, err := os.Stat(filepath.Join(root, "43")); err != nil {
		t.Fatalf("manual directory was removed: %v", err)
	}
}
