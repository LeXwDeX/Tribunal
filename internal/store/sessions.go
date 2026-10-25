package store

import "time"

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
