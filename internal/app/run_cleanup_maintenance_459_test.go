package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type maintenanceFake struct {
	refs      []repo.MergeRequestRef
	deleted   []repo.MergeRequestRef
	gcCalls   int
	refsErr   error
	deleteErr error
	gcErr     error
}

func (m *maintenanceFake) MergeRequestRefs(context.Context, string) ([]repo.MergeRequestRef, error) {
	return append([]repo.MergeRequestRef(nil), m.refs...), m.refsErr
}

func (m *maintenanceFake) DeleteMergeRequestRefs(_ context.Context, _ string, refs []repo.MergeRequestRef) (int, error) {
	m.deleted = append(m.deleted, refs...)
	return len(refs), m.deleteErr
}

func (m *maintenanceFake) GC(context.Context, string) error {
	m.gcCalls++
	return m.gcErr
}

func newMaintenanceFixture(t *testing.T) (*sqlite.Store, *workspace.Manager, *maintenanceFake) {
	t.Helper()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	job, err := st.Claim(context.Background(), "maintenance-test", []string{"workspace.onboard"}, time.Minute)
	if err != nil {
		t.Fatalf("claim onboarding: %v", err)
	}
	if job != nil {
		if err := st.Complete(context.Background(), *job); err != nil {
			t.Fatalf("complete onboarding: %v", err)
		}
	}
	return st, workspace.NewManager(t.TempDir(), nil), &maintenanceFake{}
}

