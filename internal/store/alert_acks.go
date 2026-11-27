package store

import (
	"context"
	"time"
)

// MaxAlertAcksPerUser bounds the alert acks kept for one user. A write beyond
// the bound evicts that user's oldest acks by AckedAt.
const MaxAlertAcksPerUser = 1000

// AlertAck is one user's "read" mark on an overview alert. Key and Fingerprint
// are computed by the console and stored opaquely; the server never resolves
// a key to the object it names.
type AlertAck struct {
	Key         string    `json:"key"`
	Fingerprint string    `json:"fingerprint"`
	AckedAt     time.Time `json:"acked_at"`
}

// AlertAckStore persists alert acks per user. Every method is scoped to
// userID, so one user can never read or change another user's acks.
type AlertAckStore interface {
	ListAlertAcks(ctx context.Context, userID int64) ([]AlertAck, error)
	PutAlertAck(ctx context.Context, userID int64, ack AlertAck) (*AlertAck, error)
	DeleteAlertAck(ctx context.Context, userID int64, key string) error
}
