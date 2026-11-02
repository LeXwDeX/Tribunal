package llm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type budgetAttemptAdapter struct{ n int }

func (a *budgetAttemptAdapter) Generate(context.Context, *Request) (*Response, error) {
	a.n++
	return nil, &Error{Kind: ErrTransient, Usage: Usage{InputTokens: int64(a.n)}}
}

type budgetEmptyAfterFailure struct{ calls int }

type budgetMixedSafetyAdapter struct {
	calls int
	first int64
}

func (a *budgetMixedSafetyAdapter) Generate(context.Context, *Request) (*Response, error) {
	a.calls++
	return nil, errors.Join(&Error{Kind: ErrTransient, Usage: Usage{InputTokens: a.first}}, &Error{Kind: ErrUnsafeResponse, Usage: Usage{InputTokens: 5}})
}

func TestBudgetAttemptUnsafeClassificationWinsOverRetryableError(t *testing.T) {
	for _, first := range []int64{0, 5} {
		a := &budgetMixedSafetyAdapter{first: first}
		client := &providerClient{adapter: a, baseDelay: time.Nanosecond}
		_, err := client.Generate(context.Background(), simpleRequest())
		var failure *Error
		if !errors.As(err, &failure) || failure.Kind != ErrUnsafeResponse || failure.Usage.InputTokens != 5 || a.calls != 1 {
			t.Fatalf("first=%d err=%v calls=%d", first, err, a.calls)
		}
	}
}

func (a *budgetEmptyAfterFailure) Generate(context.Context, *Request) (*Response, error) {
	a.calls++
	if a.calls == 1 {
		return nil, &Error{Kind: ErrTransient, Usage: Usage{InputTokens: 4}}
	}
	return nil, nil
}

func TestBudgetAttemptEmptyProviderResponseRetainsPriorUsage(t *testing.T) {
	a := &budgetEmptyAfterFailure{}
	client := &providerClient{adapter: a, baseDelay: time.Nanosecond}
	resp, err := client.Generate(context.Background(), simpleRequest())
	var failure *Error
	if resp != nil || !errors.As(err, &failure) || failure.Kind != ErrTransient || failure.Usage.InputTokens != 4 || a.calls != maxRetries+1 {
		t.Fatalf("response=%+v err=%v calls=%d", resp, err, a.calls)
	}
}

func TestBudgetAttemptUnknownResponsesFailureDoesNotRetry(t *testing.T) {
	client, calls := usageClient(t, "openai-responses", `{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","output":[],"usage":{"input_tokens":11,"output_tokens":7}}`, responsesOK)
	_, err := client.Generate(context.Background(), simpleRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.Kind != ErrUnsafeResponse || calls.Load() != 1 || failure.Usage.InputTokens != 11 || failure.Usage.OutputTokens != 7 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

type blockingBudgetAdapter struct {
	started   chan struct{}
	release   chan struct{}
	calls     atomic.Int32
	committed *atomic.Int64
}

func (a *blockingBudgetAdapter) Generate(ctx context.Context, _ *Request) (*Response, error) {
	a.calls.Add(1)
	a.started <- struct{}{}
	select {
	case <-a.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if a.committed != nil {
		a.committed.Store(5)
	}
	return &Response{Usage: Usage{InputTokens: 5}}, nil
}

func TestBudgetAttemptChecksAfterSemaphoreWait(t *testing.T) {
	var committed atomic.Int64
	a := &blockingBudgetAdapter{started: make(chan struct{}, 2), release: make(chan struct{}), committed: &committed}
	var mu sync.Mutex
	var records []CallRecord
	client := &providerClient{adapter: a, sem: make(chan struct{}, 1), recorder: RecorderFunc(func(_ context.Context, r CallRecord) { mu.Lock(); records = append(records, r); mu.Unlock() })}
	blocked := errors.New("budget")
	check := func(context.Context, Usage) error {
		if committed.Load() >= 5 {
			return blocked
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() { _, err := GenerateGuarded(ctx, client, simpleRequest(), check); done <- err }()
	<-a.started
	go func() { _, err := GenerateGuarded(ctx, client, simpleRequest(), check); done <- err }()
	close(a.release)
	first, second := <-done, <-done
	if !((first == nil && errors.Is(second, blocked)) || (second == nil && errors.Is(first, blocked))) || a.calls.Load() != 1 || len(records) != 1 {
		t.Fatalf("first=%v second=%v calls=%d records=%d", first, second, a.calls.Load(), len(records))
	}
}

func TestBudgetAttemptConcurrentInFlightCanOvershoot(t *testing.T) {
	var committed atomic.Int64
	a := &blockingBudgetAdapter{started: make(chan struct{}, 2), release: make(chan struct{})}
	client := &providerClient{adapter: a, sem: make(chan struct{}, 2)}
	blocked := errors.New("budget")
	check := func(context.Context, Usage) error {
		if committed.Load() >= 5 {
			return blocked
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := GenerateGuarded(ctx, client, simpleRequest(), check); done <- err }()
	}
	<-a.started
	<-a.started
	close(a.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	committed.Store(10)
	_, err := GenerateGuarded(ctx, client, simpleRequest(), check)
	if !errors.Is(err, blocked) || a.calls.Load() != 2 {
		t.Fatalf("error=%v calls=%d", err, a.calls.Load())
	}
}

func TestBudgetAttemptCancelDuringGuardMakesNoAudit(t *testing.T) {
	for _, reject := range []bool{false, true} {
		adapter := &budgetAttemptAdapter{}
		var records []CallRecord
		client := &providerClient{adapter: adapter, sem: make(chan struct{}, 1), recorder: RecorderFunc(func(_ context.Context, r CallRecord) { records = append(records, r) })}
		ctx, cancel := context.WithCancel(context.Background())
		blocked := errors.New("budget")
		_, err := GenerateGuarded(ctx, client, simpleRequest(), func(context.Context, Usage) error {
			cancel()
			if reject {
				return blocked
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) || errors.Is(err, blocked) != reject || adapter.n != 0 || len(records) != 0 {
			t.Fatalf("reject=%v err=%v calls=%d records=%d", reject, err, adapter.n, len(records))
		}
	}
}

func TestBudgetAttemptRecorderKeepsAttemptSnapshot(t *testing.T) {
	var records []CallRecord
	client := &providerClient{adapter: &budgetAttemptAdapter{}, baseDelay: time.Nanosecond, recorder: RecorderFunc(func(_ context.Context, r CallRecord) { records = append(records, r) })}
	_, err := client.Generate(context.Background(), simpleRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.Usage.InputTokens != 15 || len(records) != 5 {
		t.Fatalf("err=%v records=%d", err, len(records))
	}
	for i, record := range records {
		var f *Error
		if !errors.As(record.Err, &f) || f.Usage.InputTokens != int64(i+1) || record.Audit.Response.Usage.InputTokens != int64(i+1) {
			t.Fatalf("record %d = %+v, error=%+v", i, record.Audit, f)
		}
	}
}
