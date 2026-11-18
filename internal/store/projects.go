package store

import (
	"encoding/json"
	"time"
)

// ProjectFilter selects registered projects for the admin list endpoint. An
// empty query matches every project; a nil Enabled keeps both states. A nil
// AccessUserID is the trusted internal path; a non-nil value restricts rows to
// projects granted to that user.
type ProjectFilter struct {
	Query        string
	Enabled      *bool
	AccessUserID *int64
}

// ProjectUpdate replaces only the provided project fields.
type ProjectUpdate struct {
	Profile  *string
	Settings json.RawMessage
	Enabled  *bool
	// Revision is the optimistic-concurrency token the caller loaded. A nil
	// value keeps the pre-fence semantics: the write always applies. A
	// non-nil value applies only when it matches the stored revision, otherwise
	// the store returns ErrStaleRevision. Every applied write advances it.
	Revision *int64
}

// ProjectTokenUpdate rotates the encrypted project access token together with
// its expiry and scopes.
type ProjectTokenUpdate struct {
	TokenEnc  []byte
	ExpiresAt *time.Time
	Scopes    []string
}

// ProjectWebhookUpdate atomically records webhook provisioning state. A nil
// HookID and an empty SecretEnc keep the stored values; ClearHookID removes
// the recorded hook without discarding the encrypted secret. An empty
// ManagedBy keeps the stored ownership. State, error and events are always
// replaced.
type ProjectWebhookUpdate struct {
	HookID      *int64
	ClearHookID bool
	SecretEnc   []byte
	State       string
	Error       string
	Events      []string
	ManagedBy   string
}
