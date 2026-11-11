package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func presetTestModel() *store.Model {
	return &store.Model{
		ID: "user:deepseek", DisplayName: "deepseek", Protocol: "openai-chat",
		BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", Revision: 5, HasAPIKey: true,
		Params: store.ModelParams{
			MaxTokens: 1024, MaxConcurrency: 1,
			Temperature: ptr(0.6), Price: &config.PriceConfig{InputPerMTok: 1, OutputPerMTok: 2},
			Thinking: &config.ThinkingConfig{Type: "disabled"},
		},
	}
}

func applyModelPreset(server *Server, id, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/models/"+id+"/preset", strings.NewReader(body))
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleApplyModelPreset(response, request)
	return response
}

func findPresetChange(changes []llm.PresetChange, field string) (llm.PresetChange, bool) {
	for _, change := range changes {
		if change.Field == field {
			return change, true
		}
	}
	return llm.PresetChange{}, false
}

func TestModelPresetListReturnsShippedValuesWithoutSecrets(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-presets", nil)
	response := httptest.NewRecorder()
	server.handleListModelPresets(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	var body struct {
		Presets []llm.ModelPreset `json:"presets"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Presets) != 2 {
		t.Fatalf("presets = %d, want 2: %s", len(body.Presets), response.Body.String())
	}
	for i, id := range []string{llm.PresetDeepSeekV4ProMax, llm.PresetDeepSeekFlashMax} {
		if body.Presets[i].ID != id {
			t.Errorf("preset %d = %q, want %q", i, body.Presets[i].ID, id)
		}
		if body.Presets[i].SourceURL == "" || body.Presets[i].VerifiedAt != "2026-11-11" {
			t.Errorf("preset %d is missing its source: %+v", i, body.Presets[i])
		}
	}
	for _, secret := range []string{"api_key", "secret", "credential"} {
		if strings.Contains(strings.ToLower(response.Body.String()), secret) {
			t.Errorf("preset response contains %q: %s", secret, response.Body.String())
		}
	}
}

func TestModelPresetPreviewReportsChangesWithoutWriting(t *testing.T) {
	fake := &fakeModelStore{stored: presetTestModel()}
	server, _ := newParamsTestServer(t, fake)
	response := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	if fake.paramsCalls != 0 {
		t.Fatalf("preview wrote to the store: calls = %d", fake.paramsCalls)
	}
	var body modelPresetResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.DryRun || body.Applied || body.Revision != 5 || body.Preset != llm.PresetDeepSeekFlashMax {
		t.Fatalf("response = %+v", body)
	}
	change, ok := findPresetChange(body.Changes, "params.max_tokens")
	if !ok || change.From != float64(1024) || change.To != float64(393216) {
		t.Fatalf("max_tokens change = %+v, want 1024 -> 393216", change)
	}
	if change, ok := findPresetChange(body.Changes, "params.thinking"); !ok || change.To != nil {
		t.Fatalf("thinking change = %+v, want cleared", change)
	}
	if len(body.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", body.Conflicts)
	}
}

func TestModelPresetApplyMergesOnlyPresetFields(t *testing.T) {
	stored := presetTestModel()
	fake := &fakeModelStore{stored: stored}
	server, logs := newParamsTestServer(t, fake)
	response := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max","revision":5,"dry_run":false}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	if fake.paramsCalls != 1 || fake.paramsResult == nil {
		t.Fatalf("store calls = %d result = %+v", fake.paramsCalls, fake.paramsResult)
	}
	if got, want := strings.Join(fake.paramsPatch.Fields(), ","), strings.Join([]string{
		"params.context_window", "params.forced_tool_choice", "params.max_concurrency", "params.max_tokens",
		"params.preserve_reasoning_content", "params.reasoning", "params.thinking",
	}, ","); got != want {
		t.Fatalf("patched fields = %s, want %s", got, want)
	}
	params := fake.paramsResult.Params
	if params.MaxTokens != 393216 || params.ContextWindow != 655360 || params.MaxConcurrency != 2500 {
		t.Fatalf("numeric params = %+v", params)
	}
	if params.Reasoning == nil || params.Reasoning.Effort != "max" {
		t.Fatalf("reasoning = %+v", params.Reasoning)
	}
	if params.Thinking != nil {
		t.Fatalf("thinking = %+v, want cleared", params.Thinking)
	}
	if params.ForcedToolChoice != "unsupported" || !params.PreserveReasoningContent || params.RPM != 0 {
		t.Fatalf("capability params = %+v", params)
	}
	if params.Temperature == nil || *params.Temperature != 0.6 || params.Price == nil || params.Price.OutputPerMTok != 2 {
		t.Fatalf("unrelated params changed: %+v", params)
	}
	if fake.paramsResult.Protocol != "openai-chat" || fake.paramsResult.BaseURL != "https://api.deepseek.com" ||
		fake.paramsResult.Model != "deepseek-flash" || !fake.paramsResult.HasAPIKey {
		t.Fatalf("endpoint or credential changed: %+v", fake.paramsResult)
	}
	var body modelPresetResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Applied || body.DryRun || body.Revision != 6 || body.Effective != "new_workflows" {
		t.Fatalf("response = %+v", body)
	}
	log := logs.String()
	for _, want := range []string{
		`"event":"platform_audit"`, `"action":"apply_preset"`, `"preset":"deepseek-flash-max"`,
		`"target_id":"user:deepseek"`, `"params.max_tokens"`, `"params.thinking"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log missing %s: %s", want, log)
		}
	}
	for _, value := range []string{"393216", "1024", "0.6"} {
		if strings.Contains(log, value) {
			t.Errorf("audit log leaked parameter value %s: %s", value, log)
		}
	}
}

