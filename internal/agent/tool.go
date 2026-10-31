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
	"fmt"
	"reflect"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/llm"
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
// wraps successful output and replaces error output with safe feedback.
type Result struct {
	Content   string
	IsError   bool
	Truncated bool
}

// SubmitSpec describes the structured final result.
//
// Decode must reject unknown fields and perform business validation when the
// node requires it. Feedback never includes the decoder's error text.
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
	// ErrUnsafeResponse marks a model response that must not execute tools or
	// become a deterministic success fallback.
	ErrUnsafeResponse = unsafeResponseError{}
)

type unsafeResponseError struct{}

func (unsafeResponseError) Error() string     { return "agent rejected unsafe model response" }
func (unsafeResponseError) ErrorCode() string { return "unsafe_model_response" }

// validationFeedback accepts only fixed categories and known contract fields.
// Both the error and its optional feedback method may be untrusted.
func validationFeedback(err error) (message string) {
	message = "check the schema and review rules"
	defer func() {
		if recover() != nil {
			message = "check the schema and review rules"
		}
	}()
	var feedback interface{ AgentValidationFeedback() (string, string) }
	var field, category string
	if errors.As(err, &feedback) {
		field, category = feedback.AgentValidationFeedback()
	} else {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return message
		}
		field, category = jsonTypeField(typeErr.Field, typeErr.Type), "type"
	}
	switch category {
	case "required":
		message = "required; provide the field in the correct form"
	case "range":
		message = "outside the allowed range or length; check the schema"
	case "type":
		message = "incorrect JSON type; check the schema"
	case "null":
		message = "must not be null; provide the expected value"
	case "format":
		message = "invalid format; check the schema"
	case "value":
		message = "invalid value; check the schema and review rules"
	case "not_authorized":
		message = "not authorized by the review policy; use an allowed value"
	case "duplicate":
		message = "duplicate value; remove the duplicate"
	case "reference":
		message = "does not match an allowed reference"
	case "conflict":
		message = "inconsistent with other fields or review rules"
	case "verdict":
		message = "PASS is not allowed while severe findings are published"
	case "decision":
		message = "a judge group requires an explicit decision"
	case "root":
		message = "unknown root; choose one of the listed roots"
	case "unsafe_path":
		message = "path is outside the sandbox or is not allowed; choose a safe relative path"
	case "ignored":
		message = "path is excluded by repository policy; choose an allowed path"
	case "unavailable":
		message = "path is not accessible; choose an accessible path"
	case "line_range":
		message = "line range must satisfy 1 <= start_line <= end_line"
	case "line_limit":
		message = fmt.Sprintf("line range must cover at most %d lines", maxGitBlameLines)
	}
	if field = safeFeedbackField(field); field != "" {
		message = field + ": " + message
	}
	return message
}

func jsonTypeField(field string, typ reflect.Type) string {
	if len(field) > 128 {
		return ""
	}
	parts := strings.Split(field, ".")
	var out []string
	for _, part := range parts {
		if len(out) > 0 && len(part) > 0 && strings.Trim(part, "0123456789") == "" {
			out[len(out)-1] += "[" + part + "]"
		} else {
			if len(out) > 0 && jsonArrayField(out[len(out)-1], len(out) == 1) {
				out[len(out)-1] += "[0]"
			}
			out = append(out, part)
		}
	}
	if last := len(out) - 1; last >= 0 && jsonArrayField(out[last], last == 0) && typ != nil &&
		typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array && (out[last] != "evidence" || typ.Kind() == reflect.Struct) {
		out[last] += "[0]"
	}
	return strings.Join(out, ".")
}

// The review contracts define these arrays; error fields alone omit their indices on some Go versions.
func jsonArrayField(name string, root bool) bool {
	switch name {
	case "risk_reasons", "packages", "blocks", "files", "challenge_only", "findings", "evidence_gaps", "accepted", "rebutted",
		"assumptions", "decisions", "new_findings", "extend_requests", "modules", "conventions", "build_and_test", "risk_hotspots", "review_focus":
		return true
	case "evidence":
		return root
	default:
		return false
	}
}

// safeFeedbackField allows only code-owned names and removes untrusted indices.
func safeFeedbackField(field string) string {
	if len(field) == 0 || len(field) > 128 {
		return ""
	}
	parts := strings.Split(field, ".")
	if len(parts) > 4 {
		return ""
	}
	for i, part := range parts {
		name := part
		indexed := false
		if bracket := strings.IndexByte(part, '['); bracket >= 0 {
			name = part[:bracket]
			index := part[bracket+1:]
			if len(index) < 2 || len(index) > 4 || index[len(index)-1] != ']' {
				return ""
			}
			index = index[:len(index)-1]
			if len(index) > 1 && index[0] == '0' {
				return ""
			}
			for _, digit := range index {
				if digit < '0' || digit > '9' {
					return ""
				}
			}
			indexed = true
		}
		switch name {
		case "risk", "risk_reasons", "packages", "id", "blocks", "files", "focus", "provider", "run_reasoner", "challenge_only",
			"summary", "findings", "verdict", "evidence_status", "evidence_gaps", "accepted", "rebutted", "node", "index", "reason",
			"result_type", "severity", "dimension", "finding", "where", "why", "assumptions", "confidence", "path", "line", "side",
			"category", "title", "body", "suggestion", "evidence", "start_line", "end_line", "root", "note", "status", "suggested_severity",
			"decisions", "finding_id", "action", "new_findings", "extend_requests", "kind", "new_finding_index", "overview", "architecture",
			"modules", "purpose", "conventions", "build_and_test", "risk_hotspots", "review_focus", "pattern", "glob", "max_results", "max":
		default:
			return ""
		}
		parts[i] = name
		if indexed {
			parts[i] += "[]"
		}
	}
	return strings.Join(parts, ".")
}
