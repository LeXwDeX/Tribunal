package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/openai/openai-go/v3"
)

// ErrKind classifies failures that callers may handle differently.
type ErrKind string

const (
	ErrRateLimited    ErrKind = "rate_limited"
	ErrOverloaded     ErrKind = "overloaded"
	ErrContextTooLong ErrKind = "context_too_long"
	ErrAuth           ErrKind = "auth"
	ErrBadRequest     ErrKind = "bad_request"
	ErrUnsafeResponse ErrKind = "unsafe_model_response"
	ErrTransient      ErrKind = "transient"
	ErrCanceled       ErrKind = "canceled"
)

// ErrUnsafeProviderResponse matches unsafe responses anywhere in an error chain.
var ErrUnsafeProviderResponse = errors.New("llm: unsafe provider response")

// IsUnsafeResponse also recognizes errors that implement a custom As method.
func IsUnsafeResponse(err error) bool {
	if errors.Is(err, ErrUnsafeProviderResponse) {
		return true
	}
	var failure *Error
	return errors.As(err, &failure) && failure != nil && failure.Kind == ErrUnsafeResponse
}

// Error is a sanitized provider error. Error() never contains an upstream
// response body, prompt, credential, or request URL. The original cause is
// retained only for errors.Is/errors.As while waiting on context errors.
type Error struct {
	Kind             ErrKind       `json:"kind"`
	Status           int           `json:"status,omitempty"`
	RetryAfter       time.Duration `json:"retry_after,omitempty"`
	Usage            Usage         `json:"usage"`
	Err              error         `json:"-"`
	cause            error
	responseObserved bool
	auditStop        StopReason
}

// AttemptGuardError retains the guard cause and usage from completed attempts.
// Previous is the last provider failure, if the guard stopped a retry.
type AttemptGuardError struct {
	Err              error
	Usage            Usage
	Previous         *Error
	responseObserved bool
}

func (e *AttemptGuardError) Error() string { return e.Err.Error() }
func (e *AttemptGuardError) Unwrap() []error {
	if e.Previous == nil {
		return []error{e.Err}
	}
	return []error{e.Err, e.Previous}
}

// HasObservedResponse reports a parsed adapter response or an SDK HTTP error
// with a response, including earlier retries. It never infers this from usage.
// Guard errors use their own attempt history, not the guard's unrelated cause.
func HasObservedResponse(err error) bool {
	if err == nil {
		return false
	}
	if guard, ok := err.(*AttemptGuardError); ok {
		return guard != nil && (guard.responseObserved || (guard.Previous != nil && guard.Previous.responseObserved))
	}
	if failure, ok := err.(*Error); ok {
		if failure == nil {
			return false
		}
		if failure.responseObserved {
			return true
		}
	}
	if as, ok := err.(interface{ As(any) bool }); ok {
		var guard *AttemptGuardError
		if as.As(&guard) && guard != nil {
			if guard.responseObserved || (guard.Previous != nil && guard.Previous.responseObserved) {
				return true
			}
		} else {
			var failure *Error
			if as.As(&failure) && failure != nil && failure.responseObserved {
				return true
			}
		}
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			if HasObservedResponse(inner) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return HasObservedResponse(wrapped.Unwrap())
	}
	return false
}

// ErrorUsage returns known usage without adding overlapping nested error data.
// The boolean reports whether every observed count was nonnegative.
func ErrorUsage(err error) (Usage, bool) {
	var guard *AttemptGuardError
	if errors.As(err, &guard) && guard != nil {
		usage := AddUsage(Usage{}, guard.Usage)
		if guard.Previous != nil {
			usage.Incomplete = usage.Incomplete || guard.Previous.Usage.Incomplete || !ValidUsage(guard.Previous.Usage)
		}
		return usage, ValidUsage(guard.Usage)
	}
	var usage Usage
	valid := true
	found := false
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		if failure, ok := current.(*Error); ok && failure != nil {
			found = true
			u := failure.Usage
			valid = valid && ValidUsage(u)
			usage = MaxUsage(usage, u)
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, inner := range wrapped.Unwrap() {
				visit(inner)
			}
		case interface{ Unwrap() error }:
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	if !found {
		var failure *Error
		if errors.As(err, &failure) && failure != nil {
			visit(failure)
		}
	}
	return AddUsage(Usage{}, usage), valid
}

// Error returns a short diagnostics-safe summary.
func (e *Error) Error() string {
	if e == nil {
		return "llm: error"
	}
	if e.Kind == ErrCanceled {
		if e.Err != nil {
			return "llm: request canceled: " + e.Err.Error()
		}
		return "llm: request canceled"
	}
	msg := "llm: " + string(e.Kind)
	if e.Status != 0 {
		msg += fmt.Sprintf(" (status %d)", e.Status)
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	return msg
}

// Unwrap exposes only the sanitized error.
func (e *Error) Unwrap() error { return e.Err }

// Is preserves errors.Is(err, context.Canceled/DeadlineExceeded) without
// exposing the original transport error.
func (e *Error) Is(target error) bool {
	if target == ErrUnsafeProviderResponse && e.Kind == ErrUnsafeResponse {
		return true
	}
	if errors.Is(e.Err, target) {
		return true
	}
	return e.cause != nil && errors.Is(e.cause, target)
}

// Retryable reports whether the registry may retry this error.
func (e *Error) Retryable() bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case ErrRateLimited, ErrOverloaded, ErrTransient:
		return true
	default:
		return false
	}
}

