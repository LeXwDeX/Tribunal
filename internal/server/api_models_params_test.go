package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func savedParamsModel() *store.Model {
	return &store.Model{
		ID: "user:saved", DisplayName: "saved", Protocol: "openai-chat",
		BaseURL: "https://example.com/v1", Model: "m", Revision: 3, HasAPIKey: true,
		Params: store.ModelParams{
			MaxTokens: 4096, ContextWindow: 128000, MaxConcurrency: 2, RPM: 60,
			Temperature: ptr(0.7), Reasoning: &config.ReasoningConfig{Effort: "high"},
			Thinking: &config.ThinkingConfig{BudgetTokens: 1024},
			Price:    &config.PriceConfig{InputPerMTok: 1, OutputPerMTok: 2},
		},
	}
}

func newParamsTestServer(t *testing.T, fake *fakeModelStore) (*Server, *bytes.Buffer) {
	t.Helper()
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return newModelTestServer(t, catalog, fake)
}

func patchModelParams(server *Server, id, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/models/"+id+"/params", strings.NewReader(body))
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleUpdateModelParams(response, request)
	return response
}

func errorDetails(t *testing.T, response *httptest.ResponseRecorder) (string, map[string]string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Details []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error body %s: %v", response.Body.String(), err)
	}
	fields := map[string]string{}
	for _, detail := range envelope.Error.Details {
		fields[detail.Field] = detail.Message
	}
	return envelope.Error.Code, fields
}

func TestModelParamsUpdateMergesOnlySubmittedKeys(t *testing.T) {
	fake := &fakeModelStore{stored: savedParamsModel()}
	server, logs := newParamsTestServer(t, fake)
	response := patchModelParams(server, "user:saved", `{"revision":3,"params":{"temperature":1.25}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	if fake.paramsCalls != 1 || fake.paramsResult == nil {
		t.Fatalf("store calls = %d, result = %+v", fake.paramsCalls, fake.paramsResult)
	}
	params := fake.paramsResult.Params
	if params.Temperature == nil || *params.Temperature != 1.25 {
		t.Fatalf("temperature = %v, want 1.25", params.Temperature)
	}
	if params.MaxTokens != 4096 || params.ContextWindow != 128000 || params.MaxConcurrency != 2 || params.RPM != 60 {
		t.Fatalf("unsubmitted parameters changed: %+v", params)
	}
	if params.Reasoning == nil || params.Reasoning.Effort != "high" {
		t.Fatalf("reasoning changed: %+v", params.Reasoning)
	}
	if params.Thinking == nil || params.Thinking.BudgetTokens != 1024 {
		t.Fatalf("thinking changed: %+v", params.Thinking)
	}
	if params.Price == nil || params.Price.InputPerMTok != 1 || params.Price.OutputPerMTok != 2 {
		t.Fatalf("price changed: %+v", params.Price)
	}
	if fake.paramsResult.Revision != 4 {
		t.Fatalf("revision = %d, want 4", fake.paramsResult.Revision)
	}
	if !strings.Contains(response.Body.String(), `"id":"user:saved"`) ||
		!strings.Contains(response.Body.String(), `"effective":"new_workflows"`) {
		t.Fatalf("body = %s, want id and effective", response.Body.String())
	}
	log := logs.String()
	for _, want := range []string{`"event":"platform_audit"`, `"action":"update_params"`, `"target_id":"user:saved"`, `"actor_id":`, `"params.temperature"`} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log missing %s: %s", want, log)
		}
	}
	for _, value := range []string{"1.25", "0.7"} {
		if strings.Contains(log, value) {
			t.Errorf("audit log leaked parameter value %s: %s", value, log)
		}
	}
}

func TestModelParamsUpdateExplicitNullClearsNullableFields(t *testing.T) {
	fake := &fakeModelStore{stored: savedParamsModel()}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelParams(server, "user:saved", `{"revision":3,"params":{"temperature":null,"reasoning":null,"thinking":null,"price":null}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	params := fake.paramsResult.Params
	if params.Temperature != nil || params.Reasoning != nil || params.Thinking != nil || params.Price != nil {
		t.Fatalf("nullable fields not cleared: %+v", params)
	}
	if params.MaxTokens != 4096 || params.RPM != 60 {
		t.Fatalf("unsubmitted parameters changed: %+v", params)
	}
}

func TestModelParamsUpdateRejectsUnknownNestedFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"reasoning typo", `{"revision":3,"params":{"reasoning":{"effrot":"high"}}}`, "params.reasoning"},
		{"thinking typo", `{"revision":3,"params":{"thinking":{"budget_token":1}}}`, "params.thinking"},
		{"price typo", `{"revision":3,"params":{"price":{"input_per_mtoken":1}}}`, "params.price"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeModelStore{stored: savedParamsModel()}
			server, _ := newParamsTestServer(t, fake)
			response := patchModelParams(server, "user:saved", tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s, want 400", response.Code, response.Body.String())
			}
			if code, fields := errorDetails(t, response); code != "validation_failed" || fields[tc.field] == "" {
				t.Fatalf("code = %q details = %v, want validation failure on %s", code, fields, tc.field)
			}
			if fake.paramsCalls != 0 {
				t.Fatal("store was called for an unknown nested field")
			}
		})
	}
}

