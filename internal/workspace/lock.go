package workspace

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// ctxRWMutex is a writer-preferring reader/writer lock whose waits honor a
// context. It uses no goroutines, so a waiter that leaves cannot leak work.
type ctxRWMutex struct {
	mu        sync.Mutex
	readers   int
	writer    bool
	writers   int
	changed   chan struct{}
	waitCount atomic.Int64
	waitNanos atomic.Int64
	maxNanos  atomic.Int64
}

// LockStats is the accumulated wait observed by one lock.
type LockStats struct {
	WaitCount    int64
	WaitDuration time.Duration
	MaxWait      time.Duration
}

// LockStats returns a snapshot of the waiters this lock has observed.
func (m *ctxRWMutex) LockStats() LockStats {
	if m == nil {
		return LockStats{}
	}
	return LockStats{
		WaitCount:    m.waitCount.Load(),
		WaitDuration: time.Duration(m.waitNanos.Load()),
		MaxWait:      time.Duration(m.maxNanos.Load()),
	}
}

func (m *ctxRWMutex) observeWait(start time.Time) {
	if start.IsZero() {
		return
	}
	wait := time.Since(start)
	m.waitCount.Add(1)
	m.waitNanos.Add(int64(wait))
	for {
		max := m.maxNanos.Load()
		if int64(wait) <= max || m.maxNanos.CompareAndSwap(max, int64(wait)) {
			return
		}
	}
}

func newCtxRWMutex() *ctxRWMutex {
	return &ctxRWMutex{changed: make(chan struct{})}
}

func (m *ctxRWMutex) RLock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var waitStart time.Time
	for {
		m.mu.Lock()
		if !m.writer && m.writers == 0 {
			m.readers++
			m.mu.Unlock()
			m.observeWait(waitStart)
			return m.rUnlock, nil
		}
		if waitStart.IsZero() {
			waitStart = time.Now()
		}
		changed := m.changed
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			m.observeWait(waitStart)
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (m *ctxRWMutex) rUnlock() {
	m.mu.Lock()
	m.readers--
	changed := m.readers == 0
	if changed {
		m.broadcastLocked()
	}
	m.mu.Unlock()
}

func (m *ctxRWMutex) Lock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.writers++
	m.mu.Unlock()

	var waitStart time.Time
	for {
		m.mu.Lock()
		if !m.writer && m.readers == 0 {
			m.writer = true
			m.writers--
			m.mu.Unlock()
			m.observeWait(waitStart)
			return m.unlock, nil
		}
		if waitStart.IsZero() {
			waitStart = time.Now()
		}
		changed := m.changed
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			m.observeWait(waitStart)
			m.mu.Lock()
			m.writers--
			m.broadcastLocked()
			m.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (m *ctxRWMutex) unlock() {
	m.mu.Lock()
	if !m.writer {
		m.mu.Unlock()
		panic("workspace: unlock of unlocked write lock")
	}
	m.writer = false
	m.broadcastLocked()
	m.mu.Unlock()
}

func (m *ctxRWMutex) broadcastLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}
