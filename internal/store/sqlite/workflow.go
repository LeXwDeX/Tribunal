package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

var _ dag.EventStore = (*Store)(nil)

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// Append implements dag.EventStore. The event log and the workflow/node read
// models are written in one SQL transaction. The candidate log is validated
// with dag.Project before anything is committed, so invalid transitions,
// terminal mutations and stale attempts roll back the whole batch.
func (s *Store) Append(ctx context.Context, wfID string, expectRev int64, evs ...dag.Event) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	wfID = strings.TrimSpace(wfID)
	if wfID == "" {
		return 0, errors.New("sqlite: workflow id is required")
	}
	if expectRev < 0 {
		return 0, fmt.Errorf("sqlite: negative expect revision %d", expectRev)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite: begin append: %w", err)
	}
	defer tx.Rollback()

	existing, err := loadEventsTx(ctx, tx, wfID)
	if err != nil {
		return 0, err
	}
	lastSeq := int64(len(existing))
	if expectRev != lastSeq {
		return 0, fmt.Errorf("%w: workflow %s has revision %d, append expects %d",
			dag.ErrStaleRevision, wfID, lastSeq, expectRev)
	}
	exists, storedRev, err := workflowRowTx(ctx, tx, wfID)
	if err != nil {
		return 0, err
	}
	if lastSeq == 0 && exists {
		return 0, fmt.Errorf("sqlite: workflow %s has a read model but no events", wfID)
	}
	if lastSeq > 0 && !exists {
		return 0, fmt.Errorf("sqlite: workflow %s has events but no read model", wfID)
	}
	if exists && storedRev != lastSeq {
		return 0, fmt.Errorf("sqlite: workflow %s graph_rev %d does not match event revision %d",
			wfID, storedRev, lastSeq)
	}
	if len(evs) == 0 {
		return expectRev, nil
	}

	now := s.now().UTC()
	candidate := make([]dag.Event, 0, len(existing)+len(evs))
	candidate = append(candidate, existing...)
	for i, ev := range evs {
		next := ev
		next.Seq = lastSeq + int64(i) + 1
		if next.At.IsZero() {
			next.At = now
		}
		next.At = next.At.UTC()
		if len(next.Payload) == 0 {
			next.Payload = json.RawMessage(`{}`)
		}
		if !json.Valid(next.Payload) {
			return 0, fmt.Errorf("sqlite: event %d payload is not valid JSON", next.Seq)
		}
		if strings.TrimSpace(next.Type) == "" {
			return 0, fmt.Errorf("sqlite: event %d type is required", next.Seq)
		}
		if next.Type == dag.EventWorkflowCreated {
			var created dag.WorkflowCreatedPayload
			if err := json.Unmarshal(next.Payload, &created); err != nil {
				return 0, fmt.Errorf("sqlite: decode workflow.created: %w", err)
			}
			if created.WorkflowID != "" && created.WorkflowID != wfID {
				return 0, fmt.Errorf("sqlite: workflow.created id %q does not match %q", created.WorkflowID, wfID)
			}
		}
		candidate = append(candidate, next)
	}

	wf, err := dag.Project(candidate)
	if err != nil {
		return 0, err
	}
	if wf.Revision != lastSeq+int64(len(evs)) {
		return 0, fmt.Errorf("sqlite: projected revision %d, want %d", wf.Revision, lastSeq+int64(len(evs)))
	}

	meta, err := workflowMetaFromView(wf, wfID)
	if err != nil {
		return 0, err
	}
	if err := requireProjectTx(ctx, tx, meta.projectID); err != nil {
		return 0, err
	}
	if lastSeq == 0 {
		if err := ensureActiveReviewFreeTx(ctx, tx, meta); err != nil {
			return 0, err
		}
	}

	for _, ev := range candidate[lastSeq:] {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events
			(workflow_id, seq, type, payload, at) VALUES (?, ?, ?, ?, ?)`,
			wfID, ev.Seq, ev.Type, string(ev.Payload), formatTime(ev.At)); err != nil {
			return 0, fmt.Errorf("sqlite: insert event %d: %w", ev.Seq, err)
		}
	}

	if err := writeWorkflowTx(ctx, tx, wfID, wf, meta, lastSeq == 0, now); err != nil {
		return 0, err
	}
	if err := writeWorkflowNodesTx(ctx, tx, wfID, wf); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite: commit append: %w", err)
	}
	return wf.Revision, nil
}

func loadEventsTx(ctx context.Context, tx *sql.Tx, wfID string) ([]dag.Event, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT seq, type, payload, at FROM events WHERE workflow_id = ? ORDER BY seq`, wfID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load events: %w", err)
	}
	defer rows.Close()
	var events []dag.Event
	for rows.Next() {
		var (
			ev      dag.Event
			payload string
			at      string
		)
		if err := rows.Scan(&ev.Seq, &ev.Type, &payload, &at); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		if ev.At, err = parseTime(at); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate events: %w", err)
	}
	return events, nil
}

