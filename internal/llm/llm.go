// Package llm defines the protocol-neutral client used by agents and DAG
// nodes. It also contains adapters for the OpenAI Chat Completions, OpenAI
// Responses and Anthropic Messages protocols.
//
// The package deliberately depends only on internal/config and the official
// provider SDKs. It never imports agent, review or store.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strings"
)

// Role is a message author role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType identifies one message part.
type PartType string

const (
	PartText       PartType = "text"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
	PartReasoning  PartType = "reasoning"
)

// Part is one piece of a message.
type Part struct {
	Type       PartType    `json:"type"`
	Text       string      `json:"text,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	Reasoning  *Reasoning  `json:"reasoning,omitempty"`
}

// ToolCall is a model request to run a tool.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// Invalid is a fixed rejection category set when the call was cleared,
	// and InputBytes the length of the rejected input. Neither carries
	// provider text, and neither is serialized.
	Invalid    string `json:"-"`
	InputBytes int    `json:"-"`
}

// Tool call rejection categories. They are a fixed vocabulary safe to audit
// and to show the model.
const (
	ToolCallMissing        = "missing_call"
	ToolCallEmptyID        = "empty_id"
	ToolCallEmptyName      = "empty_name"
	ToolCallDuplicateID    = "duplicate_id"
	ToolCallEmptyInput     = "empty_input"
	ToolCallNotObject      = "not_object"
	ToolCallInvalidJSON    = "invalid_json"
	ToolCallNonString      = "non_string_field"
	ToolCallUnsupported    = "unsupported_item"
	ToolCallMalformedList  = "malformed_list"
	ToolCallUnexpectedStop = "unexpected_stop"
	// ToolCallRejectedBatch marks a valid call cleared with an invalid one.
	ToolCallRejectedBatch = "rejected_batch"
)

// invalidToolCall is an adapter marker for a call that cannot be decoded.
func invalidToolCall(kind string) Part {
	return Part{Type: PartToolCall, ToolCall: &ToolCall{Invalid: kind}}
}

// toolUseProblem picks the marker category for a rejected wire call. A
// protocol violation wins over a non-string field, so it is never recovered.
func toolUseProblem(nonString, unsupported bool) string {
	if nonString && !unsupported {
		return ToolCallNonString
	}
	return ToolCallUnsupported
}

// toolCallProblem returns the rejection category of one call, or "".
func toolCallProblem(call *ToolCall, seen map[string]bool) string {
	if call == nil {
		return ToolCallMissing
	}
	if call.Invalid != "" {
		return call.Invalid
	}
	input := bytes.TrimSpace(call.Input)
	switch {
	case call.ID == "":
		return ToolCallEmptyID
	case call.Name == "":
		return ToolCallEmptyName
	case seen[call.ID]:
		return ToolCallDuplicateID
	case len(input) == 0:
		return ToolCallEmptyInput
	case input[0] != '{':
		return ToolCallNotObject
	case !json.Valid(call.Input):
		return ToolCallInvalidJSON
	}
	return ""
}

// SanitizeToolCalls clears every tool call's identity and payload when a call
// is invalid or the stop reason does not permit tool execution. A rejected
// batch never executes, and provider-controlled strings must not reach
// transcripts, errors or audit payloads. Valid empty objects and valid empty
// batches are preserved. Cleared calls keep a fixed rejection category for
// ToolCallIssue.
func SanitizeToolCalls(message Message, allow bool) (Message, bool) {
	invalid, hasCalls := false, false
	seen := make(map[string]bool)
	problems := make(map[int]string)
	for i, part := range message.Parts {
		if part.Type != PartToolCall {
			continue
		}
		hasCalls = true
		if problem := toolCallProblem(part.ToolCall, seen); problem != "" {
			invalid = true
			problems[i] = problem
		}
		if part.ToolCall != nil {
			seen[part.ToolCall.ID] = true
		}
	}
	if !invalid && (allow || !hasCalls) {
		return message, false
	}
	// A rejected batch never executes. Clear every identity and payload so
	// provider-controlled strings cannot reach transcripts or error messages.
	message.Parts = append([]Part(nil), message.Parts...)
	for i := range message.Parts {
		part := &message.Parts[i]
		if part.Type != PartToolCall {
			continue
		}
		cleared := &ToolCall{Invalid: ToolCallUnexpectedStop}
		if problem, ok := problems[i]; ok {
			cleared.Invalid = problem
		} else if invalid {
			cleared.Invalid = ToolCallRejectedBatch
		}
		if part.ToolCall != nil {
			cleared.InputBytes = len(part.ToolCall.Input)
			if part.ToolCall.Invalid != "" {
				cleared.InputBytes = part.ToolCall.InputBytes
			}
		}
		part.ToolCall = cleared
	}
	return message, invalid
}

// ToolCallIssue returns the category and input length of the first rejected
// call in a sanitized message. Invalid calls take precedence over calls
// cleared only because the stop reason forbade execution.
func ToolCallIssue(message Message) (string, int) {
	kind, size := "", 0
	for _, part := range message.Parts {
		if part.Type != PartToolCall || part.ToolCall == nil || part.ToolCall.Invalid == "" {
			continue
		}
		issue := part.ToolCall.Invalid
		switch issue {
		case ToolCallMissing, ToolCallEmptyID, ToolCallEmptyName, ToolCallDuplicateID, ToolCallEmptyInput,
			ToolCallNotObject, ToolCallInvalidJSON, ToolCallNonString, ToolCallUnsupported, ToolCallMalformedList:
			return issue, part.ToolCall.InputBytes
		case ToolCallUnexpectedStop, ToolCallRejectedBatch:
		default:
			// Only this package sets Invalid; keep the vocabulary fixed anyway.
			return ToolCallUnsupported, part.ToolCall.InputBytes
		}
		if kind == "" {
			kind, size = issue, part.ToolCall.InputBytes
		}
	}
	return kind, size
}

// SDKs may coerce a non-string JSON value into a Go string.
func wireString(raw string) bool {
	return len(raw) > 0 && raw[0] == '"'
}

func malformedWireList(raw string, decodedCount int, required bool) bool {
	if raw == "" || raw == "null" {
		return required
	}
	var entries []json.RawMessage
	return raw[0] != '[' || json.Unmarshal([]byte(raw), &entries) != nil || len(entries) != decodedCount
}

// ToolResult is the result of running a tool.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Reasoning preserves a provider reasoning block verbatim so it can be sent
// back on the next turn. Anthropic stores thinking/redacted_thinking blocks
// (including signatures). Responses stores reasoning items (including
// encrypted_content).
type Reasoning struct {
	Protocol string          `json:"protocol"`
	Raw      json.RawMessage `json:"raw"`
	Summary  string          `json:"summary,omitempty"`
}

// Message is one conversation turn.
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
}

// Block is a cacheable prompt block.
type Block struct {
	Text  string `json:"text"`
	Cache bool   `json:"cache,omitempty"`
}

// ToolDef declares a tool available to the model.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request is one stateless model request.
type Request struct {
	System      []Block        `json:"system,omitempty"`
	Messages    []Message      `json:"messages"`
	Tools       []ToolDef      `json:"tools,omitempty"`
	ToolChoice  string         `json:"tool_choice,omitempty"` // auto | required | none | <tool name>
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	Reasoning   *ReasoningOpts `json:"reasoning,omitempty"`
}

// ReasoningOpts overrides the provider's reasoning defaults for one request.
type ReasoningOpts struct {
	Effort       string `json:"effort,omitempty"`        // low | medium | high | max, or none with a capability
	BudgetTokens int    `json:"budget_tokens,omitempty"` // Anthropic thinking budget
	// ThinkingType is the openai-chat thinking switch: enabled or disabled.
	ThinkingType string `json:"thinking_type,omitempty"`
}

// StopReason explains why the model stopped.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Response is one model response.
type Response struct {
	Message    Message    `json:"message"`
	StopReason StopReason `json:"stop_reason"`
	Usage      Usage      `json:"usage"`
	// AttemptUsage is the successful attempt's usage. The registry sets it on
	// every success, while Usage also counts failed retries. It sizes the
	// context; Usage stays the budget figure. Zero means unknown (for
	// example a direct adapter call), and callers fall back to Usage.
	AttemptUsage Usage  `json:"-"`
	Model        string `json:"model,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
}

