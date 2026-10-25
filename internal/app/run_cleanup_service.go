package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

const defaultRunCleanupInterval = time.Hour

// RunCleanupStore is the persistence subset needed to find safe deletions.
type RunCleanupStore interface {
	ListWorkspaceProjectIDs(ctx context.Context) ([]int64, error)
	ProtectedRunRefs(ctx context.Context) (map[string]struct{}, error)
	ProtectedContextSHAs(ctx context.Context, projectID int64) (map[string]struct{}, error)
}

// RunCleanupGit removes a linked worktree and its registration.
type RunCleanupGit interface {
	RemoveWorktree(ctx context.Context, mirror, dir string) error
}

// RunCleanupConfig configures RunCleanupService.
type RunCleanupConfig struct {
	Store     RunCleanupStore
	Workspace *workspace.Manager
	Git       RunCleanupGit
	After     time.Duration
	Now       func() time.Time
	Logger    *slog.Logger
}

// RunCleanupService removes stale run worktrees and orphaned context version
// directories that no persisted work references and that this process did not
// register.
type RunCleanupService struct {
	store     RunCleanupStore
	workspace *workspace.Manager
	git       RunCleanupGit
	after     time.Duration
	now       func() time.Time
	logger    *slog.Logger
}

// NewRunCleanupService validates the policy and builds the service.
func NewRunCleanupService(cfg RunCleanupConfig) (*RunCleanupService, error) {
	if cfg.Store == nil {
		return nil, errors.New("app: run cleanup store is required")
	}
	if cfg.Workspace == nil {
		return nil, errors.New("app: run cleanup workspace manager is required")
	}
	if cfg.Git == nil {
		return nil, errors.New("app: run cleanup git is required")
	}
	if cfg.After <= 0 {
		return nil, errors.New("app: run cleanup age must be positive")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &RunCleanupService{
		store:     cfg.Store,
		workspace: cfg.Workspace,
		git:       cfg.Git,
		after:     cfg.After,
		now:       now,
		logger:    logger,
	}, nil
}

// Start runs one pass immediately and then on every interval until ctx is
// cancelled. A non-positive interval uses the default.
func (s *RunCleanupService) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultRunCleanupInterval
	}
	_ = s.RunOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.RunOnce(ctx)
		}
	}
}

// RunOnce deletes expired runs and unprotected context versions. Query
// failures fail closed: no directory is touched when either protection query
// fails. One project or directory error does not stop the remaining work, and
// errors are joined at the end.
func (s *RunCleanupService) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	projectIDs, err := s.store.ListWorkspaceProjectIDs(ctx)
	if err != nil {
		return fmt.Errorf("run cleanup: list workspaces: %w", err)
	}
	cutoff := s.now().UTC().Add(-s.after)

	var (
		errs            []error
		removedRuns     int
		removedContexts int
	)
	for _, pid := range projectIDs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		ws := s.workspace.Get(pid)
		if ws == nil {
			continue
		}
		unlock, err := ws.Lock(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("run cleanup: project %d: %w", pid, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		// Read the protection set while holding the write lock: prepareRun
		// cannot create or reuse a run until this pass releases it.
		protected, err := s.store.ProtectedRunRefs(ctx)
		if err != nil {
			unlock()
			errs = append(errs, fmt.Errorf("run cleanup: project %d: list protected runs: %w", pid, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		protectedContext, err := s.store.ProtectedContextSHAs(ctx, pid)
		if err != nil {
			unlock()
			errs = append(errs, fmt.Errorf("run cleanup: project %d: list protected context shas: %w", pid, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		runs, contexts, err := s.cleanupWorkspace(ctx, ws, protected, protectedContext, cutoff)
		unlock()
		removedRuns += runs
		removedContexts += contexts
		if err != nil {
			errs = append(errs, err)
		}
	}
	s.logger.Info("run cleanup finished",
		"removed_runs", removedRuns,
		"removed_contexts", removedContexts,
		"errors", len(errs))
	for _, err := range errs {
		s.logger.Warn("run cleanup failed", "error", err.Error())
	}
	return errors.Join(errs...)
}

// cleanupWorkspace assumes the caller holds the workspace write lock. That
// lock serializes deletion with prepareRun's read lock, which guards worktree
// creation, and with context writers that share the same lock.
func (s *RunCleanupService) cleanupWorkspace(ctx context.Context, ws *workspace.Workspace, protected, protectedContext map[string]struct{}, cutoff time.Time) (int, int, error) {
	root, err := os.Lstat(ws.Root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, 0, nil
	case err != nil:
		return 0, 0, fmt.Errorf("run cleanup: project %d: inspect workspace: %w", ws.ProjectID, err)
	case root.Mode()&os.ModeSymlink != 0 || !root.IsDir():
		return 0, 0, fmt.Errorf("run cleanup: project %d: unsafe workspace root", ws.ProjectID)
	}

	runs, runErr := s.cleanupRuns(ctx, ws, protected, cutoff)
	contexts, contextErr := ws.CleanupContextDirs(protectedContext, cutoff)
	return runs, contexts, errors.Join(runErr, contextErr)
}

// cleanupRuns removes eligible worktrees. A missing runs directory is an
// idempotent no-op, but an unsafe one fails closed.
func (s *RunCleanupService) cleanupRuns(ctx context.Context, ws *workspace.Workspace, protected map[string]struct{}, cutoff time.Time) (int, error) {
	runsDir := filepath.Join(ws.Root, "runs")
	info, err := os.Lstat(runsDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("run cleanup: project %d: inspect runs: %w", ws.ProjectID, err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return 0, fmt.Errorf("run cleanup: project %d: unsafe runs directory", ws.ProjectID)
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return 0, fmt.Errorf("run cleanup: project %d: read runs: %w", ws.ProjectID, err)
	}

	registered := ws.RegisteredRuns()
	var (
		errs    []error
		removed int
	)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		runID := entry.Name()
		if !workspace.ValidRunID(runID) {
			continue
		}
		if _, ok := protected[runID]; ok {
			continue
		}
		if _, ok := registered[runID]; ok {
			continue
		}
		dir := filepath.Join(runsDir, runID)
		if !directChild(runsDir, dir) {
			continue
		}
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("run cleanup: project %d run %s: inspect: %w", ws.ProjectID, runID, err))
			continue
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			continue
		case !info.ModTime().Before(cutoff):
			continue
		}
		if err := s.git.RemoveWorktree(ctx, ws.MirrorDir(), dir); err != nil {
			errs = append(errs, fmt.Errorf("run cleanup: project %d run %s: %w", ws.ProjectID, runID, err))
			continue
		}
		info, err = os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			removed++
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("run cleanup: project %d run %s: inspect after removal: %w", ws.ProjectID, runID, err))
			continue
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			errs = append(errs, fmt.Errorf("run cleanup: project %d run %s: unexpected path after removal", ws.ProjectID, runID))
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("run cleanup: project %d run %s: remove directory: %w", ws.ProjectID, runID, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// directChild reports whether child is one direct entry below parent.
func directChild(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil || rel == "." || rel == ".." || strings.ContainsRune(rel, filepath.Separator) {
		return false
	}
	return true
}
