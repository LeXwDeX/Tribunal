package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

const (
	apiTokenPrefix           = "crc_"
	apiTokenBytes            = 32
	apiTokenNameMax          = 100
	apiTokenDefaultExpiresIn = 90
	apiTokenMaxExpiresIn     = 365
)

type createdAPIToken struct {
	store.APIToken
	Token string `json:"token"`
}

// currentTokenUser returns the OAuth session user that owns tokens. The
// bootstrap bearer client has no user row, so token management is
// browser-session only.
func currentTokenUser(r *http.Request) (store.TokenUser, bool) {
	value, ok := principalFrom(r.Context())
	if !ok || value.bearer || value.user.ID <= 0 {
		return store.TokenUser{}, false
	}
	return store.TokenUser{
		ID:       value.user.ID,
		Username: value.user.Username,
		Name:     value.user.Name,
		Role:     value.user.Role,
	}, true
}

func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	user, ok := currentTokenUser(r)
	if !ok {
		writeError(w, r, http.StatusForbidden, "forbidden", "API tokens require a browser session", nil)
		return
	}
	if s.tokens == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "token store is not configured", nil)
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	tokens, next, err := s.tokens.ListAPITokensPage(r.Context(), user.ID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if tokens == nil {
		tokens = []store.APIToken{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens, "next_cursor": next})
}

type createAPITokenRequest struct {
	Name          string `json:"name"`
	ExpiresInDays *int   `json:"expires_in_days,omitempty"`
}

func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	user, ok := currentTokenUser(r)
	if !ok {
		writeError(w, r, http.StatusForbidden, "forbidden", "API tokens require a browser session", nil)
		return
	}
	if s.tokens == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "token store is not configured", nil)
		return
	}
	var request createAPITokenRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" || len(name) > apiTokenNameMax {
		validationFailed(w, r, "name", "name must be between 1 and 100 characters")
		return
	}
	days := apiTokenDefaultExpiresIn
	if request.ExpiresInDays != nil {
		days = *request.ExpiresInDays
	}
	if days < 1 || days > apiTokenMaxExpiresIn {
		validationFailed(w, r, "expires_in_days", "expires_in_days must be between 1 and 365")
		return
	}
	expiry := s.now().UTC().Add(time.Duration(days) * 24 * time.Hour)
	expiresAt := &expiry
	plaintext, err := newAPIToken()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	sum := sha256.Sum256([]byte(plaintext))
	token, err := s.tokens.CreateAPIToken(r.Context(), user, store.APIToken{
		UserID:    user.ID,
		Name:      name,
		TokenHash: sum[:],
		ExpiresAt: expiresAt,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, createdAPIToken{APIToken: *token, Token: plaintext})
}

func (s *Server) handleDeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	user, ok := currentTokenUser(r)
	if !ok {
		writeError(w, r, http.StatusForbidden, "forbidden", "API tokens require a browser session", nil)
		return
	}
	if s.tokens == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "token store is not configured", nil)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 128 {
		validationFailed(w, r, "id", "id is required")
		return
	}
	if err := s.tokens.DeleteAPIToken(r.Context(), user.ID, id); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// newAPIToken returns a random plaintext token. Only its SHA-256 digest is
// persisted.
func newAPIToken() (string, error) {
	raw := make([]byte, apiTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return apiTokenPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}
