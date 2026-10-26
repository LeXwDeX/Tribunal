package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
}

func openStore(t *testing.T, clock *fakeClock) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := sqlite.Open(context.Background(), path, sqlite.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitFor(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitJob(t *testing.T, q *sqlite.Store, id, state string) *store.Job {
	t.Helper()
	var last *store.Job
	waitFor(t, 3*time.Second, fmt.Sprintf("job %s state %s", id, state), func() bool {
		job, err := q.GetJob(context.Background(), id)
		if err != nil {
			return false
		}
		last = job
		return job.State == state
	})
	return last
}

func runWorker(t *testing.T, w *jobs.Worker) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	})
	return cancel, done
}

func TestWorkerDispatchesRegisteredKind(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := openStore(t, clock)
	jobID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "review.run", Key: "review:1:1", Payload: json.RawMessage(`{"head":"abc"}`)})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	handled := make(chan string, 1)
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers: map[string]jobs.Handler{
			"review.run": func(ctx context.Context, j *jobs.Job) error {
				if ctx == nil {
					return errors.New("nil context")
				}
				handled <- string(j.Payload)
				return nil
			},
		},
		Workers:      1,
		Lease:        time.Second,
		Now:          clock.Now,
		PollInterval: 5 * time.Millisecond,
	})
	_, _ = runWorker(t, w)

	select {
	case payload := <-handled:
		if payload != `{"head":"abc"}` {
			t.Errorf("payload = %s", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not called")
	}
	job := waitJob(t, q, jobID, store.JobDone)
	if job.Attempts != 1 || job.LeaseOwner != "" || job.LeaseUntil != nil {
		t.Errorf("completed job = %+v", job)
	}
}

func TestWorkerRetainsUnsupportedKinds(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := openStore(t, clock)
	unknownID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "not.implemented", Key: "unknown:1"})
	if err != nil {
		t.Fatalf("Enqueue unknown: %v", err)
	}
	otherID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "review.run", Key: "review:1:1"})
	if err != nil {
		t.Fatalf("Enqueue review: %v", err)
	}

	var calls atomic.Int32
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers: map[string]jobs.Handler{
			"workspace.refresh": func(context.Context, *jobs.Job) error {
				calls.Add(1)
				return nil
			},
		},
		Workers:      1,
		Lease:        time.Second,
		Now:          clock.Now,
		PollInterval: 5 * time.Millisecond,
	})
	cancel, done := runWorker(t, w)
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unregistered handler was called")
	}
	for _, id := range []string{unknownID, otherID} {
		job, err := q.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if job.State != store.JobQueued || job.Attempts != 0 {
			t.Errorf("unsupported job %s = %+v, want untouched queued", id, job)
		}
	}
}

func TestWorkerStopsOnCancellationAndPropagatesContext(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := openStore(t, clock)
	jobID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "slow", Key: "slow:1"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers: map[string]jobs.Handler{
			"slow": func(handlerCtx context.Context, _ *jobs.Job) error {
				close(started)
				<-handlerCtx.Done()
				close(canceled)
				return handlerCtx.Err()
			},
		},
		Workers:      1,
		Lease:        time.Minute,
		Now:          clock.Now,
		PollInterval: 5 * time.Millisecond,
	})
	cancel, done := runWorker(t, w)
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler context was not cancelled")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
	// A cancelled handler must not mark the job done.
	job, err := q.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State == store.JobDone {
		t.Fatalf("cancelled handler marked job done")
	}
}

func TestWorkerLostLeaseCancelsHandler(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := openStore(t, clock)
	jobID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "leased", Key: "leased:1"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers: map[string]jobs.Handler{
			"leased": func(handlerCtx context.Context, _ *jobs.Job) error {
				close(started)
				<-handlerCtx.Done()
				close(canceled)
				return handlerCtx.Err()
			},
		},
		Workers:      1,
		Lease:        90 * time.Millisecond,
		PollInterval: time.Second,
		Now:          clock.Now,
	})
	cancel, done := runWorker(t, w)
	<-started
	// Make the stored lease expire before the next heartbeat. The recovery
	// loop could also reclaim it; either way the handler must lose its lease.
	clock.Advance(2 * time.Second)
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler kept running after lease expiry")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State == store.JobDone {
		t.Fatalf("expired job marked done: %+v", job)
	}
}

