package app

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

type countingSelfHealStore struct {
	ReviewStore
	mu    sync.Mutex
	calls int
}

func (s *countingSelfHealStore) ListProjects(context.Context) ([]store.Project, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return nil, nil
}

func (s *countingSelfHealStore) ListAppliedReviewStatusesForSelfHeal(context.Context, int64, int64, time.Time) ([]store.ReviewStatus, error) {
	return nil, nil
}

func (s *countingSelfHealStore) MarkReviewStatusVerified(context.Context, int64, int64, int64, int64, string, time.Time) error {
	return nil
}

func (s *countingSelfHealStore) callsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestRuntimeSelfHealTickerRunsHourly pins the low-frequency wiring: the
// five-minute loop stays separate and the self-heal scan runs once per hour.
func TestRuntimeSelfHealTickerRunsHourly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := &countingSelfHealStore{}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		runtime := &Runtime{
			reviews: &ReviewService{store: st, now: time.Now, logger: logger},
			logger:  logger,
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go runtime.reconcileAppliedReviewStatuses(ctx, time.Hour)
		synctest.Wait()
		if got := st.callsCount(); got != 0 {
			t.Fatalf("self-heal ran before the first interval: %d", got)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := st.callsCount(); got != 1 {
			t.Fatalf("self-heal calls after one hour = %d, want 1", got)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := st.callsCount(); got != 2 {
			t.Fatalf("self-heal calls after two hours = %d, want 2", got)
		}
	})
}
