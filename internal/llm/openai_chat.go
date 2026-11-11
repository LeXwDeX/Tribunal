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
	resp, _, err := a.GenerateWithWire(ctx, req)
	return resp, err
}

func (a *openAIChatAdapter) GenerateWithWire(ctx context.Context, req *Request) (*Response, *WireParams, error) {
	if err := validateRequest(req); err != nil {
		return nil, nil, err
	}
	effective := resolveRequest(a.cfg, req)
	if err := validateMaxTokens(effective.maxTokens); err != nil {
		return nil, nil, err
	}
	if err := validateReasoningCapabilities(effective, a.cfg); err != nil {
		return nil, nil, err
	}
	messages, reasoningContents, err := chatMessagesWithReasoning(req, a.cfg.strictRoleAlternation, a.cfg.preserveReasoningContent)
	if err != nil {
		return nil, nil, err
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
			return nil, nil, err
		}
		params.Tools = tools
		params.ToolChoice = chatToolChoice(req.ToolChoice)
	}
	wire := &WireParams{Temperature: effective.temperature}
	if effective.maxTokens > 0 {
		wire.MaxTokens = effective.maxTokens
	}
	if effective.effort != "" {
		wire.Reasoning = &WireReasoning{Effort: effective.effort}
	}
	var requestOptions []option.RequestOption
	if effective.thinkingType != "" {
		requestOptions = append(requestOptions, option.WithJSONSet("thinking.type", effective.thinkingType))
		wire.ThinkingType = effective.thinkingType
		wire.ThinkingEnabled = effective.thinkingType == "enabled"
	}
	if a.cfg.preserveReasoningContent {
		replayedMessages, replayed, err := chatMessagesReplayingReasoning(messages, reasoningContents)
		if err != nil {
			return nil, nil, err
		}
		if replayed {
			requestOptions = append(requestOptions, option.WithJSONSet("messages", replayedMessages))
			wire.ReasoningContentReplayed = true
		}
	}
	completion, err := a.sdk.Chat.Completions.New(ctx, params, requestOptions...)
	if err != nil {
		return nil, wire, sdkError(err)
	}
	response, err := chatResponseWithReasoning(completion, a.info.Model, a.cfg.preserveReasoningContent)
	return response, wire, err
}

// strictSystemNoticeHeader labels a prompt folded into a tool message so the
// model can tell it apart from the tool output it follows.
const strictSystemNoticeHeader = "System notice (from Tribunal, not tool output):"

// appendChatUserText adds system-authored user text without producing two
// adjacent user messages, which strict gateways and chat templates reject
// (#262). With strictRoleAlternation it also avoids a tool -> user sequence by
// appending the text, labelled as a system notice, after the full content of
// the last tool message, outside any untrusted-data envelope inside it.
func appendChatUserText(messages []openai.ChatCompletionMessageParamUnion, text string, strictRoleAlternation bool) []openai.ChatCompletionMessageParamUnion {
	if n := len(messages); n > 0 {
		last := messages[n-1]
		if last.OfUser != nil && last.OfUser.Content.OfString.Valid() {
			merged := *last.OfUser
			merged.Content.OfString = openai.String(merged.Content.OfString.Value + "\n\n" + text)
			messages[n-1] = openai.ChatCompletionMessageParamUnion{OfUser: &merged}
			return messages
		}
		if strictRoleAlternation && last.OfTool != nil && last.OfTool.Content.OfString.Valid() {
			folded := *last.OfTool
			folded.Content.OfString = openai.String(folded.Content.OfString.Value + "\n\n" + strictSystemNoticeHeader + "\n" + text)
			messages[n-1] = openai.ChatCompletionMessageParamUnion{OfTool: &folded}
			return messages
		}
	}
	return append(messages, openai.UserMessage(text))
}

