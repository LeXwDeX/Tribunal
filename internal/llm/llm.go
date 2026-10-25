// Package llm defines the protocol-neutral client used by agents and DAG
// nodes. It also contains adapters for the OpenAI Chat Completions, OpenAI
// Responses and Anthropic Messages protocols.
//
// The package deliberately depends only on internal/config and the official
// provider SDKs. It never imports agent, review or store.
package llm

import (
	"context"
	"encoding/json"
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
	Effort       string `json:"effort,omitempty"`        // low | medium | high (Responses)
	BudgetTokens int    `json:"budget_tokens,omitempty"` // Anthropic thinking budget
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
	Model      string     `json:"model,omitempty"`
	RequestID  string     `json:"request_id,omitempty"`
}

// Usage reports token consumption. CacheWriteTokens is reported by providers
// that expose cache writes separately.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
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
	}
	return out
}

// providerDefaults is the subset of config.ProviderConfig used by adapters.
type providerDefaults struct {
	maxTokens    int
	temperature  *float64
	effort       string
	budgetTokens int
}

type resolvedRequest struct {
	maxTokens    int
	temperature  *float64
	effort       string
	budgetTokens int
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
				if len(part.ToolCall.Input) > 0 && !json.Valid(part.ToolCall.Input) {
					return badRequestf("messages[%d].parts[%d]: tool call input is not valid JSON", i, j)
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