// ErrorCode exposes the sanitized provider failure kind to errclass. It never
// returns error text or upstream response data.
func (e *Error) ErrorCode() string {
	if e == nil {
		return ""
	}
	return string(e.Kind)
}

// ErrorStatus exposes the sanitized upstream HTTP status to DAG failure
// diagnostics. It never includes response data.
func (e *Error) ErrorStatus() int {
	if e == nil {
		return 0
	}
	return e.Status
}

func canceledError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrCanceled, Err: context.DeadlineExceeded, cause: err}
	}
	return &Error{Kind: ErrCanceled, Err: context.Canceled, cause: err}
}

func badRequestf(format string, args ...any) *Error {
	return &Error{Kind: ErrBadRequest, Err: fmt.Errorf(format, args...)}
}

// sdkError converts an SDK or transport failure into a sanitized Error.
func sdkError(err error) *Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		canceled := canceledError(err)
		var oe *openai.Error
		var ae *anthropic.Error
		canceled.responseObserved = (errors.As(err, &oe) && oe != nil && oe.Response != nil) ||
			(errors.As(err, &ae) && ae != nil && ae.Response != nil)
		return canceled
	}
	var oe *openai.Error
	if errors.As(err, &oe) {
		failure := apiError(oe.StatusCode, oe.Code, oe.Type, oe.RawJSON(), headerOf(oe.Response), err)
		failure.responseObserved = oe.Response != nil
		return failure
	}
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		failure := apiError(ae.StatusCode, "", string(ae.Type()), ae.RawJSON(), headerOf(ae.Response), err)
		failure.responseObserved = ae.Response != nil
		return failure
	}
	return &Error{Kind: ErrTransient, Err: errors.New("transport failure"), cause: err}
}

func headerOf(resp *http.Response) http.Header {
	if resp == nil {
		return nil
	}
	return resp.Header
}

func apiError(status int, code, errType, body string, header http.Header, cause error) *Error {
	kind := ErrBadRequest
	lowerType := strings.ToLower(errType)
	lowerCode := strings.ToLower(code)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = ErrAuth
	case status == http.StatusTooManyRequests:
		kind = ErrRateLimited
	case status == 529:
		kind = ErrOverloaded
	case status == http.StatusRequestTimeout:
		kind = ErrTransient
	case status >= 500:
		kind = ErrTransient
	case status == http.StatusRequestEntityTooLarge:
		kind = ErrContextTooLong
	case status == http.StatusBadRequest:
		if isContextTooLong(lowerCode + " " + lowerType + " " + limitString(body, 4096)) {
			kind = ErrContextTooLong
		}
	}
	if strings.Contains(lowerType, "overloaded") || strings.Contains(lowerCode, "overloaded") {
		kind = ErrOverloaded
	}
	return &Error{
		Kind:       kind,
		Status:     status,
		RetryAfter: parseRetryAfter(header),
		Err:        fmt.Errorf("status %d", status),
		cause:      cause,
	}
}

func isContextTooLong(text string) bool {
	text = strings.ToLower(text)
	for _, marker := range []string{
		"context_length_exceeded",
		"context length",
		"context window",
		"prompt is too long",
		"too many tokens",
		"maximum context",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func limitString(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func parseRetryAfter(header http.Header) time.Duration {
	if header == nil {
		return 0
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}
