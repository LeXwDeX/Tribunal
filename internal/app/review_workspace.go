package app

import (
	"context"
	"errors"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
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
	// The built-in ignore defaults only exclude build artifacts; they must not
	// disable the code graph. A project or repository rule still withholds it
	// and the review discloses that degradation (#457).
	return state.IndexSHA, state.State == "ready" && state.LastError == "" && !config.ReviewIgnoreRestricted(ignores) &&
		shaRE.MatchString(target) && shaRE.MatchString(state.IndexSHA) && strings.EqualFold(state.IndexSHA, target), nil
}

// canBuildTargetGraph reports whether a review may get a per-run code graph at
// all. A restricted ignore policy never reaches an index, the same boundary as
// the shared workspace index, and an index nobody can read is not worth building:
// a block the review graph runs must bind a tool of an index-backed server.
func (s *ReviewService) canBuildTargetGraph(execution *reviewExecution, ignores []string) bool {
	return s.targetGraphs != nil && execution != nil && execution.mcp != nil &&
		!config.ReviewIgnoreRestricted(ignores) && execution.bindsIndexedTool()
}

// useTargetGraph indexes the review base into a graph of its own when the shared
// workspace index is not at startSHA, and routes the index-backed MCP tools of
// execution to it. It returns nil without a graph when none applies (see
// canBuildTargetGraph) or none could be built, and the review then continues
// without those tools. A caller that is done or a revoked outbound policy stop
// the review instead. The caller closes the graph before it removes baseDir,
// holds the workspace read lock and passes a context bound to the workspace's
// outbound gate.
func (s *ReviewService) useTargetGraph(ctx context.Context, project *store.Project, ws *workspace.Workspace, execution *reviewExecution, runID, baseDir, startSHA string, ignores []string) (*targetGraph, error) {
	if baseDir == "" || !s.canBuildTargetGraph(execution, ignores) {
		return nil, nil
	}
	graph, err := s.targetGraphs.Build(ctx, targetGraphRequest{
		ProjectID: project.ID,
		Workspace: ws,
		RunID:     runID,
		BaseDir:   baseDir,
		StartSHA:  startSHA,
		MCP:       execution.mcpConfig,
		DeniedEnv: execution.deniedEnv,
		Ignore:    ignores,
	})
	var unavailable *targetGraphUnavailable
	switch {
	case errors.As(err, &unavailable):
		s.logger.Warn("target code graph unavailable; continuing without it",
			"project_id", project.ID, "error_class", errclass.Of(err))
		return nil, nil
	case err != nil:
		return nil, err
	case graph == nil:
		// A remote index-backed server serves its own index, so there is nothing
		// to build per run. Say so once, without naming the servers.
		if _, local := indexedLocalServers(execution.mcpConfig); !local {
			s.logger.Debug("target code graph skipped: an index-backed MCP server is not a local stdio server",
				"project_id", project.ID)
		}
		return nil, nil
	}
	execution.tools = routeTargetGraphTools(execution.mcp, graph, execution.blocks)
	return graph, nil
}

// rebuildTargetGraph restores the code graph of a recovered review whose shared
// index no longer describes its base commit: it prepares the base checkout again
// and indexes it. The returned cleanup closes the graph and then removes the
// base. It is nil, with nothing left behind, when no graph was built.
func (s *ReviewService) rebuildTargetGraph(ctx context.Context, project *store.Project, token string, ws *workspace.Workspace, execution *reviewExecution, mrIID int64, runID, startSHA, targetBranch string, ignores []string) (baseDir string, cleanup func(), err error) {
	if !s.canBuildTargetGraph(execution, ignores) {
		return "", nil, nil
	}
	base, _, baseCleanup, err := s.prepareReviewBase(ctx, project, token, mrIID, runID, startSHA, targetBranch)
	if err != nil {
		return "", nil, err
	}
	graph, err := s.useTargetGraph(ctx, project, ws, execution, runID, base, startSHA, ignores)
	if graph == nil {
		if baseCleanup != nil {
			baseCleanup()
		}
		return "", nil, err
	}
	return base, func() {
		graph.Close()
		if baseCleanup != nil {
			baseCleanup()
		}
	}, nil
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
