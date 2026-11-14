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
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

const defaultRunCleanupInterval = time.Hour

// legacyOrphanSettler is optional so test doubles and stores that predate
// legacy dossier settlement keep running the directory sweep. SQLite
// implements it.
type legacyOrphanSettler interface {
	SettleLegacyOrphanDossiers(ctx context.Context, before time.Time) (int64, error)
}

// RunCleanupStore is the persistence subset needed to find safe deletions.
type RunCleanupStore interface {
	ListWorkspaceProjectIDs(ctx context.Context) ([]int64, error)
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	GetWorkspace(ctx context.Context, id int64) (*store.Workspace, error)
	SettleOrphanDossiers(ctx context.Context, projectID int64) error
	ProtectedRunRefs(ctx context.Context) (map[string]struct{}, error)
	ProtectedContextSHAs(ctx context.Context, projectID int64) (map[string]struct{}, error)
}

// RunCleanupGit removes a linked worktree and its registration.
type RunCleanupGit interface {
	RemoveWorktree(ctx context.Context, mirror, dir string) error
}

// RunCleanupMaintenance is the optional mirror maintenance surface: prune MR
// head refs no protected review needs and run an automatic git gc. A fake git
// that predates it simply skips mirror maintenance.
type RunCleanupMaintenance interface {
	MergeRequestRefs(ctx context.Context, mirror string) ([]repo.MergeRequestRef, error)
	DeleteMergeRequestRefs(ctx context.Context, mirror string, refs []repo.MergeRequestRef) (int, error)
	GC(ctx context.Context, mirror string) error
}

// workspaceReclaimStore is the optional fail-closed protection surface for
// workspace reclamation and MR ref pruning.
type workspaceReclaimStore interface {
	WorkspaceReclaimStatus(ctx context.Context, projectID int64) (store.WorkspaceReclaimStatus, error)
	ProtectedMergeRequestIIDs(ctx context.Context, projectID int64) (map[int64]struct{}, error)
}

// workspaceCapacityRecorder persists the latest measurement for the stats
// response.
type workspaceCapacityRecorder interface {
	RecordWorkspaceCapacity(ctx context.Context, capacity store.WorkspaceCapacity) error
}

// RunCleanupConfig configures RunCleanupService.
type RunCleanupConfig struct {
	Store     RunCleanupStore
	Workspace *workspace.Manager
	Git       RunCleanupGit
	// Maintenance prunes stale mirror refs and runs git gc. Nil skips both; the
	// run/context cleanup still works.
	Maintenance RunCleanupMaintenance
	After       time.Duration
	// DossierRetention is the age after which an unreferenced project_id=0
	// dossier is settled. Zero reuses After. Production wires the storage
	// job retention here because a reservation cannot be retried once its
	// job audit row is gone.
	DossierRetention time.Duration
	// MaxBytes caps the total workspace root size. Zero disables the cap.
	MaxBytes int64
	// ReclaimAfter removes an inactive project workspace after this long. Zero
	// disables reclamation.
	ReclaimAfter time.Duration
	Now          func() time.Time
	Logger       *slog.Logger
}