func chatMessages(req *Request, strictRoleAlternation bool) ([]openai.ChatCompletionMessageParamUnion, error) {
	messages, _, err := chatMessagesWithReasoning(req, strictRoleAlternation, false)
	return messages, err
}

// chatMessagesWithReasoning builds the wire messages. With preserve, it also
// returns one reasoning_content value per message (empty when absent) so the
// caller can replay DeepSeek thinking parts on later assistant turns.
func chatMessagesWithReasoning(req *Request, strictRoleAlternation, preserve bool) ([]openai.ChatCompletionMessageParamUnion, []string, error) {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	reasoning := make([]string, 0, len(req.Messages)+1)
	appendMessage := func(message openai.ChatCompletionMessageParamUnion) {
		messages = append(messages, message)
		reasoning = append(reasoning, "")
	}
	if text := joinBlocks(req.System); text != "" {
		appendMessage(openai.SystemMessage(text))
	}
	for _, msg := range req.Messages {
		switch msg.Role {
		case RoleSystem:
			if text := partsText(msg.Parts); text != "" {
				appendMessage(openai.SystemMessage(text))
			}
		case RoleUser:
			for _, part := range msg.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil {
					appendMessage(openai.ToolMessage(part.ToolResult.Content, part.ToolResult.CallID))
				}
			}
			if text := partsText(msg.Parts); text != "" {
				messages = appendChatUserText(messages, text, strictRoleAlternation)
				for len(reasoning) < len(messages) {
					reasoning = append(reasoning, "")
				}
			}
		case RoleTool:
			for _, part := range msg.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil {
					appendMessage(openai.ToolMessage(part.ToolResult.Content, part.ToolResult.CallID))
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
			content := ""
			if preserve {
				var err error
				content, err = chatReasoningContent(msg.Parts)
				if err != nil {
					return nil, nil, err
				}
			}
			// Without a declared capability, Chat Completions reasoning history
			// is dropped because compatible endpoints reject it.
			if hasContent {
				messages = append(messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
				reasoning = append(reasoning, content)
			}
		}
	}
	return messages, reasoning, nil
}

// chatReasoningContent extracts the reasoning_content this adapter stored on a
// previous assistant turn. Other protocols' reasoning parts stay dropped.
func chatReasoningContent(parts []Part) (string, error) {
	for _, part := range parts {
		if part.Type != PartReasoning || part.Reasoning == nil || part.Reasoning.Protocol != "openai-chat" {
			continue
		}
		var payload struct {
			ReasoningContent string `json:"reasoning_content"`
		}
		if err := json.Unmarshal(part.Reasoning.Raw, &payload); err != nil {
			return "", badRequestf("reasoning content is not valid JSON")
		}
		if payload.ReasoningContent != "" {
			return payload.ReasoningContent, nil
		}
	}
	return "", nil
}

// chatMessagesReplayingReasoning reserializes messages with reasoning_content
// on the assistant turns that carry it. It reports false when there is nothing
// to replay, so the request body stays byte-identical without the capability.
func chatMessagesReplayingReasoning(messages []openai.ChatCompletionMessageParamUnion, reasoning []string) ([]map[string]any, bool, error) {
	replayed := false
	for _, content := range reasoning {
		if content != "" {
			replayed = true
			break
		}
	}
	if !replayed {
		return nil, false, nil
	}
	out := make([]map[string]any, len(messages))
	for i, message := range messages {
		raw, err := json.Marshal(message)
		if err != nil {
			return nil, false, badRequestf("message history cannot be serialized")
		}
		object := map[string]any{}
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, false, badRequestf("message history cannot be serialized")
		}
		if i < len(reasoning) && reasoning[i] != "" {
			object["reasoning_content"] = reasoning[i]
		}
		out[i] = object
	}
	return out, true, nil
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
	return chatResponseWithReasoning(completion, defaultModel, false)
}

