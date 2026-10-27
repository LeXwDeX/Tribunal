// Package app coordinates project registration, webhook ingress, and the
// review, workspace, and feedback orchestration services.
package app

import (
	"errors"
	"fmt"

	"github.com/LeXwDeX/tribunal/internal/errclass"
)

var (
	// ErrReviewSuperseded means a newer review generation or head SHA won.
	// The job is settled without retrying because retrying cannot help.
	ErrReviewSuperseded = errors.New("app: review superseded")
	// ErrMRClosed means the merge request is closed or merged.
	ErrMRClosed = errors.New("app: merge request closed")
	// ErrPermanentJob marks a job failure that must settle instead of retry.
	ErrPermanentJob = errors.New("app: permanent job failure")
	// ErrStalePublication means the publication fingerprint no longer matches
	// the current MR head, configuration, prompts, or review generation.
	ErrStalePublication = errors.New("app: publication is stale")
)

// PermanentJob wraps cause so job handlers can settle instead of retry.
func PermanentJob(cause error) error {
	if cause == nil {
		return ErrPermanentJob
	}
	return fmt.Errorf("%w: %w", ErrPermanentJob, cause)
}

// IsPermanentJob reports whether a handler error must settle the job.
func IsPermanentJob(err error) bool {
	return errors.Is(err, ErrPermanentJob) ||
		errors.Is(err, ErrReviewSuperseded) ||
		errors.Is(err, ErrMRClosed) ||
		errors.Is(err, ErrStalePublication)
}

// jobSettleClass names why a job settled without retry for logs. Like
// errclass.Of it never returns error text: permanent causes may wrap GitLab
// responses or dossier contents.
func jobSettleClass(err error) string {
	switch {
	case errors.Is(err, ErrMRClosed):
		return "mr_closed"
	case errors.Is(err, ErrReviewSuperseded):
		return "review_superseded"
	case errors.Is(err, ErrStalePublication):
		return "stale_publication"
	}
	if class := errclass.Of(err); class != errclass.Internal {
		return class
	}
	if errors.Is(err, ErrPermanentJob) {
		return "permanent_job"
	}
	return errclass.Internal
}

// Error is the sanitized application error consumed by internal/server.
type Error struct {
	Code       string
	Message    string
	Details    []Detail
	Upstream   int
	Compensate bool
}

// Detail is a field-level validation failure.
type Detail struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return "app: error"
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// ErrorCode exposes the sanitized code for error classification.
func (e *Error) ErrorCode() string {
	if e == nil {
		return ""
	}
	return e.Code
}

func validationError(message string, details ...Detail) *Error {
	if message == "" {
		message = "validation failed"
	}
	return &Error{Code: "validation_failed", Message: message, Details: details}
}

func conflictError(message string) *Error {
	return &Error{Code: "conflict", Message: message}
}

func notFoundError(message string) *Error {
	return &Error{Code: "not_found", Message: message}
}

func upstreamError(status int) *Error {
	message := "GitLab request failed"
	if status > 0 {
		message = fmt.Sprintf("GitLab rejected the request (status %d)", status)
	}
	return &Error{Code: "gitlab_rejected", Message: message, Upstream: status}
}

func internalError() *Error {
	return &Error{Code: "internal", Message: "internal error"}
}
