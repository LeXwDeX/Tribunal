package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/LeXwDeX/tribunal/internal/app"
)

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	RequestID string       `json:"request_id"`
	Details   []app.Detail `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details []app.Detail) {
	writeJSON(w, status, errorEnvelope{
		Error: errorBody{
			Code:      code,
			Message:   message,
			RequestID: requestID(r.Context()),
			Details:   details,
		},
	})
}

func writeAppError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *app.Error
	if !errors.As(err, &appErr) {
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	status := http.StatusInternalServerError
	switch appErr.Code {
	case "validation_failed":
		status = http.StatusBadRequest
	case "not_found":
		status = http.StatusNotFound
	case "conflict":
		status = http.StatusConflict
	case "gitlab_rejected":
		status = http.StatusUnprocessableEntity
	case "internal":
		status = http.StatusInternalServerError
	}
	writeError(w, r, status, appErr.Code, appErr.Message, appErr.Details)
}

func constantTimeEqualStrings(expected, actual string) bool {
	expectedHash := sha256.Sum256([]byte(expected))
	actualHash := sha256.Sum256([]byte(actual))
	return subtle.ConstantTimeCompare(expectedHash[:], actualHash[:]) == 1
}
