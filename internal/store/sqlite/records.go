package sqlite

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

var _ llm.Recorder = (*Store)(nil)

const maxLLMPayload = 1 << 20

// ---------------------------------------------------------------------------
// LLM call audit

// RecordLLMCall persists one provider attempt. Payloads are gzipped and
// bounded; provider credentials never appear in llm.Request or CallRecord.
func (s *Store) RecordLLMCall(ctx context.Context, call llm.CallRecord) error {
	if strings.TrimSpace(call.Meta.WorkflowID) == "" || strings.TrimSpace(call.Meta.NodeID) == "" {
		return errors.New("sqlite: llm call workflow id and node id are required")
	}
	if strings.TrimSpace(call.Provider) == "" || strings.TrimSpace(call.Model) == "" {
		return errors.New("sqlite: llm call provider and model are required")
	}
	var requestValue, responseValue any
	if call.Request != nil {
		requestValue = call.Request
	}
	if call.Response != nil {
		responseValue = call.Response
	}
	request, err := encodeGzipJSON(requestValue)
	if err != nil {
		return err
	}
	response, err := encodeGzipJSON(responseValue)
	if err != nil {
		return err
	}
	var (
		inputTokens  int64
		outputTokens int64
		cacheRead    int64
		cacheWrite   int64
		stopReason   string
		requestID    string
	)
	if call.Response != nil {
		inputTokens = call.Response.Usage.InputTokens
		outputTokens = call.Response.Usage.OutputTokens
		cacheRead = call.Response.Usage.CacheReadTokens
		cacheWrite = call.Response.Usage.CacheWriteTokens
		stopReason = string(call.Response.StopReason)
		requestID = call.Response.RequestID
	}
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO llm_calls
		(id, workflow_id, node_id, turn, provider, model, request, response,
		 input_tokens, output_tokens, cache_read, cache_write, latency_ms,
		 stop_reason, error_kind, request_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, call.Meta.WorkflowID, call.Meta.NodeID, call.Meta.Turn,
		call.Provider, call.Model, request, response, inputTokens, outputTokens,
		cacheRead, cacheWrite, call.Duration.Milliseconds(), nullString(stopReason),
		nullString(llmErrorKind(call.Err)), nullString(requestID),
		formatTime(s.now().UTC()))
	if err != nil {
		return fmt.Errorf("sqlite: record llm call: %w", err)
	}
	return nil
}

// RecordCall implements llm.Recorder. Audit failure must not fail the model
// call, so the error is deliberately dropped.
func (s *Store) RecordCall(ctx context.Context, call llm.CallRecord) {
	_ = s.RecordLLMCall(ctx, call)
}

func llmErrorKind(err error) string {
	if err == nil {
		return ""
	}
	var classified *llm.Error
	if errors.As(err, &classified) {
		return string(classified.Kind)
	}
	return "error"
}

