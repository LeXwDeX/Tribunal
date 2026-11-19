package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// graphsDirName holds the per-review target graph directories. Each review run
// owns one child named by its run ID; that child is the workspace of the
// review's private code-graph MCP server, so the server's HOME holds exactly
// one graph, built for that review's base commit.
const graphsDirName = "graphs"

// GraphsDir is the parent of the per-review target graph directories.
func (w *Workspace) GraphsDir() string { return filepath.Join(w.Root, graphsDirName) }

// EnsureGraphDir returns an empty graphs/<runID> directory for one review's
// target graph. Content left by an earlier attempt of the same run is removed
// first, so a graph never mixes two attempts. The project root and graphs/ are
// created like the other workspace directories (0700, real directories only);
// a symlink or a non-directory at any component is refused instead of
// followed. The caller must hold the workspace read lock for the whole review.
func (w *Workspace) EnsureGraphDir(runID string) (string, error) {
	if w == nil || w.ProjectID <= 0 || !filepath.IsAbs(w.Root) {
		return "", ErrUnsafePath
	}
	if !runIDRE.MatchString(runID) {
		return "", ErrInvalidRunID
	}
	graphs, err := w.ensureDir(graphsDirName)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(graphs, runID)
	if !pathWithin(graphs, dir) {
		return "", ErrUnsafePath
	}
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("workspace: inspect graph directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return "", ErrUnsafePath
	default:
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("workspace: remove stale graph directory: %w", err)
		}
	}
	if err := os.Mkdir(dir, dirMode); err != nil {
		return "", fmt.Errorf("workspace: create graph directory: %w", err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return "", fmt.Errorf("workspace: secure graph directory: %w", err)
	}
	return dir, nil
}

// RemoveGraphDir deletes one review's graph directory. A missing project root,
// graphs/ or run directory is not an error, so the call is idempotent. A
// symlinked root, graphs/ or run directory is refused with ErrUnsafePath and
// nothing behind it is touched.
func (w *Workspace) RemoveGraphDir(runID string) error {
	if w == nil || w.ProjectID <= 0 || !filepath.IsAbs(w.Root) {
		return ErrUnsafePath
	}
	if !runIDRE.MatchString(runID) {
		return ErrInvalidRunID
	}
	graphs, ok, err := w.existingGraphsDir()
	if err != nil || !ok {
		return err
	}
	dir := filepath.Join(graphs, runID)
	if !pathWithin(graphs, dir) {
		return ErrUnsafePath
	}
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("workspace: inspect graph directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return ErrUnsafePath
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("workspace: remove graph directory: %w", err)
	}
	return nil
}

// CleanupGraphDirs removes graph directories older than before whose run ID is
// not protected. A review removes its own graph directory when it ends, and it
// holds the workspace read lock for its whole run, so a directory seen under
// the write lock is a crash leftover. The caller must hold the workspace write
// lock. Entries that are not a valid run ID, symlinks and plain files are
// kept, and one failing directory does not block the others. A missing graphs/
// is a no-op.
func (w *Workspace) CleanupGraphDirs(protected map[string]struct{}, before time.Time) (int, error) {
	if w == nil || w.ProjectID <= 0 || !filepath.IsAbs(w.Root) {
		return 0, ErrUnsafePath
	}
	root, ok, err := w.existingGraphsDir()
	if err != nil || !ok {
		return 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("workspace: read graphs directory: %w", err)
	}

	var (
		errs    []error
		removed int
	)
	for _, entry := range entries {
		name := entry.Name()
		if !runIDRE.MatchString(name) {
			continue
		}
		if _, keep := protected[name]; keep {
			continue
		}
		dir := filepath.Join(root, name)
		if !pathWithin(root, dir) {
			continue
		}
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("workspace: graph %s: inspect: %w", name, err))
			continue
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			continue
		case !info.ModTime().Before(before):
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("workspace: graph %s: remove: %w", name, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// existingGraphsDir returns graphs/ when both it and the project root exist as
// real directories. A missing root or graphs/ reports false without an error;
// a symlink or a non-directory is ErrUnsafePath.
func (w *Workspace) existingGraphsDir() (string, bool, error) {
	for _, path := range []string{w.Root, w.GraphsDir()} {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", false, nil
		case err != nil:
			return "", false, fmt.Errorf("workspace: inspect graphs directory: %w", err)
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			return "", false, ErrUnsafePath
		}
	}
	return w.GraphsDir(), true, nil
}
