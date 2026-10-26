// Package errclass maps errors to short, fixed categories for logs and job
// records. It never returns error text: handler and store errors may embed
// payloads, tokens, or row identifiers.
package errclass

import (
	"context"
	"errors"
	"regexp"
)

// Fixed classes. A sanitized application code may also be returned.
const (
	Timeout  = "timeout"
	Canceled = "canceled"
	Panic    = "panic"
	Internal = "internal"
)

// ErrPanic marks a recovered handler panic.
var ErrPanic = errors.New("handler panic")

// coder is implemented by errors that carry an already sanitized code, such
// as app.Error.
type coder interface {
	ErrorCode() string
}

var codeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Of returns the class of err, or "" for nil.
func Of(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrPanic):
		return Panic
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Canceled
	}
	var c coder
	if errors.As(err, &c) {
		if code := c.ErrorCode(); codeRE.MatchString(code) {
			return code
		}
	}
	return Internal
}