func encodeGzipJSON(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode llm payload: %w", err)
	}
	if len(raw) > maxLLMPayload {
		raw, err = json.Marshal(map[string]any{"truncated": true, "bytes": len(raw)})
		if err != nil {
			return nil, fmt.Errorf("sqlite: encode truncated llm payload: %w", err)
		}
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("sqlite: gzip llm payload: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: gzip llm payload: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeGzipJSON(data []byte) (json.RawMessage, error) {
	if len(data) == 0 {
		return nil, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open llm payload: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, maxLLMPayload+1))
	if err != nil {
		return nil, fmt.Errorf("sqlite: read llm payload: %w", err)
	}
	if len(raw) > maxLLMPayload {
		return nil, errors.New("sqlite: llm payload exceeds stored bound")
	}
	if !json.Valid(raw) {
		return nil, errors.New("sqlite: llm payload is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// ListLLMCalls returns audit rows newest first. Payloads are decompressed only
// when requested.
func (s *Store) ListLLMCalls(ctx context.Context, filter store.LLMCallFilter) ([]store.LLMCall, string, error) {
	limit := normalizeLimit(filter.Limit)
	where := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if filter.WorkflowID != "" {
		where = append(where, "workflow_id = ?")
		args = append(args, filter.WorkflowID)
	}
	if filter.NodeID != "" {
		where = append(where, "node_id = ?")
		args = append(args, filter.NodeID)
	}
	if filter.Cursor != "" {
		created, id, err := decodeCursor(filter.Cursor, store.ErrInvalidCursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, created, created, id)
	}
	payload := "NULL, NULL"
	if filter.IncludePayload {
		payload = "request, response"
	}
	query := `SELECT id, workflow_id, node_id, turn, provider, model, ` + payload + `,
		input_tokens, output_tokens, cache_read, cache_write, latency_ms,
		stop_reason, error_kind, request_id, created_at
		FROM llm_calls WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list llm calls: %w", err)
	}
	defer rows.Close()
	calls := make([]store.LLMCall, 0, limit)
	for rows.Next() {
		var (
			call       store.LLMCall
			request    []byte
			response   []byte
			stopReason sql.NullString
			errorKind  sql.NullString
			requestID  sql.NullString
			createdAt  string
		)
		if err := rows.Scan(&call.ID, &call.WorkflowID, &call.NodeID, &call.Turn,
			&call.Provider, &call.Model, &request, &response, &call.InputTokens,
			&call.OutputTokens, &call.CacheRead, &call.CacheWrite, &call.LatencyMS,
			&stopReason, &errorKind, &requestID, &createdAt); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan llm call: %w", err)
		}
		if filter.IncludePayload {
			if call.Request, err = decodeGzipJSON(request); err != nil {
				return nil, "", fmt.Errorf("sqlite: decode llm request %s: %w", call.ID, err)
			}
			if call.Response, err = decodeGzipJSON(response); err != nil {
				return nil, "", fmt.Errorf("sqlite: decode llm response %s: %w", call.ID, err)
			}
		}
		call.StopReason = stopReason.String
		call.ErrorKind = errorKind.String
		call.RequestID = requestID.String
		if call.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, "", err
		}
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate llm calls: %w", err)
	}
	next := ""
	if len(calls) > limit {
		calls = calls[:limit]
		last := calls[len(calls)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return calls, next, nil
}

// ClearLLMPayloads removes stored request and response bodies older than the
// cutoff while preserving token and latency aggregates.
func (s *Store) ClearLLMPayloads(ctx context.Context, olderThan time.Time) (int64, error) {
	if olderThan.IsZero() {
		return 0, errors.New("sqlite: retention cutoff is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE llm_calls
		SET request = NULL, response = NULL
		WHERE created_at < ? AND (request IS NOT NULL OR response IS NOT NULL)`,
		formatTime(olderThan.UTC()))
	if err != nil {
		return 0, fmt.Errorf("sqlite: clear llm payloads: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: clear llm payloads: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Workspace and context documents

const workspaceColumns = `project_id, state, base_sha, index_sha, context_sha,
	last_refresh_at, last_error, updated_at`

func scanWorkspace(row rowScanner) (*store.Workspace, error) {
	var (
		ws            store.Workspace
		baseSHA       sql.NullString
		indexSHA      sql.NullString
		contextSHA    sql.NullString
		lastRefreshAt sql.NullString
		lastError     sql.NullString
		updatedAt     string
	)
	if err := row.Scan(&ws.ProjectID, &ws.State, &baseSHA, &indexSHA, &contextSHA,
		&lastRefreshAt, &lastError, &updatedAt); err != nil {
		return nil, err
	}
	ws.BaseSHA = baseSHA.String
	ws.IndexSHA = indexSHA.String
	ws.ContextSHA = contextSHA.String
	ws.LastError = lastError.String
	if lastRefreshAt.Valid {
		t, err := parseTime(lastRefreshAt.String)
		if err != nil {
			return nil, err
		}
		ws.LastRefreshAt = &t
	}
	var err error
	if ws.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &ws, nil
}

func validWorkspaceState(state string) bool {
	switch state {
	case "new", "onboarding", "ready", "refreshing", "error":
		return true
	default:
		return false
	}
}

// GetWorkspace returns one workspace state row.
func (s *Store) GetWorkspace(ctx context.Context, projectID int64) (*store.Workspace, error) {
	if projectID <= 0 {
		return nil, errors.New("sqlite: project id is required")
	}
	ws, err := scanWorkspace(s.db.QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE project_id = ?`, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workspace for project %d", store.ErrNotFound, projectID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get workspace: %w", err)
	}
	return ws, nil
}

// SaveWorkspace inserts or replaces the workspace state row.
func (s *Store) SaveWorkspace(ctx context.Context, ws store.Workspace) (*store.Workspace, error) {
	if ws.ProjectID <= 0 {
		return nil, errors.New("sqlite: workspace project id is required")
	}
	if !validWorkspaceState(ws.State) {
		return nil, fmt.Errorf("sqlite: invalid workspace state %q", ws.State)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin save workspace: %w", err)
	}
	defer tx.Rollback()
	if err := requireProjectTx(ctx, tx, ws.ProjectID); err != nil {
		return nil, err
	}
	if ws.UpdatedAt.IsZero() {
		ws.UpdatedAt = s.now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces
		(project_id, state, base_sha, index_sha, context_sha, last_refresh_at,
		 last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id) DO UPDATE SET
			state = excluded.state,
			base_sha = excluded.base_sha,
			index_sha = excluded.index_sha,
			context_sha = excluded.context_sha,
			last_refresh_at = excluded.last_refresh_at,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		ws.ProjectID, ws.State, nullString(ws.BaseSHA), nullString(ws.IndexSHA),
		nullString(ws.ContextSHA), nullableTime(ws.LastRefreshAt),
		nullString(ws.LastError), formatTime(ws.UpdatedAt.UTC())); err != nil {
		return nil, fmt.Errorf("sqlite: save workspace: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit save workspace: %w", err)
	}
	return s.GetWorkspace(ctx, ws.ProjectID)
}

// SetActiveContext points the workspace at the newest unlocked revision of
// baseSHA for every kind stored at that base. A locked current revision stays
// current; only an agents switch updates workspaces.context_sha.
func (s *Store) SetActiveContext(ctx context.Context, projectID int64, baseSHA string) error {
	if projectID <= 0 || strings.TrimSpace(baseSHA) == "" {
		return errors.New("sqlite: project id and base sha are required")
	}
	baseSHA = strings.TrimSpace(baseSHA)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin active context: %w", err)
	}
	defer tx.Rollback()

	var workspaceID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT project_id FROM workspaces WHERE project_id = ?`, projectID).Scan(&workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: workspace for project %d", store.ErrNotFound, projectID)
		}
		return fmt.Errorf("sqlite: check workspace: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT kind FROM context_docs WHERE project_id = ? AND base_sha = ? GROUP BY kind`,
		projectID, baseSHA)
	if err != nil {
		return fmt.Errorf("sqlite: list context kinds: %w", err)
	}
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: scan context kind: %w", err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("sqlite: close context kinds: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: iterate context kinds: %w", err)
	}
	if len(kinds) == 0 {
		return fmt.Errorf("%w: context document %s for project %d", store.ErrNotFound, baseSHA, projectID)
	}

	agentsSwitched := false
	for _, kind := range kinds {
		var currentLocked int
		err := tx.QueryRowContext(ctx, `SELECT locked FROM context_docs
			WHERE project_id = ? AND kind = ? AND active = 1 LIMIT 1`,
			projectID, kind).Scan(&currentLocked)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: read current context version: %w", err)
		}
		if currentLocked != 0 {
			continue
		}
		var id string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM context_docs
			 WHERE project_id = ? AND base_sha = ? AND kind = ?
			 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
			projectID, baseSHA, kind).Scan(&id); err != nil {
			return fmt.Errorf("sqlite: find context version: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE context_docs SET active = 0 WHERE project_id = ? AND kind = ? AND active = 1`,
			projectID, kind); err != nil {
			return fmt.Errorf("sqlite: clear active context: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE context_docs SET active = 1 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("sqlite: set active context version: %w", err)
		}
		if kind == "agents" {
			agentsSwitched = true
		}
	}

	if agentsSwitched {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET context_sha = ?, updated_at = ?
			WHERE project_id = ?`, baseSHA, formatTime(s.now().UTC()), projectID); err != nil {
			return fmt.Errorf("sqlite: set active context: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit active context: %w", err)
	}
	return nil
}

const contextDocColumns = `id, project_id, base_sha, kind, source, content, locked,
	model, workflow_id, created_at, updated_by, active`

func validContextKind(kind string) bool {
	return kind == "agents" || kind == "claude"
}

func scanContextDoc(row rowScanner) (*store.ContextDoc, error) {
	doc, _, err := scanContextDocAt(row, false)
	return doc, err
}

func scanContextDocAt(row rowScanner, withRowID bool) (*store.ContextDoc, int64, error) {
	var (
		doc        store.ContextDoc
		rowID      int64
		locked     int
		active     int
		model      sql.NullString
		workflowID sql.NullString
		createdAt  string
		updatedBy  sql.NullInt64
	)
	dest := []any{&doc.ID, &doc.ProjectID, &doc.BaseSHA, &doc.Kind, &doc.Source,
		&doc.Content, &locked, &model, &workflowID, &createdAt, &updatedBy, &active}
	if withRowID {
		dest = append([]any{&rowID}, dest...)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, 0, err
	}
	doc.Locked = locked != 0
	doc.Active = active != 0
	doc.Model = model.String
	doc.WorkflowID = workflowID.String
	if updatedBy.Valid {
		value := updatedBy.Int64
		doc.UpdatedBy = &value
	}
	var err error
	if doc.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, 0, err
	}
	return &doc, rowID, nil
}

func (s *Store) workspaceRefs(ctx context.Context, projectID int64) (baseSHA, contextSHA string, err error) {
	var base, contextRef sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT base_sha, context_sha FROM workspaces WHERE project_id = ?`,
		projectID).Scan(&base, &contextRef)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("sqlite: read workspace refs: %w", err)
	}
	return base.String, contextRef.String, nil
}

func applyContextFlags(doc *store.ContextDoc, baseSHA string) {
	doc.Stale = baseSHA != "" && doc.BaseSHA != baseSHA
}

// GetContextDoc returns the newest revision stored for one base and kind.
func (s *Store) GetContextDoc(ctx context.Context, projectID int64, baseSHA, kind string) (*store.ContextDoc, error) {
	if projectID <= 0 || strings.TrimSpace(baseSHA) == "" || !validContextKind(kind) {
		return nil, errors.New("sqlite: project id, base sha and kind are required")
	}
	doc, err := scanContextDoc(s.db.QueryRowContext(ctx,
		`SELECT `+contextDocColumns+` FROM context_docs
		 WHERE project_id = ? AND base_sha = ? AND kind = ?
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`, projectID, baseSHA, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: context %s %s for project %d", store.ErrNotFound, kind, baseSHA, projectID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get context document: %w", err)
	}
	wsBase, _, err := s.workspaceRefs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	applyContextFlags(doc, wsBase)
	return doc, nil
}

func (s *Store) getContextDocByID(ctx context.Context, id string) (*store.ContextDoc, error) {
	doc, err := scanContextDoc(s.db.QueryRowContext(ctx,
		`SELECT `+contextDocColumns+` FROM context_docs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: context document %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get context document: %w", err)
	}
	wsBase, _, err := s.workspaceRefs(ctx, doc.ProjectID)
	if err != nil {
		return nil, err
	}
	applyContextFlags(doc, wsBase)
	return doc, nil
}

// GetCurrentContextDoc returns the active revision. Without an active row it
// falls back to the newest stored revision.
func (s *Store) GetCurrentContextDoc(ctx context.Context, projectID int64, kind string) (*store.ContextDoc, error) {
	if projectID <= 0 || !validContextKind(kind) {
		return nil, errors.New("sqlite: project id and kind are required")
	}
	wsBase, _, err := s.workspaceRefs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	doc, err := scanContextDoc(s.db.QueryRowContext(ctx,
		`SELECT `+contextDocColumns+` FROM context_docs
		 WHERE project_id = ? AND kind = ? AND active = 1 LIMIT 1`, projectID, kind))
	if errors.Is(err, sql.ErrNoRows) {
		doc, err = scanContextDoc(s.db.QueryRowContext(ctx,
			`SELECT `+contextDocColumns+` FROM context_docs
			 WHERE project_id = ? AND kind = ?
			 ORDER BY created_at DESC, rowid DESC LIMIT 1`, projectID, kind))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: context %s for project %d", store.ErrNotFound, kind, projectID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get current context document: %w", err)
	}
	applyContextFlags(doc, wsBase)
	return doc, nil
}

const contextRowIDCursorPrefix = "rowid:"

func encodeContextCursor(at string, rowID int64) string {
	return encodeCursorParts(at, contextRowIDCursorPrefix+strconv.FormatInt(rowID, 10))
}

// decodeContextCursor accepts the rowid cursor and older id cursors. An old
// cursor is resolved to its rowid so pagination stays stable when timestamps
// are equal; a cursor whose row disappeared is invalid, never silently skipped.
func (s *Store) decodeContextCursor(ctx context.Context, projectID int64, cursor string) (string, int64, error) {
	at, token, ok := decodeCursorParts(cursor)
	if !ok {
		return "", 0, fmt.Errorf("%w: context cursor", store.ErrInvalidCursor)
	}
	if _, err := parseTime(at); err != nil {
		return "", 0, fmt.Errorf("%w: context cursor", store.ErrInvalidCursor)
	}
	if strings.HasPrefix(token, contextRowIDCursorPrefix) {
		rowID, err := strconv.ParseInt(strings.TrimPrefix(token, contextRowIDCursorPrefix), 10, 64)
		if err != nil || rowID <= 0 {
			return "", 0, fmt.Errorf("%w: context cursor", store.ErrInvalidCursor)
		}
		return at, rowID, nil
	}
	var rowID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT rowid FROM context_docs WHERE id = ? AND project_id = ?`, token, projectID).Scan(&rowID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("%w: context cursor", store.ErrInvalidCursor)
	}
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: resolve context cursor: %w", err)
	}
	return at, rowID, nil
}

// ListContextDocs returns document versions newest first.
func (s *Store) ListContextDocs(ctx context.Context, filter store.ContextDocFilter) ([]store.ContextDoc, string, error) {
	if filter.ProjectID <= 0 {
		return nil, "", errors.New("sqlite: project id is required")
	}
	if filter.Kind != "" && !validContextKind(filter.Kind) {
		return nil, "", fmt.Errorf("sqlite: invalid context kind %q", filter.Kind)
	}
	limit := normalizeLimit(filter.Limit)
	where := []string{"project_id = ?"}
	args := []any{filter.ProjectID}
	if filter.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, filter.Kind)
	}
	if filter.Cursor != "" {
		created, rowID, err := s.decodeContextCursor(ctx, filter.ProjectID, filter.Cursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND rowid < ?))")
		args = append(args, created, created, rowID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, `+contextDocColumns+` FROM context_docs
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, rowid DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list context documents: %w", err)
	}
	defer rows.Close()
	docs := make([]store.ContextDoc, 0, limit)
	rowIDs := make([]int64, 0, limit)
	for rows.Next() {
		doc, rowID, err := scanContextDocAt(rows, true)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan context document: %w", err)
		}
		docs = append(docs, *doc)
		rowIDs = append(rowIDs, rowID)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate context documents: %w", err)
	}
	wsBase, _, err := s.workspaceRefs(ctx, filter.ProjectID)
	if err != nil {
		return nil, "", err
	}
	for i := range docs {
		applyContextFlags(&docs[i], wsBase)
	}
	next := ""
	if len(docs) > limit {
		docs = docs[:limit]
		last := docs[len(docs)-1]
		next = encodeContextCursor(formatTime(last.CreatedAt), rowIDs[len(docs)-1])
	}
	return docs, next, nil
}

