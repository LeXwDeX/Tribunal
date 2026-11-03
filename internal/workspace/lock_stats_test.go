package workspace

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLockStatsRecordsWaitedRLock(t *testing.T) {
	m := newCtxRWMutex()
	unlock, err := m.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()

	waitCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := m.RLock(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RLock error = %v, want deadline exceeded", err)
	}

	stats := m.LockStats()
	if stats.WaitCount != 1 {
		t.Fatalf("WaitCount = %d, want 1", stats.WaitCount)
	}
	if stats.WaitDuration < 30*time.Millisecond {
		t.Fatalf("WaitDuration = %s, want at least 30ms", stats.WaitDuration)
	}
	if stats.MaxWait < 30*time.Millisecond {
		t.Fatalf("MaxWait = %s, want at least 30ms", stats.MaxWait)
	}
}

func TestLockStatsIgnoresImmediateAcquire(t *testing.T) {
	m := newCtxRWMutex()
	unlock, err := m.RLock(context.Background())
	if err != nil {
		t.Fatalf("RLock: %v", err)
	}
	defer unlock()

	if stats := m.LockStats(); stats != (LockStats{}) {
		t.Fatalf("LockStats = %+v, want zero", stats)
	}
}

func TestLockStatsRecordsCanceledWriteWait(t *testing.T) {
	m := newCtxRWMutex()
	unlock, err := m.RLock(context.Background())
	if err != nil {
		t.Fatalf("RLock: %v", err)
	}
	defer unlock()

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.Lock(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock error = %v, want deadline exceeded", err)
	}

	stats := m.LockStats()
	if stats.WaitCount != 1 {
		t.Fatalf("WaitCount = %d, want 1", stats.WaitCount)
	}
	if stats.WaitDuration < 20*time.Millisecond {
		t.Fatalf("WaitDuration = %s, want at least 20ms", stats.WaitDuration)
	}
}

func TestManagerLockStatsAggregatesWorkspaceLocks(t *testing.T) {
	manager := NewManager(t.TempDir(), nil)
	w := manager.Get(42)
	unlock, err := w.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := w.RLock(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RLock error = %v, want deadline exceeded", err)
	}

	stats := manager.LockStats()
	if stats.WaitCount != 1 {
		t.Fatalf("WaitCount = %d, want 1", stats.WaitCount)
	}
	if stats.WaitDuration < 20*time.Millisecond {
		t.Fatalf("WaitDuration = %s, want at least 20ms", stats.WaitDuration)
	}
	if stats.MaxWait < 20*time.Millisecond {
		t.Fatalf("MaxWait = %s, want at least 20ms", stats.MaxWait)
	}
}
