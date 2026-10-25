// Package jobs provides the persistent job queue contract and worker pool.
package jobs

import (
	"context"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// Job is the shared persistent queue record.
type Job = store.Job

// Handler executes one claimed job. The context is cancelled if the lease is
// lost or the worker stops.
type Handler func(ctx context.Context, j *Job) error

// Queue is the store-backed queue surface consumed by the worker.
type Queue interface {
	Enqueue(ctx context.Context, j Job) (id string, dup bool, err error)
	Claim(ctx context.Context, worker string, kinds []string, lease time.Duration) (*Job, error)
	Heartbeat(ctx context.Context, j Job, lease time.Duration) error
	Complete(ctx context.Context, j Job) error
	Fail(ctx context.Context, j Job, cause error, retryAt *time.Time) error
	CancelByKey(ctx context.Context, key string) (int, error)
	Recover(ctx context.Context) (int, error)
}
