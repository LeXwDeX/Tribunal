package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// LoadStreamSnapshot returns one workflow row plus the narrow projections the
// review stream diffs. The revision guard makes idle polls a single row read:
// when revision matches, no collection is loaded and every slice stays nil.
// A negative revision loads every collection, which the first poll uses.
func (s *Store) LoadStreamSnapshot(ctx context.Context, workflowID string, revision int64) (*store.StreamSnapshot, error) {
	var snap store.StreamSnapshot
	err := s.db.QueryRowContext(ctx, `SELECT w.project_id, w.status, COALESCE(w.verdict, ''),
			COALESCE(d.publication_state, ''), COALESCE(d.publication_error, ''),
			COALESCE(r.revision, 0)
		FROM workflows w
		LEFT JOIN workflow_dossiers d ON d.workflow_id = w.id
		LEFT JOIN workflow_stream_revisions r ON r.workflow_id = w.id
		WHERE w.id = ?`, workflowID).Scan(
		&snap.ProjectID, &snap.Status, &snap.Verdict,
		&snap.PublicationState, &snap.PublicationError, &snap.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %s", store.ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load stream snapshot: %w", err)
	}
	if revision >= 0 && revision == snap.Revision {
		return &snap, nil
	}
	if snap.Nodes, err = s.streamNodes(ctx, workflowID); err != nil {
		return nil, err
	}
	if snap.Findings, err = s.streamFindings(ctx, workflowID); err != nil {
		return nil, err
	}
	if snap.Publications, err = s.streamPublications(ctx, workflowID); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *Store) streamNodes(ctx context.Context, workflowID string) ([]store.StreamNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, status, attempt FROM workflow_nodes
		WHERE workflow_id = ? ORDER BY added_rev, id`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list stream nodes: %w", err)
	}
	defer rows.Close()
	nodes := make([]store.StreamNode, 0)
	for rows.Next() {
		var node store.StreamNode
		if err := rows.Scan(&node.ID, &node.Status, &node.Attempt); err != nil {
			return nil, fmt.Errorf("sqlite: scan stream node: %w", err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate stream nodes: %w", err)
	}
	return nodes, nil
}

func (s *Store) streamFindings(ctx context.Context, workflowID string) ([]store.StreamFinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(decision, '') FROM findings
		WHERE workflow_id = ? ORDER BY origin_node, origin_index, id`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list stream findings: %w", err)
	}
	defer rows.Close()
	findings := make([]store.StreamFinding, 0)
	for rows.Next() {
		var finding store.StreamFinding
		if err := rows.Scan(&finding.ID, &finding.Decision); err != nil {
			return nil, fmt.Errorf("sqlite: scan stream finding: %w", err)
		}
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate stream findings: %w", err)
	}
	return findings, nil
}

func (s *Store) streamPublications(ctx context.Context, workflowID string) ([]store.StreamPublication, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, COALESCE(gitlab_id, '') FROM publications
		 WHERE workflow_id = ? ORDER BY updated_at DESC, id DESC`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list stream publications: %w", err)
	}
	defer rows.Close()
	publications := make([]store.StreamPublication, 0)
	for rows.Next() {
		var publication store.StreamPublication
		if err := rows.Scan(&publication.ID, &publication.Kind, &publication.GitlabID); err != nil {
			return nil, fmt.Errorf("sqlite: scan stream publication: %w", err)
		}
		publications = append(publications, publication)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate stream publications: %w", err)
	}
	return publications, nil
}