func TestModelPresetConflictRequiresConfirmation(t *testing.T) {
	fake := &fakeModelStore{stored: presetTestModel()}
	overrides := &fakeProfileOverrides{
		body: `{"lanes":{"review":{"model":"user:deepseek","params":{"max_tokens":4096,"reasoning":{"effort":"high"}}}}}`,
	}
	server, logs := newParamsTestServer(t, fake)
	server.platform = &config.Config{Profiles: map[string]config.ProfileConfig{"default": {Provider: "config-model"}}}
	server.profileOverrides = overrides

	preview := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max"}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", preview.Code, preview.Body.String())
	}
	var body modelPresetResponse
	if err := json.Unmarshal(preview.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", body.Conflicts)
	}
	conflict := body.Conflicts[0]
	if conflict.Node != "lanes.review" || conflict.Source != "override" ||
		strings.Join(conflict.Fields, ",") != "max_tokens,reasoning" {
		t.Fatalf("conflict = %+v", conflict)
	}

	blocked := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max","revision":5,"dry_run":false}`)
	if blocked.Code != http.StatusConflict {
		t.Fatalf("apply status = %d, want 409: %s", blocked.Code, blocked.Body.String())
	}
	code, fields := errorDetails(t, blocked)
	if code != "override_conflict" || !strings.Contains(fields["overrides.lanes.review"], "max_tokens, reasoning") {
		t.Fatalf("error = %s %v", code, fields)
	}
	if fake.paramsCalls != 0 {
		t.Fatalf("conflicting apply wrote to the store: %d", fake.paramsCalls)
	}

	confirmed := applyModelPreset(server, "user:deepseek",
		`{"preset":"deepseek-flash-max","revision":5,"dry_run":false,"confirm_overrides":true}`)
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirmed status = %d: %s", confirmed.Code, confirmed.Body.String())
	}
	if fake.paramsCalls != 1 {
		t.Fatalf("confirmed calls = %d, want 1", fake.paramsCalls)
	}
	if !strings.Contains(overrides.body, `"max_tokens":4096`) {
		t.Fatalf("apply cleared stored profile overrides: %s", overrides.body)
	}
	if strings.Contains(logs.String(), "4096") {
		t.Fatalf("audit log leaked an override value: %s", logs.String())
	}
}

func TestModelPresetRejectsInvalidTargets(t *testing.T) {
	cases := []struct {
		name, id, body string
		status         int
		field          string
		model          func() *store.Model
	}{
		{"configured model", "config:primary", `{"preset":"deepseek-flash-max"}`, http.StatusForbidden, "", nil},
		{"unknown model", "user:missing", `{"preset":"deepseek-flash-max"}`, http.StatusNotFound, "", nil},
		{"unknown id", "deepseek", `{"preset":"deepseek-flash-max"}`, http.StatusNotFound, "", nil},
		{"deleted model", "user:deepseek", `{"preset":"deepseek-flash-max"}`, http.StatusNotFound, "", func() *store.Model {
			model := presetTestModel()
			deleted := time.Now()
			model.DeletedAt = &deleted
			return model
		}},
		{"unknown preset", "user:deepseek", `{"preset":"missing"}`, http.StatusBadRequest, "preset", nil},
		{"apply without revision", "user:deepseek", `{"preset":"deepseek-flash-max","dry_run":false}`, http.StatusBadRequest, "revision", nil},
		{"wrong protocol", "user:deepseek", `{"preset":"deepseek-flash-max"}`, http.StatusBadRequest, "preset", func() *store.Model {
			model := presetTestModel()
			model.Protocol = "anthropic"
			return model
		}},
		{"other endpoint", "user:deepseek", `{"preset":"deepseek-flash-max"}`, http.StatusBadRequest, "preset", func() *store.Model {
			model := presetTestModel()
			model.BaseURL = "https://api.example.com/v1"
			return model
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := presetTestModel()
			if tc.model != nil {
				stored = tc.model()
			}
			fake := &fakeModelStore{stored: stored}
			server, _ := newParamsTestServer(t, fake)
			response := applyModelPreset(server, tc.id, tc.body)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			if tc.field != "" {
				if _, fields := errorDetails(t, response); fields[tc.field] == "" {
					t.Fatalf("missing field %s: %s", tc.field, response.Body.String())
				}
			}
			if fake.paramsCalls != 0 {
				t.Fatalf("invalid request wrote to the store: %d", fake.paramsCalls)
			}
		})
	}
}

