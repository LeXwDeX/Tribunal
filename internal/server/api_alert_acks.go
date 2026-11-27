package server

import (
	"net/http"
	"regexp"

	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	alertAckBodyLimit      = 4 << 10
	alertAckFingerprintMax = 256
)

// alertAckKeyPattern bounds alert keys. Keys are opaque to the server: it
// never resolves one to a project or review, so a key cannot probe existence.
var alertAckKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// alertAckUser returns the browser session user for alert acks. Bearer clients
// (bootstrap and API tokens) are rejected: acks are a console preference of
// the signed-in user, and the bootstrap user has no users row.
func (s *Server) alertAckUser(w http.ResponseWriter, r *http.Request) (store.TokenUser, bool) {
	user, ok := currentTokenUser(r)
	if !ok {
		writeError(w, r, http.StatusForbidden, "forbidden", "alert acks require a browser session", nil)
		return store.TokenUser{}, false
	}
	if s.alertAcks == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "alert ack store is not configured", nil)
		return store.TokenUser{}, false
	}
	return user, true
}

func validAlertAckFingerprint(value string) bool {
	if len(value) == 0 || len(value) > alertAckFingerprintMax {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func (s *Server) handleListAlertAcks(w http.ResponseWriter, r *http.Request) {
	user, ok := s.alertAckUser(w, r)
	if !ok {
		return
	}
	acks, err := s.alertAcks.ListAlertAcks(r.Context(), user.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if acks == nil {
		acks = []store.AlertAck{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"acks": acks})
}

type putAlertAckRequest struct {
	Fingerprint string `json:"fingerprint"`
}

func (s *Server) handlePutAlertAck(w http.ResponseWriter, r *http.Request) {
	user, ok := s.alertAckUser(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if !alertAckKeyPattern.MatchString(key) {
		validationFailed(w, r, "key", "key must be 1 to 128 characters of A-Z, a-z, 0-9, '.', '_', ':' or '-'")
		return
	}
	var request putAlertAckRequest
	if err := decodeJSON(w, r, alertAckBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if !validAlertAckFingerprint(request.Fingerprint) {
		validationFailed(w, r, "fingerprint", "fingerprint must be 1 to 256 bytes of printable ASCII")
		return
	}
	ack, err := s.alertAcks.PutAlertAck(r.Context(), user.ID, store.AlertAck{
		Key:         key,
		Fingerprint: request.Fingerprint,
		AckedAt:     s.now().UTC(),
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

func (s *Server) handleDeleteAlertAck(w http.ResponseWriter, r *http.Request) {
	user, ok := s.alertAckUser(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if !alertAckKeyPattern.MatchString(key) {
		validationFailed(w, r, "key", "key must be 1 to 128 characters of A-Z, a-z, 0-9, '.', '_', ':' or '-'")
		return
	}
	if err := s.alertAcks.DeleteAlertAck(r.Context(), user.ID, key); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
