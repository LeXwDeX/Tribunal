package llm

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// The preset values are a capability contract verified against the official
// DeepSeek documentation. Changing any number requires updating SourceURL,
// VerifiedAt and the matching documentation section (#405).
func TestDeepSeekPresetsGoldenValues(t *testing.T) {
	presets := Presets()
	if len(presets) != 2 {
		t.Fatalf("Presets() = %d presets, want 2", len(presets))
	}
	want := []struct {
		id          string
		display     string
		model       string
		concurrency int
	}{
		{"deepseek-v4-pro-max", "DeepSeek V4 Pro (maximum)", "deepseek-v4-pro", 500},
		{"deepseek-flash-max", "DeepSeek Flash (maximum)", "deepseek-flash", 2500},
	}
	for i, expected := range want {
		preset := presets[i]
		if preset.ID != expected.id || preset.DisplayName != expected.display || preset.Model != expected.model {
			t.Fatalf("preset %d = %s/%s/%s, want %s/%s/%s", i, preset.ID, preset.DisplayName, preset.Model,
				expected.id, expected.display, expected.model)
		}
		if preset.Protocol != "openai-chat" || preset.BaseURL != "https://api.deepseek.com" {
			t.Errorf("%s endpoint = %s %s", preset.ID, preset.Protocol, preset.BaseURL)
		}
		params := preset.Params
		if params.MaxTokens != 393216 {
			t.Errorf("%s max_tokens = %d, want 393216", preset.ID, params.MaxTokens)
		}
		if params.ContextWindow != 655360 {
			t.Errorf("%s context_window = %d, want 655360", preset.ID, params.ContextWindow)
		}
		if params.Reasoning == nil || params.Reasoning.Effort != "max" {
			t.Errorf("%s reasoning = %+v, want effort max", preset.ID, params.Reasoning)
		}
		if params.Thinking != nil {
			t.Errorf("%s thinking = %+v, want unset", preset.ID, params.Thinking)
		}
		if params.MaxConcurrency != expected.concurrency {
			t.Errorf("%s max_concurrency = %d, want %d", preset.ID, params.MaxConcurrency, expected.concurrency)
		}
		if params.RPM != 0 {
			t.Errorf("%s rpm = %d, want 0", preset.ID, params.RPM)
		}
		if params.ForcedToolChoice != "unsupported" {
			t.Errorf("%s forced_tool_choice = %q, want unsupported", preset.ID, params.ForcedToolChoice)
		}
		if params.StrictRoleAlternation {
			t.Errorf("%s strict_role_alternation = true, want false", preset.ID)
		}
		if !params.PreserveReasoningContent {
			t.Errorf("%s preserve_reasoning_content = false, want true", preset.ID)
		}
		if params.Temperature != nil {
			t.Errorf("%s temperature = %v, want unset", preset.ID, *params.Temperature)
		}
		if preset.SourceURL != "https://api-docs.deepseek.com/quick_start/pricing" {
			t.Errorf("%s source_url = %q", preset.ID, preset.SourceURL)
		}
		if preset.VerifiedAt != "2026-11-11" {
			t.Errorf("%s verified_at = %q, want 2026-11-11", preset.ID, preset.VerifiedAt)
		}
		if err := config.ValidateProviderConfig("preset", ModelConfig(store.Model{
			Protocol: preset.Protocol, BaseURL: preset.BaseURL, Model: preset.Model, Params: preset.Params,
		}), config.ProviderValidationOptions{}); err != nil {
			t.Errorf("%s does not pass provider validation: %v", preset.ID, err)
		}
	}
}

func TestPresetLookupAndIsolation(t *testing.T) {
	if _, ok := Preset("missing"); ok {
		t.Fatal("Preset(missing) = ok")
	}
	first, ok := Preset(PresetDeepSeekFlashMax)
	if !ok {
		t.Fatal("flash preset is missing")
	}
	first.Params.Reasoning.Effort = "low"
	first.Params.MaxTokens = 1
	second, _ := Preset(PresetDeepSeekFlashMax)
	if second.Params.Reasoning.Effort != "max" || second.Params.MaxTokens != 393216 {
		t.Fatal("preset values are shared mutable state")
	}
	listed := Presets()
	listedDisplay := listed[0].DisplayName
	listed[0].DisplayName = "changed"
	if again := Presets(); again[0].DisplayName != listedDisplay {
		t.Fatal("Presets() returns shared mutable state")
	}
}

func TestPresetPatchOnlyChangesPresetFields(t *testing.T) {
	temperature := 0.6
	current := store.ModelParams{
		MaxTokens: 1024, MaxConcurrency: 1,
		Temperature: &temperature,
		Thinking:    &config.ThinkingConfig{Type: "disabled"},
	}
	preset, _ := Preset(PresetDeepSeekFlashMax)
	patch, changes := preset.PatchFor(current)
	if got := len(changes); got != 7 {
		t.Fatalf("changes = %d %+v, want 7", got, changes)
	}
	next := patch.Apply(current)
	if next.MaxTokens != 393216 || next.ContextWindow != 655360 || next.MaxConcurrency != 2500 {
		t.Fatalf("numeric params = %+v", next)
	}
	if next.Reasoning == nil || next.Reasoning.Effort != "max" {
		t.Fatalf("reasoning = %+v", next.Reasoning)
	}
	if next.Thinking != nil {
		t.Fatalf("thinking must be cleared, got %+v", next.Thinking)
	}
	if next.ForcedToolChoice != "unsupported" || next.StrictRoleAlternation || !next.PreserveReasoningContent || next.RPM != 0 {
		t.Fatalf("capability params = %+v", next)
	}
	if !reflect.DeepEqual(next.Temperature, &temperature) {
		t.Fatalf("temperature changed: %v", next.Temperature)
	}

	again, secondChanges := preset.PatchFor(next)
	if len(secondChanges) != 0 || len(again.Fields()) != 0 {
		t.Fatalf("reapplying changed %v: %+v", again.Fields(), secondChanges)
	}
	if !reflect.DeepEqual(again.Apply(next), next) {
		t.Fatal("empty patch changed params")
	}
}

func TestPresetPatchReportsFieldNamesOnly(t *testing.T) {
	preset, _ := Preset(PresetDeepSeekV4ProMax)
	_, changes := preset.PatchFor(store.ModelParams{MaxTokens: 1024})
	seen := map[string]bool{}
	for _, change := range changes {
		seen[change.Field] = true
	}
	for _, field := range []string{
		"params.max_tokens", "params.context_window", "params.reasoning", "params.max_concurrency",
		"params.forced_tool_choice", "params.preserve_reasoning_content",
	} {
		if !seen[field] {
			t.Errorf("missing change %s: %+v", field, changes)
		}
	}
	body, err := json.Marshal(changes)
	if err != nil {
		t.Fatalf("marshal changes: %v", err)
	}
	if !json.Valid(body) {
		t.Fatalf("changes JSON = %s", body)
	}
}