// Load implements dag.EventStore. An unknown workflow yields no events and no
// error, matching the in-memory store; Engine maps that to ErrNotFound.
func (s *Store) Load(ctx context.Context, wfID string) ([]dag.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, type, payload, at FROM events WHERE workflow_id = ? ORDER BY seq`, wfID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load events: %w", err)
	}
	defer rows.Close()
	var events []dag.Event
	for rows.Next() {
		var (
			ev      dag.Event
			payload string
			at      string
		)
		if err := rows.Scan(&ev.Seq, &ev.Type, &payload, &at); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		if ev.At, err = parseTime(at); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate events: %w", err)
	}
	return events, nil
}

// ListUnfinished implements dag.EventStore.
func (s *Store) ListUnfinished(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM workflows WHERE status IN (?, ?) ORDER BY created_at, id`,
		string(dag.WorkflowPending), string(dag.WorkflowRunning))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list unfinished workflows: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan unfinished workflow: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate unfinished workflows: %w", err)
	}
	return ids, nil
}

func workflowRowTx(ctx context.Context, tx *sql.Tx, wfID string) (bool, int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx, `SELECT graph_rev FROM workflows WHERE id = ?`, wfID).Scan(&rev)
	switch {
	case err == nil:
		return true, rev, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, 0, nil
	default:
		return false, 0, fmt.Errorf("sqlite: check workflow: %w", err)
	}
}

func requireProjectTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if projectID <= 0 {
		return errors.New("sqlite: workflow project id must be positive")
	}
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&one)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: project %d", store.ErrNotFound, projectID)
	default:
		return fmt.Errorf("sqlite: check project: %w", err)
	}
}

type workflowMeta struct {
	projectID   int64
	kind        string
	mrIID       *int64
	headSHA     string
	baseSHA     string
	fingerprint string
	profile     string
	trigger     string
	triggeredBy *int64
}

func workflowMetaFromView(wf dag.WorkflowView, wfID string) (workflowMeta, error) {
	var meta workflowMeta
	raw := wf.Meta
	if metaValue := strings.TrimSpace(raw["project_id"]); metaValue != "" {
		id, err := strconv.ParseInt(metaValue, 10, 64)
		if err != nil || id <= 0 {
			return meta, fmt.Errorf("sqlite: workflow %s project_id %q is invalid", wfID, metaValue)
		}
		meta.projectID = id
	}
	if meta.projectID <= 0 {
		return meta, fmt.Errorf("sqlite: workflow %s metadata requires a positive project_id", wfID)
	}
	meta.kind = strings.TrimSpace(raw["kind"])
	switch meta.kind {
	case "review", "onboarding":
	case "":
		return meta, fmt.Errorf("sqlite: workflow %s metadata requires kind", wfID)
	default:
		return meta, fmt.Errorf("sqlite: workflow %s metadata kind %q is invalid", wfID, meta.kind)
	}
	meta.trigger = strings.TrimSpace(raw["trigger"])
	switch meta.trigger {
	case "mr_open", "mr_update", "command", "api", "push", "register":
	case "":
		return meta, fmt.Errorf("sqlite: workflow %s metadata requires trigger", wfID)
	default:
		return meta, fmt.Errorf("sqlite: workflow %s metadata trigger %q is invalid", wfID, meta.trigger)
	}
	if metaValue := strings.TrimSpace(raw["mr_iid"]); metaValue != "" {
		iid, err := strconv.ParseInt(metaValue, 10, 64)
		if err != nil || iid <= 0 {
			return meta, fmt.Errorf("sqlite: workflow %s mr_iid %q is invalid", wfID, metaValue)
		}
		meta.mrIID = &iid
	}
	if meta.kind == "review" && meta.mrIID == nil {
		return meta, fmt.Errorf("sqlite: review workflow %s metadata requires mr_iid", wfID)
	}
	if metaValue := strings.TrimSpace(raw["triggered_by"]); metaValue != "" {
		user, err := strconv.ParseInt(metaValue, 10, 64)
		if err != nil || user <= 0 {
			return meta, fmt.Errorf("sqlite: workflow %s triggered_by %q is invalid", wfID, metaValue)
		}
		meta.triggeredBy = &user
	}
	meta.headSHA = strings.TrimSpace(raw["head_sha"])
	meta.baseSHA = strings.TrimSpace(raw["base_sha"])
	meta.fingerprint = strings.TrimSpace(raw["fingerprint"])
	meta.profile = strings.TrimSpace(raw["profile"])
	return meta, nil
}

