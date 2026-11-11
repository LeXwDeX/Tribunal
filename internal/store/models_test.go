package store

import (
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestMergeModelParamsPrefersExplicitValuesAndReportsSources(t *testing.T) {
	explicitTemperature := 0.0
	explicitReasoning := &config.ReasoningConfig{Effort: "low"}
	defaultTemperature := 0.7
	defaultReasoning := &config.ReasoningConfig{Effort: "high"}
	defaultThinking := &config.ThinkingConfig{BudgetTokens: 2048, Type: "enabled"}
	explicit := ModelParams{
		MaxTokens:                100,
		Temperature:              &explicitTemperature,
		Reasoning:                explicitReasoning,
		ForcedToolChoice:         "supported",
		StrictRoleAlternation:    true,
		AllowReasoningEffortNone: false,
	}
	defaults := ModelParams{
		MaxTokens:                4096,
		ContextWindow:            128000,
		Temperature:              &defaultTemperature,
		Reasoning:                defaultReasoning,
		Thinking:                 defaultThinking,
		ForcedToolChoice:         "unsupported",
		StrictRoleAlternation:    false,
		MaxConcurrency:           4,
		RPM:                      60,
		AllowReasoningEffortNone: true,
		PreserveReasoningContent: true,
		OutputConfigEffort:       true,
		ResponsesParams:          "unsupported",
	}
	merged, sources := MergeModelParams(explicit, defaults)
	if merged.MaxTokens != 100 || sources["max_tokens"] != ParamSourceModel {
		t.Fatalf("max_tokens = %d (%s), want explicit 100", merged.MaxTokens, sources["max_tokens"])
	}
	if merged.ContextWindow != 128000 || sources["context_window"] != ParamSourceDefault {
		t.Fatalf("context_window = %d (%s), want default 128000", merged.ContextWindow, sources["context_window"])
	}
	// An explicit zero temperature is a real value and must win.
	if merged.Temperature == nil || *merged.Temperature != 0 || sources["temperature"] != ParamSourceModel {
		t.Fatalf("temperature = %v (%s), want explicit 0", merged.Temperature, sources["temperature"])
	}
	if merged.Reasoning != explicitReasoning || sources["reasoning"] != ParamSourceModel {
		t.Fatalf("reasoning = %+v (%s), want explicit object", merged.Reasoning, sources["reasoning"])
	}
	if merged.Thinking != defaultThinking || sources["thinking"] != ParamSourceDefault {
		t.Fatalf("thinking = %+v (%s), want default object", merged.Thinking, sources["thinking"])
	}
	if merged.ForcedToolChoice != "supported" || sources["forced_tool_choice"] != ParamSourceModel {
		t.Fatalf("forced_tool_choice = %q (%s)", merged.ForcedToolChoice, sources["forced_tool_choice"])
	}
	if !merged.StrictRoleAlternation || sources["strict_role_alternation"] != ParamSourceModel {
		t.Fatalf("strict_role_alternation = %v (%s)", merged.StrictRoleAlternation, sources["strict_role_alternation"])
	}
	if !merged.AllowReasoningEffortNone || sources["allow_reasoning_effort_none"] != ParamSourceDefault {
		t.Fatalf("allow_reasoning_effort_none = %v (%s)", merged.AllowReasoningEffortNone, sources["allow_reasoning_effort_none"])
	}
	if merged.MaxConcurrency != 4 || merged.RPM != 60 || !merged.PreserveReasoningContent ||
		!merged.OutputConfigEffort || merged.ResponsesParams != "unsupported" {
		t.Fatalf("inherited parameters = %+v", merged)
	}
	if _, ok := sources["preserve_reasoning_content"]; !ok {
		t.Fatal("inherited boolean missing from param sources")
	}
}

func TestMergeModelParamsLeavesBothUnsetLayersEmpty(t *testing.T) {
	merged, sources := MergeModelParams(ModelParams{MaxConcurrency: 2}, ModelParams{})
	if merged.MaxConcurrency != 2 || sources["max_concurrency"] != ParamSourceModel {
		t.Fatalf("max_concurrency = %d (%s)", merged.MaxConcurrency, sources["max_concurrency"])
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %v, want only max_concurrency", sources)
	}
	if merged.MaxTokens != 0 || merged.Temperature != nil || merged.Reasoning != nil || merged.Thinking != nil {
		t.Fatalf("unset parameters materialized: %+v", merged)
	}
}

// Zero values and false mean "unset", so a default wins over them. This is
// the documented trade-off of the non-pointer fields.
func TestMergeModelParamsTreatsZeroValuesAsUnset(t *testing.T) {
	defaultTemperature := 0.4
	merged, sources := MergeModelParams(
		ModelParams{MaxTokens: 0, ForcedToolChoice: "", StrictRoleAlternation: false, OutputConfigEffort: false},
		ModelParams{MaxTokens: 8192, ForcedToolChoice: "auto", StrictRoleAlternation: true, OutputConfigEffort: true, Temperature: &defaultTemperature},
	)
	if merged.MaxTokens != 8192 || merged.ForcedToolChoice != "auto" || !merged.StrictRoleAlternation || !merged.OutputConfigEffort {
		t.Fatalf("zero-valued explicit parameters did not inherit defaults: %+v", merged)
	}
	for name, source := range sources {
		if source != ParamSourceDefault {
			t.Fatalf("%s source = %q, want default", name, source)
		}
	}
}
