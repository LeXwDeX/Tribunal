package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	defaultLease      = 5 * time.Minute
	defaultPoll       = time.Second
	cleanupTimeout    = 5 * time.Second
	maxRetryBackoff   = 30 * time.Second
	firstRetryBackoff = time.Second
)

// ErrDeferred reports temporary ownership contention, not a failed attempt.
var ErrDeferred = errors.New("jobs: execution deferred")

// WorkerConfig configures a Worker. Zero values select conservative defaults.
type WorkerConfig struct {
	Handlers     map[string]Handler
	Workers      int
	Lease        time.Duration
	PollInterval time.Duration
	Now          func() time.Time
	RetryBackoff func(attempt int) time.Duration
	Logger       *slog.Logger
}

// Worker claims, executes, and settles persistent jobs. It only claims kinds
// with a registered handler, so unsupported jobs stay queued.
type Worker struct {
	queue    Queue
	handlers map[string]Handler
	kinds    []string
	workers  int
	lease    time.Duration
	poll     time.Duration
	now      func() time.Time
	backoff  func(attempt int) time.Duration
	logger   *slog.Logger
}

// NewWorker builds a Worker. The handler map is copied.
func NewWorker(q Queue, cfg WorkerConfig) *Worker {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.Lease <= 0 {
		cfg.Lease = defaultLease
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPoll
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RetryBackoff == nil {
		cfg.RetryBackoff = defaultRetryBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	handlers := make(map[string]Handler, len(cfg.Handlers))
	kinds := make([]string, 0, len(cfg.Handlers))
	for kind, handler := range cfg.Handlers {
		if handler == nil {
			continue
		}
		handlers[kind] = handler
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return &Worker{
		queue:    q,
		handlers: handlers,
		kinds:    kinds,
		workers:  cfg.Workers,
		lease:    cfg.Lease,
		poll:     cfg.PollInterval,
		now:      cfg.Now,
		backoff:  cfg.RetryBackoff,
		logger:   cfg.Logger,
	}
}

// Run serves jobs until ctx is cancelled, then waits for all worker goroutines
// to stop. Expired leases are recovered periodically even when no handler is
// registered. Run returns nil on normal cancellation.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil || w.queue == nil {
		return errors.New("jobs: worker queue is required")
	}
	if err := ctx.Err(); err != nil {
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.recoveryLoop(ctx)
	}()

	if len(w.handlers) > 0 {
		baseID, err := uuid.NewV7()
		base := "worker"
		if err == nil {
			base = baseID.String()
		}
		for i := 0; i < w.workers; i++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				w.workLoop(ctx, id)
			}(base + ":" + strconv.Itoa(i))
		}
	}
	wg.Wait()
	return nil
}

func (w *Worker) recoveryLoop(ctx context.Context) {
	w.recoverOnce(ctx)
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.recoverOnce(ctx)
		}
	}
}

func (w *Worker) recoverOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	n, err := w.queue.Recover(cctx)
	if err != nil {
		w.logger.Warn("job recovery failed")
		return
	}
	if n > 0 {
		w.logger.Info("job leases recovered", "count", n)
	}
}

func (w *Worker) workLoop(ctx context.Context, id string) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := w.queue.Claim(ctx, id, w.kinds, w.lease)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.logger.Warn("job claim failed")
			if !sleep(ctx, w.poll) {
				return
			}
			continue
		}
		if job == nil {
			if !sleep(ctx, w.poll) {
				return
			}
			continue
		}
		w.execute(ctx, job)
	}
}

// execute runs one handler with lease heartbeats. The ownership fields used
// for heartbeat/complete/fail are copied before the handler can mutate them.
func (w *Worker) execute(ctx context.Context, claimed *Job) {
	handler := w.handlers[claimed.Kind]
	if handler == nil {
		// Claim filters on registered kinds, so this only guards misuse.
		return
	}
	owner := *claimed
	runJob := *claimed
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- callHandler(runCtx, handler, &runJob)
	}()

	interval := w.lease / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// leaseUntil is a conservative local view of the stored lease. A failed
	// heartbeat that is not a lost lease is retried until this passes.
	leaseUntil := w.now().Add(w.lease)
	if owner.LeaseUntil != nil {
		leaseUntil = *owner.LeaseUntil
	}

	for {
		select {
		case err := <-done:
			if err == nil {
				w.complete(ctx, owner)
			} else if ctx.Err() == nil && errors.Is(err, ErrDeferred) {
				w.deferJob(ctx, owner)
			} else if ctx.Err() == nil {
				w.fail(ctx, owner, err)
			}
			return
		case <-ticker.C:
			sent := w.now()
			err := w.heartbeat(ctx, owner)
			if err == nil {
				leaseUntil = sent.Add(w.lease)
				continue
			}
			if errors.Is(err, store.ErrLeaseLost) || !w.now().Before(leaseUntil) {
				// A job without a live lease must not keep running.
				cancel()
				<-done
				return
			}
		case <-ctx.Done():
			cancel()
			<-done
			return
		}
	}
}

func (w *Worker) deferJob(parent context.Context, job Job) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
	defer cancel()
	if err := w.queue.Defer(ctx, job, w.now().Add(w.poll)); err != nil && !errors.Is(err, store.ErrLeaseLost) {
		w.logger.Warn("job deferral failed", "job_id", job.ID, "kind", job.Kind)
	}
}

func callHandler(ctx context.Context, handler Handler, job *Job) (err error) {
	defer func() {
		if recover() != nil {
			err = errclass.ErrPanic
		}
	}()
	return handler(ctx, job)
}

func (w *Worker) heartbeat(parent context.Context, job Job) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
	defer cancel()
	err := w.queue.Heartbeat(ctx, job, w.lease)
	if err != nil {
		if !errors.Is(err, store.ErrLeaseLost) {
			w.logger.Warn("job heartbeat failed; retrying until the lease expires", "job_id", job.ID, "kind", job.Kind)
		}
		return err
	}
	return nil
}

func (w *Worker) complete(parent context.Context, job Job) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
	defer cancel()
	if err := w.queue.Complete(ctx, job); err != nil && !errors.Is(err, store.ErrLeaseLost) {
		w.logger.Warn("job completion failed", "job_id", job.ID, "kind", job.Kind)
	}
}

func (w *Worker) fail(parent context.Context, job Job, cause error) {
	retryAt := w.now().Add(w.backoff(job.Attempts))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
	defer cancel()
	// Only the sanitized class is logged or persisted: the handler error text
	// may carry payload, token, or other sensitive text.
	w.logger.Warn("job attempt failed", "job_id", job.ID, "kind", job.Kind,
		"attempt", job.Attempts, "error_class", errclass.Of(cause))
	if err := w.queue.Fail(ctx, job, cause, &retryAt); err != nil && !errors.Is(err, store.ErrLeaseLost) {
		w.logger.Warn("job failure settlement failed", "job_id", job.ID, "kind", job.Kind)
	}
}

func defaultRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := firstRetryBackoff
	for i := 1; i < attempt && delay < maxRetryBackoff; i++ {
		delay *= 2
	}
	if delay > maxRetryBackoff {
		delay = maxRetryBackoff
	}
	return delay
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
