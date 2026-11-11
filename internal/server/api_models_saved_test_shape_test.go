package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// The console reads `model` and `latency_ms` from both the saved and draft
// model test endpoints; the saved endpoint must not leak Go field names.
func TestSavedModelTestUsesSnakeCaseFields(t *testing.T) {
	s := &Server{llm: &fakeLLM{result: ProviderTestResult{Model: "m-1", LatencyMS: 42}}, logger: newTestLogger(io.Discard)}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/models/config:alpha/test", nil)
	request.SetPathValue("id", "config:alpha")
	response := httptest.NewRecorder()
	s.handleTestModel(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["model"] != "m-1" || body["latency_ms"] != float64(42) {
		t.Fatalf("body = %v, want model=m-1 latency_ms=42", body)
	}
	if _, ok := body["Model"]; ok {
		t.Fatalf("body = %v, must not contain Go field names", body)
	}
}

// Model responses no longer expose a money price parameter.
func TestModelViewDoesNotExposePrice(t *testing.T) {
	raw, err := json.Marshal(modelView{ID: "user:x", Params: store.ModelParams{MaxTokens: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "price") {
		t.Fatalf("model view exposes price: %s", raw)
	}
}
