package review

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/llm"
)

// sandboxDeniedClass marks a node failure caused by the evidence sandbox or
// ignore policy rejecting a path.
const sandboxDeniedClass = "sandbox_denied"

// Fixed classes for run failures that previously collapsed into
// executor_error (#454).
const (
	noSubmitClass       = "no_submit"
	invalidSubmitClass  = "invalid_submit"
	contextTooLongClass = "context_too_long"
	tokenBudgetClass    = "budget_exhausted"
)

// contextTooLongDetail is the fixed detail for a provider context overflow.
const contextTooLongDetail = "model context window exceeded"

// classifiedError carries a fixed class and an optional code-owned detail for
// the DAG failure event while keeping the original error chain for
// errors.Is/As.
type classifiedError struct {
	class  string
	detail string
	err    error
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

// ErrorDetail is the fixed, sanitized summary persisted with the failure. The
// DAG engine only accepts code-owned details and never raw error text.
func (e *classifiedError) ErrorDetail() string {
	if e == nil {
		return ""
	}
	return e.detail
}

// classifyError adds a durable failure class and, when known, a sanitized
// detail for known causes. An existing fixed class (unsafe response, timeout,
// cancellation, provider class) always wins; unknown errors stay unwrapped so
// the scheduler keeps the existing executor_error reason.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, agent.ErrUnsafePath) || errors.Is(err, agent.ErrIgnoredPath) {
		return &classifiedError{class: sandboxDeniedClass, err: err}
	}
	if class := errclass.Of(err); class != "" && class != errclass.Internal {
		detail := ""
		if class == contextTooLongClass {
			detail = contextTooLongDetail
		}
		return &classifiedError{class: class, detail: detail, err: err}
	}
	class, detail := fixedFailure(err)
	if class == "" {
		return err
	}
	return &classifiedError{class: class, detail: detail, err: err}
}

// failureClass reports the class a failure event will persist, including the
// generic fallback. It is used for logging only and never returns error text.
func failureClass(err error) string {
	if errors.Is(err, agent.ErrUnsafePath) || errors.Is(err, agent.ErrIgnoredPath) {
		return sandboxDeniedClass
	}
	if class := errclass.Of(err); class != "" && class != errclass.Internal {
		return class
	}
	if class, _ := fixedFailure(err); class != "" {
		return class
	}
	return "executor_error"
}

// failureTurns reports the model requests the failed run completed, when the
// error carries a partial run result.
func failureTurns(err error) int {
	var partial *PartialRunError
	if errors.As(err, &partial) && partial != nil && partial.Turns > 0 {
		return partial.Turns
	}
	var usageErr *usageError
	if errors.As(err, &usageErr) && usageErr != nil {
		return usageErr.turns
	}
	return 0
}

// fixedFailure maps the known run failures to their fixed class and a
// code-owned detail. Details contain only fixed words and counts, never
// provider text.
func fixedFailure(err error) (class, detail string) {
	if err == nil {
		return "", ""
	}
	// Fail-closed causes keep their existing class even when a joined error
	// also carries ErrNoSubmit or ErrInvalidSubmit.
	if errors.Is(err, agent.ErrUnsafeResponse) || llm.IsUnsafeResponse(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", ""
	}
	var partial *PartialRunError
	if errors.As(err, &partial) && partial != nil && partial.Class != "" {
		return partial.Class, partial.failureDetail()
	}
	switch {
	case errors.Is(err, errTokenBudgetExhausted):
		return tokenBudgetClass, budgetFailureDetail(err)
	case errors.Is(err, agent.ErrNoSubmit):
		return noSubmitClass, submitFailureDetail("submit_result missing", failureTurns(err))
	case errors.Is(err, agent.ErrInvalidSubmit):
		return invalidSubmitClass, submitFailureDetail("submit_result rejected", failureTurns(err))
	}
	var failure *llm.Error
	if errors.As(err, &failure) && failure != nil && failure.Kind == llm.ErrContextTooLong {
		return contextTooLongClass, contextTooLongDetail
	}
	return "", ""
}

func submitFailureDetail(prefix string, turns int) string {
	if turns > 0 {
		return fmt.Sprintf("%s after %d turns", prefix, turns)
	}
	return prefix
}

func budgetFailureDetail(err error) string {
	var budget *tokenBudgetError
	if errors.As(err, &budget) && budget != nil && budget.max > 0 {
		return fmt.Sprintf("workflow token budget exhausted (%d of %d tokens)", budget.used, budget.max)
	}
	return "workflow token budget exhausted"
}

// partialRunClass names the fixed class of a partial run from its cause. An
// empty result leaves the error to the generic classification.
func partialRunClass(err error) string {
	class, _ := fixedFailure(err)
	return class
}

// failureDetail is the sanitized detail for a partial run. It mirrors
// fixedFailure so both paths agree on the persisted wording.
func (e *PartialRunError) failureDetail() string {
	if e == nil {
		return ""
	}
	switch e.Class {
	case noSubmitClass:
		return submitFailureDetail("submit_result missing", e.Turns)
	case invalidSubmitClass:
		return submitFailureDetail("submit_result rejected", e.Turns)
	case contextTooLongClass:
		return contextTooLongDetail
	case tokenBudgetClass:
		return budgetFailureDetail(e.err)
	}
	return ""
}