// SaveContextDoc appends a generated revision. A locked current revision stays
// active: the new revision is stored but not switched to. Retrying the same
// workflow returns its existing revision instead of appending another one.
func (s *Store) SaveContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error) {
	return s.saveContextDoc(ctx, doc, false)
}

// SaveManualContextDoc appends an explicit administrator edit and makes it the
// active revision. Previous revisions keep their content and lock flag.
func (s *Store) SaveManualContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error) {
	return s.saveContextDoc(ctx, doc, true)
}

func (s *Store) saveContextDoc(ctx context.Context, doc store.ContextDoc, manual bool) (*store.ContextDoc, error) {
	if doc.ProjectID <= 0 || strings.TrimSpace(doc.BaseSHA) == "" {
		return nil, errors.New("sqlite: context document project id and base sha are required")
	}
	if !validContextKind(doc.Kind) {
		return nil, fmt.Errorf("sqlite: invalid context kind %q", doc.Kind)
	}
	if strings.TrimSpace(doc.Content) == "" {
		return nil, errors.New("sqlite: context document content is required")
	}
	doc.BaseSHA = strings.TrimSpace(doc.BaseSHA)
	doc.WorkflowID = strings.TrimSpace(doc.WorkflowID)
	if manual {
		doc.Source = "manual"
	} else {
		doc.Source = "generated"
	}
	now := s.now().UTC()
	if doc.ID == "" {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		doc.ID = id
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin save context document: %w", err)
	}
	defer tx.Rollback()
	if err := requireProjectTx(ctx, tx, doc.ProjectID); err != nil {
		return nil, err
	}

	// A workflow retry must not append a second generated revision. The lookup
	// and the insert share one transaction, and SQLite serializes writers.
	if !manual && doc.WorkflowID != "" {
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM context_docs
			WHERE project_id = ? AND base_sha = ? AND kind = ?
				AND source = 'generated' AND workflow_id = ?
			ORDER BY created_at, rowid LIMIT 1`,
			doc.ProjectID, doc.BaseSHA, doc.Kind, doc.WorkflowID).Scan(&existingID)
		switch {
		case err == nil:
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("sqlite: commit context document retry: %w", err)
			}
			return s.getContextDocByID(ctx, existingID)
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("sqlite: find context document retry: %w", err)
		}
	}

	var currentLocked int
	err = tx.QueryRowContext(ctx, `SELECT locked FROM context_docs
		WHERE project_id = ? AND kind = ? AND active = 1 LIMIT 1`,
		doc.ProjectID, doc.Kind).Scan(&currentLocked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlite: read current context document: %w", err)
	}
	// A manual save explicitly replaces the current revision, including a
	// locked one. A generated save only becomes current when nothing is locked.
	activate := manual || currentLocked == 0

	doc.CreatedAt = now
	doc.Locked = manual && doc.Locked
	if _, err := tx.ExecContext(ctx, `INSERT INTO context_docs
		(id, project_id, base_sha, kind, source, content, locked, model,
		 workflow_id, created_at, updated_by, active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		doc.ID, doc.ProjectID, doc.BaseSHA, doc.Kind, doc.Source, doc.Content,
		boolInt(doc.Locked), nullString(doc.Model), nullString(doc.WorkflowID),
		formatTime(doc.CreatedAt), nullableInt(doc.UpdatedBy)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: context document already exists", store.ErrConflict)
		}
		return nil, fmt.Errorf("sqlite: insert context document: %w", err)
	}
	if activate {
		if _, err := tx.ExecContext(ctx,
			`UPDATE context_docs SET active = 0
			 WHERE project_id = ? AND kind = ? AND active = 1`,
			doc.ProjectID, doc.Kind); err != nil {
			return nil, fmt.Errorf("sqlite: clear active context document: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE context_docs SET active = 1 WHERE id = ?`, doc.ID); err != nil {
			return nil, fmt.Errorf("sqlite: activate context document: %w", err)
		}
		// workspaces.context_sha is the review-facing agents base. Claude has
		// its own active revision and must not overwrite that reference.
		if doc.Kind == "agents" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE workspaces SET context_sha = ?, updated_at = ? WHERE project_id = ?`,
				doc.BaseSHA, formatTime(now), doc.ProjectID); err != nil {
				return nil, fmt.Errorf("sqlite: update workspace context sha: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit context document: %w", err)
	}
	return s.getContextDocByID(ctx, doc.ID)
}

// LockContextDoc toggles the lock flag of the active revision without touching
// content. Older revisions keep their own flags.
func (s *Store) LockContextDoc(ctx context.Context, projectID int64, baseSHA, kind string, locked bool, userID *int64) error {
	if projectID <= 0 || strings.TrimSpace(baseSHA) == "" || !validContextKind(kind) {
		return errors.New("sqlite: project id, base sha and kind are required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE context_docs
		SET locked = ?, updated_by = COALESCE(?, updated_by)
		WHERE project_id = ? AND base_sha = ? AND kind = ? AND active = 1`,
		boolInt(locked), nullableInt(userID), projectID, baseSHA, kind)
	if err != nil {
		return fmt.Errorf("sqlite: lock context document: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: lock context document: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: context %s %s for project %d", store.ErrNotFound, kind, baseSHA, projectID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Feedback

const feedbackColumns = `id, project_id, mr_iid, publication_id, finding_id,
	signal, gitlab_user_id, observed_at`

func scanFeedback(row rowScanner) (*store.Feedback, error) {
	var (
		f          store.Feedback
		findingID  sql.NullString
		gitlabUser sql.NullInt64
		observedAt string
	)
	if err := row.Scan(&f.ID, &f.ProjectID, &f.MRIID, &f.PublicationID, &findingID,
		&f.Signal, &gitlabUser, &observedAt); err != nil {
		return nil, err
	}
	f.FindingID = findingID.String
	if gitlabUser.Valid {
		value := gitlabUser.Int64
		f.GitlabUserID = &value
	}
	var err error
	if f.ObservedAt, err = parseTime(observedAt); err != nil {
		return nil, err
	}
	return &f, nil
}

// UpsertFeedback records a signal idempotently. A nil GitLab user is anonymous
// and is matched with IS NULL, because SQLite unique indexes treat NULLs as
// distinct.
func (s *Store) UpsertFeedback(ctx context.Context, f store.Feedback) (*store.Feedback, error) {
	if f.ProjectID <= 0 || f.MRIID <= 0 {
		return nil, errors.New("sqlite: feedback project id and mr iid are required")
	}
	if strings.TrimSpace(f.PublicationID) == "" || strings.TrimSpace(f.Signal) == "" {
		return nil, errors.New("sqlite: feedback publication id and signal are required")
	}
	now := s.now().UTC()
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	} else {
		f.ObservedAt = f.ObservedAt.UTC()
	}

	match := `publication_id = ? AND signal = ? AND gitlab_user_id IS NULL`
	matchArgs := []any{f.PublicationID, f.Signal}
	if f.GitlabUserID != nil {
		match = `publication_id = ? AND signal = ? AND gitlab_user_id = ?`
		matchArgs = append(matchArgs, *f.GitlabUserID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin feedback: %w", err)
	}
	defer tx.Rollback()
	var existingID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM feedback WHERE `+match, matchArgs...).Scan(&existingID)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE feedback
			SET finding_id = ?, observed_at = ? WHERE id = ?`,
			nullString(f.FindingID), formatTime(f.ObservedAt), existingID); err != nil {
			return nil, fmt.Errorf("sqlite: update feedback: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("sqlite: commit feedback: %w", err)
		}
		return s.getFeedbackByID(ctx, existingID)
	case errors.Is(err, sql.ErrNoRows):
		if f.ID == "" {
			id, err := newID()
			if err != nil {
				return nil, err
			}
			f.ID = id
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO feedback
			(id, project_id, mr_iid, publication_id, finding_id, signal,
			 gitlab_user_id, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, f.ProjectID, f.MRIID, f.PublicationID, nullString(f.FindingID),
			f.Signal, nullableInt(f.GitlabUserID), formatTime(f.ObservedAt))
		if err != nil {
			if isConstraintErr(err) {
				return nil, fmt.Errorf("%w: feedback already exists", store.ErrConflict)
			}
			return nil, fmt.Errorf("sqlite: insert feedback: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("sqlite: commit feedback: %w", err)
		}
		return &f, nil
	default:
		return nil, fmt.Errorf("sqlite: read feedback: %w", err)
	}
}

func (s *Store) getFeedbackByID(ctx context.Context, id string) (*store.Feedback, error) {
	f, err := scanFeedback(s.db.QueryRowContext(ctx,
		`SELECT `+feedbackColumns+` FROM feedback WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: feedback %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get feedback: %w", err)
	}
	return f, nil
}

func feedbackWhere(filter store.FeedbackFilter) ([]string, []any) {
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
	if filter.PublicationID != "" {
		where = append(where, "publication_id = ?")
		args = append(args, filter.PublicationID)
	}
	if filter.Signal != "" {
		where = append(where, "signal = ?")
		args = append(args, filter.Signal)
	}
	if !filter.From.IsZero() {
		where = append(where, "observed_at >= ?")
		args = append(args, formatTime(filter.From.UTC()))
	}
	if !filter.To.IsZero() {
		where = append(where, "observed_at < ?")
		args = append(args, formatTime(filter.To.UTC()))
	}
	if clause, accessArgs := projectAccessClause("feedback.project_id", filter.AccessUserID); clause != "" {
		where = append(where, clause)
		args = append(args, accessArgs...)
	}
	return where, args
}

// ListFeedback returns feedback newest first.
func (s *Store) ListFeedback(ctx context.Context, filter store.FeedbackFilter) ([]store.Feedback, string, error) {
	where, args := feedbackWhere(filter)
	limit := normalizeLimit(filter.Limit)
	if filter.Cursor != "" {
		observed, id, err := decodeCursor(filter.Cursor, store.ErrInvalidCursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(observed_at < ? OR (observed_at = ? AND id < ?))")
		args = append(args, observed, observed, id)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+feedbackColumns+` FROM feedback
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY observed_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list feedback: %w", err)
	}
	defer rows.Close()
	items := make([]store.Feedback, 0, limit)
	for rows.Next() {
		f, err := scanFeedback(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan feedback: %w", err)
		}
		items = append(items, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate feedback: %w", err)
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeCursor(last.ObservedAt, last.ID)
	}
	return items, next, nil
}

// FeedbackTotals returns counts by signal and, when attribution exists, by
// provider and review role. Raw totals always cover every stored signal; the
// provider records only include signals that resolve to a persisted finding.
func (s *Store) FeedbackTotals(ctx context.Context, filter store.FeedbackFilter) (store.FeedbackTotals, error) {
	where, args := feedbackWhere(filter)
	totals := store.FeedbackTotals{
		BySignal:   make(map[string]int64),
		ByProvider: make([]store.FeedbackProviderStats, 0),
	}
	rows, err := s.db.QueryContext(ctx, `SELECT signal, COUNT(*) FROM feedback
		WHERE `+strings.Join(where, " AND ")+` GROUP BY signal ORDER BY signal`, args...)
	if err != nil {
		return totals, fmt.Errorf("sqlite: feedback totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			signal string
			count  int64
		)
		if err := rows.Scan(&signal, &count); err != nil {
			return totals, fmt.Errorf("sqlite: scan feedback total: %w", err)
		}
		totals.BySignal[signal] = count
		totals.Total += count
	}
	if err := rows.Err(); err != nil {
		return totals, fmt.Errorf("sqlite: iterate feedback totals: %w", err)
	}
	providerTotals, err := s.feedbackProviderTotals(ctx, where, args)
	if err != nil {
		return totals, err
	}
	totals.ByProvider = providerTotals
	return totals, nil
}

// feedbackProviderTotals attributes feedback to the provider that proposed a
// finding and to the provider of the node that challenged it. Each role joins
// the primary key of its own table, so additional nodes or LLM calls cannot
// multiply a signal. A publication must be a discussion for the same workflow,
// project and merge request; summary and unlinked feedback stay unattributed.
func (s *Store) feedbackProviderTotals(ctx context.Context, where []string, args []any) ([]store.FeedbackProviderStats, error) {
	feedbackSubquery := `SELECT id, project_id, mr_iid, publication_id, finding_id, signal
		FROM feedback WHERE ` + strings.Join(where, " AND ")
	baseJoin := ` FROM (` + feedbackSubquery + `) fb
		JOIN findings f ON f.id = fb.finding_id
		JOIN publications p ON p.id = fb.publication_id AND p.kind = 'discussion'
		JOIN workflows w ON w.id = f.workflow_id AND w.id = p.workflow_id`
	consistency := ` WHERE w.kind = 'review'
		  AND p.project_id = fb.project_id AND p.mr_iid = fb.mr_iid
		  AND w.project_id = fb.project_id AND w.mr_iid = fb.mr_iid`
	proposerSource := `SELECT 'proposer' AS role, f.origin_provider AS provider, fb.signal AS signal` +
		baseJoin + consistency + `
		  AND f.origin_provider IS NOT NULL AND f.origin_provider <> ''`
	challengerSource := `SELECT 'challenger' AS role, n.provider AS provider, fb.signal AS signal` +
		baseJoin + `
		JOIN workflow_nodes n ON n.workflow_id = f.workflow_id AND n.id = f.challenge_node` +
		consistency + `
		  AND n.provider IS NOT NULL AND n.provider <> ''`
	query := `SELECT role, provider, signal, COUNT(*) FROM (` +
		proposerSource + `
			UNION ALL
			` + challengerSource + `
			)
		GROUP BY role, provider, signal
		ORDER BY provider, role, signal`
	// The attribution fragment appears in both UNION branches, so its filter
	// arguments are bound twice.
	queryArgs := append(append([]any(nil), args...), args...)
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: feedback provider totals: %w", err)
	}
	defer rows.Close()
	entries := make([]store.FeedbackProviderStats, 0)
	index := make(map[string]int)
	for rows.Next() {
		var (
			role     string
			provider string
			signal   string
			count    int64
		)
		if err := rows.Scan(&role, &provider, &signal, &count); err != nil {
			return nil, fmt.Errorf("sqlite: scan feedback provider total: %w", err)
		}
		key := provider + "\x00" + role
		position, ok := index[key]
		if !ok {
			position = len(entries)
			index[key] = position
			entries = append(entries, store.FeedbackProviderStats{
				Provider: provider, Role: role, BySignal: make(map[string]int64),
			})
		}
		entries[position].Total += count
		entries[position].BySignal[signal] += count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate feedback provider totals: %w", err)
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// Aggregates for the M6 API

func statsConditions(filter store.StatsFilter, alias string) ([]string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if !filter.From.IsZero() {
		conditions = append(conditions, alias+".created_at >= ?")
		args = append(args, formatTime(filter.From.UTC()))
	}
	if !filter.To.IsZero() {
		conditions = append(conditions, alias+".created_at < ?")
		args = append(args, formatTime(filter.To.UTC()))
	}
	if filter.ProjectID != nil {
		conditions = append(conditions, alias+".project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if clause, accessArgs := projectAccessClause(alias+".project_id", filter.AccessUserID); clause != "" {
		conditions = append(conditions, clause)
		args = append(args, accessArgs...)
	}
	return conditions, args
}

type statsBucket struct {
	runs       int64
	costMicros int64
	durations  []float64
}

type groupAccumulator struct {
	runs       int64
	costMicros int64
	durations  []float64
	days       map[string]*statsBucket
}

func dayBucket(days map[string]*statsBucket, day string) *statsBucket {
	bucket, ok := days[day]
	if !ok {
		bucket = &statsBucket{}
		days[day] = bucket
	}
	return bucket
}

func projectDaysFor(series map[int64]map[string]*statsBucket, projectID int64) map[string]*statsBucket {
	days, ok := series[projectID]
	if !ok {
		days = make(map[string]*statsBucket)
		series[projectID] = days
	}
	return days
}

func groupAccumulatorFor(groups map[string]*groupAccumulator, key string) *groupAccumulator {
	group, ok := groups[key]
	if !ok {
		group = &groupAccumulator{days: make(map[string]*statsBucket)}
		groups[key] = group
	}
	return group
}

func (g *groupAccumulator) add(day string, cost int64, duration *float64) {
	g.runs++
	g.costMicros += cost
	bucket := dayBucket(g.days, day)
	bucket.runs++
	bucket.costMicros += cost
	if duration != nil {
		g.durations = append(g.durations, *duration)
		bucket.durations = append(bucket.durations, *duration)
	}
}

func durationSummary(values []float64) (int64, *float64, *float64) {
	if len(values) == 0 {
		return 0, nil, nil
	}
	var sum float64
	for _, value := range values {
		sum += value
	}
	avg := sum / float64(len(values))
	p95 := percentile95(values)
	return int64(len(values)), &avg, &p95
}

func dailyTrend(days map[string]*statsBucket) []store.DailyStats {
	names := make([]string, 0, len(days))
	for day := range days {
		names = append(names, day)
	}
	sort.Strings(names)
	trend := make([]store.DailyStats, 0, len(names))
	for _, day := range names {
		bucket := days[day]
		samples, avg, p95 := durationSummary(bucket.durations)
		trend = append(trend, store.DailyStats{
			Day:             day,
			Runs:            bucket.runs,
			CostMicros:      bucket.costMicros,
			DurationSamples: samples,
			AvgDurationMS:   avg,
			P95DurationMS:   p95,
		})
	}
	return trend
}

func providerTrend(groups map[string]*groupAccumulator) []store.ProviderStats {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]store.ProviderStats, 0, len(names))
	for _, name := range names {
		group := groups[name]
		samples, avg, p95 := durationSummary(group.durations)
		out = append(out, store.ProviderStats{
			Provider:        name,
			Runs:            group.runs,
			CostMicros:      group.costMicros,
			DurationSamples: samples,
			AvgDurationMS:   avg,
			P95DurationMS:   p95,
			Trend:           dailyTrend(group.days),
		})
	}
	return out
}

func blockTrend(groups map[string]*groupAccumulator) []store.BlockStats {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]store.BlockStats, 0, len(names))
	for _, name := range names {
		group := groups[name]
		samples, avg, p95 := durationSummary(group.durations)
		out = append(out, store.BlockStats{
			Block:           name,
			Runs:            group.runs,
			CostMicros:      group.costMicros,
			DurationSamples: samples,
			AvgDurationMS:   avg,
			P95DurationMS:   p95,
			Trend:           dailyTrend(group.days),
		})
	}
	return out
}

// Stats aggregates workflow outcomes, findings decisions, costs and per-model
// audit usage. Unknown denominators stay nil.
func (s *Store) Stats(ctx context.Context, filter store.StatsFilter) (store.Stats, error) {
	stats := store.Stats{
		ByProject:         make([]store.ProjectStats, 0),
		ByModel:           make([]store.ModelStats, 0),
		ByProvider:        make([]store.ProviderStats, 0),
		ByBlock:           make([]store.BlockStats, 0),
		QualityByProvider: make([]store.ProviderQualityStats, 0),
		Trend:             make([]store.DailyStats, 0),
		Decisions:         make(map[string]int64),
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && !filter.From.Before(filter.To) {
		return stats, errors.New("sqlite: stats from must be before to")
	}
	conditions, args := statsConditions(filter, "w")
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	projectIndex := make(map[int64]int)
	durations := make([]float64, 0)
	projectDurations := make(map[int64][]float64)
	topDays := make(map[string]*statsBucket)
	projectDays := make(map[int64]map[string]*statsBucket)
	rows, err := s.db.QueryContext(ctx, `SELECT
		w.project_id, w.verdict, w.started_at, w.finished_at, w.cost_micros, w.created_at
		FROM workflows w `+where, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats workflows: %w", err)
	}
	for rows.Next() {
		var (
			projectID int64
			verdict   sql.NullString
			started   sql.NullString
			finished  sql.NullString
			cost      int64
			created   string
		)
		if err := rows.Scan(&projectID, &verdict, &started, &finished, &cost, &created); err != nil {
			rows.Close()
			return stats, fmt.Errorf("sqlite: scan stats workflow: %w", err)
		}
		createdAt, err := parseTime(created)
		if err != nil {
			rows.Close()
			return stats, err
		}
		day := createdAt.UTC().Format("2006-01-02")
		stats.Runs++
		stats.CostMicros += cost
		topBucket := dayBucket(topDays, day)
		topBucket.runs++
		topBucket.costMicros += cost
		index, ok := projectIndex[projectID]
		if !ok {
			index = len(stats.ByProject)
			projectIndex[projectID] = index
			stats.ByProject = append(stats.ByProject, store.ProjectStats{ProjectID: projectID})
		}
		stats.ByProject[index].Runs++
		stats.ByProject[index].CostMicros += cost
		projectBucket := dayBucket(projectDaysFor(projectDays, projectID), day)
		projectBucket.runs++
		projectBucket.costMicros += cost
		if verdict.String == "BLOCKING" {
			stats.Blocking++
			stats.ByProject[index].Blocking++
		}
		if started.Valid && finished.Valid {
			from, err := parseTime(started.String)
			if err != nil {
				rows.Close()
				return stats, err
			}
			to, err := parseTime(finished.String)
			if err != nil {
				rows.Close()
				return stats, err
			}
			if !to.Before(from) {
				duration := float64(to.Sub(from).Milliseconds())
				durations = append(durations, duration)
				projectDurations[projectID] = append(projectDurations[projectID], duration)
				topBucket.durations = append(topBucket.durations, duration)
				projectBucket.durations = append(projectBucket.durations, duration)
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("sqlite: iterate stats workflows: %w", err)
	}
	rows.Close()

	if stats.Runs > 0 {
		ratio := float64(stats.Blocking) / float64(stats.Runs)
		stats.BlockingRatio = &ratio
	}
	stats.DurationSamples, stats.AvgDurationMS, stats.P95DurationMS = durationSummary(durations)
	stats.Trend = dailyTrend(topDays)

	providerGroups := make(map[string]*groupAccumulator)
	blockGroups := make(map[string]*groupAccumulator)
	nodeWhere := where
	if nodeWhere == "" {
		nodeWhere = "WHERE "
	} else {
		nodeWhere += " AND "
	}
	nodeWhere += "COALESCE(n.started_at, '') <> ''"
	nodeRows, err := s.db.QueryContext(ctx, `SELECT
		n.provider, n.kind, n.cost_micros, n.started_at, n.finished_at, w.created_at
		FROM workflow_nodes n JOIN workflows w ON w.id = n.workflow_id `+nodeWhere, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats nodes: %w", err)
	}
	for nodeRows.Next() {
		var (
			provider sql.NullString
			kind     string
			cost     int64
			started  string
			finished sql.NullString
			created  string
		)
		if err := nodeRows.Scan(&provider, &kind, &cost, &started, &finished, &created); err != nil {
			nodeRows.Close()
			return stats, fmt.Errorf("sqlite: scan stats node: %w", err)
		}
		createdAt, err := parseTime(created)
		if err != nil {
			nodeRows.Close()
			return stats, err
		}
		startedAt, err := parseTime(started)
		if err != nil {
			nodeRows.Close()
			return stats, err
		}
		day := createdAt.UTC().Format("2006-01-02")
		var duration *float64
		if finished.Valid {
			finishedAt, err := parseTime(finished.String)
			if err != nil {
				nodeRows.Close()
				return stats, err
			}
			if !finishedAt.Before(startedAt) {
				value := float64(finishedAt.Sub(startedAt).Milliseconds())
				duration = &value
			}
		}
		if provider.String != "" {
			groupAccumulatorFor(providerGroups, provider.String).add(day, cost, duration)
		}
		groupAccumulatorFor(blockGroups, kind).add(day, cost, duration)
	}
	if err := nodeRows.Err(); err != nil {
		nodeRows.Close()
		return stats, fmt.Errorf("sqlite: iterate stats nodes: %w", err)
	}
	nodeRows.Close()
	stats.ByProvider = providerTrend(providerGroups)
	stats.ByBlock = blockTrend(blockGroups)

	decisionRows, err := s.db.QueryContext(ctx, `SELECT
		COALESCE(f.decision, ''), w.project_id, COUNT(*)
		FROM findings f JOIN workflows w ON w.id = f.workflow_id `+where+`
		GROUP BY COALESCE(f.decision, ''), w.project_id`, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats findings: %w", err)
	}
	var decisionTotal int64
	for decisionRows.Next() {
		var (
			decision  string
			projectID int64
			count     int64
		)
		if err := decisionRows.Scan(&decision, &projectID, &count); err != nil {
			decisionRows.Close()
			return stats, fmt.Errorf("sqlite: scan stats finding: %w", err)
		}
		if decision == "" {
			continue
		}
		stats.Decisions[decision] += count
		decisionTotal += count
		index, ok := projectIndex[projectID]
		if !ok {
			index = len(stats.ByProject)
			projectIndex[projectID] = index
			stats.ByProject = append(stats.ByProject, store.ProjectStats{ProjectID: projectID})
		}
		if decision == "publish" || decision == "mark" {
			stats.Published += count
			stats.ByProject[index].Published += count
		}
	}
	if err := decisionRows.Err(); err != nil {
		decisionRows.Close()
		return stats, fmt.Errorf("sqlite: iterate stats findings: %w", err)
	}
	decisionRows.Close()
	if decisionTotal > 0 {
		ratio := float64(stats.Decisions["drop"]) / float64(decisionTotal)
		stats.OverturnedRatio = &ratio
	}
	quality, err := s.providerQualityStats(ctx, where, args)
	if err != nil {
		return stats, err
	}
	stats.QualityByProvider = quality

	modelIndex := make(map[string]int)
	modelRows, err := s.db.QueryContext(ctx, `SELECT
		c.provider, c.model, COUNT(*),
		COALESCE(SUM(CASE WHEN c.error_kind IS NULL OR c.error_kind = '' THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(c.input_tokens), 0), COALESCE(SUM(c.output_tokens), 0),
		COALESCE(AVG(c.latency_ms), 0)
		FROM llm_calls c JOIN workflows w ON w.id = c.workflow_id `+where+`
		GROUP BY c.provider, c.model ORDER BY c.provider, c.model`, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats llm calls: %w", err)
	}
	for modelRows.Next() {
		var (
			entry      store.ModelStats
			avgLatency float64
		)
		if err := modelRows.Scan(&entry.Provider, &entry.Model, &entry.Calls,
			&entry.Errors, &entry.InputTokens, &entry.OutputTokens, &avgLatency); err != nil {
			modelRows.Close()
			return stats, fmt.Errorf("sqlite: scan stats llm call: %w", err)
		}
		entry.AvgLatencyMS = &avgLatency
		modelIndex[entry.Provider+"\x00"+entry.Model] = len(stats.ByModel)
		stats.ByModel = append(stats.ByModel, entry)
	}
	if err := modelRows.Err(); err != nil {
		modelRows.Close()
		return stats, fmt.Errorf("sqlite: iterate stats llm calls: %w", err)
	}
	modelRows.Close()

	latencyRows, err := s.db.QueryContext(ctx, `SELECT c.provider, c.model, c.latency_ms
		FROM llm_calls c JOIN workflows w ON w.id = c.workflow_id `+where+`
		ORDER BY c.provider, c.model, c.latency_ms`, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats latencies: %w", err)
	}
	latencies := make(map[string][]float64)
	for latencyRows.Next() {
		var (
			provider string
			model    string
			latency  float64
		)
		if err := latencyRows.Scan(&provider, &model, &latency); err != nil {
			latencyRows.Close()
			return stats, fmt.Errorf("sqlite: scan latency: %w", err)
		}
		key := provider + "\x00" + model
		latencies[key] = append(latencies[key], latency)
	}
	if err := latencyRows.Err(); err != nil {
		latencyRows.Close()
		return stats, fmt.Errorf("sqlite: iterate latencies: %w", err)
	}
	latencyRows.Close()
	for i := range stats.ByModel {
		entry := &stats.ByModel[i]
		if entry.Calls > 0 {
			rate := float64(entry.Errors) / float64(entry.Calls)
			entry.ErrorRate = &rate
		}
		if values := latencies[entry.Provider+"\x00"+entry.Model]; len(values) > 0 {
			p50 := percentile(values, 50)
			entry.P50LatencyMS = &p50
		}
	}

	costRows, err := s.db.QueryContext(ctx, `SELECT
		n.provider, n.model, COALESCE(SUM(n.cost_micros), 0)
		FROM workflow_nodes n JOIN workflows w ON w.id = n.workflow_id `+where+`
		AND n.provider IS NOT NULL AND n.provider <> ''
		AND n.model IS NOT NULL AND n.model <> ''
		GROUP BY n.provider, n.model`, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats model costs: %w", err)
	}
	for costRows.Next() {
		var (
			provider string
			model    string
			cost     int64
		)
		if err := costRows.Scan(&provider, &model, &cost); err != nil {
			costRows.Close()
			return stats, fmt.Errorf("sqlite: scan model cost: %w", err)
		}
		key := provider + "\x00" + model
		index, ok := modelIndex[key]
		if !ok {
			index = len(stats.ByModel)
			modelIndex[key] = index
			stats.ByModel = append(stats.ByModel, store.ModelStats{Provider: provider, Model: model})
		}
		stats.ByModel[index].CostMicros += cost
	}
	if err := costRows.Err(); err != nil {
		costRows.Close()
		return stats, fmt.Errorf("sqlite: iterate model costs: %w", err)
	}
	costRows.Close()

	feedback, err := s.FeedbackTotals(ctx, store.FeedbackFilter{
		ProjectID:    filter.ProjectID,
		AccessUserID: filter.AccessUserID,
		From:         filter.From,
		To:           filter.To,
	})
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats feedback: %w", err)
	}
	stats.Feedback = &feedback

	for i := range stats.ByProject {
		projectID := stats.ByProject[i].ProjectID
		samples, avg, p95 := durationSummary(projectDurations[projectID])
		stats.ByProject[i].DurationSamples = samples
		stats.ByProject[i].AvgDurationMS = avg
		stats.ByProject[i].P95DurationMS = p95
		stats.ByProject[i].Trend = dailyTrend(projectDays[projectID])
	}
	sortProjectStats(stats.ByProject)
	return stats, nil
}

type providerQualityAccumulator struct {
	proposed       int64
	valid          int64
	invalid        int64
	overturned     int64
	refuted        int64
	refutedValid   int64
	refutedInvalid int64
}

// providerQualityStats combines proposer labels and challenger outcomes. A
// finding contributes to both roles when it has an origin provider and a
// resolved challenge provider. Only findings attributed to a non-empty
// provider enter the result.
func (s *Store) providerQualityStats(ctx context.Context, where string, args []any) ([]store.ProviderQualityStats, error) {
	findingWhere := where
	if findingWhere == "" {
		findingWhere = "WHERE "
	} else {
		findingWhere += " AND "
	}
	groups := make(map[string]*providerQualityAccumulator)

	proposerRows, err := s.db.QueryContext(ctx, `SELECT
		f.origin_provider, COUNT(*),
		COALESCE(SUM(CASE WHEN f.annotation = 'valid' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN f.annotation = 'invalid' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN f.challenge_status = 'refuted' THEN 1 ELSE 0 END), 0)
		FROM findings f JOIN workflows w ON w.id = f.workflow_id `+findingWhere+`
		f.origin_provider IS NOT NULL AND f.origin_provider <> ''
		GROUP BY f.origin_provider`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: stats proposer quality: %w", err)
	}
	for proposerRows.Next() {
		var (
			provider   string
			proposed   int64
			valid      int64
			invalid    int64
			overturned int64
		)
		if err := proposerRows.Scan(&provider, &proposed, &valid, &invalid, &overturned); err != nil {
			proposerRows.Close()
			return nil, fmt.Errorf("sqlite: scan proposer quality: %w", err)
		}
		group := qualityAccumulatorFor(groups, provider)
		group.proposed += proposed
		group.valid += valid
		group.invalid += invalid
		group.overturned += overturned
	}
	if err := proposerRows.Err(); err != nil {
		proposerRows.Close()
		return nil, fmt.Errorf("sqlite: iterate proposer quality: %w", err)
	}
	proposerRows.Close()

	challengerRows, err := s.db.QueryContext(ctx, `SELECT
		n.provider, COUNT(*),
		COALESCE(SUM(CASE WHEN f.annotation = 'valid' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN f.annotation = 'invalid' THEN 1 ELSE 0 END), 0)
		FROM findings f
		JOIN workflows w ON w.id = f.workflow_id
		JOIN workflow_nodes n ON n.workflow_id = f.workflow_id AND n.id = f.challenge_node `+findingWhere+`
		f.challenge_status = 'refuted'
		AND n.provider IS NOT NULL AND n.provider <> ''
		GROUP BY n.provider`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: stats challenger quality: %w", err)
	}
	for challengerRows.Next() {
		var (
			provider string
			refuted  int64
			valid    int64
			invalid  int64
		)
		if err := challengerRows.Scan(&provider, &refuted, &valid, &invalid); err != nil {
			challengerRows.Close()
			return nil, fmt.Errorf("sqlite: scan challenger quality: %w", err)
		}
		group := qualityAccumulatorFor(groups, provider)
		group.refuted += refuted
		group.refutedValid += valid
		group.refutedInvalid += invalid
	}
	if err := challengerRows.Err(); err != nil {
		challengerRows.Close()
		return nil, fmt.Errorf("sqlite: iterate challenger quality: %w", err)
	}
	challengerRows.Close()

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]store.ProviderQualityStats, 0, len(names))
	for _, name := range names {
		group := groups[name]
		entry := store.ProviderQualityStats{
			Provider:        name,
			Proposed:        group.proposed,
			Valid:           group.valid,
			Invalid:         group.invalid,
			Reviewed:        group.valid + group.invalid,
			Overturned:      group.overturned,
			Refuted:         group.refuted,
			RefutedReviewed: group.refutedValid + group.refutedInvalid,
			FalseRefuted:    group.refutedValid,
		}
		if entry.Reviewed > 0 {
			ratio := float64(entry.Valid) / float64(entry.Reviewed)
			entry.Accuracy = &ratio
		}
		if entry.Proposed > 0 {
			ratio := float64(entry.Overturned) / float64(entry.Proposed)
			entry.OverturnedRatio = &ratio
		}
		if entry.RefutedReviewed > 0 {
			ratio := float64(entry.FalseRefuted) / float64(entry.RefutedReviewed)
			entry.FalseKillRate = &ratio
		}
		out = append(out, entry)
	}
	return out, nil
}

func qualityAccumulatorFor(groups map[string]*providerQualityAccumulator, provider string) *providerQualityAccumulator {
	group, ok := groups[provider]
	if !ok {
		group = &providerQualityAccumulator{}
		groups[provider] = group
	}
	return group
}

func percentile95(values []float64) float64 {
	return percentile(values, 95)
}

func percentile(values []float64, p int) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func sortProjectStats(items []store.ProjectStats) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].ProjectID < items[j].ProjectID
	})
}
