package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type anthropicAdapter struct {
	info ProviderInfo
	cfg  providerDefaults
	sdk  anthropic.Client
}

func newAnthropicAdapter(info ProviderInfo, cfg providerDefaults, apiKey string, httpClient *http.Client) *anthropicAdapter {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	}
	if info.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(info.BaseURL))
	}
	return &anthropicAdapter{info: info, cfg: cfg, sdk: anthropic.NewClient(opts...)}
}

func (a *anthropicAdapter) Generate(ctx context.Context, req *Request) (*Response, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	effective := resolveRequest(a.cfg, req)
	if effective.maxTokens <= 0 {
		return nil, badRequestf("anthropic requires max_tokens")
	}
	thinking := effective.budgetTokens > 0
	if thinking && forcedToolChoice(req.ToolChoice) {
		// Extended thinking is incompatible with forced tool use. Keep the
		// forced choice so the final submit round stays reliable.
		thinking = false
	}
	if thinking && effective.budgetTokens < 1024 {
		return nil, badRequestf("thinking budget_tokens must be at least 1024")
	}
	messages, err := anthropicMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.info.Model),
		MaxTokens: int64(effective.maxTokens),
		Messages:  messages,
		System:    anthropicSystem(req.System, req.Messages),
	}
	if thinking {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(int64(effective.budgetTokens))
		// Anthropic rejects temperature when thinking is enabled.
	} else if effective.temperature != nil {
		params.Temperature = anthropic.Float(*effective.temperature)
	}
	if len(req.Tools) > 0 {
		tools, err := anthropicTools(req.Tools)
		if err != nil {
			return nil, err
		}
		params.Tools = tools
		params.ToolChoice = anthropicToolChoice(req.ToolChoice)
	}
	message, err := a.sdk.Messages.New(ctx, params)
	if err != nil {
		return nil, sdkError(err)
	}
	return anthropicResponse(message, a.info.Model)
}

func anthropicSystem(blocks []Block, messages []Message) []anthropic.TextBlockParam {
	out := make([]anthropic.TextBlockParam, 0, len(blocks))
	for _, block := range blocks {
		if block.Text == "" {
			continue
		}
		param := anthropic.TextBlockParam{Text: block.Text}
		if block.Cache {
			param.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		out = append(out, param)
	}
	for _, msg := range messages {
		if msg.Role != RoleSystem {
			continue
		}
		if text := partsText(msg.Parts); text != "" {
			out = append(out, anthropic.TextBlockParam{Text: text})
		}
	}
	return out
}

func anthropicMessages(messages []Message) ([]anthropic.MessageParam, error) {
	out := make([]anthropic.MessageParam, 0, len(messages))
	for i, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			continue
		case RoleUser, RoleTool:
			var blocks []anthropic.ContentBlockParamUnion
			// Tool results must precede ordinary text in a user turn.
			for _, part := range msg.Parts {
				if part.Type == PartToolResult {
					blocks = append(blocks, anthropic.NewToolResultBlock(part.ToolResult.CallID, part.ToolResult.Content, part.ToolResult.IsError))
				}
			}
			for _, part := range msg.Parts {
				switch part.Type {
				case PartText:
					if part.Text != "" {
						blocks = append(blocks, anthropic.NewTextBlock(part.Text))
					}
				case PartToolResult:
				case PartToolCall, PartReasoning:
					return nil, badRequestf("messages[%d]: part is not valid in a %s message", i, msg.Role)
				}
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		case RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			for j, part := range msg.Parts {
				switch part.Type {
				case PartText:
					if part.Text != "" {
						blocks = append(blocks, anthropic.NewTextBlock(part.Text))
					}
				case PartReasoning:
					var block anthropic.ContentBlockParamUnion
					if err := json.Unmarshal(part.Reasoning.Raw, &block); err != nil {
						return nil, badRequestf("messages[%d].parts[%d]: reasoning block is not valid", i, j)
					}
					if block.OfThinking == nil && block.OfRedactedThinking == nil {
						return nil, badRequestf("messages[%d].parts[%d]: reasoning block is not a thinking block", i, j)
					}
					blocks = append(blocks, block)
				case PartToolCall:
					input := part.ToolCall.Input
					if len(input) == 0 {
						input = json.RawMessage(`{}`)
					}
					blocks = append(blocks, anthropic.NewToolUseBlock(part.ToolCall.ID, input, part.ToolCall.Name))
				case PartToolResult:
					return nil, badRequestf("messages[%d].parts[%d]: tool result is not valid in an assistant message", i, j)
				}
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewAssistantMessage(blocks...))
			}
		}
	}
	return out, nil
}

