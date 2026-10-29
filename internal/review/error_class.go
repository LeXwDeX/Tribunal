package review

import (
	"errors"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/errclass"
)

// sandboxDeniedClass marks a node failure caused by the evidence sandbox or
// ignore policy rejecting a path.
const sandboxDeniedClass = "sandbox_denied"

// classifiedError carries a fixed class for the DAG failure event while
// keeping the original error chain for errors.Is/As.
type classifiedError struct {
	class string
	err   error
}

func (e *classifiedError) Error() string {
	if e == nil || e.err == nil {
		return "review: classified error"
	}
	return e.err.Error()
}

func (e *classifiedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *classifiedError) ErrorClass() string {
	if e == nil {
		return ""
	}
	return e.class
}

// classifyError adds a durable failure class for known causes. Unknown errors
// stay unwrapped so the scheduler keeps the existing executor_error reason.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, agent.ErrUnsafePath) || errors.Is(err, agent.ErrIgnoredPath) {
		return &classifiedError{class: sandboxDeniedClass, err: err}
	}
	class := errclass.Of(err)
	if class == "" || class == errclass.Internal {
		return err
	}
	return &classifiedError{class: class, err: err}
}
