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
	DeleteProfileOverrides(context.Context, string, int64) error
}