func TestModelParamsUpdateRejectsNullInsideObjectParameters(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"thinking budget", `{"revision":3,"params":{"thinking":{"budget_tokens":null}}}`},
		{"thinking type", `{"revision":3,"params":{"thinking":{"type":null}}}`},
		{"reasoning effort", `{"revision":3,"params":{"reasoning":{"effort":null}}}`},
		{"price field", `{"revision":3,"params":{"price":{"input_per_mtok":null}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeModelStore{stored: savedParamsModel()}
			server, _ := newParamsTestServer(t, fake)
			response := patchModelParams(server, "user:saved", tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s, want 400", response.Code, response.Body.String())
			}
			code, fields := errorDetails(t, response)
			if code != "validation_failed" {
				t.Fatalf("code = %q, want validation_failed", code)
			}
			message := fields["params.thinking"] + fields["params.reasoning"] + fields["params.price"]
			if !strings.Contains(message, "top-level null") {
				t.Fatalf("details = %v, want a top-level null hint", fields)
			}
			if fake.paramsCalls != 0 {
				t.Fatal("store was called for a nested null")
			}
		})
	}
}

// Object parameters are replaced wholesale, so an omitted nested field resets
// to its zero value. The API documents this; callers must resend the object.
func TestModelParamsUpdateReplacesObjectParametersWholesale(t *testing.T) {
	fake := &fakeModelStore{stored: savedParamsModel()}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelParams(server, "user:saved", `{"revision":3,"params":{"price":{"input_per_mtok":7}}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	price := fake.paramsResult.Params.Price
	if price == nil || price.InputPerMTok != 7 || price.OutputPerMTok != 0 {
		t.Fatalf("price = %+v, want input 7 and output reset to 0", price)
	}
}

func TestModelParamsUpdateRejectsInvalidParameters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"negative rpm", `{"revision":3,"params":{"rpm":-1}}`, "params.rpm"},
		{"temperature above range", `{"revision":3,"params":{"temperature":2.5}}`, "params.temperature"},
		{"temperature below range", `{"revision":3,"params":{"temperature":-0.1}}`, "params.temperature"},
		{"negative max tokens", `{"revision":3,"params":{"max_tokens":-1}}`, "params.max_tokens"},
		{"negative max concurrency", `{"revision":3,"params":{"max_concurrency":-1}}`, "params.max_concurrency"},
		{"invalid thinking type", `{"revision":3,"params":{"thinking":{"type":"always"}}}`, "params.thinking.type"},
		{"invalid responses params", `{"revision":3,"params":{"responses_params":"never"}}`, "params.responses_params"},
		{"effort none without capability", `{"revision":3,"params":{"reasoning":{"effort":"none"}}}`, "params.reasoning.effort"},
		{"null required parameter", `{"revision":3,"params":{"rpm":null}}`, "params.rpm"},
		{"null capability parameter", `{"revision":3,"params":{"preserve_reasoning_content":null}}`, "params.preserve_reasoning_content"},
		{"unknown parameter", `{"revision":3,"params":{"nope":1}}`, "params.nope"},
		{"wrong value type", `{"revision":3,"params":{"temperature":"hot"}}`, "params.temperature"},
		{"empty params", `{"revision":3,"params":{}}`, "params"},
		{"missing revision", `{"params":{"rpm":1}}`, "revision"},
		{"zero revision", `{"revision":0,"params":{"rpm":1}}`, "revision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeModelStore{stored: savedParamsModel()}
			server, _ := newParamsTestServer(t, fake)
			response := patchModelParams(server, "user:saved", tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s, want 400", response.Code, response.Body.String())
			}
			code, fields := errorDetails(t, response)
			if code != "validation_failed" || fields[tc.field] == "" {
				t.Fatalf("code = %q details = %v, want validation_failed on %s", code, fields, tc.field)
			}
			if fake.paramsCalls != 0 {
				t.Fatalf("store was called %d times for an invalid request", fake.paramsCalls)
			}
		})
	}
}