func TestWorkerRetriesThenCompletes(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := openStore(t, clock)
	jobID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "flaky", Key: "flaky:1", MaxAttempts: 3})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	var attempts atomic.Int32
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers: map[string]jobs.Handler{
			"flaky": func(context.Context, *jobs.Job) error {
				if attempts.Add(1) < 3 {
					return errors.New("temporary failure with secret-text")
				}
				return nil
			},
		},
		Workers:      1,
		Lease:        time.Second,
		Now:          clock.Now,
		PollInterval: 5 * time.Millisecond,
		RetryBackoff: func(int) time.Duration { return 0 },
	})
	_, _ = runWorker(t, w)
	job := waitJob(t, q, jobID, store.JobDone)
	if job.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", job.Attempts)
	}
	if attempts.Load() != 3 {
		t.Fatalf("handler calls = %d, want 3", attempts.Load())
	}
}

func TestWorkerRecoveryRunsWithEmptyHandlers(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := openStore(t, clock)
	jobID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "abandoned", Key: "abandoned:1"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Claim(ctx, "crashed-worker", []string{"abandoned"}, 10*time.Second)
	if err != nil || claimed == nil {
		t.Fatalf("Claim: %v, %v", claimed, err)
	}
	clock.Advance(11 * time.Second)

	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Workers:      1,
		Lease:        10 * time.Second,
		PollInterval: 10 * time.Millisecond,
		Now:          clock.Now,
	})
	_, _ = runWorker(t, w)
	job := waitJob(t, q, jobID, store.JobQueued)
	if job.Attempts != 1 || job.LeaseOwner != "" {
		t.Fatalf("recovered job = %+v", job)
	}
}

func TestWorkerDoesNotRunQueuedSuccessorEarly(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := openStore(t, clock)
	key := "review:1:7"
	if _, _, err := q.Enqueue(ctx, jobs.Job{Kind: "review.run", Key: key, Payload: json.RawMessage(`{"head":"a"}`)}); err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	release := make(chan struct{})
	started := make(chan string, 2)
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers: map[string]jobs.Handler{
			"review.run": func(_ context.Context, j *jobs.Job) error {
				started <- string(j.Payload)
				if string(j.Payload) == `{"head":"a"}` {
					<-release
				}
				return nil
			},
		},
		Workers:      1,
		Lease:        time.Minute,
		Now:          clock.Now,
		PollInterval: 5 * time.Millisecond,
	})
	_, _ = runWorker(t, w)

	select {
	case payload := <-started:
		if payload != `{"head":"a"}` {
			t.Fatalf("first payload = %s", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first handler did not start")
	}
	successorID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "review.run", Key: key, Payload: json.RawMessage(`{"head":"b"}`)})
	if err != nil {
		t.Fatalf("Enqueue successor: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	successor, err := q.GetJob(ctx, successorID)
	if err != nil {
		t.Fatalf("GetJob successor: %v", err)
	}
	if successor.State != store.JobQueued {
		t.Fatalf("successor state = %s, want queued while first is running", successor.State)
	}
	close(release)

	select {
	case payload := <-started:
		if payload != `{"head":"b"}` {
			t.Fatalf("second payload = %s", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("successor handler did not start after first completed")
	}
	waitJob(t, q, successorID, store.JobDone)
}

// flakyHeartbeatQueue fails heartbeats with a configurable error while
// delegating everything else to the real store.
type flakyHeartbeatQueue struct {
	*sqlite.Store
	mu       sync.Mutex
	failures int // remaining failures; negative fails forever
	err      error
	calls    atomic.Int32
}

func (q *flakyHeartbeatQueue) Heartbeat(ctx context.Context, j store.Job, lease time.Duration) error {
	q.calls.Add(1)
	q.mu.Lock()
	fail := q.failures != 0
	if q.failures > 0 {
		q.failures--
	}
	err := q.err
	q.mu.Unlock()
	if fail {
		return err
	}
	return q.Store.Heartbeat(ctx, j, lease)
}

// heartbeatHandler blocks until release or cancellation and reports which.
func heartbeatHandler(started, canceled chan struct{}, release <-chan struct{}) jobs.Handler {
	return func(ctx context.Context, _ *jobs.Job) error {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
			return ctx.Err()
		case <-release:
			return nil
		}
	}
}

func TestWorkerTransientHeartbeatErrorKeepsJobRunning(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := &flakyHeartbeatQueue{Store: openStore(t, clock), failures: 2, err: errors.New("database is locked")}
	jobID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "flaky", Key: "flaky:1"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers:     map[string]jobs.Handler{"flaky": heartbeatHandler(started, canceled, release)},
		Lease:        60 * time.Millisecond,
		PollInterval: time.Second,
		Now:          clock.Now,
	})
	runWorker(t, w)
	<-started
	waitFor(t, 3*time.Second, "heartbeat after transient failures", func() bool { return q.calls.Load() >= 3 })
	select {
	case <-canceled:
		t.Fatal("transient heartbeat error cancelled the handler")
	default:
	}
	close(release)
	waitJob(t, q.Store, jobID, store.JobDone)
}

