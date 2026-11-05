package sqlite

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// ListStatusOnlyReviews lists terminal review statuses that settled without a
// workflow: a recognizable detail (see store.StatusDetail*) and no review
// workflow for the same project, MR and head. Rows are newest first with the
// same access semantics as ListWorkflows. The cursor id is "project:mr".
func (s *Store) ListStatusOnlyReviews(ctx context.Context, filter store.StatusOnlyFilter, cursor string, limit int) ([]store.StatusOnlyReview, string, error) {
	limit = normalizeLimit(limit)
	where := []string{
		"rs.state IN (?, ?)",
		"rs.detail IN (?, ?, ?)",
		`NOT EXISTS (SELECT 1 FROM workflows w WHERE w.kind = 'review'
			AND w.project_id = rs.project_id AND w.mr_iid = rs.mr_iid
			AND lower(w.head_sha) = lower(rs.head_sha))`,
	}
	args := []any{
		store.ReviewStatusPass, store.ReviewStatusIncomplete,
		store.StatusDetailEmptyDiff, store.StatusDetailNoReviewable, store.StatusDetailSnapshotOmission,
	}
	if filter.ProjectID != nil {
		where = append(where, "rs.project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.MRIID != nil {
		where = append(where, "rs.mr_iid = ?")
		args = append(args, *filter.MRIID)
	}
	if filter.Verdict != "" {
		where = append(where, "upper(rs.state) = upper(?)")
		args = append(args, filter.Verdict)
	}
	if !filter.Since.IsZero() {
		where = append(where, "rs.updated_at >= ?")
		args = append(args, formatTime(filter.Since.UTC()))
	}
	accessClause := projectAccessClause
	if filter.GrantOnly {
		accessClause = projectGrantClause
	}
	if clause, accessArgs := accessClause("rs.project_id", filter.AccessUserID); clause != "" {
		where = append(where, clause)
		args = append(args, accessArgs...)
	}
	if cursor != "" {
		updated, id, err := decodeCursor(cursor, store.ErrInvalidCursor)
		if err != nil {
			return nil, "", err
		}
		projectRaw, mrRaw, ok := strings.Cut(id, ":")
		project, perr := strconv.ParseInt(projectRaw, 10, 64)
		mr, merr := strconv.ParseInt(mrRaw, 10, 64)
		if !ok || perr != nil || merr != nil {
			return nil, "", store.ErrInvalidCursor
		}
		where = append(where, `(rs.updated_at < ? OR (rs.updated_at = ? AND
			(rs.project_id < ? OR (rs.project_id = ? AND rs.mr_iid < ?))))`)
		args = append(args, updated, updated, project, project, mr)
	}
	query := `SELECT rs.project_id, rs.mr_iid, rs.head_sha, rs.state, rs.detail, rs.updated_at
		FROM review_status rs WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY rs.updated_at DESC, rs.project_id DESC, rs.mr_iid DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list status-only reviews: %w", err)
	}
	defer rows.Close()
	out := make([]store.StatusOnlyReview, 0, limit)
	for rows.Next() {
		var (
			row        store.StatusOnlyReview
			updatedRaw string
		)
		if err := rows.Scan(&row.ProjectID, &row.MRIID, &row.HeadSHA, &row.State, &row.Detail, &updatedRaw); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan status-only review: %w", err)
		}
		if row.UpdatedAt, err = parseTime(updatedRaw); err != nil {
			return nil, "", err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate status-only reviews: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = store.StatusOnlyCursor(out[limit-1])
	}
	return out, next, nil
}