// RunCleanupService removes stale run worktrees and orphaned context version
// directories that no persisted work references and that this process did not
// register. Each pass also maintains the mirror, records workspace capacity,
// and reclaims inactive project workspaces (#459).
type RunCleanupService struct {
	store            RunCleanupStore
	workspace        *workspace.Manager
	git              RunCleanupGit
	maintenance      RunCleanupMaintenance
	after            time.Duration
	dossierRetention time.Duration
	maxBytes         int64
	reclaimAfter     time.Duration
	now              func() time.Time
	logger           *slog.Logger
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
	if cfg.DossierRetention < 0 {
		return nil, errors.New("app: dossier retention must not be negative")
	}
	if cfg.MaxBytes < 0 {
		return nil, errors.New("app: workspace max bytes must not be negative")
	}
	if cfg.ReclaimAfter < 0 {
		return nil, errors.New("app: workspace reclaim age must not be negative")
	}
	dossierRetention := cfg.DossierRetention
	if dossierRetention == 0 {
		dossierRetention = cfg.After
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
		store:            cfg.Store,
		workspace:        cfg.Workspace,
		git:              cfg.Git,
		maintenance:      cfg.Maintenance,
		after:            cfg.After,
		dossierRetention: dossierRetention,
		maxBytes:         cfg.MaxBytes,
		reclaimAfter:     cfg.ReclaimAfter,
		now:              now,
		logger:           logger,
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

// RunOnce settles expired project_id=0 dossiers, then deletes expired runs
// and unprotected context versions. Query failures fail closed: no directory
// is touched when either protection query fails. One project or directory
// error does not stop the remaining work, and errors are joined at the end.
func (s *RunCleanupService) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var (
		errs            []error
		settledDossiers int64
	)
	if settler, ok := s.store.(legacyOrphanSettler); ok {
		settled, err := settler.SettleLegacyOrphanDossiers(ctx, s.now().UTC().Add(-s.dossierRetention))
		settledDossiers = settled
		if err != nil {
			errs = append(errs, fmt.Errorf("run cleanup: settle legacy orphan dossiers: %w", err))
		}
	}
	projectIDs, err := s.store.ListWorkspaceProjectIDs(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("run cleanup: list workspaces: %w", err))...)
	}
	cutoff := s.now().UTC().Add(-s.after)
	var (
		removedRuns     int
		removedContexts int
		reclaimed       int
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
		project, projectErr := s.store.GetProject(ctx, pid)
		state, stateErr := s.store.GetWorkspace(ctx, pid)
		if projectErr != nil || stateErr != nil || project == nil || state == nil || project.ID != pid || state.ProjectID != pid {
			unlock()
			for _, lookupErr := range []error{projectErr, stateErr} {
				if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
					errs = append(errs, fmt.Errorf("run cleanup: project %d: verify ownership: %w", pid, lookupErr))
				}
			}
			continue
		}
		// Read the protection set while holding the write lock: prepareRun
		// cannot create or reuse a run until this pass releases it.
		if err := s.store.SettleOrphanDossiers(ctx, pid); err != nil {
			unlock()
			errs = append(errs, fmt.Errorf("run cleanup: project %d: settle orphan dossiers: %w", pid, err))
			continue
		}
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
		if maintenanceErr := s.maintainMirror(ctx, ws); maintenanceErr != nil {
			err = errors.Join(err, maintenanceErr)
		}
		reclaimedNow, reclaimErr := s.reclaimWorkspace(ctx, ws, project)
		if reclaimedNow {
			reclaimed++
		}
		if reclaimErr != nil {
			err = errors.Join(err, reclaimErr)
		}
		unlock()
		removedRuns += runs
		removedContexts += contexts
		if err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.measureCapacity(ctx); err != nil {
		errs = append(errs, err)
	}
	s.logger.Info("run cleanup finished",
		"settled_dossiers", settledDossiers,
		"removed_runs", removedRuns,
		"removed_contexts", removedContexts,
		"reclaimed_workspaces", reclaimed,
		"errors", len(errs))
	for _, err := range errs {
		s.logger.Warn("run cleanup failed", "error_class", errclass.Of(err))
	}
	return errors.Join(errs...)
}

// maintainMirror prunes MR head refs that no unfinished review still needs and
// runs git's automatic gc. Protection queries are fail-closed: when the store
// cannot prove which merge requests are active, neither refs nor objects are
// touched. The caller holds the workspace write lock, so no prepareRun can be
// fetching a ref concurrently (#459).
func (s *RunCleanupService) maintainMirror(ctx context.Context, ws *workspace.Workspace) error {
	if s.maintenance == nil {
		return nil
	}
	mirror := ws.MirrorDir()
	info, err := os.Lstat(mirror)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("run cleanup: project %d: inspect mirror: %w", ws.ProjectID, err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return fmt.Errorf("run cleanup: project %d: unsafe mirror", ws.ProjectID)
	}
	protection, ok := s.store.(workspaceReclaimStore)
	if !ok {
		return nil
	}
	protected, err := protection.ProtectedMergeRequestIIDs(ctx, ws.ProjectID)
	if err != nil {
		return fmt.Errorf("run cleanup: project %d: list protected merge requests: %w", ws.ProjectID, err)
	}
	refs, err := s.maintenance.MergeRequestRefs(ctx, mirror)
	if err != nil {
		return fmt.Errorf("run cleanup: project %d: list merge request refs: %w", ws.ProjectID, err)
	}
	stale := make([]repo.MergeRequestRef, 0, len(refs))
	for _, ref := range refs {
		if _, keep := protected[ref.IID]; keep {
			continue
		}
		stale = append(stale, ref)
	}
	if len(stale) > 0 {
		deleted, err := s.maintenance.DeleteMergeRequestRefs(ctx, mirror, stale)
		if deleted > 0 {
			s.logger.Info("merge request refs pruned", "project_id", ws.ProjectID, "refs", deleted)
		}
		if err != nil {
			return fmt.Errorf("run cleanup: project %d: delete merge request refs: %w", ws.ProjectID, err)
		}
	}
	if err := s.maintenance.GC(ctx, mirror); err != nil {
		return fmt.Errorf("run cleanup: project %d: gc mirror: %w", ws.ProjectID, err)
	}
	return nil
}

