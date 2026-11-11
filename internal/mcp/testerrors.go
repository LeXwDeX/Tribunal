package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Fixed connection-test categories returned by the extension test API. The
// HTTP layer maps these values to status codes and fixed messages; upstream
// error text never reaches the caller.
const (
	TestCategoryInvalidConfig    = "invalid_config"
	TestCategoryConnectTimeout   = "connect_timeout"
	TestCategoryConnectFailed    = "connect_failed"
	TestCategoryHandshakeFailed  = "handshake_failed"
	TestCategoryListToolsTimeout = "list_tools_timeout"
	TestCategoryListToolsFailed  = "list_tools_failed"
	TestCategoryTooManyTools     = "too_many_tools"
	TestCategoryCleanupFailed    = "cleanup_failed"
)

var (
	// ErrConnectTimeout reports a server connection that exceeded its deadline.
	ErrConnectTimeout = errors.New("mcp: connect timed out")
	// ErrConnectFailed reports a server that could not be reached.
	ErrConnectFailed = errors.New("mcp: connect failed")
	// ErrHandshakeFailed reports an MCP initialize failure after the transport
	// connected.
	ErrHandshakeFailed = errors.New("mcp: handshake failed")
	// ErrListToolsTimeout reports a tools/list call that exceeded its deadline.
	ErrListToolsTimeout = errors.New("mcp: list tools timed out")
	// ErrTooManyTools reports a server that advertises more than the limit.
	ErrTooManyTools = errors.New("mcp: server advertises too many tools")
	// ErrCleanupFailed reports a test session that could not be closed.
	ErrCleanupFailed = errors.New("mcp: test cleanup failed")
)

// TestError classifies one connection-test failure. Its Error text names only
// the fixed category, so a wrapped cause can never leak configuration, URLs or
// upstream output into a response or a log.
type TestError struct {
	Category string
	Err      error
}

func (e *TestError) Error() string {
	if e == nil {
		return "mcp: test failed"
	}
	return "mcp: test failed: " + e.Category
}

// Unwrap exposes the classified cause to errors.Is and errors.As.
func (e *TestError) Unwrap() error { return e.Err }

// TestCategory returns the fixed category carried by err, or an empty string
// when err is not a classified test failure.
func TestCategory(err error) string {
	var testErr *TestError
	if errors.As(err, &testErr) && testErr.Category != "" {
		return testErr.Category
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalidConfig):
		return TestCategoryInvalidConfig
	case errors.Is(err, ErrConnectTimeout):
		return TestCategoryConnectTimeout
	case errors.Is(err, ErrHandshakeFailed):
		return TestCategoryHandshakeFailed
	case errors.Is(err, ErrConnectFailed):
		return TestCategoryConnectFailed
	case errors.Is(err, ErrListToolsTimeout):
		return TestCategoryListToolsTimeout
	case errors.Is(err, ErrTooManyTools):
		return TestCategoryTooManyTools
	case errors.Is(err, ErrListTools):
		return TestCategoryListToolsFailed
	case errors.Is(err, ErrCleanupFailed):
		return TestCategoryCleanupFailed
	case errors.Is(err, context.DeadlineExceeded):
		return TestCategoryConnectTimeout
	}
	return ""
}

// NewTestError attaches a fixed category to one failure.
func NewTestError(category string, err error) error {
	return &TestError{Category: category, Err: err}
}

// classifyConnectError maps a raw or already-wrapped connect failure to a
// fixed category.
func classifyConnectError(err error) string {
	switch {
	case err == nil:
		return TestCategoryConnectFailed
	case errors.Is(err, ErrConnectTimeout), errors.Is(err, context.DeadlineExceeded):
		return TestCategoryConnectTimeout
	case errors.Is(err, ErrHandshakeFailed):
		return TestCategoryHandshakeFailed
	case isHandshakeError(err):
		return TestCategoryHandshakeFailed
	default:
		return TestCategoryConnectFailed
	}
}

// isHandshakeError reports failures raised after the transport was established:
// protocol errors and an early close. Process and network failures stay
// connect_failed.
func isHandshakeError(err error) bool {
	var rpcErr *jsonrpc.Error
	var execErr *exec.Error
	var pathErr *os.PathError
	var netErr net.Error
	switch {
	case errors.As(err, &rpcErr):
		return true
	case errors.As(err, &execErr), errors.As(err, &pathErr), errors.As(err, &netErr):
		return false
	case errors.Is(err, sdk.ErrConnectionClosed):
		return true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	default:
		return false
	}
}

// connectSentinel returns the sentinel used to wrap one fixed category.
func connectSentinel(category string) error {
	switch category {
	case TestCategoryConnectTimeout:
		return ErrConnectTimeout
	case TestCategoryHandshakeFailed:
		return ErrHandshakeFailed
	default:
		return ErrConnectFailed
	}
}

// listToolsError wraps a tools/list failure with its fixed category.
func listToolsError(err error) error {
	category := TestCategoryListToolsFailed
	switch {
	case errors.Is(err, ErrTooManyTools):
		category = TestCategoryTooManyTools
	case errors.Is(err, ErrListToolsTimeout):
		category = TestCategoryListToolsTimeout
	}
	return NewTestError(category, err)
}

// connectTestError classifies an acquire failure and preserves the sentinel
// and the original cause for errors.Is.
func connectTestError(err error) error {
	category := classifyConnectError(err)
	return NewTestError(category, fmt.Errorf("%w: %w", connectSentinel(category), err))
}
