package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/store"
)

var _ store.AlertAckStore = (*Store)(nil)

// ListAlertAcks returns every alert ack owned by userID, newest first. The
// per-user cap keeps the result bounded, so it is not paginated.
func (s *Store) ListAlertAcks(ctx context.Context, userID int64) ([]store.AlertAck, error) {
	if userID <= 0 {
		return nil, errors.New("sqlite: alert ack user id is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT alert_key, fingerprint, acked_at FROM alert_acks
		WHERE user_id = ? ORDER BY acked_at DESC, alert_key ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list alert acks: %w", err)
	}
	defer rows.Close()
	acks := []store.AlertAck{}
	for rows.Next() {
		var ack store.AlertAck
		var acked string
		if err := rows.Scan(&ack.Key, &ack.Fingerprint, &acked); err != nil {
			return nil, fmt.Errorf("sqlite: scan alert ack: %w", err)
		}
		if ack.AckedAt, err = parseTime(acked); err != nil {
			return nil, err
		}
		acks = append(acks, ack)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list alert acks: %w", err)
	}
	return acks, nil
}

// PutAlertAck stores or replaces one ack and evicts the user's oldest acks
// beyond store.MaxAlertAcksPerUser in the same transaction. A missing user row
// is reported as store.ErrNotFound.
func (s *Store) PutAlertAck(ctx context.Context, userID int64, ack store.AlertAck) (*store.AlertAck, error) {
	if userID <= 0 {
		return nil, errors.New("sqlite: alert ack user id is required")
	}
	if strings.TrimSpace(ack.Key) == "" || ack.Fingerprint == "" {
		return nil, errors.New("sqlite: alert ack key and fingerprint are required")
	}
	if ack.AckedAt.IsZero() {
		ack.AckedAt = s.now()
	}
	ack.AckedAt = ack.AckedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin put alert ack: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO alert_acks (user_id, alert_key, fingerprint, acked_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, alert_key) DO UPDATE SET
			fingerprint = excluded.fingerprint,
			acked_at = excluded.acked_at`,
		userID, ack.Key, ack.Fingerprint, formatTime(ack.AckedAt)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: user %d", store.ErrNotFound, userID)
		}
		return nil, fmt.Errorf("sqlite: put alert ack: %w", err)
	}
	// The row just written is always kept, even if the clock stepped back and
	// its acked_at is the oldest; the others keep the newest cap-1 in list
	// order (acked_at DESC, alert_key ASC), so the last listed row goes first.
	if _, err := tx.ExecContext(ctx, `DELETE FROM alert_acks WHERE user_id = ? AND alert_key IN (
		SELECT alert_key FROM alert_acks WHERE user_id = ? AND alert_key <> ?
		ORDER BY acked_at DESC, alert_key ASC LIMIT -1 OFFSET ?)`,
		userID, userID, ack.Key, store.MaxAlertAcksPerUser-1); err != nil {
		return nil, fmt.Errorf("sqlite: evict alert acks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit put alert ack: %w", err)
	}
	return &ack, nil
}

// DeleteAlertAck removes one ack owned by userID. A missing ack is not an
// error, so undo stays idempotent and reveals nothing about other users.
func (s *Store) DeleteAlertAck(ctx context.Context, userID int64, key string) error {
	if userID <= 0 || strings.TrimSpace(key) == "" {
		return errors.New("sqlite: alert ack user id and key are required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alert_acks WHERE user_id = ? AND alert_key = ?`, userID, key); err != nil {
		return fmt.Errorf("sqlite: delete alert ack: %w", err)
	}
	return nil
}