func writeManagedWorkspace(t *testing.T, manager *workspace.Manager, projectID int64) *workspace.Workspace {
	t.Helper()
	ws := manager.Get(projectID)
	if err := os.MkdirAll(ws.MirrorDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root, workspace.ManagedMarker),
		[]byte(workspace.ManagedMarkerValue(projectID)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

func terminalStatus(t *testing.T, st *sqlite.Store, mrIID int64) {
	t.Helper()
	ctx := context.Background()
	result, err := st.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: mrIID, HeadSHA: sha40})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: mrIID, Generation: result.Generation, HeadSHA: sha40, State: store.ReviewStatusPass,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := st.GetReviewStatus(ctx, 42, mrIID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkReviewStatusApplied(ctx, 42, mrIID, int64(row.Version), 1, "hash", ""); err != nil {
		t.Fatal(err)
	}
}

// TestRunCleanupPrunesOnlyUnprotectedMergeRequestRefs keeps refs for reviews
// that have not reached a terminal state (#459).
func TestRunCleanupPrunesOnlyUnprotectedMergeRequestRefs(t *testing.T) {
	ctx := context.Background()
	st, manager, maintenance := newMaintenanceFixture(t)
	ws := writeManagedWorkspace(t, manager, 42)

	// MR 7 stays queued; MR 8 is terminal.
	if _, err := st.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40}); err != nil {
		t.Fatal(err)
	}
	terminalStatus(t, st, 8)

	maintenance.refs = []repo.MergeRequestRef{
		{IID: 7, Name: "refs/merge-requests/7/head"},
		{IID: 8, Name: "refs/merge-requests/8/head"},
	}
	service, err := NewRunCleanupService(RunCleanupConfig{
		Store:       st,
		Workspace:   manager,
		Git:         &cleanupTestGit{},
		Maintenance: maintenance,
		After:       time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(maintenance.deleted) != 1 || maintenance.deleted[0].IID != 8 {
		t.Fatalf("deleted refs = %+v, want only terminal MR 8", maintenance.deleted)
	}
	if maintenance.gcCalls != 1 {
		t.Fatalf("gc calls = %d, want 1", maintenance.gcCalls)
	}
	if _, err := os.Stat(ws.MirrorDir()); err != nil {
		t.Fatalf("mirror removed: %v", err)
	}
}

// TestRunCleanupReclaimsOnlyInactiveUnprotectedWorkspace keeps active work and
// missing ownership metadata.
func TestRunCleanupReclaimsOnlyInactiveUnprotectedWorkspace(t *testing.T) {
	ctx := context.Background()

	t.Run("inactive workspace is reclaimed", func(t *testing.T) {
		st, manager, _ := newMaintenanceFixture(t)
		ws := writeManagedWorkspace(t, manager, 42)
		service, err := NewRunCleanupService(RunCleanupConfig{
			Store: st, Workspace: manager, Git: &cleanupTestGit{},
			After: time.Hour, ReclaimAfter: 24 * time.Hour,
			Now: func() time.Time { return time.Now().Add(48 * time.Hour) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if _, err := os.Stat(ws.Root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inactive workspace still exists: %v", err)
		}
	})

	t.Run("active review is protected", func(t *testing.T) {
		st, manager, _ := newMaintenanceFixture(t)
		ws := writeManagedWorkspace(t, manager, 42)
		if _, err := st.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40}); err != nil {
			t.Fatal(err)
		}
		service, err := NewRunCleanupService(RunCleanupConfig{
			Store: st, Workspace: manager, Git: &cleanupTestGit{},
			After: time.Hour, ReclaimAfter: 24 * time.Hour,
			Now: func() time.Time { return time.Now().Add(48 * time.Hour) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if _, err := os.Stat(ws.Root); err != nil {
			t.Fatalf("protected workspace removed: %v", err)
		}
	})

	t.Run("missing ownership marker is kept", func(t *testing.T) {
		st, manager, _ := newMaintenanceFixture(t)
		ws := manager.Get(42)
		if err := os.MkdirAll(ws.MirrorDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		service, err := NewRunCleanupService(RunCleanupConfig{
			Store: st, Workspace: manager, Git: &cleanupTestGit{},
			After: time.Hour, ReclaimAfter: 24 * time.Hour,
			Now: func() time.Time { return time.Now().Add(48 * time.Hour) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if _, err := os.Stat(ws.Root); err != nil {
			t.Fatalf("unowned workspace removed: %v", err)
		}
	})

	t.Run("foreign ownership marker is kept", func(t *testing.T) {
		st, manager, _ := newMaintenanceFixture(t)
		ws := writeManagedWorkspace(t, manager, 42)
		if err := os.WriteFile(filepath.Join(ws.Root, workspace.ManagedMarker),
			[]byte(workspace.ManagedMarkerValue(99)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		service, err := NewRunCleanupService(RunCleanupConfig{
			Store: st, Workspace: manager, Git: &cleanupTestGit{},
			After: time.Hour, ReclaimAfter: 24 * time.Hour,
			Now: func() time.Time { return time.Now().Add(48 * time.Hour) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if _, err := os.Stat(ws.Root); err != nil {
			t.Fatalf("foreign workspace removed: %v", err)
		}
	})
}

// TestRunCleanupSkipsReclaimWhenProtectionQueryFails is the fail-closed branch:
// a failing protection query keeps the directory and reports the error.
func TestRunCleanupSkipsReclaimWhenProtectionQueryFails(t *testing.T) {
	root := t.TempDir()
	manager := workspace.NewManager(root, nil)
	ws := manager.Get(7)
	if err := os.MkdirAll(ws.MirrorDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root, workspace.ManagedMarker),
		[]byte(workspace.ManagedMarkerValue(7)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &failingReclaimStore{cleanupTestStore: &cleanupTestStore{
		projects: []int64{7}, protected: map[string]struct{}{},
		protectedContext: map[int64]map[string]struct{}{}, contextErr: map[int64]error{},
	}}
	service, err := NewRunCleanupService(RunCleanupConfig{
		Store: st, Workspace: manager, Git: &cleanupTestGit{},
		After: time.Hour, ReclaimAfter: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = service.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce ignored the protection query failure")
	}
	if _, statErr := os.Stat(ws.Root); statErr != nil {
		t.Fatalf("workspace removed after protection failure: %v", statErr)
	}
}

type failingReclaimStore struct {
	*cleanupTestStore
}

func (s *failingReclaimStore) GetProject(_ context.Context, id int64) (*store.Project, error) {
	return &store.Project{ID: id, Enabled: true}, nil
}

func (s *failingReclaimStore) WorkspaceReclaimStatus(context.Context, int64) (store.WorkspaceReclaimStatus, error) {
	return store.WorkspaceReclaimStatus{}, errors.New("protection query failed")
}

func (s *failingReclaimStore) ProtectedMergeRequestIIDs(context.Context, int64) (map[int64]struct{}, error) {
	return nil, errors.New("protection query failed")
}

// TestRunCleanupMeasuresAndAlertsCapacity exposes the cap through the store.
func TestRunCleanupMeasuresAndAlertsCapacity(t *testing.T) {
	ctx := context.Background()
	st, manager, _ := newMaintenanceFixture(t)
	ws := writeManagedWorkspace(t, manager, 42)
	big := filepath.Join(ws.Root, "payload.bin")
	if err := os.WriteFile(big, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewRunCleanupService(RunCleanupConfig{
		Store: st, Workspace: manager, Git: &cleanupTestGit{},
		After: time.Hour, MaxBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	capacity, err := st.WorkspaceCapacity(ctx)
	if err != nil || capacity == nil {
		t.Fatalf("WorkspaceCapacity = %+v, %v", capacity, err)
	}
	if capacity.TotalBytes < 4096 || capacity.LimitBytes != 1024 || !capacity.OverLimit {
		t.Fatalf("capacity = %+v", capacity)
	}
}
