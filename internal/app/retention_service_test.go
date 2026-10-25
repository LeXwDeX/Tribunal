package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

type retentionClock struct {
	mu  sync.Mutex
	now time.Time
}

func newRetentionClock() *retentionClock {
	return &retentionClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
}

func (c *retentionClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *retentionClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type retentionCounts struct {
	mu          sync.Mutex
	llmCalls    int64
	deliveries  int64
	jobs        int64
	contextDocs int64
}

func (c *retentionCounts) add(llmCalls, deliveries, jobs, contextDocs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.llmCalls += llmCalls
	c.deliveries += deliveries
	c.jobs += jobs
	c.contextDocs += contextDocs
}

func (c *retentionCounts) snapshot() (int64, int64, int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.llmCalls, c.deliveries, c.jobs, c.contextDocs
}

// countingRetentionStore wraps the real store and records returned counts.
type countingRetentionStore struct {
	inner  RetentionStore
	counts *retentionCounts
}

func (c countingRetentionStore) DeleteLLMCallsBefore(ctx context.Context, before time.Time) (int64, error) {
	n, err := c.inner.DeleteLLMCallsBefore(ctx, before)
	c.counts.add(n, 0, 0, 0)
	return n, err
}

func (c countingRetentionStore) DeleteDeliveriesBefore(ctx context.Context, before time.Time) (int64, error) {
	n, err := c.inner.DeleteDeliveriesBefore(ctx, before)
	c.counts.add(0, n, 0, 0)
	return n, err
}

func (c countingRetentionStore) DeleteTerminalJobsBefore(ctx context.Context, before time.Time) (int64, error) {
	n, err := c.inner.DeleteTerminalJobsBefore(ctx, before)
	c.counts.add(0, 0, n, 0)
	return n, err
}

func (c countingRetentionStore) PruneContextDocs(ctx context.Context, keep int) (int64, error) {
	n, err := c.inner.PruneContextDocs(ctx, keep)
	c.counts.add(0, 0, 0, n)
	return n, err
}

// retentionStub is a fixed-response store used for error isolation and Start.
type retentionStub struct {
	mu          sync.Mutex
	calls       map[string]int
	deliveryErr error
}

func newRetentionStub() *retentionStub {
	return &retentionStub{calls: make(map[string]int)}
}

func (s *retentionStub) record(category string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[category]++
	return 1
}

func (s *retentionStub) callCount(category string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[category]
}

func (s *retentionStub) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, count := range s.calls {
		total += count
	}
	return total
}

func (s *retentionStub) DeleteLLMCallsBefore(context.Context, time.Time) (int64, error) {
	return s.record("llm_calls"), nil
}

func (s *retentionStub) DeleteDeliveriesBefore(context.Context, time.Time) (int64, error) {
	s.record("deliveries")
	s.mu.Lock()
	err := s.deliveryErr
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return 1, nil
}

func (s *retentionStub) DeleteTerminalJobsBefore(context.Context, time.Time) (int64, error) {
	return s.record("jobs"), nil
}

func (s *retentionStub) PruneContextDocs(context.Context, int) (int64, error) {
	return s.record("context_docs"), nil
}

func TestRetentionServiceRunOnceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	clock := newRetentionClock()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	onboardingID, err := st.RegisterProject(ctx, store.Project{
		ID:                1,
		PathWithNamespace: "group/project",
		WebURL:            "https://gitlab.example.com/group/project",
		DefaultBranch:     "main",
		TokenEnc:          []byte("token"),
		WebhookSecretEnc:  []byte("secret"),
	})
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	if err := st.RecordLLMCall(ctx, llm.CallRecord{
		Provider: "provider",
		Model:    "model",
		Meta:     llm.CallMeta{WorkflowID: "wf", NodeID: "node", Turn: 1},
	}); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}
	if _, _, err := st.RecordDelivery(ctx, store.Delivery{
		UUID: "delivery-1", ProjectID: 1, Event: "Push Hook", Outcome: store.DeliveryIgnored,
	}, nil); err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
	doneID, _, err := st.Enqueue(ctx, store.Job{Kind: "retention.done", Key: "retention:done"})
	if err != nil {
		t.Fatalf("Enqueue done: %v", err)
	}
	done, err := st.Claim(ctx, "worker", []string{"retention.done"}, time.Minute)
	if err != nil || done == nil {
		t.Fatalf("Claim done = %v, %v", done, err)
	}
	if err := st.Complete(ctx, *done); err != nil {
		t.Fatalf("Complete done: %v", err)
	}
	if _, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 1, BaseSHA: "a", Kind: "agents", Content: "old context",
	}); err != nil {
		t.Fatalf("SaveContextDoc a: %v", err)
	}
	queuedID, _, err := st.Enqueue(ctx, store.Job{Kind: "retention.keep", Key: "retention:keep"})
	if err != nil {
		t.Fatalf("Enqueue queued: %v", err)
	}

	clock.Advance(48 * time.Hour)
	if err := st.RecordLLMCall(ctx, llm.CallRecord{
		Provider: "provider",
		Model:    "model",
		Meta:     llm.CallMeta{WorkflowID: "wf", NodeID: "node", Turn: 2},
	}); err != nil {
		t.Fatalf("RecordLLMCall fresh: %v", err)
	}
	if _, _, err := st.RecordDelivery(ctx, store.Delivery{
		UUID: "delivery-2", ProjectID: 1, Event: "Push Hook", Outcome: store.DeliveryIgnored,
	}, nil); err != nil {
		t.Fatalf("RecordDelivery fresh: %v", err)
	}
	if _, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 1, BaseSHA: "b", Kind: "agents", Content: "new context",
	}); err != nil {
		t.Fatalf("SaveContextDoc b: %v", err)
	}
	if _, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 1, BaseSHA: "c", Kind: "agents", Content: "current context",
	}); err != nil {
		t.Fatalf("SaveContextDoc c: %v", err)
	}

	var logs bytes.Buffer
	counts := &retentionCounts{}
	svc, err := NewRetentionService(RetentionConfig{
		Store:             countingRetentionStore{inner: st, counts: counts},
		LLMCallRetention:  24 * time.Hour,
		DeliveryRetention: 24 * time.Hour,
		JobRetention:      24 * time.Hour,
		ContextVersions:   1,
		Now:               clock.Now,
		Logger:            slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewRetentionService: %v", err)
	}
	if err := svc.RunOnce(ctx); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if llmCalls, deliveries, jobs, contextDocs := counts.snapshot(); llmCalls != 1 || deliveries != 1 || jobs != 1 || contextDocs != 1 {
		t.Fatalf("first counts = %d/%d/%d/%d, want 1/1/1/1", llmCalls, deliveries, jobs, contextDocs)
	}
	for _, want := range []string{"llm_calls=1", "deliveries=1", "jobs=1", "context_docs=1", "errors=0"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q does not contain %q", logs.String(), want)
		}
	}

	calls, _, err := st.ListLLMCalls(ctx, store.LLMCallFilter{})
	if err != nil {
		t.Fatalf("ListLLMCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Turn != 2 {
		t.Errorf("llm calls = %+v, want the fresh row only", calls)
	}
	if _, err := st.GetJob(ctx, doneID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("terminal job lookup error = %v, want ErrNotFound", err)
	}
	for _, activeID := range []string{onboardingID, queuedID} {
		job, err := st.GetJob(ctx, activeID)
		if err != nil {
			t.Fatalf("GetJob %s: %v", activeID, err)
		}
		if job.State != store.JobQueued {
			t.Errorf("job %s state = %s, want queued", activeID, job.State)
		}
	}
	if _, err := st.GetContextDoc(ctx, 1, "a", "agents"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("pruned context error = %v, want ErrNotFound", err)
	}
	if _, err := st.GetContextDoc(ctx, 1, "b", "agents"); err != nil {
		t.Errorf("kept context: %v", err)
	}
	if n, err := st.DeleteDeliveriesBefore(ctx, clock.Now().Add(-24*time.Hour)); err != nil || n != 0 {
		t.Errorf("remaining delivery count = %d, %v; want the fresh row only", n, err)
	}

	logs.Reset()
	if err := svc.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if llmCalls, deliveries, jobs, contextDocs := counts.snapshot(); llmCalls != 1 || deliveries != 1 || jobs != 1 || contextDocs != 1 {
		t.Fatalf("cumulative counts = %d/%d/%d/%d, want 1/1/1/1", llmCalls, deliveries, jobs, contextDocs)
	}
	for _, want := range []string{"llm_calls=0", "deliveries=0", "jobs=0", "context_docs=0"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("second log %q does not contain %q", logs.String(), want)
		}
	}
}

func TestRetentionServiceRunOnceContinuesOnCategoryError(t *testing.T) {
	stub := newRetentionStub()
	stub.deliveryErr = errors.New("database is closed")
	var logs bytes.Buffer
	svc, err := NewRetentionService(RetentionConfig{
		Store:             stub,
		LLMCallRetention:  time.Hour,
		DeliveryRetention: 2 * time.Hour,
		JobRetention:      3 * time.Hour,
		ContextVersions:   2,
		Logger:            slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewRetentionService: %v", err)
	}
	err = svc.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "delete deliveries") {
		t.Fatalf("RunOnce error = %v, want a delivery failure", err)
	}
	for _, category := range []string{"llm_calls", "deliveries", "jobs", "context_docs"} {
		if got := stub.callCount(category); got != 1 {
			t.Errorf("%s calls = %d, want 1", category, got)
		}
	}
	for _, want := range []string{"retention cleanup failed", "errors=1", "deliveries=0"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q does not contain %q", logs.String(), want)
		}
	}
}

func TestRetentionServiceStartStopsOnCancel(t *testing.T) {
	stub := newRetentionStub()
	svc, err := NewRetentionService(RetentionConfig{
		Store:             stub,
		LLMCallRetention:  time.Hour,
		DeliveryRetention: time.Hour,
		JobRetention:      time.Hour,
		ContextVersions:   1,
	})
	if err != nil {
		t.Fatalf("NewRetentionService: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Start(ctx, 5*time.Millisecond)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for stub.totalCalls() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := stub.totalCalls(); got < 4 {
		cancel()
		t.Fatalf("start calls = %d, want at least 4", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not stop after cancellation")
	}
}

func TestNewRetentionServiceValidation(t *testing.T) {
	stub := newRetentionStub()
	valid := RetentionConfig{
		Store:             stub,
		LLMCallRetention:  time.Hour,
		DeliveryRetention: time.Hour,
		JobRetention:      time.Hour,
		ContextVersions:   1,
	}
	if _, err := NewRetentionService(valid); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*RetentionConfig)
	}{
		{name: "nil store", mutate: func(cfg *RetentionConfig) { cfg.Store = nil }},
		{name: "llm call retention", mutate: func(cfg *RetentionConfig) { cfg.LLMCallRetention = 0 }},
		{name: "delivery retention", mutate: func(cfg *RetentionConfig) { cfg.DeliveryRetention = -time.Hour }},
		{name: "job retention", mutate: func(cfg *RetentionConfig) { cfg.JobRetention = 0 }},
		{name: "context versions", mutate: func(cfg *RetentionConfig) { cfg.ContextVersions = 0 }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			if _, err := NewRetentionService(cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
