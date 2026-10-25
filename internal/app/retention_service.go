package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

const defaultRetentionInterval = 6 * time.Hour

// RetentionStore is the cleanup subset of the SQLite store.
type RetentionStore interface {
	DeleteLLMCallsBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteDeliveriesBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteTerminalJobsBefore(ctx context.Context, before time.Time) (int64, error)
	PruneContextDocs(ctx context.Context, keep int) (int64, error)
}

// RetentionConfig configures RetentionService.
type RetentionConfig struct {
	Store             RetentionStore
	LLMCallRetention  time.Duration
	DeliveryRetention time.Duration
	JobRetention      time.Duration
	ContextVersions   int
	Interval          time.Duration
	Now               func() time.Time
	Logger            *slog.Logger
}

// RetentionService runs the periodic cleanup described in the data model.
type RetentionService struct {
	store             RetentionStore
	llmCallRetention  time.Duration
	deliveryRetention time.Duration
	jobRetention      time.Duration
	contextVersions   int
	interval          time.Duration
	now               func() time.Time
	logger            *slog.Logger
}

// NewRetentionService validates the retention policy and builds the service.
func NewRetentionService(cfg RetentionConfig) (*RetentionService, error) {
	if cfg.Store == nil {
		return nil, errors.New("app: retention service store is required")
	}
	if cfg.LLMCallRetention <= 0 || cfg.DeliveryRetention <= 0 || cfg.JobRetention <= 0 {
		return nil, errors.New("app: retention durations must be positive")
	}
	if cfg.ContextVersions <= 0 {
		return nil, errors.New("app: retention context versions must be positive")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultRetentionInterval
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &RetentionService{
		store:             cfg.Store,
		llmCallRetention:  cfg.LLMCallRetention,
		deliveryRetention: cfg.DeliveryRetention,
		jobRetention:      cfg.JobRetention,
		contextVersions:   cfg.ContextVersions,
		interval:          interval,
		now:               now,
		logger:            logger,
	}, nil
}

// Start runs one pass immediately and then on every interval until ctx is
// cancelled. A non-positive interval uses the configured interval.
func (s *RetentionService) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = s.interval
	}
	_ = s.RunOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.RunOnce(ctx)
		}
	}
}

// RunOnce runs each cleanup independently, logs the counts, and joins the
// category errors so one failure does not block the others.
func (s *RetentionService) RunOnce(ctx context.Context) error {
	now := s.now().UTC()
	var errs []error

	fail := func(category string, err error) {
		errs = append(errs, fmt.Errorf("retention: %s: %w", category, err))
		s.logger.Warn("retention cleanup failed", "category", category, "error", err.Error())
	}

	var llmCalls, deliveries, jobs, contextDocs int64
	var err error
	if llmCalls, err = s.store.DeleteLLMCallsBefore(ctx, now.Add(-s.llmCallRetention)); err != nil {
		fail("delete llm calls", err)
	}
	if deliveries, err = s.store.DeleteDeliveriesBefore(ctx, now.Add(-s.deliveryRetention)); err != nil {
		fail("delete deliveries", err)
	}
	if jobs, err = s.store.DeleteTerminalJobsBefore(ctx, now.Add(-s.jobRetention)); err != nil {
		fail("delete jobs", err)
	}
	if contextDocs, err = s.store.PruneContextDocs(ctx, s.contextVersions); err != nil {
		fail("prune context documents", err)
	}

	s.logger.Info("retention cleanup finished",
		"llm_calls", llmCalls,
		"deliveries", deliveries,
		"jobs", jobs,
		"context_docs", contextDocs,
		"errors", len(errs))
	return errors.Join(errs...)
}