func anthropicTools(tools []ToolDef) ([]anthropic.ToolUnionParam, error) {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		raw := tool.InputSchema
		if len(raw) == 0 {
			raw = json.RawMessage(`{"type":"object"}`)
		}
		var schema anthropic.ToolInputSchemaParam
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, badRequestf("tool %q has an invalid input schema", tool.Name)
		}
		param := &anthropic.ToolParam{Name: tool.Name, InputSchema: schema}
		if tool.Description != "" {
			param.Description = anthropic.String(tool.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: param})
	}
	return out, nil
}

func anthropicToolChoice(choice string) anthropic.ToolChoiceUnionParam {
	switch choice {
	case "", "auto":
		return anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	case "required":
		return anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	case "none":
		return anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
	default:
		return anthropic.ToolChoiceParamOfTool(choice)
	}
}

func forcedToolChoice(choice string) bool {
	switch choice {
	case "", "auto", "none":
		return false
	}
	return true
}

func anthropicResponse(message *anthropic.Message, defaultModel string) (*Response, error) {
	if message == nil {
		return nil, &Error{Kind: ErrTransient, Err: errors.New("provider returned an empty response")}
	}
	out := Message{Role: RoleAssistant}
	hasToolCall := false
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				out.Parts = append(out.Parts, Part{Type: PartText, Text: block.Text})
			}
		case "thinking":
			out.Parts = append(out.Parts, Part{
				Type: PartReasoning,
				Reasoning: &Reasoning{
					Protocol: "anthropic",
					Raw:      json.RawMessage(block.RawJSON()),
					Summary:  block.Thinking,
				},
			})
		case "redacted_thinking":
			out.Parts = append(out.Parts, Part{
				Type: PartReasoning,
				Reasoning: &Reasoning{
					Protocol: "anthropic",
					Raw:      json.RawMessage(block.RawJSON()),
					Summary:  "[redacted]",
				},
			})
		case "tool_use":
			input := block.Input
			if !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			out.Parts = append(out.Parts, Part{
				Type:     PartToolCall,
				ToolCall: &ToolCall{ID: block.ID, Name: block.Name, Input: input},
			})
			hasToolCall = true
		}
	}
	stop := StopEndTurn
	switch message.StopReason {
	case anthropic.StopReasonToolUse:
		stop = StopToolUse
	case anthropic.StopReasonMaxTokens, anthropic.StopReasonModelContextWindowExceeded:
		stop = StopMaxTokens
	case anthropic.StopReasonRefusal:
		stop = StopRefusal
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		stop = StopEndTurn
	case anthropic.StopReasonPauseTurn:
		stop = StopOther
	default:
		stop = StopOther
	}
	if hasToolCall {
		stop = StopToolUse
	}
	model := string(message.Model)
	if model == "" {
		model = defaultModel
	}
	return &Response{
		Message:    out,
		StopReason: stop,
		Usage: Usage{
			// Anthropic reports uncached input, cache reads and cache creation separately.
			InputTokens:      message.Usage.InputTokens + message.Usage.CacheReadInputTokens + message.Usage.CacheCreationInputTokens,
			OutputTokens:     message.Usage.OutputTokens,
			CacheReadTokens:  message.Usage.CacheReadInputTokens,
			CacheWriteTokens: message.Usage.CacheCreationInputTokens,
			ReasoningTokens:  message.Usage.OutputTokensDetails.ThinkingTokens,
		},
		Model:     model,
		RequestID: message.ID,
	}, nil
}