func TestModelPresetApplyMapsStaleRevision(t *testing.T) {
	fake := &fakeModelStore{stored: presetTestModel(), paramsUpdateErr: store.ErrStaleRevision}
	server, _ := newParamsTestServer(t, fake)
	response := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max","revision":4,"dry_run":false}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if code, _ := errorDetails(t, response); code != "stale_revision" {
		t.Fatalf("error code = %q, want stale_revision", code)
	}
}

func TestModelPresetOverrideReadFailureIsUnavailable(t *testing.T) {
	fake := &fakeModelStore{stored: presetTestModel()}
	server, _ := newParamsTestServer(t, fake)
	server.platform = &config.Config{Profiles: map[string]config.ProfileConfig{"default": {Provider: "config-model"}}}
	server.profileOverrides = &fakeProfileOverrides{getErr: errors.New("read failed")}
	response := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max"}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", response.Code, response.Body.String())
	}
	if fake.paramsCalls != 0 {
		t.Fatalf("unavailable overrides still wrote to the store: %d", fake.paramsCalls)
	}
}

func TestModelPresetApplyAlreadyAtPresetDoesNotWrite(t *testing.T) {
	stored := presetTestModel()
	preset, _ := llm.Preset(llm.PresetDeepSeekFlashMax)
	stored.Params = preset.Params
	fake := &fakeModelStore{stored: stored}
	server, _ := newParamsTestServer(t, fake)
	response := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max","revision":5,"dry_run":false}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body modelPresetResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Applied || len(body.Changes) != 0 || body.Revision != 5 {
		t.Fatalf("response = %+v", body)
	}
	if fake.paramsCalls != 0 {
		t.Fatalf("no-op apply wrote to the store: %d", fake.paramsCalls)
	}
}

func TestModelPresetAcceptsDeepSeekPathVariant(t *testing.T) {
	stored := presetTestModel()
	stored.BaseURL = "https://api.deepseek.com/v1"
	fake := &fakeModelStore{stored: stored}
	server, _ := newParamsTestServer(t, fake)
	response := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

func TestModelPresetEqualOverrideValueIsNotAConflict(t *testing.T) {
	fake := &fakeModelStore{stored: presetTestModel()}
	overrides := &fakeProfileOverrides{
		body: `{"lanes":{"review":{"model":"user:deepseek","params":{"max_tokens":393216,"reasoning":{"effort":"max"}}}}}`,
	}
	server, _ := newParamsTestServer(t, fake)
	server.platform = &config.Config{Profiles: map[string]config.ProfileConfig{"default": {Provider: "config-model"}}}
	server.profileOverrides = overrides

	preview := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max"}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", preview.Code, preview.Body.String())
	}
	var body modelPresetResponse
	if err := json.Unmarshal(preview.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Conflicts) != 0 {
		t.Fatalf("equal overrides reported as conflicts: %+v", body.Conflicts)
	}
	applied := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max","revision":5,"dry_run":false}`)
	if applied.Code != http.StatusOK {
		t.Fatalf("apply status = %d, want 200: %s", applied.Code, applied.Body.String())
	}
	if fake.paramsCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", fake.paramsCalls)
	}
}

func TestModelPresetThinkingOnlyConflictRequiresConfirmation(t *testing.T) {
	fake := &fakeModelStore{stored: presetTestModel()}
	overrides := &fakeProfileOverrides{
		body: `{"lanes":{"review":{"model":"user:deepseek","params":{"thinking":{"type":"disabled"}}}}}`,
	}
	server, _ := newParamsTestServer(t, fake)
	server.platform = &config.Config{Profiles: map[string]config.ProfileConfig{"default": {Provider: "config-model"}}}
	server.profileOverrides = overrides

	preview := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max"}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", preview.Code, preview.Body.String())
	}
	var body modelPresetResponse
	if err := json.Unmarshal(preview.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Conflicts) != 1 || strings.Join(body.Conflicts[0].Fields, ",") != "thinking" {
		t.Fatalf("conflicts = %+v, want thinking only", body.Conflicts)
	}
	blocked := applyModelPreset(server, "user:deepseek", `{"preset":"deepseek-flash-max","revision":5,"dry_run":false}`)
	if blocked.Code != http.StatusConflict {
		t.Fatalf("apply status = %d, want 409: %s", blocked.Code, blocked.Body.String())
	}
	if fake.paramsCalls != 0 {
		t.Fatalf("conflicting apply wrote to the store: %d", fake.paramsCalls)
	}
}
