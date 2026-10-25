package llm

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

type openAIResponsesAdapter struct {
	info ProviderInfo
	cfg  providerDefaults
	sdk  openai.Client
}

func newOpenAIResponsesAdapter(info ProviderInfo, cfg providerDefaults, apiKey string, httpClient *http.Client) *openAIResponsesAdapter {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	}
	if info.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(info.BaseURL))
	}
	return &openAIResponsesAdapter{info: info, cfg: cfg, sdk: openai.NewClient(opts...)}
}

func (a *openAIResponsesAdapter) Generate(ctx context.Context, req *Request) (*Response, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	effective := resolveRequest(a.cfg, req)
	input, err := responsesInput(req)
	if err != nil {
		return nil, err
	}
	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(a.info.Model),
		Store: openai.Bool(false),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		Include: []responses.ResponseIncludable{
			responses.ResponseIncludableReasoningEncryptedContent,
		},
	}
	// Stateless mode: every turn sends the full explicit item history.
	// previous_response_id is intentionally never set.
	if text := joinBlocks(req.System); text != "" {
		params.Instructions = openai.String(text)
	}
	if effective.maxTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(effective.maxTokens))
	}
	if effective.temperature != nil {
		params.Temperature = openai.Float(*effective.temperature)
	}
	if effective.effort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(effective.effort)}
	}
	if len(req.Tools) > 0 {
		tools, err := responsesTools(req.Tools)
		if err != nil {
			return nil, err
		}
		params.Tools = tools
		params.ToolChoice = responsesToolChoice(req.ToolChoice)
	}
	// X-Session-Id is not part of the OpenAI API. The compatibility gateway
	// uses it to isolate provider state per attempt, so every attempt gets a
	// fresh opaque value.
	generated, err := a.sdk.Responses.New(ctx, params, option.WithHeader("X-Session-Id", rand.Text()))
	if err != nil {
		return nil, sdkError(err)
	}
	return responsesResponse(generated, a.info.Model)
}

func responsesInput(req *Request) (responses.ResponseInputParam, error) {
	items := make(responses.ResponseInputParam, 0, len(req.Messages))
	for i, msg := range req.Messages {
		switch msg.Role {
		case RoleSystem:
			if text := partsText(msg.Parts); text != "" {
				items = append(items, responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleSystem))
			}
		case RoleUser, RoleTool:
			for j, part := range msg.Parts {
				switch part.Type {
				case PartText:
					if part.Text != "" {
						items = append(items, responses.ResponseInputItemParamOfMessage(part.Text, responses.EasyInputMessageRoleUser))
					}
				case PartToolResult:
					items = append(items, responsesFunctionOutput(part.ToolResult))
				case PartReasoning, PartToolCall:
					return nil, badRequestf("messages[%d].parts[%d]: part is not valid in a %s message", i, j, msg.Role)
				}
			}
		case RoleAssistant:
			for j, part := range msg.Parts {
				switch part.Type {
				case PartText:
					if part.Text != "" {
						items = append(items, responses.ResponseInputItemParamOfMessage(part.Text, responses.EasyInputMessageRoleAssistant))
					}
				case PartToolCall:
					input := part.ToolCall.Input
					if len(input) == 0 {
						input = json.RawMessage(`{}`)
					}
					items = append(items, responses.ResponseInputItemParamOfFunctionCall(string(input), part.ToolCall.ID, part.ToolCall.Name))
				case PartReasoning:
					var item responses.ResponseInputItemUnionParam
					if len(part.Reasoning.Raw) > 0 {
						if err := json.Unmarshal(part.Reasoning.Raw, &item); err != nil {
							return nil, badRequestf("messages[%d].parts[%d]: reasoning item is not a valid Responses item", i, j)
						}
					}
					if item.OfReasoning == nil {
						return nil, badRequestf("messages[%d].parts[%d]: reasoning item is missing", i, j)
					}
					items = append(items, item)
				case PartToolResult:
					return nil, badRequestf("messages[%d].parts[%d]: tool result is not valid in an assistant message", i, j)
				}
			}
		}
	}
	return items, nil
}

