package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// statusOnlyKind marks list items that have no workflow. Workflow-backed
// items keep kind "review"; status-only items have no id, so the console
// cannot link them to the graph page.
const statusOnlyKind = "status_only"

// statusOnlyReview is a review that settled without a workflow (empty diff,
// everything ignored, omitted snapshot). Verdict is the settled state and
// Reason the persisted detail; CreatedAt is when the status settled.
type statusOnlyReview struct {
	Kind      string    `json:"kind"`
	ProjectID int64     `json:"project_id"`
	MRIID     int64     `json:"mr_iid"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	Verdict   string    `json:"verdict"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// listReviewItems returns one page of the review list: workflow summaries,
// merged newest first with status-only reviews when the filters allow them.
// Without status-only rows the page and cursor are exactly the workflow ones.
func (s *Server) listReviewItems(ctx context.Context, filter store.WorkflowFilter, rawCursor string, limit int) ([]any, string, error) {
	cursor, err := decodeReviewCursor(rawCursor, statusOnlyApplies(filter))
	if err != nil {
		return nil, "", err
	}
	var (
		workflows []store.Workflow
		wNext     string
		statuses  []store.StatusOnlyReview
		sNext     string
	)
	if !cursor.WDone {
		if workflows, wNext, err = s.store.ListWorkflows(ctx, filter, cursor.W, limit); err != nil {
			return nil, "", err
		}
	}
	if !cursor.SDone {
		statusFilter := store.StatusOnlyFilter{
			ProjectID: filter.ProjectID, AccessUserID: filter.AccessUserID, GrantOnly: filter.GrantOnly,
			MRIID: filter.MRIID, Verdict: filter.Verdict, Since: filter.Since,
		}
		if statuses, sNext, err = s.store.ListStatusOnlyReviews(ctx, statusFilter, cursor.S, limit); err != nil {
			return nil, "", err
		}
	}

	// Merge two newest-first streams; a workflow wins a timestamp tie.
	taken := make([]store.Workflow, 0, len(workflows))
	order := make([]bool, 0, limit) // true: next item is a workflow
	i, j := 0, 0
	for len(order) < limit && (i < len(workflows) || j < len(statuses)) {
		if j >= len(statuses) || (i < len(workflows) && !workflows[i].CreatedAt.Before(statuses[j].UpdatedAt)) {
			taken = append(taken, workflows[i])
			order = append(order, true)
			i++
		} else {
			order = append(order, false)
			j++
		}
	}
	summaries, err := s.summarizeReviewPage(ctx, taken)
	if err != nil {
		return nil, "", err
	}
	items := make([]any, 0, len(order))
	wi, si := 0, 0
	for _, isWorkflow := range order {
		if isWorkflow {
			items = append(items, summaries[wi])
			wi++
			continue
		}
		row := statuses[si]
		items = append(items, statusOnlyReview{
			Kind: statusOnlyKind, ProjectID: row.ProjectID, MRIID: row.MRIID, HeadSHA: row.HeadSHA,
			Verdict: row.State, Reason: row.Detail, CreatedAt: row.UpdatedAt,
		})
		si++
	}

	next := reviewCursor{WDone: cursor.WDone, SDone: cursor.SDone, W: cursor.W, S: cursor.S}
	if !cursor.WDone {
		switch {
		case i == len(workflows):
			next.W, next.WDone = wNext, wNext == ""
		case i > 0:
			next.W = store.EncodeCursor(workflows[i-1].CreatedAt, workflows[i-1].ID)
		}
	}
	if !cursor.SDone {
		switch {
		case j == len(statuses):
			next.S, next.SDone = sNext, sNext == ""
		case j > 0:
			next.S = store.StatusOnlyCursor(statuses[j-1])
		}
	}
	return items, next.encode(), nil
}

// statusOnlyApplies reports whether status-only rows can match the filters.
// They have no workflow status, publication, profile or trigger, so any of
// those filters excludes them. Verdict, project, MR and since still apply.
func statusOnlyApplies(f store.WorkflowFilter) bool {
	return f.Status == "" && f.PublicationState == "" && f.Profile == "" && f.Trigger == "" && f.HeadSHA == ""
}

// reviewCursor is the position in the two merged streams. W and S are the
// stores' own cursors; WDone and SDone mean that stream is exhausted.
type reviewCursor struct {
	W     string `json:"w,omitempty"`
	WDone bool   `json:"wd,omitempty"`
	S     string `json:"s,omitempty"`
	SDone bool   `json:"sd,omitempty"`
}

const mergedCursorPrefix = "m1."

// decodeReviewCursor accepts the plain workflow cursor (older clients and
// requests whose filters exclude status-only rows) or a merged cursor.
func decodeReviewCursor(raw string, withStatus bool) (reviewCursor, error) {
	if !withStatus {
		return reviewCursor{W: raw, SDone: true}, nil
	}
	if raw == "" {
		return reviewCursor{}, nil
	}
	if !strings.HasPrefix(raw, mergedCursorPrefix) {
		return reviewCursor{W: raw, SDone: true}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, mergedCursorPrefix))
	if err != nil {
		return reviewCursor{}, store.ErrInvalidCursor
	}
	var cursor reviewCursor
	if err := json.Unmarshal(data, &cursor); err != nil || (cursor.WDone && cursor.SDone) {
		return reviewCursor{}, store.ErrInvalidCursor
	}
	return cursor, nil
}

// encode returns the cursor for the next page, or "" when both streams end.
// With the status-only stream exhausted it is the plain workflow cursor.
func (c reviewCursor) encode() string {
	switch {
	case c.WDone && c.SDone:
		return ""
	case c.SDone:
		return c.W
	}
	data, _ := json.Marshal(c)
	return mergedCursorPrefix + base64.RawURLEncoding.EncodeToString(data)
}
