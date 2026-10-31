package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

type openAIChatAdapter struct {
	info ProviderInfo
	cfg  providerDefaults
	sdk  openai.Client
}

func newOpenAIChatAdapter(info ProviderInfo, cfg providerDefaults, apiKey string, httpClient *http.Client) *openAIChatAdapter {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	}
	if info.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(info.BaseURL))
	}
	return &openAIChatAdapter{info: info, cfg: cfg, sdk: openai.NewClient(opts...)}
}

func (a *openAIChatAdapter) Generate(ctx context.Context, req *Request) (*Response, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	effective := resolveRequest(a.cfg, req)
	messages, err := chatMessages(req)
	if err != nil {
		return nil, err
	}
	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(a.info.Model),
		Messages: messages,
	}
	if effective.maxTokens > 0 {
		// max_tokens is the most widely supported spelling on compatible
		// endpoints (vLLM, Ollama, hosted gateways).
		params.MaxTokens = openai.Int(int64(effective.maxTokens))
	}
	if effective.temperature != nil {
		params.Temperature = openai.Float(*effective.temperature)
	}
	if effective.effort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(effective.effort)
	}
	if len(req.Tools) > 0 {
		tools, err := chatTools(req.Tools)
		if err != nil {
			return nil, err
		}
		params.Tools = tools
		params.ToolChoice = chatToolChoice(req.ToolChoice)
	}
	completion, err := a.sdk.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, sdkError(err)
	}
	return chatResponse(completion, a.info.Model)
}

func chatMessages(req *Request) ([]openai.ChatCompletionMessageParamUnion, error) {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if text := joinBlocks(req.System); text != "" {
		messages = append(messages, openai.SystemMessage(text))
	}
	for _, msg := range req.Messages {
		switch msg.Role {
		case RoleSystem:
			if text := partsText(msg.Parts); text != "" {
				messages = append(messages, openai.SystemMessage(text))
			}
		case RoleUser:
			var results []openai.ChatCompletionMessageParamUnion
			for _, part := range msg.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil {
					results = append(results, openai.ToolMessage(part.ToolResult.Content, part.ToolResult.CallID))
				}
			}
			if text := partsText(msg.Parts); text != "" {
				results = append(results, openai.UserMessage(text))
			}
			messages = append(messages, results...)
		case RoleTool:
			for _, part := range msg.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil {
					messages = append(messages, openai.ToolMessage(part.ToolResult.Content, part.ToolResult.CallID))
				}
			}
		case RoleAssistant:
			assistant := openai.ChatCompletionAssistantMessageParam{}
			hasContent := false
			if text := partsText(msg.Parts); text != "" {
				assistant.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(text),
				}
				hasContent = true
			}
			var calls []openai.ChatCompletionMessageToolCallUnionParam
			for _, part := range msg.Parts {
				if part.Type != PartToolCall || part.ToolCall == nil {
					continue
				}
				calls = append(calls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: part.ToolCall.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      part.ToolCall.Name,
							Arguments: string(part.ToolCall.Input),
						},
					},
				})
			}
			if len(calls) > 0 {
				assistant.ToolCalls = calls
				hasContent = true
			}
			// Chat Completions rejects unsupported reasoning history; drop it.
			if hasContent {
				messages = append(messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
			}
		}
	}
	return messages, nil
}

func chatTools(tools []ToolDef) ([]openai.ChatCompletionToolUnionParam, error) {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		var parameters map[string]any
		if len(tool.InputSchema) > 0 {
			if err := json.Unmarshal(tool.InputSchema, &parameters); err != nil {
				return nil, badRequestf("tool %q has an invalid input schema", tool.Name)
			}
		}
		definition := shared.FunctionDefinitionParam{
			Name:       tool.Name,
			Parameters: parameters,
		}
		if tool.Description != "" {
			definition.Description = openai.String(tool.Description)
		}
		out = append(out, openai.ChatCompletionFunctionTool(definition))
	}
	return out, nil
}

func chatToolChoice(choice string) openai.ChatCompletionToolChoiceOptionUnionParam {
	switch choice {
	case "", "auto":
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String(string(openai.ChatCompletionToolChoiceOptionAutoAuto)),
		}
	case "required":
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String(string(openai.ChatCompletionToolChoiceOptionAutoRequired)),
		}
	case "none":
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String(string(openai.ChatCompletionToolChoiceOptionAutoNone)),
		}
	default:
		return openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{
			Name: choice,
		})
	}
}

func chatResponse(completion *openai.ChatCompletion, defaultModel string) (*Response, error) {
	usage := Usage{}
	if completion != nil {
		usage = chatUsage(completion.Usage)
	}
	if completion == nil || len(completion.Choices) == 0 {
		return nil, &Error{Kind: ErrTransient, Err: errors.New("provider returned no choices"), Usage: usage}
	}
	choice := completion.Choices[0]
	message := Message{Role: RoleAssistant}
	if choice.Message.Content != "" {
		message.Parts = append(message.Parts, Part{Type: PartText, Text: choice.Message.Content})
	}
	if choice.Message.Refusal != "" {
		message.Parts = append(message.Parts, Part{Type: PartText, Text: choice.Message.Refusal})
	}
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" {
			// An unknown or missing type cannot be executed. Keep an invalid
			// marker so the whole batch fails closed instead of dropping it.
			message.Parts = append(message.Parts, Part{Type: PartToolCall, ToolCall: &ToolCall{}})
			continue
		}
		fn := call.AsFunction()
		message.Parts = append(message.Parts, Part{
			Type: PartToolCall,
			ToolCall: &ToolCall{
				ID:    fn.ID,
				Name:  fn.Function.Name,
				Input: json.RawMessage(fn.Function.Arguments),
			},
		})
	}
	stop := StopOther
	switch choice.FinishReason {
	case "stop":
		stop = StopEndTurn
	case "tool_calls", "function_call":
		stop = StopToolUse
	case "length":
		stop = StopMaxTokens
	case "content_filter":
		stop = StopRefusal
	}
	if choice.Message.Refusal != "" {
		stop = StopRefusal
	}
	message, _ = SanitizeToolCalls(message, stop == StopToolUse)
	model := completion.Model
	if model == "" {
		model = defaultModel
	}
	return &Response{
		Message:    message,
		StopReason: stop,
		Usage:      usage,
		Model:      model,
		RequestID:  completion.ID,
	}, nil
}

func chatUsage(usage openai.CompletionUsage) Usage {
	return Usage{
		InputTokens:      usage.PromptTokens,
		OutputTokens:     usage.CompletionTokens,
		CacheReadTokens:  usage.PromptTokensDetails.CachedTokens,
		CacheWriteTokens: usage.PromptTokensDetails.CacheWriteTokens,
		ReasoningTokens:  usage.CompletionTokensDetails.ReasoningTokens,
	}
}
