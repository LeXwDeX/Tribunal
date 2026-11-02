package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/openai/openai-go/v3"
)

type turnAsError struct{ failure *Error }

func (e turnAsError) Error() string { return "classified failure" }
func (e turnAsError) As(target any) bool {
	if out, ok := target.(**Error); ok {
		*out = e.failure
		return true
	}
	return false
}

type turnAsWrapper struct{ err error }

func (e turnAsWrapper) Error() string      { return "wrapped failure" }
func (e turnAsWrapper) Unwrap() error      { return e.err }
func (e turnAsWrapper) As(target any) bool { return errors.As(e.err, target) }

func TestTurnObservationErrorChains(t *testing.T) {
	observed := &Error{Kind: ErrUnsafeResponse, responseObserved: true}
	plain := &Error{Kind: ErrTransient, Usage: Usage{InputTokens: 5}}
	guard := &AttemptGuardError{Err: observed}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed nil failure", (*Error)(nil), false},
		{"typed nil guard", (*AttemptGuardError)(nil), false},
		{"usage is not evidence", plain, false},
		{"wrapped", fmt.Errorf("wrapped: %w", observed), true},
		{"joined", errors.Join(plain, observed), true},
		{"custom As", turnAsError{failure: observed}, true},
		{"guard cause is not this call", fmt.Errorf("wrapped: %w", guard), false},
		{"custom As cannot attribute guard cause", turnAsWrapper{err: guard}, false},
		{"guard previous response", &AttemptGuardError{Err: context.Canceled, Previous: observed}, true},
		{"joined independent guard and response", errors.Join(guard, observed), true},
		{"custom As with joined guard and response", turnAsWrapper{err: errors.Join(guard, observed)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasObservedResponse(tc.err); got != tc.want {
				t.Fatalf("HasObservedResponse(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
	wrapped := fmt.Errorf("wrapped: %w", observed)
	var failure *Error
	if !errors.Is(wrapped, observed) || !errors.Is(wrapped, ErrUnsafeProviderResponse) || !errors.As(wrapped, &failure) || failure != observed {
		t.Fatalf("error chain changed: %v", wrapped)
	}
}

type turnSequenceAdapter struct {
	errs  []error
	calls int
}

func (a *turnSequenceAdapter) Generate(context.Context, *Request) (*Response, error) {
	err := a.errs[a.calls]
	a.calls++
	return nil, err
}

func TestTurnObservationRetryHistory(t *testing.T) {
	for _, stop := range []string{"guard", "final"} {
		t.Run(stop, func(t *testing.T) {
			first := &Error{Kind: ErrTransient, Usage: Usage{InputTokens: 3}, responseObserved: true}
			second := &Error{Kind: ErrTransient, Usage: Usage{InputTokens: 2}}
			if stop == "final" {
				second.Kind = ErrAuth
			}
			adapter := &turnSequenceAdapter{errs: []error{first, second}}
			client := &providerClient{adapter: adapter, baseDelay: time.Nanosecond}
			blocked := errors.New("budget exhausted")
			checks := 0
			_, err := GenerateGuarded(context.Background(), client, simpleRequest(), func(context.Context, Usage) error {
				checks++
				if checks == 3 {
					return blocked
				}
				return nil
			})
			var failure *Error
			usage, valid := ErrorUsage(err)
			if adapter.calls != 2 || !HasObservedResponse(err) || !errors.As(err, &failure) || !valid ||
				usage.InputTokens != 5 || errors.Is(err, blocked) != (stop == "guard") {
				t.Fatalf("err=%v calls=%d checks=%d usage=%+v", err, adapter.calls, checks, usage)
			}
		})
	}
}

type turnCancelAdapter struct{ cancel context.CancelFunc }

func (a turnCancelAdapter) Generate(context.Context, *Request) (*Response, error) {
	a.cancel()
	return nil, &Error{Kind: ErrTransient, Usage: Usage{InputTokens: 4}, responseObserved: true}
}

func TestTurnObservationCancellationDuringRetryBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &providerClient{adapter: turnCancelAdapter{cancel: cancel}, baseDelay: time.Nanosecond}
	_, err := client.Generate(ctx, simpleRequest())
	usage, valid := ErrorUsage(err)
	if !errors.Is(err, context.Canceled) || !HasObservedResponse(err) || !valid || usage.InputTokens != 4 {
		t.Fatalf("err=%v usage=%+v", err, usage)
	}
}

func TestTurnObservationCanceledSDKError(t *testing.T) {
	for _, provider := range []error{
		&openai.Error{Response: &http.Response{StatusCode: http.StatusBadRequest}},
		&anthropic.Error{Response: &http.Response{StatusCode: http.StatusBadRequest}},
	} {
		err := sdkError(errors.Join(context.Canceled, provider))
		if !errors.Is(err, context.Canceled) || !HasObservedResponse(err) || err.Kind != ErrCanceled {
			t.Fatalf("err=%v observed=%v", err, HasObservedResponse(err))
		}
	}
	if HasObservedResponse(sdkError(context.Canceled)) {
		t.Fatal("cancellation without a response was marked observed")
	}
}

func TestTurnObservationSDKErrorRequiresResponse(t *testing.T) {
	for _, provider := range []error{
		&openai.Error{StatusCode: http.StatusBadRequest, Response: &http.Response{StatusCode: http.StatusBadRequest}},
		&anthropic.Error{StatusCode: http.StatusBadRequest, Response: &http.Response{StatusCode: http.StatusBadRequest}},
	} {
		err := sdkError(provider)
		if !HasObservedResponse(err) || err.Kind != ErrBadRequest || err.Status != http.StatusBadRequest {
			t.Fatalf("err=%v observed=%v", err, HasObservedResponse(err))
		}
	}
	for _, provider := range []error{&openai.Error{StatusCode: http.StatusBadRequest}, &anthropic.Error{StatusCode: http.StatusBadRequest}} {
		if err := sdkError(provider); HasObservedResponse(err) {
			t.Fatalf("SDK error without response marked observed: %v", err)
		}
	}
}

type turnInvalidUsageAdapter struct{}

func (turnInvalidUsageAdapter) Generate(context.Context, *Request) (*Response, error) {
	return &Response{Usage: Usage{InputTokens: 5, OutputTokens: -1}}, nil
}

func TestTurnObservationRegistryRejectsInvalidUsage(t *testing.T) {
	client := &providerClient{adapter: turnInvalidUsageAdapter{}}
	resp, err := client.Generate(context.Background(), simpleRequest())
	usage, valid := ErrorUsage(err)
	if resp != nil || !IsUnsafeResponse(err) || !HasObservedResponse(err) || !valid || usage != (Usage{InputTokens: 5, Incomplete: true}) {
		t.Fatalf("resp=%+v err=%v usage=%+v valid=%v", resp, err, usage, valid)
	}
}

type turnConcurrentAdapter struct{}

func (turnConcurrentAdapter) Generate(_ context.Context, req *Request) (*Response, error) {
	return nil, &Error{Kind: ErrAuth, responseObserved: req.Messages[0].Parts[0].Text == "observed"}
}

func TestTurnObservationConcurrentCallsAreIndependent(t *testing.T) {
	client := &providerClient{adapter: turnConcurrentAdapter{}}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(observed bool) {
			defer wg.Done()
			req := simpleRequest()
			if observed {
				req.Messages[0].Parts[0].Text = "observed"
			}
			_, err := client.Generate(context.Background(), req)
			if got := HasObservedResponse(err); got != observed {
				t.Errorf("HasObservedResponse(%v)=%v, want %v", err, got, observed)
			}
		}(i%2 == 0)
	}
	wg.Wait()
}
