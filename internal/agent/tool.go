// Package agent runs the bounded tool loop shared by every agent node.
//
// The loop calls one llm.Client, executes tools, and accepts the final result
// only through the submit_result tool. Builtin tools are read-only and limited
// to sandbox roots; no repository code is ever executed.
package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
)

const (
	// SubmitToolName is the tool every run must call to finish.
	SubmitToolName = "submit_result"
	// DefaultMaxTurns bounds model requests when RunSpec.MaxTurns is zero.
	DefaultMaxTurns = 30
	// DefaultMaxToolOutput bounds one tool result in characters.
	DefaultMaxToolOutput = 20000
	// maxSubmitRetries is the number of attempts after the first invalid
	// submit_result that still receive feedback from the model.
	maxSubmitRetries = 2
	// minToolOutputChars keeps the untrusted-data wrapper and its truncation
	// marker inside a tiny configured output limit.
	minToolOutputChars = 256
)

// Tool is one tool exposed to the model. Call input is untrusted model
// output; implementations must validate it. Run wraps non-submit results in
// an untrusted-data envelope, so direct callers of Call must do the same.
type Tool interface {
	Def() llm.ToolDef
	Call(ctx context.Context, input json.RawMessage) (Result, error)
}

// Result is the output of one tool call. Content is untrusted data: Run
// wraps it with a random nonce before returning it to the model.
type Result struct {
	Content   string
	IsError   bool
	Truncated bool
}

// SubmitSpec describes the structured final result.
//
// Decode must reject unknown fields and perform business validation when the
// node requires it. Its error text is returned to the model.
type SubmitSpec struct {
	Schema json.RawMessage
	Decode func(raw json.RawMessage) (any, error)
}

// RunSpec configures one agent run.
//
// MaxTokens is forwarded to llm.Request.MaxTokens when positive.
type RunSpec struct {
	Client        llm.Client
	System        []llm.Block
	Prompt        string
	Tools         []Tool
	Submit        SubmitSpec
	MaxTurns      int
	MaxToolOutput int
	MaxTokens     int
	Temperature   *float64
	Reasoning     *llm.ReasoningOpts
	// BeforeTurn runs before every model request with the usage already
	// consumed by this run. A non-nil error stops the loop without another
	// Generate call.
	BeforeTurn func(context.Context, llm.Usage) error
}

// RunResult is the accepted output and the conversation that produced it.
// Transcript excludes System and stores provider messages unchanged.
type RunResult struct {
	Output     any
	Usage      llm.Usage
	Turns      int
	Transcript []llm.Message
}

var (
	// ErrNoSubmit reports a run that ended without a valid submit_result.
	ErrNoSubmit = errors.New("agent finished without submit_result")
	// ErrInvalidSubmit reports submit_result validation failures that
	// exhausted the retry budget (or the remaining turns).
	ErrInvalidSubmit = errors.New("agent submit_result failed validation")
)