func ensureActiveReviewFreeTx(ctx context.Context, tx *sql.Tx, meta workflowMeta) error {
	if meta.kind != "review" || meta.mrIID == nil {
		return nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM workflows
		WHERE project_id = ? AND mr_iid = ? AND kind = 'review' AND status IN ('pending', 'running')
		LIMIT 1`, meta.projectID, *meta.mrIID).Scan(&id)
	switch {
	case err == nil:
		return fmt.Errorf("%w: active review %s for project %d MR %d",
			store.ErrConflict, id, meta.projectID, *meta.mrIID)
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("sqlite: check active review: %w", err)
	}
}

func writeWorkflowTx(ctx context.Context, tx *sql.Tx, wfID string, wf dag.WorkflowView, meta workflowMeta, create bool, now time.Time) error {
	if create {
		_, err := tx.ExecContext(ctx, `INSERT INTO workflows
			(id, kind, project_id, mr_iid, head_sha, base_sha, fingerprint, profile,
			 trigger, triggered_by, status, graph_rev, extends_used, verdict,
			 cancel_reason, error, tokens_in, tokens_out, cost_micros, created_at,
			 started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?)`,
			wfID, meta.kind, meta.projectID, nullableInt(meta.mrIID),
			nullString(meta.headSHA), nullString(meta.baseSHA),
			nullString(meta.fingerprint), nullString(meta.profile),
			meta.trigger, nullableInt(meta.triggeredBy), string(wf.Status), wf.Revision,
			wf.ExtendsUsed, nullString(wf.CancelReason), nullString(wf.Error),
			wf.Usage.InputTokens, wf.Usage.OutputTokens, wf.Usage.CostMicros,
			formatTime(wf.CreatedAt), nullableTime(nonZeroTime(wf.StartedAt)),
			nullableTime(nonZeroTime(wf.FinishedAt)))
		if err != nil {
			if isConstraintErr(err) {
				return fmt.Errorf("%w: workflow %s already exists or conflicts with an active review", store.ErrConflict, wfID)
			}
			return fmt.Errorf("sqlite: insert workflow: %w", err)
		}
		return nil
	}

	res, err := tx.ExecContext(ctx, `UPDATE workflows SET
		status = ?, graph_rev = ?, extends_used = ?, cancel_reason = ?, error = ?,
		tokens_in = ?, tokens_out = ?, cost_micros = ?, started_at = ?, finished_at = ?
		WHERE id = ?`,
		string(wf.Status), wf.Revision, wf.ExtendsUsed, nullString(wf.CancelReason),
		nullString(wf.Error), wf.Usage.InputTokens, wf.Usage.OutputTokens,
		wf.Usage.CostMicros, nullableTime(nonZeroTime(wf.StartedAt)),
		nullableTime(nonZeroTime(wf.FinishedAt)), wfID)
	if err != nil {
		return fmt.Errorf("sqlite: update workflow: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: update workflow: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: workflow %s", store.ErrNotFound, wfID)
	}
	return nil
}

func writeWorkflowNodesTx(ctx context.Context, tx *sql.Tx, wfID string, wf dag.WorkflowView) error {
	for _, id := range wf.Order {
		view := wf.Nodes[id]
		deps, err := json.Marshal(view.Spec.DependsOn)
		if err != nil {
			return fmt.Errorf("sqlite: encode depends_on for %s: %w", id, err)
		}
		if view.Spec.DependsOn == nil {
			deps = []byte("[]")
		}
		params := view.Spec.Params
		if len(params) == 0 || !json.Valid(params) {
			params = json.RawMessage(`{}`)
		}
		reason := view.Error
		if reason == "" && view.Status.Terminal() {
			reason = view.Reason
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workflow_nodes
			(workflow_id, id, kind, status, required, depends_on, params, attempt,
			 provider, model, output, error, tokens_in, tokens_out, cost_micros,
			 added_rev, queued_at, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (workflow_id, id) DO UPDATE SET
				kind = excluded.kind,
				status = excluded.status,
				required = excluded.required,
				depends_on = excluded.depends_on,
				params = excluded.params,
				attempt = excluded.attempt,
				provider = excluded.provider,
				model = excluded.model,
				output = excluded.output,
				error = excluded.error,
				tokens_in = excluded.tokens_in,
				tokens_out = excluded.tokens_out,
				cost_micros = excluded.cost_micros,
				added_rev = excluded.added_rev,
				queued_at = excluded.queued_at,
				started_at = excluded.started_at,
				finished_at = excluded.finished_at`,
			wfID, id, view.Spec.Kind, string(view.Status), boolInt(view.Spec.Required),
			string(deps), string(params), view.Attempt,
			nullString(view.Provider), nullString(view.Model), nullableJSON(view.Output),
			nullString(reason), view.Usage.InputTokens, view.Usage.OutputTokens,
			view.Usage.CostMicros, view.AddedRev,
			nullableTime(nonZeroTime(view.QueuedAt)), nullableTime(nonZeroTime(view.StartedAt)),
			nullableTime(nonZeroTime(view.FinishedAt)))
		if err != nil {
			return fmt.Errorf("sqlite: upsert workflow node %s: %w", id, err)
		}
	}
	return nil
}

func nonZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	value := t.UTC()
	return &value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

