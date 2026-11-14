package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/LeXwDeX/tribunal/internal/app"
)

// modelToolProbeResponse distinguishes the tool-call probe from the
// connectivity probe exposed by the existing /test route.
type modelToolProbeResponse struct {
	OK        bool   `json:"ok"`
	Model     string `json:"model"`
	LatencyMS int64  `json:"latency_ms"`
	Probe     string `json:"probe"`
	ToolCall  bool   `json:"tool_call"`
}

// handleProbeModelTools tests a catalog model (configured or saved) with one
// real submit_result tool call. It only reads the registry: nothing is stored,
// no defaults change and no llm_call rows are recorded.
func (s *Server) handleProbeModelTools(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "provider testing is unavailable", nil)
		return
	}
	id := r.PathValue("id")
	snapshot := s.models.Snapshot()
	model, ok := snapshot.Resolve(id)
	if !ok {
		writeError(w, r, http.StatusNotFound, "not_found", "model not found", nil)
		return
	}
	if model.Unavailable {
		writeError(w, r, http.StatusConflict, "model_unavailable", "model is unavailable", nil)
		return
	}
	client, err := snapshot.Client(id)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "provider_test_failed", "provider test failed", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), draftTestTimeout)
	defer cancel()
	result, err := app.ProbeProviderTools(ctx, client)
	if err != nil {
		if errors.Is(err, app.ErrToolCallUnsupported) {
			writeError(w, r, http.StatusUnprocessableEntity, "tool_call_unsupported", "model answered without the submit_result tool call", nil)
			return
		}
		writeError(w, r, http.StatusUnprocessableEntity, "provider_test_failed", "provider test failed", nil)
		return
	}
	writeJSON(w, http.StatusOK, modelToolProbeResponse{
		OK: true, Model: result.Model, LatencyMS: result.LatencyMS,
		Probe: result.Probe, ToolCall: result.ToolCall,
	})
}
