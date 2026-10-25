package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
)

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// One deadline covers body reading and persistence. Reset it after the
	// read so a keep-alive connection can serve its next request normally.
	deadline := time.Now().Add(webhookTimeout)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	r.Body = http.MaxBytesReader(w, r.Body, webhookBodyLimit)
	body, err := io.ReadAll(r.Body)
	_ = controller.SetReadDeadline(time.Time{})
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
		if !time.Now().Before(deadline) {
			writeWebhookResponse(w, r, map[string]any{"ignored": "request_timeout"})
			return
		}
		writeWebhookResponse(w, r, map[string]any{"ignored": "bad_request"})
		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	result, err := s.webhooks.Handle(ctx, app.WebhookRequest{
		Event: r.Header.Get("X-Gitlab-Event"),
		Token: r.Header.Get("X-Gitlab-Token"),
		UUID:  r.Header.Get("X-Gitlab-Event-UUID"),
		Body:  body,
	})
	if err != nil {
		// The service already sanitized the reason; do not log the body or error.
		s.logger.Warn("webhook processing failed",
			"request_id", requestID(r.Context()),
			"event", r.Header.Get("X-Gitlab-Event"),
		)
		writeWebhookResponse(w, r, map[string]any{"ignored": "storage_error"})
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
	payload["request_id"] = requestID(r.Context())
	writeJSON(w, http.StatusOK, payload)
}
