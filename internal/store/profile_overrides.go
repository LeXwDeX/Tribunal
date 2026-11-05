package store

import (
	"context"
	"time"
)

// ProfileOverrides is the persisted override document and its revision.
type ProfileOverrides struct {
	Profile       string    `json:"profile"`
	OverridesJSON string    `json:"-"`
	Revision      int64     `json:"revision"`
	UpdatedBy     int64     `json:"updated_by"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ProfileOverrideStore keeps override updates and model references atomic.
type ProfileOverrideStore interface {
	GetProfileOverrides(context.Context, string) (*ProfileOverrides, error)
	PutProfileOverrides(context.Context, string, string, int64, int64) (*ProfileOverrides, error)
	// DeleteProfileOverrides resets the document at the given revision, as the
	// given user, and returns the stored result. A missing row or a different
	// revision is ErrStaleRevision.
	DeleteProfileOverrides(ctx context.Context, profile string, revision, userID int64) (*ProfileOverrides, error)
	ListProfileModelReferences(context.Context, string) ([]ProfileModelReference, error)
}

// ProfileModelReference identifies one node that references a model.
type ProfileModelReference struct {
	Profile string `json:"profile"`
	Node    string `json:"node"`
}
