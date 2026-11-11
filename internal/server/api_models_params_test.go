package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	response := patchModelParams(server, "user:saved", `{"revision":3,"params":{"temperature":null,"reasoning":null}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	params := fake.paramsResult.Params
	if params.Temperature != nil || params.Reasoning != nil {
		t.Fatalf("nullable fields not cleared: temperature=%v reasoning=%v", params.Temperature, params.Reasoning)
	}
	if params.MaxTokens != 4096 || params.RPM != 60 {
		t.Fatalf("unsubmitted parameters changed: %+v", params)
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
		{"zero max tokens", `{"revision":3,"params":{"max_tokens":0}}`, "params.max_tokens"},
		{"zero max concurrency", `{"revision":3,"params":{"max_concurrency":0}}`, "params.max_concurrency"},
		{"null required parameter", `{"revision":3,"params":{"rpm":null}}`, "params.rpm"},
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

func TestModelRecordUpdateRequiresKeyWhenEndpointChanges(t *testing.T) {
	stored := savedParamsModel()
	for _, tc := range []struct {
		name       string
		protocol   string
		baseURL    string
		extra      string
		wantStatus int
	}{
		{"protocol changes without key", "anthropic", stored.BaseURL, "", http.StatusBadRequest},
		{"host changes without key", stored.Protocol, "https://other.example/v1", "", http.StatusBadRequest},
		{"port changes without key", stored.Protocol, "https://example.com:8443/v1", "", http.StatusBadRequest},
		{"same endpoint other path without key", stored.Protocol, "https://example.com/v2", "", http.StatusOK},
		{"protocol changes with key", "anthropic", stored.BaseURL, `,"api_key":"new-key-canary"`, http.StatusOK},
		{"host changes with empty key", stored.Protocol, "https://other.example/v1", `,"api_key":""`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeModelStore{stored: stored}
			server, logs := newParamsTestServer(t, fake)
			body := `{"display_name":"saved","protocol":"` + tc.protocol + `","base_url":"` + tc.baseURL +
				`","model":"m","params":{"max_tokens":16,"max_concurrency":1},"revision":3` + tc.extra + `}`
			request := httptest.NewRequest(http.MethodPatch, "/api/v1/models/user:saved", strings.NewReader(body))
			request.SetPathValue("id", "user:saved")
			response := httptest.NewRecorder()
			server.handleUpdateModel(response, request)
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d body = %s, want %d", response.Code, response.Body.String(), tc.wantStatus)
			}
			if tc.wantStatus == http.StatusBadRequest {
				code, fields := errorDetails(t, response)
				if code != "validation_failed" || fields["api_key"] == "" {
					t.Fatalf("code = %q details = %v, want api_key validation failure", code, fields)
				}
				if fake.updated != nil {
					t.Fatal("invalid endpoint change was written")
				}
				return
			}
			if fake.updated == nil {
				t.Fatal("valid update did not reach the store")
			}
			if strings.Contains(logs.String(), "new-key-canary") {
				t.Fatal("credential leaked into the audit log")
			}
		})
	}
}