// Usage reports token consumption. InputTokens includes cache reads and writes;
// the cache fields are details, not additional tokens. CacheWriteTokens is
// reported by providers that expose cache writes separately. Incomplete marks
// detected invalid or unrepresentable counts; false does not prove completeness.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	// CacheMissTokens is the provider-reported uncached prompt prefix
	// (DeepSeek prompt_cache_miss_tokens). It details InputTokens and is not
	// added to it again. omitempty keeps existing audit responses unchanged.
	CacheMissTokens int64 `json:"cache_miss_tokens,omitempty"`
	Incomplete      bool  `json:"-"`
}

// ValidUsage rejects invalid provider counts before they can affect a budget.
func ValidUsage(u Usage) bool {
	return u.InputTokens >= 0 && u.OutputTokens >= 0 && u.CacheReadTokens >= 0 && u.CacheWriteTokens >= 0 &&
		u.ReasoningTokens >= 0 && u.CacheMissTokens >= 0
}

// AddUsage preserves nonnegative known usage and saturates on overflow.
func AddUsage(a, b Usage) Usage {
	incomplete := a.Incomplete || b.Incomplete
	add := func(x, y int64) int64 {
		if x < 0 {
			incomplete = true
			x = 0
		}
		if y < 0 {
			incomplete = true
			y = 0
		}
		if x > math.MaxInt64-y {
			incomplete = true
			return math.MaxInt64
		}
		return x + y
	}
	out := Usage{InputTokens: add(a.InputTokens, b.InputTokens), OutputTokens: add(a.OutputTokens, b.OutputTokens),
		CacheReadTokens: add(a.CacheReadTokens, b.CacheReadTokens), CacheWriteTokens: add(a.CacheWriteTokens, b.CacheWriteTokens),
		ReasoningTokens: add(a.ReasoningTokens, b.ReasoningTokens), CacheMissTokens: add(a.CacheMissTokens, b.CacheMissTokens)}
	out.Incomplete = incomplete || out.InputTokens > math.MaxInt64-out.OutputTokens
	return out
}