// chatResponseWithReasoning additionally keeps provider reasoning_content as a
// reasoning part when the provider declares preserve_reasoning_content.
func chatResponseWithReasoning(completion *openai.ChatCompletion, defaultModel string, preserveReasoning bool) (*Response, error) {
	usage := Usage{}
	usageComplete := true
	if completion != nil {
		usage, usageComplete = chatUsage(completion.Usage, completion.RawJSON())
	}
	if !usageComplete {
		return nil, &Error{Kind: ErrUnsafeResponse, Err: errors.New("provider returned invalid token usage"), Usage: usage, responseObserved: true}
	}
	if completion == nil || len(completion.Choices) == 0 {
		return nil, &Error{Kind: ErrTransient, Err: errors.New("provider returned no choices"), Usage: usage, responseObserved: completion != nil}
	}
	choice := completion.Choices[0]
	message := Message{Role: RoleAssistant}
	if reasoning, ok := chatReasoningPart(completion, preserveReasoning); ok {
		message.Parts = append(message.Parts, reasoning)
	}
	if choice.Message.Content != "" {
		message.Parts = append(message.Parts, Part{Type: PartText, Text: choice.Message.Content})
	}
	if choice.Message.Refusal != "" {
		message.Parts = append(message.Parts, Part{Type: PartText, Text: choice.Message.Refusal})
	}
	// The SDK can silently discard a malformed tool_calls value. Keep an
	// invalid call marker rather than treating the turn as ordinary text.
	if malformedWireList(choice.Message.JSON.ToolCalls.Raw(), len(choice.Message.ToolCalls), false) {
		message.Parts = append(message.Parts, invalidToolCall(ToolCallMalformedList))
	}
	if raw := choice.Message.JSON.FunctionCall.Raw(); raw != "" && raw != "null" {
		message.Parts = append(message.Parts, invalidToolCall(ToolCallUnsupported))
	}
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" {
			// An unknown or missing type cannot be executed. Keep an invalid
			// marker so the whole batch fails closed instead of dropping it.
			message.Parts = append(message.Parts, invalidToolCall(ToolCallUnsupported))
			continue
		}
		fn := call.AsFunction()
		if !wireString(fn.JSON.ID.Raw()) || !wireString(fn.Function.JSON.Name.Raw()) || !wireString(fn.Function.JSON.Arguments.Raw()) {
			message.Parts = append(message.Parts, invalidToolCall(ToolCallNonString))
			continue
		}
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

// chatReasoningPart reads the DeepSeek Chat Completions reasoning_content
// extension from the provider's original JSON. The SDK types do not model the
// field, so only a declared provider preserves it.
func chatReasoningPart(completion *openai.ChatCompletion, preserve bool) (Part, bool) {
	if !preserve || completion == nil {
		return Part{}, false
	}
	var raw struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(completion.RawJSON()), &raw); err != nil || len(raw.Choices) == 0 ||
		raw.Choices[0].Message.ReasoningContent == "" {
		return Part{}, false
	}
	payload, err := json.Marshal(struct {
		ReasoningContent string `json:"reasoning_content"`
	}{ReasoningContent: raw.Choices[0].Message.ReasoningContent})
	if err != nil {
		return Part{}, false
	}
	return Part{Type: PartReasoning, Reasoning: &Reasoning{Protocol: "openai-chat", Raw: payload}}, true
}

func chatUsage(usage openai.CompletionUsage, responseRaw string) (Usage, bool) {
	if raw := usage.RawJSON(); raw != "" || responseRaw != "" {
		return wireUsageFromResponse(responseRaw, raw, chatUsageFields)
	}
	converted := Usage{
		InputTokens:      usage.PromptTokens,
		OutputTokens:     usage.CompletionTokens,
		CacheReadTokens:  usage.PromptTokensDetails.CachedTokens,
		CacheWriteTokens: usage.PromptTokensDetails.CacheWriteTokens,
		ReasoningTokens:  usage.CompletionTokensDetails.ReasoningTokens,
	}
	return converted, ValidUsage(converted)
}