func TestWorkerHeartbeatLeaseLostCancelsHandler(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := &flakyHeartbeatQueue{Store: openStore(t, clock), failures: -1, err: fmt.Errorf("heartbeat: %w", store.ErrLeaseLost)}
	if _, _, err := q.Enqueue(ctx, jobs.Job{Kind: "lost", Key: "lost:1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	started, canceled := make(chan struct{}), make(chan struct{})
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers:     map[string]jobs.Handler{"lost": heartbeatHandler(started, canceled, nil)},
		Lease:        60 * time.Millisecond,
		PollInterval: time.Second,
		Now:          clock.Now,
	})
	runWorker(t, w)
	<-started
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("lost lease did not cancel the handler")
	}
}

func TestWorkerPersistentHeartbeatErrorCancelsAfterLeaseExpires(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := &flakyHeartbeatQueue{Store: openStore(t, clock), failures: -1, err: errors.New("database is locked")}
	if _, _, err := q.Enqueue(ctx, jobs.Job{Kind: "stuck", Key: "stuck:1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	started, canceled := make(chan struct{}), make(chan struct{})
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers:     map[string]jobs.Handler{"stuck": heartbeatHandler(started, canceled, nil)},
		Lease:        60 * time.Millisecond,
		PollInterval: time.Second,
		Now:          clock.Now,
	})
	runWorker(t, w)
	<-started
	waitFor(t, 3*time.Second, "failed heartbeats", func() bool { return q.calls.Load() >= 3 })
	select {
	case <-canceled:
		t.Fatal("handler cancelled while its lease was still valid")
	default:
	}
	clock.Advance(time.Second)
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler kept running after its lease expired")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestWorkerRecordsSanitizedFailureClass(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	q := openStore(t, clock)
	jobID, _, err := q.Enqueue(ctx, jobs.Job{Kind: "boom", Key: "boom:1", MaxAttempts: 1})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	var logs lockedBuffer
	w := jobs.NewWorker(q, jobs.WorkerConfig{
		Handlers: map[string]jobs.Handler{
			"boom": func(context.Context, *jobs.Job) error {
				panic("token=SECRET-TOKEN")
			},
		},
		Workers:      1,
		Lease:        time.Second,
		Now:          clock.Now,
		PollInterval: 5 * time.Millisecond,
		Logger:       slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	_, _ = runWorker(t, w)
	job := waitJob(t, q, jobID, store.JobFailed)
	if !strings.Contains(job.LastError, "(panic)") || strings.Contains(job.LastError, "SECRET") {
		t.Fatalf("last_error = %q", job.LastError)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), `"error_class":"panic"`) {
		if time.Now().After(deadline) {
			t.Fatalf("failure log lacks class: %s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("log leaked handler text: %s", logs.String())
	}
}