const workflowColumns = `id, kind, project_id, mr_iid, head_sha, base_sha, fingerprint,
	profile, trigger, triggered_by, status, graph_rev, extends_used, verdict,
	cancel_reason, error,
	COALESCE((SELECT d.publication_state FROM workflow_dossiers d
		WHERE d.workflow_id = workflows.id), ''),
	COALESCE((SELECT d.publication_error FROM workflow_dossiers d
		WHERE d.workflow_id = workflows.id), ''),
	tokens_in, tokens_out, cost_micros, created_at,
	started_at, finished_at`

func scanWorkflow(row rowScanner) (*store.Workflow, error) {
	var (
		wf         store.Workflow
		mrIID      sql.NullInt64
		headSHA    sql.NullString
		baseSHA    sql.NullString
		fingerpr   sql.NullString
		profile    sql.NullString
		triggerBy  sql.NullInt64
		verdict    sql.NullString
		cancel     sql.NullString
		errText    sql.NullString
		pubState   sql.NullString
		pubError   sql.NullString
		createdAt  string
		startedAt  sql.NullString
		finishedAt sql.NullString
	)
	if err := row.Scan(&wf.ID, &wf.Kind, &wf.ProjectID, &mrIID, &headSHA, &baseSHA,
		&fingerpr, &profile, &wf.Trigger, &triggerBy, &wf.Status, &wf.GraphRev,
		&wf.ExtendsUsed, &verdict, &cancel, &errText, &pubState, &pubError,
		&wf.TokensIn, &wf.TokensOut, &wf.CostMicros, &createdAt, &startedAt, &finishedAt); err != nil {
		return nil, err
	}
	if mrIID.Valid {
		value := mrIID.Int64
		wf.MRIID = &value
	}
	wf.HeadSHA = headSHA.String
	wf.BaseSHA = baseSHA.String
	wf.Fingerprint = fingerpr.String
	wf.Profile = profile.String
	if triggerBy.Valid {
		value := triggerBy.Int64
		wf.TriggeredBy = &value
	}
	wf.Verdict = verdict.String
	wf.CancelReason = cancel.String
	wf.Error = errText.String
	wf.PublicationState = pubState.String
	wf.PublicationError = pubError.String
	var err error
	if wf.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if startedAt.Valid {
		t, err := parseTime(startedAt.String)
		if err != nil {
			return nil, err
		}
		wf.StartedAt = &t
	}
	if finishedAt.Valid {
		t, err := parseTime(finishedAt.String)
		if err != nil {
			return nil, err
		}
		wf.FinishedAt = &t
	}
	return &wf, nil
}

// GetWorkflow returns one projected workflow.
func (s *Store) GetWorkflow(ctx context.Context, id string) (*store.Workflow, error) {
	wf, err := scanWorkflow(s.db.QueryRowContext(ctx,
		`SELECT `+workflowColumns+` FROM workflows WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get workflow: %w", err)
	}
	return wf, nil
}

// FindActiveReview returns the pending or running review for one MR.
func (s *Store) FindActiveReview(ctx context.Context, projectID, mrIID int64) (*store.Workflow, error) {
	if projectID <= 0 || mrIID <= 0 {
		return nil, errors.New("sqlite: project id and mr iid are required")
	}
	wf, err := scanWorkflow(s.db.QueryRowContext(ctx,
		`SELECT `+workflowColumns+` FROM workflows
		 WHERE project_id = ? AND mr_iid = ? AND kind = 'review'
		   AND status IN ('pending', 'running')
		 ORDER BY created_at DESC, id DESC LIMIT 1`, projectID, mrIID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: active review for project %d MR %d", store.ErrNotFound, projectID, mrIID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: find active review: %w", err)
	}
	return wf, nil
}

// SetWorkflowVerdict stores the final review verdict. INCOMPLETE is a valid
// terminal result; a non-PASS verdict must never be treated as a successful
// publication. It stays separate from the event projection so it may be
// written after the workflow finishes.
func (s *Store) SetWorkflowVerdict(ctx context.Context, id, verdict string) error {
	switch verdict {
	case "PASS", "BLOCKING", "INCOMPLETE":
	default:
		return fmt.Errorf("sqlite: invalid verdict %q", verdict)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE workflows SET verdict = ? WHERE id = ?`, verdict, id)
	if err != nil {
		return fmt.Errorf("sqlite: set workflow verdict: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: set workflow verdict: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: workflow %s", store.ErrNotFound, id)
	}
	return nil
}

// LastPublishedHead returns the head SHA of the most recent summary publication
// for an MR. It returns an empty string when nothing was published.
func (s *Store) LastPublishedHead(ctx context.Context, projectID, mrIID int64) (string, error) {
	var sha string
	err := s.db.QueryRowContext(ctx, `SELECT w.head_sha
		FROM publications p JOIN workflows w ON w.id = p.workflow_id
		WHERE p.project_id = ? AND p.mr_iid = ?
		  AND p.kind = 'summary'
		  AND w.head_sha IS NOT NULL AND w.head_sha <> ''
		ORDER BY p.updated_at DESC, p.id DESC LIMIT 1`, projectID, mrIID).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: last published head: %w", err)
	}
	return sha, nil
}

