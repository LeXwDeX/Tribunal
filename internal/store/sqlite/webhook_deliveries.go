package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// ListWebhookDeliverySummaries groups recorded webhook deliveries by project
// and merge request for the read-only "why was this MR not reviewed" view. It
// exposes counts and fixed reasons only; the upstream request body is never
// stored, returned or logged. The access filter is applied before grouping.
//
// One pagination run is bound to the delivery time upper bound taken at its
// first page, so a delivery that arrives between pages cannot move an MR across
// the cursor and hide it from the rest of the run (#521). Grouping, ordering and
// LIMIT happen in SQL, and the counts query reads only the selected page.
func (s *Store) ListWebhookDeliverySummaries(ctx context.Context, filter store.WebhookDeliveryFilter, cursor string, limit int) ([]store.WebhookDeliverySummary, string, error) {
	limit = normalizeLimit(limit)
	conditions := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if filter.ProjectID != nil {
		conditions = append(conditions, "d.project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.MRIID != nil {
		conditions = append(conditions, "d.mr_iid = ?")
		args = append(args, *filter.MRIID)
	}
	if clause, accessArgs := projectAccessClause("d.project_id", filter.AccessUserID); clause != "" {
		conditions = append(conditions, clause)
		args = append(args, accessArgs...)
	}

	var (
		bound    listCursor
		snapshot time.Time
		err      error
	)
	if cursor != "" {
		bound, err = decodeListCursor(cursor, listCursorDeliveries)
		if err != nil {
			return nil, "", err
		}
		if !sameProjectID(bound.ProjectID, filter.ProjectID) {
			return nil, "", fmt.Errorf("%w: webhook delivery filter changed", store.ErrInvalidCursor)
		}
		if !sameMRIID(bound.MRIID, filter.MRIID) {
			return nil, "", fmt.Errorf("%w: webhook delivery filter changed", store.ErrInvalidCursor)
		}
		snapshot, err = parseTime(bound.Snapshot)
		if err != nil || snapshot.IsZero() {
			return nil, "", fmt.Errorf("%w: webhook delivery cursor", store.ErrInvalidCursor)
		}
	} else {
		snapshot, err = s.webhookDeliverySnapshot(ctx, conditions, args)
		if err != nil {
			return nil, "", err
		}
	}

	page, hasMore, err := s.webhookDeliveryPage(ctx, conditions, args, bound, snapshot, limit, cursor != "")
	if err != nil {
		return nil, "", err
	}
	next := ""
	if hasMore {
		last := page[len(page)-1]
		next = encodeListCursor(listCursor{
			Kind:         listCursorDeliveries,
			ProjectID:    filter.ProjectID,
			MRIID:        optionalMRIID(filter.MRIID),
			Snapshot:     formatTime(snapshot),
			At:           formatTime(last.last),
			RowProjectID: last.projectID,
			RowMRIID:     last.mrIID,
		})
	}
	if len(page) == 0 {
		return nil, next, nil
	}
	summaries, err := s.webhookDeliveryCounts(ctx, page, snapshot)
	if err != nil {
		return nil, "", err
	}
	return summaries, next, nil
}

// webhookDeliverySnapshot fixes the upper bound for one pagination run: the
// request time, raised to the newest stored delivery so a row written with a
// slightly ahead clock is still inside the snapshot.
func (s *Store) webhookDeliverySnapshot(ctx context.Context, conditions []string, args []any) (time.Time, error) {
	var latest sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(d.received_at) FROM webhook_deliveries d
		WHERE `+strings.Join(conditions, " AND "), args...).Scan(&latest); err != nil {
		return time.Time{}, fmt.Errorf("sqlite: webhook delivery snapshot: %w", err)
	}
	snapshot := s.now().UTC()
	if latest.Valid && latest.String != "" {
		at, err := parseTime(latest.String)
		if err != nil {
			return time.Time{}, err
		}
		if at.After(snapshot) {
			snapshot = at
		}
	}
	return snapshot, nil
}

type webhookDeliveryPageRow struct {
	projectID int64
	mrIID     int64
	last      time.Time
}

// webhookDeliveryPage selects one page of (project, MR) keys ordered by last
// delivery time. The GROUP BY, keyset bound and LIMIT all run in SQL; the
// snapshot keeps the selected row set immutable for the run.
func (s *Store) webhookDeliveryPage(ctx context.Context, conditions []string, args []any, bound listCursor, snapshot time.Time, limit int, hasCursor bool) ([]webhookDeliveryPageRow, bool, error) {
	pageConditions := append(append([]string(nil), conditions...), "d.received_at <= ?")
	pageArgs := append(append([]any(nil), args...), formatTime(snapshot))
	having := ""
	if hasCursor {
		having = ` HAVING MAX(d.received_at) < ? OR (MAX(d.received_at) = ? AND (d.project_id > ? OR (d.project_id = ? AND d.mr_iid > ?)))`
		pageArgs = append(pageArgs, bound.At, bound.At, bound.RowProjectID, bound.RowProjectID, bound.RowMRIID)
	}
	pageArgs = append(pageArgs, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT d.project_id, d.mr_iid, MAX(d.received_at) AS last_received
		FROM webhook_deliveries d
		WHERE `+strings.Join(pageConditions, " AND ")+`
		GROUP BY d.project_id, d.mr_iid`+having+`
		ORDER BY last_received DESC, d.project_id ASC, d.mr_iid ASC
		LIMIT ?`, pageArgs...)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: list webhook deliveries: %w", err)
	}
	defer rows.Close()

	page := make([]webhookDeliveryPageRow, 0, limit+1)
	for rows.Next() {
		var (
			row      webhookDeliveryPageRow
			received string
		)
		if err := rows.Scan(&row.projectID, &row.mrIID, &received); err != nil {
			return nil, false, fmt.Errorf("sqlite: scan webhook delivery: %w", err)
		}
		at, err := parseTime(received)
		if err != nil {
			return nil, false, err
		}
		row.last = at
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("sqlite: iterate webhook deliveries: %w", err)
	}
	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}
	return page, hasMore, nil
}

