package workspace

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLifecycleLockExcludesReadersAndWriters(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(1)
	unlock, err := ws.LifecycleLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ws.LifecycleRLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LifecycleRLock under write lock = %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := ws.LifecycleLock(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second LifecycleLock = %v", err)
	}
	unlock()
	r1, err := ws.LifecycleRLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := ws.LifecycleRLock(context.Background())
	if err != nil {
		t.Fatalf("readers must share: %v", err)
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel3()
	if _, err := ws.LifecycleLock(ctx3); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LifecycleLock under readers = %v", err)
	}
	r1()
	r2()
}

func TestLifecycleLockIndependentOfContentLock(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(1)
	unlockRead, err := ws.RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlockRead()
	// Queue a content writer; it must not block lifecycle readers or writers.
	wctx, wcancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if u, err := ws.Lock(wctx); err == nil {
			u()
		}
	}()
	defer func() { wcancel(); <-done }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	u, err := ws.LifecycleRLock(ctx)
	if err != nil {
		t.Fatalf("LifecycleRLock blocked by content lock: %v", err)
	}
	u()
}
