package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

func validationFailed(w http.ResponseWriter, r *http.Request, field, message string) {
	writeError(w, r, http.StatusBadRequest, "validation_failed", message, []app.Detail{{Field: field, Message: message}})
}

func badRequest(w http.ResponseWriter, r *http.Request, message string) {
	writeError(w, r, http.StatusBadRequest, "bad_request", message, nil)
}

// decodeOptionalJSON is decodeJSON with an empty body treated as an empty
// object. Trigger/refresh endpoints accept {"profile":...} but also no body.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("server: multiple JSON values")
		}
		return err
	}
	return nil
}

// decodeRequestError maps JSON decode failures to the documented envelope
// without echoing body content.
func decodeRequestError(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		badRequest(w, r, "request body is too large")
		return
	}
	validationFailed(w, r, "body", "invalid JSON request body")
}

func pageLimit(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return defaultPageLimit, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maxPageLimit {
		return 0, errors.New("limit must be between 1 and 200")
	}
	return value, nil
}

func queryInt64(r *http.Request, name string) (*int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return nil, errors.New(name + " must be a positive integer")
	}
	return &value, nil
}

func queryBool(r *http.Request, name string) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	switch raw {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	default:
		return false, errors.New(name + " must be true or false")
	}
}

// queryTime accepts RFC 3339 (the web console sends toISOString) and a plain
// date. The returned time is UTC.
func queryTime(r *http.Request, name string) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return time.Time{}, nil
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02", raw); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, errors.New(name + " must be an RFC 3339 timestamp")
}

// contextKind reads ?kind= and defaults to agents. Only agents and claude are
// valid, matching store.SaveManualContextDoc.
func contextKind(r *http.Request) (string, error) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" {
		return "agents", nil
	}
	if kind != "agents" && kind != "claude" {
		return "", errors.New("kind must be agents or claude")
	}
	return kind, nil
}

// encodeFindingCursor stores the last returned finding ID. Findings have a
// stable (origin_node, origin_index, id) order and the store returns them all,
// so the ID is enough to resume.
func encodeFindingCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeFindingCursor(cursor string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) == 0 {
		return "", store.ErrInvalidCursor
	}
	return string(raw), nil
}

func (s *Server) requireStore(w http.ResponseWriter, r *http.Request) bool {
	if s.store == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "store is not configured", nil)
		return false
	}
	return true
}

func (s *Server) requireReviews(w http.ResponseWriter, r *http.Request) bool {
	if s.reviews == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "review service is not configured", nil)
		return false
	}
	return true
}

func (s *Server) requireExtensions(w http.ResponseWriter, r *http.Request) bool {
	if s.extensions == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "extension service is not configured", nil)
		return false
	}
	return true
}

// writeServiceError prefers the sanitized app error and falls back to store
// sentinels so a plain store.ErrNotFound from an injected fake still maps to
// 404.
func (s *Server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *app.Error
	if errors.As(err, &appErr) {
		writeAppError(w, r, err)
		return
	}
	s.writeStoreError(w, r, err)
}

// writeStoreError maps store sentinels to the documented error envelope. It
// never logs the error text because it can embed row identifiers.
func (s *Server) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client is gone; writing a response would be pointless.
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "not found", nil)
	case errors.Is(err, store.ErrInvalidCursor):
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid cursor", nil)
	case errors.Is(err, store.ErrContextLocked):
		writeError(w, r, http.StatusConflict, "conflict", "context document is locked", nil)
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrSuperseded):
		writeError(w, r, http.StatusConflict, "conflict", "conflict", nil)
	default:
		s.logger.Error("admin store request failed",
			"request_id", requestID(r.Context()), "path", r.URL.Path)
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
	}
}