// webhookDeliveryCounts aggregates outcome and reason counts for exactly the
// selected page keys, within the same snapshot.
func (s *Store) webhookDeliveryCounts(ctx context.Context, page []webhookDeliveryPageRow, snapshot time.Time) ([]store.WebhookDeliverySummary, error) {
	keyConditions := make([]string, 0, len(page))
	countArgs := make([]any, 0, len(page)*2+1)
	countArgs = append(countArgs, formatTime(snapshot))
	for _, row := range page {
		keyConditions = append(keyConditions, `(d.project_id = ? AND d.mr_iid = ?)`)
		countArgs = append(countArgs, row.projectID, row.mrIID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.project_id, d.mr_iid, d.outcome, COALESCE(d.reason, ''), COUNT(*)
		FROM webhook_deliveries d
		WHERE d.received_at <= ? AND (`+strings.Join(keyConditions, " OR ")+`)
		GROUP BY d.project_id, d.mr_iid, d.outcome, COALESCE(d.reason, '')`, countArgs...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list webhook deliveries: %w", err)
	}
	defer rows.Close()

	type summaryKey struct {
		projectID int64
		mrIID     int64
	}
	summaries := make(map[summaryKey]*store.WebhookDeliverySummary, len(page))
	for _, row := range page {
		summaries[summaryKey{projectID: row.projectID, mrIID: row.mrIID}] = &store.WebhookDeliverySummary{
			ProjectID:      row.projectID,
			MRIID:          row.mrIID,
			LastReceivedAt: row.last,
		}
	}
	for rows.Next() {
		var (
			projectID int64
			mrIID     int64
			outcome   string
			reason    string
			count     int64
		)
		if err := rows.Scan(&projectID, &mrIID, &outcome, &reason, &count); err != nil {
			return nil, fmt.Errorf("sqlite: scan webhook delivery: %w", err)
		}
		summary, ok := summaries[summaryKey{projectID: projectID, mrIID: mrIID}]
		if !ok {
			continue
		}
		summary.Deliveries += count
		switch outcome {
		case store.DeliveryEnqueued:
			summary.Enqueued += count
		case store.DeliveryIgnored:
			summary.Ignored += count
			if reason != "" {
				if summary.Reasons == nil {
					summary.Reasons = make(map[string]int64)
				}
				summary.Reasons[reason] += count
			}
		case store.DeliveryRejected:
			summary.Rejected += count
			if reason == "invalid_secret" {
				summary.RejectedInvalidSecret += count
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate webhook deliveries: %w", err)
	}

	result := make([]store.WebhookDeliverySummary, 0, len(page))
	for _, row := range page {
		if summary, ok := summaries[summaryKey{projectID: row.projectID, mrIID: row.mrIID}]; ok {
			result = append(result, *summary)
		}
	}
	return result, nil
}

func sameMRIID(cursor int64, filter *int64) bool {
	if filter == nil {
		return cursor == 0
	}
	return cursor == *filter
}

func optionalMRIID(filter *int64) int64 {
	if filter == nil {
		return 0
	}
	return *filter
}
