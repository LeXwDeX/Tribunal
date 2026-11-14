package store

import (
	"errors"
	"time"
)

// ErrTokenExpired marks an API token that exists but passed its expiry. It is
// reported alongside ErrNotFound so callers that only care about validity keep
// working, while the HTTP layer can answer token_expired without revealing
// anything about another user's token.
var ErrTokenExpired = errors.New("store: api token expired")

// Session is one persisted browser session. IDHash is the SHA-256 digest of
// the random cookie token; the plaintext token never reaches storage.
type Session struct {
	IDHash    []byte
	UserID    int64
	Username  string
	Name      string
	Role      string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Default session limits. PerUser keeps one account from evicting other
// users by logging in repeatedly; Total is only a storage backstop that
// evicts non-admin sessions before admin sessions.
const (
	DefaultSessionsPerUser = 10
	DefaultSessionsTotal   = 4096
)

// SessionLimits bounds persisted browser sessions. Zero or negative fields
// use the defaults.
type SessionLimits struct {
	PerUser int
	Total   int
}

// Normalized returns the limits with defaults applied.
func (l SessionLimits) Normalized() SessionLimits {
	if l.PerUser <= 0 {
		l.PerUser = DefaultSessionsPerUser
	}
	if l.Total <= 0 {
		l.Total = DefaultSessionsTotal
	}
	return l
}