// Zero max_tokens means "do not send" for the OpenAI protocols but the
// Anthropic API requires a positive limit (#404).
func TestModelParamsUpdateRejectsZeroMaxTokensForAnthropic(t *testing.T) {
	stored := savedParamsModel()
	stored.Protocol = "anthropic"
	fake := &fakeModelStore{stored: stored}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelParams(server, "user:saved", `{"revision":3,"params":{"max_tokens":0}}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", response.Code, response.Body.String())
	}
	if _, fields := errorDetails(t, response); fields["params.max_tokens"] == "" {
		t.Fatalf("details = %v, want params.max_tokens", fields)
	}
	if fake.paramsCalls != 0 {
		t.Fatal("invalid parameters reached the store")
	}
}

// Anthropic extended thinking rejects a budget below 1024 at request time, so
// the stored-model validation must report it on the parameter field (#406).
func TestModelParamsUpdateRejectsSmallAnthropicThinkingBudget(t *testing.T) {
	stored := savedParamsModel()
	stored.Protocol = "anthropic"
	fake := &fakeModelStore{stored: stored}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelParams(server, "user:saved", `{"revision":3,"params":{"thinking":{"budget_tokens":512}}}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", response.Code, response.Body.String())
	}
	if _, fields := errorDetails(t, response); fields["params.thinking.budget_tokens"] == "" {
		t.Fatalf("details = %v, want params.thinking.budget_tokens", fields)
	}
	if fake.paramsCalls != 0 {
		t.Fatal("invalid parameters reached the store")
	}
}

func TestModelParamsUpdateRejectsConfiguredModels(t *testing.T) {
	fake := &fakeModelStore{stored: savedParamsModel()}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelParams(server, "config:primary", `{"revision":3,"params":{"rpm":1}}`)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s, want 403", response.Code, response.Body.String())
	}
	if fake.paramsCalls != 0 {
		t.Fatal("configured model reached the store")
	}
}

func TestModelParamsUpdateReturnsNotFoundForUnknownOrDeletedModels(t *testing.T) {
	server, _ := newParamsTestServer(t, &fakeModelStore{stored: savedParamsModel()})
	if response := patchModelParams(server, "user:missing", `{"revision":3,"params":{"rpm":1}}`); response.Code != http.StatusNotFound {
		t.Fatalf("unknown model status = %d body = %s, want 404", response.Code, response.Body.String())
	}
	deleted := savedParamsModel()
	now := time.Now()
	deleted.DeletedAt = &now
	server, _ = newParamsTestServer(t, &fakeModelStore{stored: deleted})
	if response := patchModelParams(server, deleted.ID, `{"revision":3,"params":{"rpm":1}}`); response.Code != http.StatusNotFound {
		t.Fatalf("deleted model status = %d body = %s, want 404", response.Code, response.Body.String())
	}
}

