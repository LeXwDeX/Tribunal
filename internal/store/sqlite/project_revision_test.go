package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// TestProjectRevisionFencesStaleSettingsWrites covers #517: a stale settings
// draft must not overwrite the settings another writer saved in between.
func TestProjectRevisionFencesStaleSettingsWrites(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)

	initial, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if initial.Revision != 1 {
		t.Fatalf("new project revision = %d, want 1", initial.Revision)
	}

	saved, err := s.UpdateProject(ctx, 42, store.ProjectUpdate{
		Settings: json.RawMessage(`{"language":"zh-CN"}`),
		Revision: ptrInt64(initial.Revision),
	})
	if err != nil {
		t.Fatalf("conditional update: %v", err)
	}
	if saved.Revision != initial.Revision+1 {
		t.Fatalf("saved revision = %d, want %d", saved.Revision, initial.Revision+1)
	}

	_, err = s.UpdateProject(ctx, 42, store.ProjectUpdate{
		Settings: json.RawMessage(`{"ignore":["docs/**"]}`),
		Revision: ptrInt64(initial.Revision),
	})
	if !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale update error = %v, want ErrStaleRevision", err)
	}

	current, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject after stale write: %v", err)
	}
	if current.Revision != saved.Revision {
		t.Fatalf("revision after stale write = %d, want %d", current.Revision, saved.Revision)
	}
	if string(current.Settings) != `{"language":"zh-CN"}` {
		t.Fatalf("stale write overwrote settings: %s", current.Settings)
	}
}

// TestProjectUpdateWithoutRevisionKeepsLegacySemantics keeps API-token scripts
// working: omitting revision still writes, but the row revision advances so
// clients that do hold a revision notice the change.
func TestProjectUpdateWithoutRevisionKeepsLegacySemantics(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)

	updated, err := s.UpdateProject(ctx, 42, store.ProjectUpdate{Enabled: ptrBool(false)})
	if err != nil {
		t.Fatalf("update without revision: %v", err)
	}
	if updated.Enabled {
		t.Fatal("project enabled = true, want false")
	}
	if updated.Revision != 2 {
		t.Fatalf("revision = %d, want 2", updated.Revision)
	}

	if _, err := s.UpdateProject(ctx, 42, store.ProjectUpdate{
		Enabled:  ptrBool(true),
		Revision: ptrInt64(1),
	}); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("update with pre-write revision error = %v, want ErrStaleRevision", err)
	}
}

// TestProjectRevisionConcurrentWritersSerialize starts two writers from the
// same revision; exactly one may win and the loser must see a conflict.
func TestProjectRevisionConcurrentWritersSerialize(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)

	initial, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for writer := 0; writer < 2; writer++ {
		settings := json.RawMessage(fmt.Sprintf(`{"custom_rules":["writer-%d"]}`, writer))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.UpdateProject(ctx, 42, store.ProjectUpdate{
				Settings: settings,
				Revision: ptrInt64(initial.Revision),
			})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var succeeded, conflicted int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrStaleRevision):
			conflicted++
		default:
			t.Fatalf("concurrent update error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded = %d, conflicted = %d, want 1 and 1", succeeded, conflicted)
	}
	final, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject final: %v", err)
	}
	if final.Revision != initial.Revision+1 {
		t.Fatalf("final revision = %d, want %d", final.Revision, initial.Revision+1)
	}
}
