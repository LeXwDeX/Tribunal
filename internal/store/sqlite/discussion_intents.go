package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

const unknownDiscussionForDossier = `EXISTS (
	SELECT 1 FROM workflows w JOIN discussion_intents i
	  ON i.project_id = w.project_id AND i.mr_iid = w.mr_iid AND i.kind = 'discussion'
	WHERE w.id = workflow_dossiers.workflow_id AND (i.gitlab_id = '' OR i.verification_required = 1)
)`

// ReserveDiscussionIntent persists uncertainty before the first send. The
// returned ID is non-empty only when an earlier attempt was confirmed.
func (s *Store) ReserveDiscussionIntent(ctx context.Context, projectID, mrIID int64, key string) (created bool, gitlabID string, retErr error) {
	if projectID <= 0 || mrIID <= 0 || strings.TrimSpace(key) == "" || key == "*" {
		return false, "", errors.New("sqlite: invalid discussion intent key")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, "", fmt.Errorf("sqlite: acquire discussion intent connection: %w", err)
	}
	priorSync := 0
	restore := false
	defer func() {
		if restore {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := conn.ExecContext(cleanupCtx, fmt.Sprintf("PRAGMA main.synchronous = %d", priorSync))
			var actual int
			if err == nil {
				err = conn.QueryRowContext(cleanupCtx, `PRAGMA main.synchronous`).Scan(&actual)
				if err == nil && actual != priorSync {
					err = errors.New("synchronous setting was not restored")
				}
			}
			cancel()
			if err != nil {
				// Raw's ErrBadConn makes database/sql discard the physical connection.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				created, gitlabID = false, ""
				retErr = errors.Join(retErr, fmt.Errorf("sqlite: restore discussion intent sync: %w", err))
			}
		}
		if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			created, gitlabID = false, ""
			retErr = errors.Join(retErr, fmt.Errorf("sqlite: release discussion intent connection: %w", err))
		}
	}()
	var journal string
	if err := conn.QueryRowContext(ctx, `PRAGMA main.journal_mode`).Scan(&journal); err != nil {
		return false, "", fmt.Errorf("sqlite: check discussion intent journal: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		return false, "", errors.New("sqlite: discussion intent requires a durable WAL database")
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA main.synchronous`).Scan(&priorSync); err != nil {
		return false, "", fmt.Errorf("sqlite: check discussion intent sync: %w", err)
	}
	if priorSync < 0 || priorSync > 3 {
		return false, "", errors.New("sqlite: invalid synchronous setting")
	}
	if priorSync < 2 {
		restore = true
		if _, err := conn.ExecContext(ctx, `PRAGMA main.synchronous = FULL`); err != nil {
			return false, "", fmt.Errorf("sqlite: set discussion intent sync: %w", err)
		}
		var actual int
		if err := conn.QueryRowContext(ctx, `PRAGMA main.synchronous`).Scan(&actual); err != nil {
			return false, "", fmt.Errorf("sqlite: verify discussion intent sync: %w", err)
		} else if actual < 2 {
			return false, "", errors.New("sqlite: discussion intent sync is not FULL")
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, "", fmt.Errorf("sqlite: begin discussion intent: %w", err)
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN verification_required = 1 THEN '' ELSE gitlab_id END FROM discussion_intents
		WHERE project_id = ? AND mr_iid = ? AND kind = 'discussion' AND publish_key IN (?, '*')
		ORDER BY (publish_key = ?) DESC LIMIT 1`, projectID, mrIID, key, key).Scan(&gitlabID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", fmt.Errorf("sqlite: read discussion intent: %w", err)
	}
	created = errors.Is(err, sql.ErrNoRows)
	if created {
		if _, err := tx.ExecContext(ctx, `INSERT INTO discussion_intents
			(project_id, mr_iid, kind, publish_key) VALUES (?, ?, 'discussion', ?)`, projectID, mrIID, key); err != nil {
			return false, "", fmt.Errorf("sqlite: reserve discussion intent: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, "", fmt.Errorf("sqlite: commit discussion intent: %w", err)
	}
	return created, gitlabID, nil
}

// ConfirmDiscussionIntent records only a response ID or a uniquely matched
// remote marker. An existing different ID is never overwritten.
func (s *Store) ConfirmDiscussionIntent(ctx context.Context, projectID, mrIID int64, key, id string) error {
	if projectID <= 0 || mrIID <= 0 || strings.TrimSpace(key) == "" || key == "*" || strings.TrimSpace(id) == "" {
		return errors.New("sqlite: invalid discussion confirmation")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO discussion_intents
		(project_id, mr_iid, kind, publish_key, gitlab_id) VALUES (?, ?, 'discussion', ?, ?)
		ON CONFLICT (project_id, mr_iid, kind, publish_key) DO UPDATE SET gitlab_id = excluded.gitlab_id, verification_required = 0
		WHERE discussion_intents.gitlab_id = '' OR discussion_intents.gitlab_id = excluded.gitlab_id`,
		projectID, mrIID, key, id)
	if err != nil {
		return fmt.Errorf("sqlite: confirm discussion intent: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: confirm discussion intent: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: discussion confirmation changed", store.ErrConflict)
	}
	return nil
}

// RejectDiscussionIntent clears only a fresh attempt whose HTTP 400 response
// proved GitLab rejected it. All other outcomes retain the uncertainty fence.
func (s *Store) RejectDiscussionIntent(ctx context.Context, projectID, mrIID int64, key string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM discussion_intents
		WHERE project_id = ? AND mr_iid = ? AND kind = 'discussion' AND publish_key = ? AND gitlab_id = ''`,
		projectID, mrIID, key)
	if err != nil {
		return fmt.Errorf("sqlite: reject discussion intent: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: reject discussion intent: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: discussion intent changed", store.ErrConflict)
	}
	return nil
}

func (s *Store) HasUnknownDiscussionIntent(ctx context.Context, projectID, mrIID int64) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM discussion_intents
		WHERE project_id = ? AND mr_iid = ? AND kind = 'discussion' AND (gitlab_id = '' OR verification_required = 1))`, projectID, mrIID).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("sqlite: check discussion intents: %w", err)
	}
	return found != 0, nil
}