// ListWorkflows returns workflows newest first. The opaque cursor continues
// after the last returned row.
func (s *Store) ListWorkflows(ctx context.Context, filter store.WorkflowFilter, cursor string, limit int) ([]store.Workflow, string, error) {
	limit = normalizeLimit(limit)
	where := []string{"1 = 1"}
	args := make([]any, 0, 8)
	if filter.ProjectID != nil {
		where = append(where, "project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.MRIID != nil {
		where = append(where, "mr_iid = ?")
		args = append(args, *filter.MRIID)
	}
	if filter.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, filter.Kind)
	}
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Verdict != "" {
		where = append(where, "verdict = ?")
		args = append(args, filter.Verdict)
	}
	if filter.PublicationState != "" {
		where = append(where, `EXISTS (SELECT 1 FROM workflow_dossiers d
			WHERE d.workflow_id = workflows.id AND d.publication_state = ?)`)
		args = append(args, filter.PublicationState)
	}
	if filter.Profile != "" {
		where = append(where, "profile = ?")
		args = append(args, filter.Profile)
	}
	if filter.Trigger != "" {
		where = append(where, "trigger = ?")
		args = append(args, filter.Trigger)
	}
	if filter.HeadSHA != "" {
		where = append(where, "head_sha = ?")
		args = append(args, filter.HeadSHA)
	}
	if !filter.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, formatTime(filter.Since.UTC()))
	}
	if clause, accessArgs := projectAccessClause("workflows.project_id", filter.AccessUserID); clause != "" {
		where = append(where, clause)
		args = append(args, accessArgs...)
	}
	if cursor != "" {
		created, id, err := decodeCursor(cursor, store.ErrInvalidCursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, created, created, id)
	}
	query := `SELECT ` + workflowColumns + ` FROM workflows WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list workflows: %w", err)
	}
	defer rows.Close()
	workflows := make([]store.Workflow, 0, limit)
	for rows.Next() {
		wf, err := scanWorkflow(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan workflow: %w", err)
		}
		workflows = append(workflows, *wf)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate workflows: %w", err)
	}
	next := ""
	if len(workflows) > limit {
		workflows = workflows[:limit]
		last := workflows[len(workflows)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return workflows, next, nil
}

const workflowNodeColumns = `workflow_id, id, kind, status, required, depends_on,
	params, attempt, provider, model, output, error, tokens_in, tokens_out,
	cost_micros, added_rev, queued_at, started_at, finished_at`

func scanWorkflowNode(row rowScanner) (*store.WorkflowNode, error) {
	var (
		node       store.WorkflowNode
		required   int
		dependsOn  string
		params     string
		provider   sql.NullString
		model      sql.NullString
		output     sql.NullString
		errText    sql.NullString
		queuedAt   sql.NullString
		startedAt  sql.NullString
		finishedAt sql.NullString
	)
	if err := row.Scan(&node.WorkflowID, &node.ID, &node.Kind, &node.Status, &required,
		&dependsOn, &params, &node.Attempt, &provider, &model, &output, &errText,
		&node.TokensIn, &node.TokensOut, &node.CostMicros, &node.AddedRev,
		&queuedAt, &startedAt, &finishedAt); err != nil {
		return nil, err
	}
	node.Required = required != 0
	if dependsOn == "" {
		dependsOn = "[]"
	}
	if err := json.Unmarshal([]byte(dependsOn), &node.DependsOn); err != nil {
		return nil, fmt.Errorf("sqlite: decode depends_on: %w", err)
	}
	if params == "" {
		params = "{}"
	}
	node.Params = json.RawMessage(params)
	node.Provider = provider.String
	node.Model = model.String
	if output.Valid {
		node.Output = json.RawMessage(output.String)
	}
	node.Error = errText.String
	if queuedAt.Valid {
		t, err := parseTime(queuedAt.String)
		if err != nil {
			return nil, err
		}
		node.QueuedAt = &t
	}
	if startedAt.Valid {
		t, err := parseTime(startedAt.String)
		if err != nil {
			return nil, err
		}
		node.StartedAt = &t
	}
	if finishedAt.Valid {
		t, err := parseTime(finishedAt.String)
		if err != nil {
			return nil, err
		}
		node.FinishedAt = &t
	}
	return &node, nil
}

// GetWorkflowNodes returns projected nodes in graph order.
func (s *Store) GetWorkflowNodes(ctx context.Context, workflowID string) ([]store.WorkflowNode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+workflowNodeColumns+` FROM workflow_nodes
		 WHERE workflow_id = ? ORDER BY added_rev, id`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list workflow nodes: %w", err)
	}
	defer rows.Close()
	nodes := make([]store.WorkflowNode, 0)
	for rows.Next() {
		node, err := scanWorkflowNode(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan workflow node: %w", err)
		}
		nodes = append(nodes, *node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate workflow nodes: %w", err)
	}
	return nodes, nil
}

const publicationColumns = `id, project_id, mr_iid, kind, publish_key, gitlab_id,
	workflow_id, fingerprint, body_hash, created_at, updated_at`

func scanPublication(row rowScanner) (*store.Publication, error) {
	var (
		p         store.Publication
		gitlabID  sql.NullString
		bodyHash  sql.NullString
		createdAt string
		updatedAt string
	)
	if err := row.Scan(&p.ID, &p.ProjectID, &p.MRIID, &p.Kind, &p.PublishKey,
		&gitlabID, &p.WorkflowID, &p.Fingerprint, &bodyHash, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	p.GitlabID = gitlabID.String
	p.BodyHash = bodyHash.String
	var err error
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetPublication returns one publication by its idempotency key.
func (s *Store) GetPublication(ctx context.Context, projectID, mrIID int64, kind, publishKey string) (*store.Publication, error) {
	p, err := scanPublication(s.db.QueryRowContext(ctx,
		`SELECT `+publicationColumns+` FROM publications
		 WHERE project_id = ? AND mr_iid = ? AND kind = ? AND publish_key = ?`,
		projectID, mrIID, kind, publishKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: publication %s/%d/%s", store.ErrNotFound, kind, mrIID, publishKey)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get publication: %w", err)
	}
	return p, nil
}

// UpsertPublication inserts or updates a publication under the unique
// (project, MR, kind, publish_key) key. The original ID and creation time are
// preserved on retry.
func (s *Store) UpsertPublication(ctx context.Context, p store.Publication) (*store.Publication, error) {
	if p.ProjectID <= 0 || p.MRIID <= 0 {
		return nil, errors.New("sqlite: publication project id and mr iid are required")
	}
	if strings.TrimSpace(p.Kind) == "" || strings.TrimSpace(p.PublishKey) == "" {
		return nil, errors.New("sqlite: publication kind and publish key are required")
	}
	if strings.TrimSpace(p.WorkflowID) == "" {
		return nil, errors.New("sqlite: publication workflow id is required")
	}
	if strings.TrimSpace(p.Fingerprint) == "" {
		return nil, errors.New("sqlite: publication fingerprint is required")
	}
	if p.ID == "" {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		p.ID = id
	}
	now := s.now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now

	if _, err := s.db.ExecContext(ctx, `INSERT INTO publications
		(id, project_id, mr_iid, kind, publish_key, gitlab_id, workflow_id,
		 fingerprint, body_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id, mr_iid, kind, publish_key) DO UPDATE SET
			gitlab_id = excluded.gitlab_id,
			workflow_id = excluded.workflow_id,
			fingerprint = excluded.fingerprint,
			body_hash = excluded.body_hash,
			updated_at = excluded.updated_at`,
		p.ID, p.ProjectID, p.MRIID, p.Kind, p.PublishKey, nullString(p.GitlabID),
		p.WorkflowID, p.Fingerprint, nullString(p.BodyHash),
		formatTime(p.CreatedAt), formatTime(p.UpdatedAt)); err != nil {
		return nil, fmt.Errorf("sqlite: upsert publication: %w", err)
	}
	return s.GetPublication(ctx, p.ProjectID, p.MRIID, p.Kind, p.PublishKey)
}

// ListPublications returns publications for one workflow.
func (s *Store) ListPublications(ctx context.Context, workflowID string) ([]store.Publication, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+publicationColumns+` FROM publications
		 WHERE workflow_id = ? ORDER BY updated_at DESC, id DESC`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list publications: %w", err)
	}
	defer rows.Close()
	publications := make([]store.Publication, 0)
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan publication: %w", err)
		}
		publications = append(publications, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate publications: %w", err)
	}
	return publications, nil
}

const findingColumns = `id, workflow_id, origin_node, origin_index, origin_provider,
	severity, category, path, line, side, title, body, evidence, confidence,
	dedupe_group, votes, challenge_status, challenge_node, challenge_evidence,
	decision, decision_reason, final_severity, position_ok, publish_key,
	annotation, annotated_by, annotated_at`

func scanFinding(row rowScanner) (*store.Finding, error) {
	var (
		f                 store.Finding
		originProvider    sql.NullString
		path              sql.NullString
		line              sql.NullInt64
		side              sql.NullString
		evidence          sql.NullString
		confidence        sql.NullString
		dedupeGroup       sql.NullString
		challengeStatus   sql.NullString
		challengeNode     sql.NullString
		challengeEvidence sql.NullString
		decision          sql.NullString
		decisionReason    sql.NullString
		finalSeverity     sql.NullString
		positionOK        sql.NullInt64
		publishKey        sql.NullString
		annotatedBy       sql.NullString
		annotatedAt       sql.NullString
	)
	if err := row.Scan(&f.ID, &f.WorkflowID, &f.OriginNode, &f.OriginIndex,
		&originProvider, &f.Severity, &f.Category, &path, &line, &side, &f.Title,
		&f.Body, &evidence, &confidence, &dedupeGroup, &f.Votes, &challengeStatus,
		&challengeNode, &challengeEvidence, &decision, &decisionReason,
		&finalSeverity, &positionOK, &publishKey, &f.Annotation, &annotatedBy,
		&annotatedAt); err != nil {
		return nil, err
	}
	f.OriginProvider = originProvider.String
	f.Path = path.String
	if line.Valid {
		value := int(line.Int64)
		f.Line = &value
	}
	f.Side = side.String
	f.Evidence = evidence.String
	f.Confidence = confidence.String
	f.DedupeGroup = dedupeGroup.String
	f.ChallengeStatus = challengeStatus.String
	f.ChallengeNode = challengeNode.String
	if challengeEvidence.Valid {
		f.ChallengeEvidence = json.RawMessage(challengeEvidence.String)
	}
	f.Decision = decision.String
	f.DecisionReason = decisionReason.String
	f.FinalSeverity = finalSeverity.String
	if positionOK.Valid {
		value := positionOK.Int64 != 0
		f.PositionOK = &value
	}
	f.PublishKey = publishKey.String
	f.AnnotatedBy = annotatedBy.String
	if annotatedAt.Valid {
		value, err := parseTime(annotatedAt.String)
		if err != nil {
			return nil, err
		}
		f.AnnotatedAt = &value
	}
	return &f, nil
}

// existingFindingAnnotation is the human-owned part of a finding row. Producer
// replacements carry the current value forward and never write their own.
type existingFindingAnnotation struct {
	annotation  string
	annotatedBy string
	annotatedAt *time.Time
}

// ReplaceFindings atomically replaces all findings for one workflow. Human
// annotations survive by ID; producer-supplied annotation fields are ignored.
func (s *Store) ReplaceFindings(ctx context.Context, workflowID string, findings []store.Finding) error {
	if strings.TrimSpace(workflowID) == "" {
		return errors.New("sqlite: workflow id is required")
	}
	seen := make(map[string]struct{}, len(findings))
	for i := range findings {
		f := &findings[i]
		if f.WorkflowID == "" {
			f.WorkflowID = workflowID
		}
		if f.WorkflowID != workflowID {
			return fmt.Errorf("sqlite: finding %s belongs to workflow %s", f.ID, f.WorkflowID)
		}
		if strings.TrimSpace(f.ID) == "" {
			id, err := newID()
			if err != nil {
				return err
			}
			f.ID = id
		}
		if _, duplicate := seen[f.ID]; duplicate {
			return fmt.Errorf("%w: duplicate finding %s", store.ErrConflict, f.ID)
		}
		seen[f.ID] = struct{}{}
		if strings.TrimSpace(f.OriginNode) == "" || strings.TrimSpace(f.Severity) == "" ||
			strings.TrimSpace(f.Category) == "" || strings.TrimSpace(f.Title) == "" ||
			strings.TrimSpace(f.Body) == "" {
			return fmt.Errorf("sqlite: finding %s is missing required fields", f.ID)
		}
		if f.Line != nil && *f.Line < 0 {
			return fmt.Errorf("sqlite: finding %s line is negative", f.ID)
		}
		if len(f.ChallengeEvidence) > 0 && !json.Valid(f.ChallengeEvidence) {
			return fmt.Errorf("sqlite: finding %s challenge evidence is not valid JSON", f.ID)
		}
		if f.Votes == 0 {
			f.Votes = 1
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin replace findings: %w", err)
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workflows WHERE id = ?`, workflowID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: workflow %s", store.ErrNotFound, workflowID)
		}
		return fmt.Errorf("sqlite: check workflow: %w", err)
	}
	existing, err := loadFindingAnnotations(ctx, tx, workflowID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM findings WHERE workflow_id = ?`, workflowID); err != nil {
		return fmt.Errorf("sqlite: clear findings: %w", err)
	}
	for _, f := range findings {
		var line, positionOK any
		if f.Line != nil {
			line = *f.Line
		}
		if f.PositionOK != nil {
			positionOK = boolInt(*f.PositionOK)
		}
		human := existing[f.ID]
		_, err := tx.ExecContext(ctx, `INSERT INTO findings
			(id, workflow_id, origin_node, origin_index, origin_provider, severity,
			 category, path, line, side, title, body, evidence, confidence,
			 dedupe_group, votes, challenge_status, challenge_node, challenge_evidence,
			 decision, decision_reason, final_severity, position_ok, publish_key,
			 annotation, annotated_by, annotated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, workflowID, f.OriginNode, f.OriginIndex, nullString(f.OriginProvider),
			f.Severity, f.Category, nullString(f.Path), line, nullString(f.Side),
			f.Title, f.Body, nullString(f.Evidence), nullString(f.Confidence),
			nullString(f.DedupeGroup), f.Votes, nullString(f.ChallengeStatus),
			nullString(f.ChallengeNode), nullableJSON(f.ChallengeEvidence),
			nullString(f.Decision), nullString(f.DecisionReason),
			nullString(f.FinalSeverity), positionOK, nullString(f.PublishKey),
			human.annotation, nullString(human.annotatedBy), nullableTime(human.annotatedAt))
		if err != nil {
			if isConstraintErr(err) {
				return fmt.Errorf("%w: finding %s", store.ErrConflict, f.ID)
			}
			return fmt.Errorf("sqlite: insert finding %s: %w", f.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit findings: %w", err)
	}
	return nil
}

func loadFindingAnnotations(ctx context.Context, tx *sql.Tx, workflowID string) (map[string]existingFindingAnnotation, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, annotation, annotated_by, annotated_at FROM findings WHERE workflow_id = ?`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load finding annotations: %w", err)
	}
	existing := make(map[string]existingFindingAnnotation)
	for rows.Next() {
		var (
			id          string
			annotation  string
			annotatedBy sql.NullString
			annotatedAt sql.NullString
		)
		if err := rows.Scan(&id, &annotation, &annotatedBy, &annotatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("sqlite: scan finding annotation: %w", err)
		}
		entry := existingFindingAnnotation{annotation: annotation, annotatedBy: annotatedBy.String}
		if annotatedAt.Valid {
			value, err := parseTime(annotatedAt.String)
			if err != nil {
				rows.Close()
				return nil, err
			}
			entry.annotatedAt = &value
		}
		existing[id] = entry
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("sqlite: iterate finding annotations: %w", err)
	}
	rows.Close()
	return existing, nil
}

// SetFindingAnnotation atomically sets or clears the manual annotation on one
// finding scoped to its workflow. The caller supplies the authenticated actor;
// the store generates the timestamp. An empty annotation clears both.
func (s *Store) SetFindingAnnotation(ctx context.Context, workflowID, findingID, annotation, annotatedBy string) (store.Finding, error) {
	if strings.TrimSpace(workflowID) == "" || strings.TrimSpace(findingID) == "" {
		return store.Finding{}, errors.New("sqlite: workflow and finding ids are required")
	}
	if !store.ValidFindingAnnotation(annotation) {
		return store.Finding{}, fmt.Errorf("sqlite: unsupported finding annotation %q", annotation)
	}
	annotatedBy = strings.TrimSpace(annotatedBy)
	var at any
	if annotation != store.FindingAnnotationNone {
		at = formatTime(s.now().UTC())
	} else {
		annotatedBy = ""
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Finding{}, fmt.Errorf("sqlite: begin finding annotation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE findings
		SET annotation = ?, annotated_by = ?, annotated_at = ?
		WHERE id = ? AND workflow_id = ?`,
		annotation, nullString(annotatedBy), at, findingID, workflowID)
	if err != nil {
		return store.Finding{}, fmt.Errorf("sqlite: update finding annotation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return store.Finding{}, fmt.Errorf("sqlite: finding annotation rows: %w", err)
	}
	if affected == 0 {
		return store.Finding{}, fmt.Errorf("%w: finding %s", store.ErrNotFound, findingID)
	}
	row := tx.QueryRowContext(ctx, `SELECT `+findingColumns+` FROM findings
		WHERE id = ? AND workflow_id = ?`, findingID, workflowID)
	finding, err := scanFinding(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Finding{}, fmt.Errorf("%w: finding %s", store.ErrNotFound, findingID)
		}
		return store.Finding{}, fmt.Errorf("sqlite: load finding annotation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.Finding{}, fmt.Errorf("sqlite: commit finding annotation: %w", err)
	}
	return *finding, nil
}

// ListFindings returns findings for one workflow in stable origin order.
func (s *Store) ListFindings(ctx context.Context, filter store.FindingFilter) ([]store.Finding, error) {
	if strings.TrimSpace(filter.WorkflowID) == "" {
		return nil, errors.New("sqlite: finding workflow id is required")
	}
	where := []string{"workflow_id = ?"}
	args := []any{filter.WorkflowID}
	if filter.Decision != "" {
		where = append(where, "decision = ?")
		args = append(args, filter.Decision)
	}
	if filter.Severity != "" {
		where = append(where, "COALESCE(NULLIF(final_severity, ''), severity) = ?")
		args = append(args, filter.Severity)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+findingColumns+` FROM findings
		WHERE `+strings.Join(where, " AND ")+` ORDER BY origin_node, origin_index, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list findings: %w", err)
	}
	defer rows.Close()
	findings := make([]store.Finding, 0)
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan finding: %w", err)
		}
		findings = append(findings, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate findings: %w", err)
	}
	return findings, nil
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}

func encodeCursor(at time.Time, id string) string {
	return encodeCursorParts(formatTime(at), id)
}

func decodeCursor(cursor string, sentinel error) (string, string, error) {
	at, id, ok := decodeCursorParts(cursor)
	if !ok {
		return "", "", fmt.Errorf("%w", sentinel)
	}
	if _, err := parseTime(at); err != nil {
		return "", "", fmt.Errorf("%w", sentinel)
	}
	return at, id, nil
}

func encodeCursorParts(at, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at + "\x00" + id))
}

func decodeCursorParts(cursor string) (string, string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", false
	}
	at, id, ok := strings.Cut(string(raw), "\x00")
	if !ok || at == "" || id == "" {
		return "", "", false
	}
	return at, id, true
}
