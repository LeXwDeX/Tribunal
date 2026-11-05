package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestProfileOverridesReferencesAndRevision(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	m, err := s.CreateModel(ctx, store.Model{DisplayName: "model", Protocol: "openai-chat", Model: "m", Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"explore":{"model":"` + m.ID + `"}}`
	if _, err := s.PutProfileOverrides(ctx, "default", body, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SoftDeleteModel(ctx, m.ID, m.Revision, 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete referenced model: got %v, want conflict", err)
	}
	if _, err := s.PutProfileOverrides(ctx, "default", `{}`, 0, 1); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale update: got %v", err)
	}
	if _, err := s.PutProfileOverrides(ctx, "default", `{"explore":{"model":"user:missing"}}`, 1, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing reference: got %v", err)
	}
	got, err := s.GetProfileOverrides(ctx, "default")
	if err != nil || got.Revision != 1 || got.OverridesJSON == `{}` {
		t.Fatalf("failed write must preserve state: %+v %v", got, err)
	}
	if _, err := s.DeleteProfileOverrides(ctx, "default", 7, 9); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale delete: got %v", err)
	}
	if got, _ := s.GetProfileOverrides(ctx, "default"); got.Revision != 1 || got.OverridesJSON == `{}` {
		t.Fatalf("stale delete changed state: %+v", got)
	}
	reset, err := s.DeleteProfileOverrides(ctx, "default", 1, 9)
	if err != nil || reset.Revision != 2 || reset.UpdatedBy != 9 || reset.OverridesJSON != `{}` {
		t.Fatalf("reset result: %+v %v", reset, err)
	}
	got, err = s.GetProfileOverrides(ctx, "default")
	if err != nil || got.Revision != 2 || got.OverridesJSON != `{}` || got.UpdatedBy != 9 {
		t.Fatalf("reset: %+v %v", got, err)
	}
	if _, err := s.DeleteProfileOverrides(ctx, "missing", 1, 9); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("delete without a row: got %v", err)
	}
	if err := s.SoftDeleteModel(ctx, m.ID, m.Revision, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutProfileOverrides(ctx, "default", body, 2, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted reference: got %v", err)
	}
}

func TestConcurrentProfileOverrideWritesUseRevision(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PutProfileOverrides(ctx, "default", `{}`, 0, 1)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var success, stale int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, store.ErrStaleRevision):
			stale++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success=%d stale=%d, want one of each", success, stale)
	}
}

func TestProfileReferenceAndModelDeleteAreAtomic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "race.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	m, err := first.CreateModel(ctx, store.Model{DisplayName: "model", Protocol: "openai-chat", Model: "m", Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := first.PutProfileOverrides(ctx, "default", `{"explore":{"model":"`+m.ID+`"}}`, 0, 1)
		results <- err
	}()
	go func() {
		<-start
		results <- second.SoftDeleteModel(ctx, m.ID, m.Revision, 1)
	}()
	close(start)
	a, b := <-results, <-results
	if a != nil && b != nil {
		t.Fatalf("both operations failed: %v; %v", a, b)
	}
	var invalid int
	if err := first.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_profile_model_refs r JOIN llm_models m ON m.id=r.model_id WHERE m.deleted_at IS NOT NULL`).Scan(&invalid); err != nil {
		t.Fatal(err)
	}
	if invalid != 0 {
		t.Fatal("override references a deleted model")
	}
}

// A model ID with surrounding whitespace would execute trimmed but be indexed
// raw, leaving the model deletable while a review uses it.
func TestProfileOverridesRejectUntrimmedModelIDs(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	m, err := s.CreateModel(ctx, store.Model{DisplayName: "model", Protocol: "openai-chat", Model: "m", Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutProfileOverrides(ctx, "default", `{"explore":{"model":" `+m.ID+`"}}`, 0, 1); err == nil {
		t.Fatal("an untrimmed model ID was stored")
	}
	if err := s.SoftDeleteModel(ctx, m.ID, m.Revision, 1); err != nil {
		t.Fatalf("nothing references the model: %v", err)
	}
}