func TestModelParamsUpdateMapsStaleRevision(t *testing.T) {
	fake := &fakeModelStore{stored: savedParamsModel(), paramsUpdateErr: store.ErrStaleRevision}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelParams(server, "user:saved", `{"revision":2,"params":{"rpm":1}}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409", response.Code, response.Body.String())
	}
	if code, _ := errorDetails(t, response); code != "stale_revision" {
		t.Fatalf("code = %q, want stale_revision", code)
	}
}

func paramsWith(mutate func(*store.ModelParams)) store.ModelParams {
	params := savedParamsModel().Params
	mutate(&params)
	return params
}

// Every supported key is exercised for both setting and clearing/resetting.
// The expected merged parameters are compared as a whole so an accidentally
// dropped unsubmitted field fails the test.
func TestModelParamsUpdateSetsAndClearsEachKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		want  *store.ModelParams
		field string
	}{
		{"set max_tokens", `{"max_tokens":8192}`, ptr(paramsWith(func(p *store.ModelParams) { p.MaxTokens = 8192 })), ""},
		{"set context_window", `{"context_window":64000}`, ptr(paramsWith(func(p *store.ModelParams) { p.ContextWindow = 64000 })), ""},
		{"set max_concurrency", `{"max_concurrency":4}`, ptr(paramsWith(func(p *store.ModelParams) { p.MaxConcurrency = 4 })), ""},
		{"set rpm", `{"rpm":30}`, ptr(paramsWith(func(p *store.ModelParams) { p.RPM = 30 })), ""},
		{"set temperature", `{"temperature":1.5}`, ptr(paramsWith(func(p *store.ModelParams) { p.Temperature = ptr(1.5) })), ""},
		{"set reasoning", `{"reasoning":{"effort":"low"}}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.Reasoning = &config.ReasoningConfig{Effort: "low"}
		})), ""},
		{"set thinking", `{"thinking":{"budget_tokens":2048}}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.Thinking = &config.ThinkingConfig{BudgetTokens: 2048}
		})), ""},
		{"set thinking type", `{"thinking":{"type":"enabled"}}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.Thinking = &config.ThinkingConfig{Type: "enabled"}
		})), ""},
		{"set price", `{"price":{"input_per_mtok":3,"output_per_mtok":4}}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.Price = &config.PriceConfig{InputPerMTok: 3, OutputPerMTok: 4}
		})), ""},
		{"set forced_tool_choice", `{"forced_tool_choice":"supported"}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.ForcedToolChoice = "supported"
		})), ""},
		{"set strict_role_alternation", `{"strict_role_alternation":true}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.StrictRoleAlternation = true
		})), ""},
		{"set preserve_reasoning_content", `{"preserve_reasoning_content":true}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.PreserveReasoningContent = true
		})), ""},
		{"set allow_reasoning_effort_none", `{"allow_reasoning_effort_none":true}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.AllowReasoningEffortNone = true
		})), ""},
		{"set output_config_effort", `{"output_config_effort":true}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.OutputConfigEffort = true
		})), ""},
		{"set responses_params", `{"responses_params":"unsupported"}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.ResponsesParams = "unsupported"
		})), ""},
		{"set effort none with capability", `{"allow_reasoning_effort_none":true,"reasoning":{"effort":"none"}}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.AllowReasoningEffortNone = true
			p.Reasoning = &config.ReasoningConfig{Effort: "none"}
		})), ""},
		{"clear temperature", `{"temperature":null}`, ptr(paramsWith(func(p *store.ModelParams) { p.Temperature = nil })), ""},
		{"clear reasoning", `{"reasoning":null}`, ptr(paramsWith(func(p *store.ModelParams) { p.Reasoning = nil })), ""},
		{"clear thinking", `{"thinking":null}`, ptr(paramsWith(func(p *store.ModelParams) { p.Thinking = nil })), ""},
		{"clear price", `{"price":null}`, ptr(paramsWith(func(p *store.ModelParams) { p.Price = nil })), ""},
		{"reset context_window", `{"context_window":0}`, ptr(paramsWith(func(p *store.ModelParams) { p.ContextWindow = 0 })), ""},
		{"reset rpm", `{"rpm":0}`, ptr(paramsWith(func(p *store.ModelParams) { p.RPM = 0 })), ""},
		{"reset forced_tool_choice", `{"forced_tool_choice":""}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.ForcedToolChoice = ""
		})), ""},
		{"reset strict_role_alternation", `{"strict_role_alternation":false}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.StrictRoleAlternation = false
		})), ""},
		{"reset max_tokens", `{"max_tokens":0}`, ptr(paramsWith(func(p *store.ModelParams) { p.MaxTokens = 0 })), ""},
		{"reset max_concurrency", `{"max_concurrency":0}`, ptr(paramsWith(func(p *store.ModelParams) { p.MaxConcurrency = 0 })), ""},
		{"reset preserve_reasoning_content", `{"preserve_reasoning_content":false}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.PreserveReasoningContent = false
		})), ""},
		{"reset allow_reasoning_effort_none", `{"allow_reasoning_effort_none":false}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.AllowReasoningEffortNone = false
		})), ""},
		{"reset output_config_effort", `{"output_config_effort":false}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.OutputConfigEffort = false
		})), ""},
		{"reset responses_params", `{"responses_params":""}`, ptr(paramsWith(func(p *store.ModelParams) {
			p.ResponsesParams = ""
		})), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeModelStore{stored: savedParamsModel()}
			server, _ := newParamsTestServer(t, fake)
			response := patchModelParams(server, "user:saved", `{"revision":3,"params":`+tc.body+`}`)
			if tc.field != "" {
				if response.Code != http.StatusBadRequest {
					t.Fatalf("status = %d body = %s, want 400", response.Code, response.Body.String())
				}
				if _, fields := errorDetails(t, response); fields[tc.field] == "" {
					t.Fatalf("details = %v, want %s", fields, tc.field)
				}
				if fake.paramsCalls != 0 {
					t.Fatal("invalid parameters reached the store")
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
			}
			if fake.paramsResult == nil {
				t.Fatal("parameter update did not reach the store")
			}
			if got := fake.paramsResult.Params; !reflect.DeepEqual(got, *tc.want) {
				t.Fatalf("merged params = %+v, want %+v", got, *tc.want)
			}
		})
	}
}

func TestModelParamsUpdateRejectsMalformedJSON(t *testing.T) {
	for _, body := range []string{
		`{"revision":3,"params":`,
		`not json`,
		`{"revision":3,"params":{"rpm":1},"api_key":"not-allowed"}`,
	} {
		fake := &fakeModelStore{stored: savedParamsModel()}
		server, _ := newParamsTestServer(t, fake)
		response := patchModelParams(server, "user:saved", body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, response.Code)
			continue
		}
		if code, fields := errorDetails(t, response); code != "validation_failed" || fields["body"] == "" {
			t.Errorf("body %q: code = %q details = %v, want body validation failure", body, code, fields)
		}
		if fake.paramsCalls != 0 {
			t.Errorf("body %q: malformed request reached the store", body)
		}
	}
}

func TestModelParamsUpdateReturns503WhenLookupFails(t *testing.T) {
	fake := &fakeModelStore{
		stored:      savedParamsModel(),
		getModelErr: errors.New("sqlite: get model failed"),
	}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelParams(server, "user:saved", `{"revision":3,"params":{"rpm":1}}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s, want 503", response.Code, response.Body.String())
	}
	if code, _ := errorDetails(t, response); code != "unavailable" {
		t.Fatalf("code = %q, want unavailable", code)
	}
	if fake.paramsCalls != 0 {
		t.Fatal("store write ran after an unavailable lookup")
	}
}