// reclaimWorkspace removes one inactive project workspace. The caller holds the
// workspace write lock. Every check is fail-closed: registered in-process runs,
// missing or invalid ownership metadata, an unprovable protection state, or
// missing activity evidence all keep the directory.
func (s *RunCleanupService) reclaimWorkspace(ctx context.Context, ws *workspace.Workspace, project *store.Project) (bool, error) {
	if s.reclaimAfter <= 0 {
		return false, nil
	}
	// A disabled project is paused on purpose; keep its checkout until it is
	// enabled again or deleted explicitly.
	if project == nil || !project.Enabled {
		return false, nil
	}
	protection, ok := s.store.(workspaceReclaimStore)
	if !ok {
		return false, nil
	}
	if len(ws.RegisteredRuns()) > 0 {
		return false, nil
	}
	status, err := protection.WorkspaceReclaimStatus(ctx, ws.ProjectID)
	if err != nil {
		return false, fmt.Errorf("run cleanup: project %d: workspace protection: %w", ws.ProjectID, err)
	}
	if status.Protected || status.LastActivity.IsZero() {
		return false, nil
	}
	if s.now().UTC().Sub(status.LastActivity) < s.reclaimAfter {
		return false, nil
	}
	if err := workspaceOwnership(ws); err != nil {
		return false, nil
	}
	if err := removeWorkspaceRoot(s.workspace.Root(), ws); err != nil {
		return false, fmt.Errorf("run cleanup: project %d: reclaim workspace: %w", ws.ProjectID, err)
	}
	s.logger.Info("workspace reclaimed", "project_id", ws.ProjectID,
		"inactive_hours", int(s.now().UTC().Sub(status.LastActivity).Hours()))
	return true, nil
}

// workspaceOwnership verifies the managed marker written by the workspace
// manager. A missing or unreadable marker is not proof of ownership, so the
// directory is kept.
func workspaceOwnership(ws *workspace.Workspace) error {
	info, err := os.Lstat(ws.Root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return workspace.ErrUnsafePath
	}
	marker := filepath.Join(ws.Root, workspace.ManagedMarker)
	markerInfo, err := os.Lstat(marker)
	if err != nil || markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
		return workspace.ErrUnsafePath
	}
	data, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(data)) != workspace.ManagedMarkerValue(ws.ProjectID) {
		return workspace.ErrUnsafePath
	}
	return nil
}

// removeWorkspaceRoot deletes exactly the direct project directory below the
// manager root.
func removeWorkspaceRoot(managerRoot string, ws *workspace.Workspace) error {
	rel, err := filepath.Rel(managerRoot, ws.Root)
	if err != nil || rel == "." || rel != strconv.FormatInt(ws.ProjectID, 10) {
		return workspace.ErrUnsafePath
	}
	info, err := os.Lstat(ws.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return workspace.ErrUnsafePath
	}
	return os.RemoveAll(ws.Root)
}

// measureCapacity walks the workspace root once per pass when a cap is
// configured, persists the result, and warns when the cap is exceeded. The walk
// is metadata-only and never follows symlinks.
func (s *RunCleanupService) measureCapacity(ctx context.Context) error {
	if s.maxBytes <= 0 {
		return nil
	}
	total, err := directorySize(s.workspace.Root())
	if err != nil {
		return fmt.Errorf("run cleanup: measure workspace capacity: %w", err)
	}
	capacity := store.WorkspaceCapacity{
		MeasuredAt: s.now().UTC(),
		TotalBytes: total,
		LimitBytes: s.maxBytes,
		OverLimit:  total > s.maxBytes,
	}
	if recorder, ok := s.store.(workspaceCapacityRecorder); ok {
		if err := recorder.RecordWorkspaceCapacity(ctx, capacity); err != nil {
			return fmt.Errorf("run cleanup: record workspace capacity: %w", err)
		}
	}
	if capacity.OverLimit {
		s.logger.Warn("workspace capacity exceeded",
			"total_bytes", capacity.TotalBytes, "limit_bytes", capacity.LimitBytes)
	}
	return nil
}

// directorySize sums regular file sizes below root. A missing root is empty.
func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return total, err
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
