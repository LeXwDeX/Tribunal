package llm

import (
	"reflect"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// Preset IDs for the shipped model presets.
const (
	PresetDeepSeekV4ProMax = "deepseek-v4-pro-max"
	PresetDeepSeekFlashMax = "deepseek-flash-max"
)

// DeepSeek preset facts were verified against the official model table on
// VerifiedAt. A preset value is a capability contract: changing any number
// requires a new verification date, a matching SourceURL and an update to
// docs/operations.md and docs/design/06-config.md (#405).
const (
	deepSeekSourceURL  = "https://api-docs.deepseek.com/quick_start/pricing"
	deepSeekVerifiedAt = "2026-11-11"
)

// ModelPreset is a verified model definition shipped with the server. It
// contains no credential and no price; cost fields are owned by #407.
//
// ContextWindow is the input window after reserving the maximum output:
// 1,048,576 total context minus 393,216 max output = 655,360.
type ModelPreset struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name"`
	Protocol    string            `json:"protocol"`
	BaseURL     string            `json:"base_url"`
	Model       string            `json:"model"`
	Params      store.ModelParams `json:"params"`
	SourceURL   string            `json:"source_url"`
	VerifiedAt  string            `json:"verified_at"`
}

// PresetChange names one model parameter a preset would change, with the
// values for the preview response. Audit logs keep only the field names.
type PresetChange struct {
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

// PresetNodeOverrideFields lists the node-override parameters that shadow a
// preset. A stored profile override on one of them keeps the node below the
// preset values until an administrator removes it.
func PresetNodeOverrideFields() []string {
	return []string{"max_tokens", "reasoning", "thinking"}
}

// Presets returns the shipped presets in a stable order. The result is a
// deep copy, so callers cannot mutate the shipped values.
func Presets() []ModelPreset {
	return []ModelPreset{deepSeekPreset(PresetDeepSeekV4ProMax, "DeepSeek V4 Pro (maximum)", "deepseek-v4-pro", 500),
		deepSeekPreset(PresetDeepSeekFlashMax, "DeepSeek Flash (maximum)", "deepseek-flash", 2500)}
}

// Preset returns one shipped preset by ID.
func Preset(id string) (ModelPreset, bool) {
	for _, preset := range Presets() {
		if preset.ID == id {
			return preset, true
		}
	}
	return ModelPreset{}, false
}

func deepSeekPreset(id, displayName, model string, maxConcurrency int) ModelPreset {
	return ModelPreset{
		ID: id, DisplayName: displayName, Protocol: "openai-chat",
		BaseURL: "https://api.deepseek.com", Model: model,
		Params: store.ModelParams{
			MaxTokens: 393216, ContextWindow: 655360,
			Reasoning:                &config.ReasoningConfig{Effort: "max"},
			ForcedToolChoice:         "unsupported",
			StrictRoleAlternation:    false,
			MaxConcurrency:           maxConcurrency,
			RPM:                      0,
			PreserveReasoningContent: true,
		},
		SourceURL: deepSeekSourceURL, VerifiedAt: deepSeekVerifiedAt,
	}
}

// PatchFor returns the merge patch that applies the preset to current and the
// parameter changes it makes. Fields already at the preset value are omitted,
// so applying the preset twice changes nothing. The patch never contains the
// endpoint, model name or credential; an explicit thinking switch is cleared
// so a stored "disabled" cannot override the preset effort.
func (p ModelPreset) PatchFor(current store.ModelParams) (store.ModelParamsPatch, []PresetChange) {
	patch := store.ModelParamsPatch{}
	changes := []PresetChange{}
	add := func(field string, from, to any) {
		changes = append(changes, PresetChange{Field: field, From: from, To: to})
	}
	if current.MaxTokens != p.Params.MaxTokens {
		value := p.Params.MaxTokens
		patch.MaxTokens = &value
		add("params.max_tokens", current.MaxTokens, value)
	}
	if current.ContextWindow != p.Params.ContextWindow {
		value := p.Params.ContextWindow
		patch.ContextWindow = &value
		add("params.context_window", current.ContextWindow, value)
	}
	if !reflect.DeepEqual(current.Reasoning, p.Params.Reasoning) {
		if p.Params.Reasoning != nil {
			value := *p.Params.Reasoning
			patch.Reasoning = store.ParamUpdate[config.ReasoningConfig]{Set: true, Value: &value}
			add("params.reasoning", current.Reasoning, value)
		} else {
			patch.Reasoning = store.ParamUpdate[config.ReasoningConfig]{Set: true}
			add("params.reasoning", current.Reasoning, nil)
		}
	}
	if current.MaxConcurrency != p.Params.MaxConcurrency {
		value := p.Params.MaxConcurrency
		patch.MaxConcurrency = &value
		add("params.max_concurrency", current.MaxConcurrency, value)
	}
	if current.RPM != p.Params.RPM {
		value := p.Params.RPM
		patch.RPM = &value
		add("params.rpm", current.RPM, value)
	}
	if current.ForcedToolChoice != p.Params.ForcedToolChoice {
		value := p.Params.ForcedToolChoice
		patch.ForcedToolChoice = &value
		add("params.forced_tool_choice", current.ForcedToolChoice, value)
	}
	if current.StrictRoleAlternation != p.Params.StrictRoleAlternation {
		value := p.Params.StrictRoleAlternation
		patch.StrictRoleAlternation = &value
		add("params.strict_role_alternation", current.StrictRoleAlternation, value)
	}
	if current.PreserveReasoningContent != p.Params.PreserveReasoningContent {
		value := p.Params.PreserveReasoningContent
		patch.PreserveReasoningContent = &value
		add("params.preserve_reasoning_content", current.PreserveReasoningContent, value)
	}
	if p.Params.Thinking == nil && current.Thinking != nil {
		patch.Thinking = store.ParamUpdate[config.ThinkingConfig]{Set: true}
		add("params.thinking", current.Thinking, nil)
	} else if p.Params.Thinking != nil && !reflect.DeepEqual(current.Thinking, p.Params.Thinking) {
		value := *p.Params.Thinking
		patch.Thinking = store.ParamUpdate[config.ThinkingConfig]{Set: true, Value: &value}
		add("params.thinking", current.Thinking, value)
	}
	return patch, changes
}
