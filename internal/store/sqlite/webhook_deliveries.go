package sqlite

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// ListWebhookDeliverySummaries groups recorded webhook deliveries by project
// and merge request for the read-only "why was this MR not reviewed" view. It
// exposes counts and fixed reasons only; the upstream request body is never
// stored, returned or logged. The access filter is applied before grouping.
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
	rows, err := s.db.QueryContext(ctx, `SELECT
		d.project_id, d.mr_iid, d.outcome, COALESCE(d.reason, ''), COUNT(*), MAX(d.received_at)
		FROM webhook_deliveries d
		WHERE `+strings.Join(conditions, " AND ")+`
		GROUP BY d.project_id, d.mr_iid, d.outcome, COALESCE(d.reason, '')`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list webhook deliveries: %w", err)
	}
	defer rows.Close()

	type summaryKey struct {
		projectID int64
		mrIID     int64
	}
	groups := make(map[summaryKey]*store.WebhookDeliverySummary)
	order := make([]summaryKey, 0)
	for rows.Next() {
		var (
			projectID int64
			mrIID     int64
			outcome   string
			reason    string
			count     int64
			received  string
		)
		if err := rows.Scan(&projectID, &mrIID, &outcome, &reason, &count, &received); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan webhook delivery: %w", err)
		}
		at, err := parseTime(received)
		if err != nil {
			return nil, "", err
		}
		key := summaryKey{projectID: projectID, mrIID: mrIID}
		summary, ok := groups[key]
		if !ok {
			summary = &store.WebhookDeliverySummary{ProjectID: projectID, MRIID: mrIID}
			groups[key] = summary
			order = append(order, key)
		}
		summary.Deliveries += count
		if at.After(summary.LastReceivedAt) {
			summary.LastReceivedAt = at
		}
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
		return nil, "", fmt.Errorf("sqlite: iterate webhook deliveries: %w", err)
	}

	summaries := make([]store.WebhookDeliverySummary, 0, len(order))
	for _, key := range order {
		summaries = append(summaries, *groups[key])
	}
	sort.Slice(summaries, func(i, j int) bool {
		return deliverySummaryBefore(summaries[i], summaries[j])
	})

	start := 0
	if cursor != "" {
		bound, err := decodeListCursor(cursor, listCursorDeliveries)
		if err != nil {
			return nil, "", err
		}
		if !sameProjectID(bound.ProjectID, filter.ProjectID) {
			return nil, "", fmt.Errorf("%w: webhook delivery filter changed", store.ErrInvalidCursor)
		}
		if !sameMRIID(bound.MRIID, filter.MRIID) {
			return nil, "", fmt.Errorf("%w: webhook delivery filter changed", store.ErrInvalidCursor)
		}
		boundAt, err := parseTime(bound.At)
		if err != nil {
			return nil, "", fmt.Errorf("%w: webhook delivery cursor", store.ErrInvalidCursor)
		}
		for start < len(summaries) && !deliverySummaryAfter(summaries[start], boundAt, bound.RowProjectID, bound.RowMRIID) {
			start++
		}
	}
	end := start + limit
	next := ""
	if end < len(summaries) {
		last := summaries[end-1]
		next = encodeListCursor(listCursor{
			Kind:         listCursorDeliveries,
			ProjectID:    filter.ProjectID,
			MRIID:        optionalMRIID(filter.MRIID),
			At:           formatTime(last.LastReceivedAt),
			RowProjectID: last.ProjectID,
			RowMRIID:     last.MRIID,
		})
	} else {
		end = len(summaries)
	}
	if start > end {
		start = end
	}
	page := append([]store.WebhookDeliverySummary(nil), summaries[start:end]...)
	return page, next, nil
}

// deliverySummaryBefore is the display order: most recently active MR first,
// then project and MR for a stable tie-break.
func deliverySummaryBefore(a, b store.WebhookDeliverySummary) bool {
	if !a.LastReceivedAt.Equal(b.LastReceivedAt) {
		return a.LastReceivedAt.After(b.LastReceivedAt)
	}
	if a.ProjectID != b.ProjectID {
		return a.ProjectID < b.ProjectID
	}
	return a.MRIID < b.MRIID
}

// deliverySummaryAfter reports whether row sorts after the cursor position.
func deliverySummaryAfter(row store.WebhookDeliverySummary, at time.Time, projectID, mrIID int64) bool {
	if !row.LastReceivedAt.Equal(at) {
		return row.LastReceivedAt.Before(at)
	}
	if row.ProjectID != projectID {
		return row.ProjectID > projectID
	}
	return row.MRIID > mrIID
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