func recordUpdateBody(protocol, baseURL, extra string) string {
	return `{"display_name":"saved","protocol":"` + protocol + `","base_url":"` + baseURL +
		`","model":"m","params":{"max_tokens":16,"max_concurrency":1},"revision":3` + extra + `}`
}

func patchModelRecord(server *Server, id, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/models/"+id, strings.NewReader(body))
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleUpdateModel(response, request)
	return response
}

func TestModelRecordUpdateRequiresKeyWhenEndpointChanges(t *testing.T) {
	stored := savedParamsModel()
	for _, tc := range []struct {
		name           string
		protocol       string
		baseURL        string
		extra          string
		hasKey         bool
		wantStatus     int
		wantCredential *string
	}{
		{"protocol changes without key", "anthropic", stored.BaseURL, "", true, http.StatusBadRequest, nil},
		{"host changes without key", stored.Protocol, "https://other.example/v1", "", true, http.StatusBadRequest, nil},
		{"port changes without key", stored.Protocol, "https://example.com:8443/v1", "", true, http.StatusBadRequest, nil},
		{"same endpoint other path without key", stored.Protocol, "https://example.com/v2", "", true, http.StatusOK, nil},
		{"protocol changes with key", "anthropic", stored.BaseURL, `,"api_key":"new-key-canary"`, true, http.StatusOK, ptr("new-key-canary")},
		{"host changes with empty key", stored.Protocol, "https://other.example/v1", `,"api_key":""`, true, http.StatusOK, ptr("")},
		{"keyless model changes endpoint without key", "anthropic", "https://other.example/v1", "", false, http.StatusOK, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := *stored
			saved.HasAPIKey = tc.hasKey
			fake := &fakeModelStore{stored: &saved}
			server, logs := newParamsTestServer(t, fake)
			response := patchModelRecord(server, "user:saved", recordUpdateBody(tc.protocol, tc.baseURL, tc.extra))
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d body = %s, want %d", response.Code, response.Body.String(), tc.wantStatus)
			}
			if tc.wantStatus == http.StatusBadRequest {
				code, fields := errorDetails(t, response)
				if code != "validation_failed" || fields["api_key"] == "" {
					t.Fatalf("code = %q details = %v, want api_key validation failure", code, fields)
				}
				if fake.updated != nil || fake.updateCredential != nil {
					t.Fatal("invalid endpoint change reached the store")
				}
				return
			}
			if fake.updated == nil {
				t.Fatal("valid update did not reach the store")
			}
			switch {
			case tc.wantCredential == nil && fake.updateCredential != nil:
				t.Fatalf("credential = %q, want omitted (preserve stored key)", *fake.updateCredential)
			case tc.wantCredential != nil && (fake.updateCredential == nil || *fake.updateCredential != *tc.wantCredential):
				t.Fatalf("credential = %v, want %q", fake.updateCredential, *tc.wantCredential)
			}
			if strings.Contains(logs.String(), "new-key-canary") {
				t.Fatal("credential leaked into the audit log")
			}
		})
	}
}