func responsesFunctionOutput(result *ToolResult) responses.ResponseInputItemUnionParam {
	return responses.ResponseInputItemUnionParam{
		OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
			CallID: openai.String(result.CallID),
			Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
				OfString: openai.String(result.Content),
			},
		},
	}
}

func responsesTools(tools []ToolDef) ([]responses.ToolUnionParam, error) {
	out := make([]responses.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		parameters := map[string]any{}
		if len(tool.InputSchema) > 0 {
			if err := json.Unmarshal(tool.InputSchema, &parameters); err != nil {
				return nil, badRequestf("tool %q has an invalid input schema", tool.Name)
			}
		}
		function := &responses.FunctionToolParam{
			Name:       tool.Name,
			Parameters: parameters,
		}
		if tool.Description != "" {
			function.Description = openai.String(tool.Description)
		}
		out = append(out, responses.ToolUnionParam{OfFunction: function})
	}
	return out, nil
}

func responsesToolChoice(choice string) responses.ResponseNewParamsToolChoiceUnion {
	switch choice {
	case "", "auto":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsAuto),
		}
	case "required":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsRequired),
		}
	case "none":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsNone),
		}
	default:
		return responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: choice},
		}
	}
}

func responsesResponse(generated *responses.Response, defaultModel string) (*Response, error) {
	if generated == nil {
		return nil, &Error{Kind: ErrTransient, Err: errors.New("provider returned an empty response")}
	}
	if generated.Status == responses.ResponseStatusFailed {
		return nil, &Error{Kind: ErrTransient, Err: errors.New("provider reported a failed response")}
	}
	message := Message{Role: RoleAssistant}
	refused := false
	for _, item := range generated.Output {
		switch item.Type {
		case "message":
			var text strings.Builder
			for _, content := range item.Content {
				switch content.Type {
				case "output_text":
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(content.Text)
				case "refusal":
					refused = true
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(content.Refusal)
				}
			}
			if text.Len() > 0 {
				message.Parts = append(message.Parts, Part{Type: PartText, Text: text.String()})
			}
		case "function_call":
			fn := item.AsFunctionCall()
			input := json.RawMessage(fn.Arguments)
			if !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			message.Parts = append(message.Parts, Part{
				Type:     PartToolCall,
				ToolCall: &ToolCall{ID: fn.CallID, Name: fn.Name, Input: input},
			})
		case "reasoning":
			raw := json.RawMessage(item.RawJSON())
			summary := reasoningSummary(item.Summary)
			message.Parts = append(message.Parts, Part{
				Type: PartReasoning,
				Reasoning: &Reasoning{
					Protocol: "openai-responses",
					Raw:      raw,
					Summary:  summary,
				},
			})
		}
	}
	stop := StopEndTurn
	hasToolCall := false
	for _, part := range message.Parts {
		if part.Type == PartToolCall {
			hasToolCall = true
			break
		}
	}
	switch {
	case hasToolCall:
		stop = StopToolUse
	case refused:
		stop = StopRefusal
	case generated.Status == responses.ResponseStatusIncomplete && generated.IncompleteDetails.Reason == "max_output_tokens":
		stop = StopMaxTokens
	}
	model := string(generated.Model)
	if model == "" {
		model = defaultModel
	}
	return &Response{
		Message:    message,
		StopReason: stop,
		Usage: Usage{
			InputTokens:      generated.Usage.InputTokens,
			OutputTokens:     generated.Usage.OutputTokens,
			CacheReadTokens:  generated.Usage.InputTokensDetails.CachedTokens,
			CacheWriteTokens: generated.Usage.InputTokensDetails.CacheWriteTokens,
			ReasoningTokens:  generated.Usage.OutputTokensDetails.ReasoningTokens,
		},
		Model:     model,
		RequestID: generated.ID,
	}, nil
}

func reasoningSummary(summary []responses.ResponseReasoningItemSummary) string {
	var b strings.Builder
	for _, item := range summary {
		if item.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(item.Text)
	}
	return b.String()
}
