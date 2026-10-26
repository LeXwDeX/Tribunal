package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteStoreErrorInternalTimeoutIsUnavailable(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}

	// The client is still connected; the deadline came from inside the server.
	for _, err := range []error{
		fmt.Errorf("query token=SECRET: %w", context.DeadlineExceeded),
		context.Canceled,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		s.writeStoreError(rec, req, err)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("internal %v status = %d, want 503", err, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"code":"unavailable"`) {
			t.Fatalf("internal %v body = %s", err, rec.Body.String())
		}
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("log leaked error text: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"error_class":"timeout"`) {
		t.Fatalf("log lacks error class: %s", logs.String())
	}
}

func TestWriteStoreErrorSkipsResponseWhenClientIsGone(t *testing.T) {
	s := &Server{logger: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil).WithContext(ctx)
	s.writeStoreError(rec, req, context.Canceled)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("response written for a gone client: %d %q", rec.Code, rec.Body.String())
	}
}

func TestWriteStoreErrorLogsClassForInternalError(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	s.writeStoreError(rec, req, errors.New("row 42 token=SECRET"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(logs.String(), "SECRET") || !strings.Contains(logs.String(), `"error_class":"internal"`) {
		t.Fatalf("log = %s", logs.String())
	}
}