func TestModelRecordUpdateReturnsNotFoundForUnknownOrDeletedModels(t *testing.T) {
	body := recordUpdateBody("openai-chat", "https://example.com/v1", "")
	server, _ := newParamsTestServer(t, &fakeModelStore{})
	if response := patchModelRecord(server, "user:missing", body); response.Code != http.StatusNotFound {
		t.Fatalf("unknown model status = %d body = %s, want 404", response.Code, response.Body.String())
	}
	deleted := savedParamsModel()
	now := time.Now()
	deleted.DeletedAt = &now
	server, _ = newParamsTestServer(t, &fakeModelStore{stored: deleted})
	if response := patchModelRecord(server, deleted.ID, body); response.Code != http.StatusNotFound {
		t.Fatalf("deleted model status = %d body = %s, want 404", response.Code, response.Body.String())
	}
}

func TestModelRecordUpdateReturns503WhenEndpointLookupFails(t *testing.T) {
	fake := &fakeModelStore{
		stored:      savedParamsModel(),
		endpointErr: errors.New("sqlite: get model endpoint failed"),
	}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelRecord(server, "user:saved", recordUpdateBody("openai-chat", "https://example.com/v1", ""))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s, want 503", response.Code, response.Body.String())
	}
	if code, _ := errorDetails(t, response); code != "unavailable" {
		t.Fatalf("code = %q, want unavailable", code)
	}
	if fake.updated != nil {
		t.Fatal("store write ran after an unavailable endpoint lookup")
	}
}

// The full-record update reads only the endpoint columns, so a corrupt
// params_json (surfaced here as a GetModel error) cannot block the repair.
func TestModelRecordUpdateDoesNotReadParameters(t *testing.T) {
	fake := &fakeModelStore{
		stored:      savedParamsModel(),
		getModelErr: errors.New("sqlite: corrupt model record"),
	}
	server, _ := newParamsTestServer(t, fake)
	response := patchModelRecord(server, "user:saved", recordUpdateBody("openai-chat", "https://example.com/v1", ""))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	if fake.updated == nil {
		t.Fatal("full-record update was blocked by unreadable parameters")
	}
}
