package app

import (
	"context"
	"errors"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

const workspaceEvidenceReason = "workspace material identity unavailable; rerun review"
const contextMaterialVersion = "1"

// reviewIndexState must be called while holding the workspace read lock.
func (s *ReviewService) reviewIndexState(ctx context.Context, projectID int64, target string, ignores []string) (string, bool, error) {
	state, err := s.store.GetWorkspace(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if state == nil {
		return "", false, nil
	}
	return state.IndexSHA, state.State == "ready" && state.LastError == "" && len(ignores) == 0 &&
		shaRE.MatchString(target) && shaRE.MatchString(state.IndexSHA) && strings.EqualFold(state.IndexSHA, target), nil
}

func (s *ReviewService) cancelWorkspaceWorkflow(ctx context.Context, id string) error {
	if err := s.engine.Cancel(ctx, id, "workspace material identity unavailable; rerun review"); err != nil && !errors.Is(err, dag.ErrNotFound) {
		return err
	}
	if err := s.waitTerminal(ctx, id); err != nil && !errors.Is(err, dag.ErrNotFound) {
		return err
	}
	return s.skipPublication(ctx, id, workspaceEvidenceReason)
}
