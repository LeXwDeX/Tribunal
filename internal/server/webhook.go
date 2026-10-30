package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/errclass"
)

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// One deadline covers body reading and persistence. Reset it only on
	// success: failed requests close, and their final body drain must stay bounded.
	deadline := time.Now().Add(webhookTimeout)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	r.Body = http.MaxBytesReader(w, r.Body, webhookBodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// The body was not fully received. Do not let net/http wait for the
		// remainder on keep-alive; close after the bounded response.
		r.Close = true
		w.Header().Set("Connection", "close")
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeWebhookResponse(w, r, map[string]any{"ignored": "body_too_large"})
			return
		}
		var timeoutErr interface{ Timeout() bool }
		if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
			writeWebhookStatus(w, r, http.StatusServiceUnavailable, map[string]any{"ignored": "request_timeout"})
			return
		}
		writeWebhookResponse(w, r, map[string]any{"ignored": "bad_request"})
		return
	}
	_ = controller.SetReadDeadline(time.Time{})

	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	result, err := s.webhooks.Handle(ctx, app.WebhookRequest{
		Event: r.Header.Get("X-Gitlab-Event"),
		Token: r.Header.Get("X-Gitlab-Token"),
		UUID:  r.Header.Get("X-Gitlab-Event-UUID"),
		Body:  body,
	})
	if err != nil {
		// The delivery was neither recorded nor enqueued. A 503 makes the
		// failure visible when received; resend availability depends on GitLab.
		s.logger.Warn("webhook processing failed",
			"request_id", requestID(r.Context()),
			"event", r.Header.Get("X-Gitlab-Event"),
			"error_class", errclass.Of(err),
		)
		writeWebhookStatus(w, r, http.StatusServiceUnavailable, map[string]any{"ignored": "storage_error"})
		return
	}
	if result.Unauthorized {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "invalid webhook token", nil)
		return
	}
	if result.Accepted {
		writeWebhookResponse(w, r, map[string]any{"accepted": true, "job_id": result.JobID})
		return
	}
	ignored := result.Ignored
	if ignored == "" {
		ignored = "ignored"
	}
	writeWebhookResponse(w, r, map[string]any{"ignored": ignored})
}

func writeWebhookResponse(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	writeWebhookStatus(w, r, http.StatusOK, payload)
}

func writeWebhookStatus(w http.ResponseWriter, r *http.Request, status int, payload map[string]any) {
	payload["request_id"] = requestID(r.Context())
	writeJSON(w, status, payload)
}