// MaxUsage combines overlapping reports without counting the same tokens twice.
func MaxUsage(a, b Usage) Usage {
	a = AddUsage(Usage{}, a)
	b = AddUsage(Usage{}, b)
	return AddUsage(Usage{}, Usage{
		InputTokens:      max(a.InputTokens, b.InputTokens),
		OutputTokens:     max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:  max(a.CacheReadTokens, b.CacheReadTokens),
		CacheWriteTokens: max(a.CacheWriteTokens, b.CacheWriteTokens),
		ReasoningTokens:  max(a.ReasoningTokens, b.ReasoningTokens),
		CacheMissTokens:  max(a.CacheMissTokens, b.CacheMissTokens),
		Incomplete:       a.Incomplete || b.Incomplete,
	})
}

// Client is the single interface used by the agent loop.
type Client interface {
	Generate(ctx context.Context, req *Request) (*Response, error)
	Info() ProviderInfo
}

// ProviderInfo identifies a configured provider.
type ProviderInfo struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	// ContextWindow is the configured context size in tokens; zero is unknown.
	ContextWindow int `json:"context_window,omitempty"`
	// ForcedToolChoice reports that the agent loop may force submit_result
	// with tool_choice on the last turn (#259).
	ForcedToolChoice bool `json:"forced_tool_choice,omitempty"`
	// Unavailable marks a configured provider whose base URL cannot pass the
	// outbound egress policy. The process still starts; calls fail closed.
	Unavailable       bool   `json:"unavailable,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// ModelIdentity returns a stable identity for votes and challenger selection.
// Providers with the same endpoint and model are the same model even when
// their display names or API protocols differ. A missing BaseURL groups
// providers that share a model; the actual upstream default cannot be
// observed here, so this is deliberately conservative.
func (p ProviderInfo) ModelIdentity() string {
	model := strings.TrimSpace(p.Model)
	if model == "" {
		return ""
	}
	return normalizeEndpoint(p.BaseURL) + "\x1f" + model
}

func normalizeEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return strings.TrimSuffix(raw, "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// resolveRequest applies provider defaults without mutating the caller request.
func resolveRequest(cfg providerDefaults, req *Request) resolvedRequest {
	out := resolvedRequest{
		maxTokens:    cfg.maxTokens,
		temperature:  cfg.temperature,
		effort:       cfg.effort,
		budgetTokens: cfg.budgetTokens,
		thinkingType: cfg.thinkingType,
	}
	if req.MaxTokens > 0 {
		out.maxTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		out.temperature = req.Temperature
	}
	if req.Reasoning != nil {
		if req.Reasoning.Effort != "" {
			out.effort = req.Reasoning.Effort
		}
		if req.Reasoning.BudgetTokens > 0 {
			out.budgetTokens = req.Reasoning.BudgetTokens
		}
		if req.Reasoning.ThinkingType != "" {
			out.thinkingType = req.Reasoning.ThinkingType
		}
	}
	return out
}

// providerDefaults is the subset of config.ProviderConfig used by adapters.
type providerDefaults struct {
	maxTokens    int
	temperature  *float64
	effort       string
	budgetTokens int
	thinkingType string
	// Capability declarations. Adapters enforce them so a registry built
	// without ValidateProviderConfig (draft clients, embedders) stays safe.
	allowReasoningEffortNone bool
	preserveReasoningContent bool
	outputConfigEffort       bool
	responsesParams          string
	// strictRoleAlternation is honoured by the openai-chat adapter only.
	strictRoleAlternation bool
}

type resolvedRequest struct {
	maxTokens    int
	temperature  *float64
	effort       string
	budgetTokens int
	thinkingType string
}

// validateMaxTokens enforces the protocol-neutral bound. A zero value means
// "do not send"; the Anthropic adapter rejects it separately because its API
// requires a positive limit.
func validateMaxTokens(maxTokens int) *Error {
	if maxTokens < 0 {
		return badRequestf("max_tokens must not be negative")
	}
	return nil
}

// validateReasoningCapabilities enforces capability declarations that the
// adapters can send on the wire. Configuration validation applies the same
// rules, but a registry can be built without it.
func validateReasoningCapabilities(effective resolvedRequest, cfg providerDefaults) *Error {
	switch effective.thinkingType {
	case "", "enabled", "disabled":
	default:
		return badRequestf("thinking.type must be enabled or disabled")
	}
	if effective.effort == "none" && !cfg.allowReasoningEffortNone {
		return badRequestf("reasoning.effort none requires allow_reasoning_effort_none")
	}
	return nil
}

func joinBlocks(blocks []Block) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block.Text)
	}
	return b.String()
}

func partsText(parts []Part) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Type != PartText || part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

func validateRequest(req *Request) *Error {
	if req == nil {
		return badRequestf("request must not be nil")
	}
	if len(req.Messages) == 0 {
		return badRequestf("request must contain at least one message")
	}
	for i, msg := range req.Messages {
		switch msg.Role {
		case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		default:
			return badRequestf("messages[%d] has an invalid role", i)
		}
		for j, part := range msg.Parts {
			switch part.Type {
			case PartText:
			case PartToolCall:
				if msg.Role != RoleAssistant {
					return badRequestf("messages[%d].parts[%d]: tool calls require an assistant message", i, j)
				}
				if part.ToolCall == nil || part.ToolCall.ID == "" || part.ToolCall.Name == "" {
					return badRequestf("messages[%d].parts[%d]: tool call id and name are required", i, j)
				}
				input := bytes.TrimSpace(part.ToolCall.Input)
				if len(input) == 0 || input[0] != '{' || !json.Valid(part.ToolCall.Input) {
					return badRequestf("messages[%d].parts[%d]: tool call input must be a JSON object", i, j)
				}
			case PartToolResult:
				if msg.Role != RoleUser && msg.Role != RoleTool {
					return badRequestf("messages[%d].parts[%d]: tool results require a user or tool message", i, j)
				}
				if part.ToolResult == nil || part.ToolResult.CallID == "" {
					return badRequestf("messages[%d].parts[%d]: tool result call id is required", i, j)
				}
			case PartReasoning:
				if msg.Role != RoleAssistant {
					return badRequestf("messages[%d].parts[%d]: reasoning requires an assistant message", i, j)
				}
				if part.Reasoning == nil {
					return badRequestf("messages[%d].parts[%d]: reasoning payload is required", i, j)
				}
				if len(part.Reasoning.Raw) > 0 && !json.Valid(part.Reasoning.Raw) {
					return badRequestf("messages[%d].parts[%d]: reasoning payload is not valid JSON", i, j)
				}
			default:
				return badRequestf("messages[%d].parts[%d] has an invalid part type", i, j)
			}
		}
	}
	for i, tool := range req.Tools {
		if tool.Name == "" {
			return badRequestf("tools[%d]: name is required", i)
		}
		if len(tool.InputSchema) > 0 && !json.Valid(tool.InputSchema) {
			return badRequestf("tools[%d]: input schema is not valid JSON", i)
		}
	}
	return nil
}
