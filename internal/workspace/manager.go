// Package workspace owns the platform's per-project checkout layout: a bare
// mirror, a fixed base worktree, temporary run worktrees, context documents
// and project-level skills.
//
// The layout is fixed so external indexes (codebase-memory) can keep using
// the same base path across refreshes. Worktrees are created from the local
// mirror only; this package never clones, runs repository scripts, or deletes
// directories it did not create.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
)

const (
	defaultRoot = "data/workspaces"
	dirMode     = 0o700
	fileMode    = 0o600
)

var (
	// ErrUnsafePath reports a symlinked or escaping workspace path.
	ErrUnsafePath = errors.New("workspace: unsafe path")
	// ErrBaseConflict reports a base directory that is not this mirror's
	// linked worktree.
	ErrBaseConflict = errors.New("workspace: base directory is not owned by this mirror")
	// ErrRunExists reports an already existing run directory.
	ErrRunExists = errors.New("workspace: run already exists")
	// ErrRunNotRegistered reports a cleanup for a run this workspace does not
	// own.
	ErrRunNotRegistered = errors.New("workspace: run is not registered")
	// ErrInvalidRunID reports a workflow ID unsafe for a directory name.
	ErrInvalidRunID = errors.New("workspace: invalid run ID")
	// ErrContextLocked reports a write to a locked context document.
	ErrContextLocked = errors.New("workspace: context document is locked")
	// ErrInvalidContextName reports a document outside the allowlist.
	ErrInvalidContextName = errors.New("workspace: invalid context document name")
	// ErrInvalidSHA reports a revision that is not a full object ID.
	ErrInvalidSHA = errors.New("workspace: invalid object ID")
)

var (
	runIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	shaRE   = regexp.MustCompile(`^[0-9a-fA-F]{40}$|^[0-9a-fA-F]{64}$`)
)

// Manager caches one Workspace per project so all callers share the same
// lock and run registry.
type Manager struct {
	root string
	git  *repo.Git
	mu   sync.Mutex
	byID map[int64]*Workspace
}

// NewManager creates a workspace manager rooted at root. An empty root uses
// data/workspaces, matching the service configuration default.
func NewManager(root string, git *repo.Git) *Manager {
	if strings.TrimSpace(root) == "" {
		root = defaultRoot
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	root = filepath.Clean(root)
	if git == nil {
		git = &repo.Git{}
	}
	return &Manager{root: root, git: git, byID: make(map[int64]*Workspace)}
}

// Root returns the normalized absolute workspace root.
func (m *Manager) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

// Get returns the shared workspace for a positive GitLab project ID. It
// performs no I/O; directories are created by the operations that need them.
func (m *Manager) Get(pid int64) *Workspace {
	if m == nil || pid <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID == nil {
		m.byID = make(map[int64]*Workspace)
	}
	if w, ok := m.byID[pid]; ok {
		return w
	}
	w := &Workspace{
		ProjectID: pid,
		Root:      filepath.Join(m.root, strconv.FormatInt(pid, 10)),
		git:       m.git,
		lock:      newCtxRWMutex(),
		runs:      make(map[string]string),
	}
	m.byID[pid] = w
	return w
}

// Workspace is one project's directory tree and lock domain.
type Workspace struct {
	ProjectID int64
	Root      string

	git  *repo.Git
	lock *ctxRWMutex

	runsMu sync.Mutex
	runs   map[string]string
}

// Lock takes the write lock used by workspace refresh. The returned function
// must be called exactly once.
func (w *Workspace) Lock(ctx context.Context) (func(), error) {
	return w.lock.Lock(ctx)
}

// RLock takes the read lock used while a review consumes a base snapshot.
// The returned function must be called exactly once.
func (w *Workspace) RLock(ctx context.Context) (func(), error) {
	return w.lock.RLock(ctx)
}

// MirrorDir is the fixed bare mirror path for this project.
func (w *Workspace) MirrorDir() string { return filepath.Join(w.Root, "mirror.git") }

// BaseDir is the fixed base checkout path for this project.
func (w *Workspace) BaseDir() string { return filepath.Join(w.Root, "base") }

// SkillsDir is the project-level skill directory.
func (w *Workspace) SkillsDir() string { return filepath.Join(w.Root, "skills") }

// MCPConfigPath is the project-level Claude-compatible MCP configuration.
func (w *Workspace) MCPConfigPath() string { return filepath.Join(w.Root, "mcp.json") }

// BaseSHA reports the commit checked out in base, or "" when base is missing,
// not detached, or unreadable.
func (w *Workspace) BaseSHA() string {
	baseInfo, err := os.Lstat(w.BaseDir())
	if err != nil || baseInfo.Mode()&os.ModeSymlink != 0 || !baseInfo.IsDir() {
		return ""
	}
	gitDir, ok := w.baseGitDirOwned()
	if !ok {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	sha := strings.TrimSpace(string(head))
	if !shaRE.MatchString(sha) {
		return ""
	}
	return strings.ToLower(sha)
}

// Refresh fetches ref from remote and moves base to that commit. The caller
// must hold the write lock. It returns the resolved base SHA and never
// re-clones an existing mirror or replaces a base it does not own.
func (w *Workspace) Refresh(ctx context.Context, remote, ref string, auth repo.Auth) (string, error) {
	if _, err := w.ensureDir(); err != nil {
		return "", err
	}
	fullRef, err := repo.NormalizeRef(ref)
	if err != nil {
		return "", err
	}
	mirror := w.MirrorDir()
	if err := w.git.EnsureMirror(ctx, mirror, remote, auth); err != nil {
		return "", err
	}
	if err := w.git.Fetch(ctx, mirror, auth, "+"+fullRef+":refs/workspace/base"); err != nil {
		return "", err
	}
	sha, err := w.git.Resolve(ctx, mirror, "refs/workspace/base")
	if err != nil {
		return "", err
	}
	if err := w.prepareBase(ctx, sha); err != nil {
		return "", err
	}
	return sha, nil
}

func (w *Workspace) prepareBase(ctx context.Context, sha string) error {
	base := w.BaseDir()
	info, err := os.Lstat(base)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return w.git.AddWorktree(ctx, w.MirrorDir(), base, sha)
	case err != nil:
		return fmt.Errorf("workspace: inspect base: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return ErrUnsafePath
	case !info.IsDir():
		return ErrBaseConflict
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		return fmt.Errorf("workspace: inspect base: %w", err)
	}
	if len(entries) == 0 {
		if err := os.Remove(base); err != nil {
			return fmt.Errorf("workspace: remove empty base: %w", err)
		}
		return w.git.AddWorktree(ctx, w.MirrorDir(), base, sha)
	}
	if !w.ownsBase() {
		return ErrBaseConflict
	}
	return w.git.Checkout(ctx, base, sha)
}

// ownsBase reports whether base is a linked worktree of this project's mirror.
func (w *Workspace) ownsBase() bool {
	_, ok := w.baseGitDirOwned()
	return ok
}

// baseGitDirOwned verifies that the base .git link points at one of this
// mirror's worktrees and that the worktree points back at base.
func (w *Workspace) baseGitDirOwned() (string, bool) {
	gitDir, ok := w.resolveBaseGitDir()
	if !ok {
		return "", false
	}
	back, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return "", false
	}
	backPath := strings.TrimSpace(string(back))
	if !filepath.IsAbs(backPath) {
		backPath = filepath.Join(gitDir, backPath)
	}
	expected, err := filepath.EvalSymlinks(filepath.Join(w.BaseDir(), ".git"))
	if err != nil {
		return "", false
	}
	backResolved, err := filepath.EvalSymlinks(backPath)
	if err != nil || backResolved != expected {
		return "", false
	}
	return gitDir, true
}

// resolveBaseGitDir returns the base worktree git directory only when it sits
// under this project's mirror/worktrees subtree.
func (w *Workspace) resolveBaseGitDir() (string, bool) {
	gitFile := filepath.Join(w.BaseDir(), ".git")
	info, err := os.Lstat(gitFile)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", false
	}
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return "", false
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(w.BaseDir(), gitDir)
	}
	mirror, err := filepath.EvalSymlinks(w.MirrorDir())
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(gitDir))
	if err != nil {
		return "", false
	}
	if !pathWithin(filepath.Join(mirror, "worktrees"), resolved) {
		return "", false
	}
	return resolved, true
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ValidRunID reports whether id is safe to use as a direct runs/ directory
// name.
func ValidRunID(id string) bool { return runIDRE.MatchString(id) }

// RegisteredRuns returns a snapshot of the runs this workspace created and
// has not cleaned up yet. Periodic cleanup must not delete these directories.
func (w *Workspace) RegisteredRuns() map[string]string {
	w.runsMu.Lock()
	defer w.runsMu.Unlock()
	out := make(map[string]string, len(w.runs))
	for id, dir := range w.runs {
		out[id] = dir
	}
	return out
}

// NewRun creates a detached worktree for headSHA under runs/<wfID>. The
// cleanup function removes only this run and reports failures.
func (w *Workspace) NewRun(ctx context.Context, wfID, headSHA string) (string, func() error, error) {
	if !runIDRE.MatchString(wfID) {
		return "", nil, ErrInvalidRunID
	}
	if err := repo.ValidateSHA(headSHA); err != nil {
		return "", nil, ErrInvalidSHA
	}
	runsDir, err := w.ensureDir("runs")
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(runsDir, wfID)
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, ErrUnsafePath
		}
		return "", nil, ErrRunExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("workspace: inspect run: %w", err)
	}
	// Register before creating the worktree so a concurrent cleanup skips it.
	// The directory does not exist yet, so cleanup that wins this race sees a
	// registration without a directory and deletes nothing.
	w.runsMu.Lock()
	if _, exists := w.runs[wfID]; exists {
		w.runsMu.Unlock()
		return "", nil, ErrRunExists
	}
	w.runs[wfID] = dir
	w.runsMu.Unlock()
	if err := w.git.AddWorktree(ctx, w.MirrorDir(), dir, headSHA); err != nil {
		w.runsMu.Lock()
		if w.runs[wfID] == dir {
			delete(w.runs, wfID)
		}
		w.runsMu.Unlock()
		return "", nil, err
	}

	cleanupCtx := context.WithoutCancel(ctx)
	cleanup := func() error { return w.cleanupRun(cleanupCtx, wfID, dir) }
	return dir, cleanup, nil
}

func (w *Workspace) cleanupRun(ctx context.Context, wfID, dir string) error {
	w.runsMu.Lock()
	registered, ok := w.runs[wfID]
	w.runsMu.Unlock()
	if !ok || registered != dir {
		return ErrRunNotRegistered
	}

	if err := w.git.RemoveWorktree(ctx, w.MirrorDir(), dir); err != nil {
		return err
	}

	w.runsMu.Lock()
	if w.runs[wfID] == dir {
		delete(w.runs, wfID)
	}
	w.runsMu.Unlock()

	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workspace: inspect run after removal: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafePath
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace: run path is not a directory")
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("workspace: remove run directory: %w", err)
	}
	return nil
}

// ensureDir creates the project root plus the named path components with 0700
// permissions. Every existing component must be a real directory; symlinked
// components are rejected instead of followed.
func (w *Workspace) ensureDir(parts ...string) (string, error) {
	if w == nil || w.ProjectID <= 0 || !filepath.IsAbs(w.Root) {
		return "", ErrUnsafePath
	}
	if err := os.MkdirAll(w.Root, dirMode); err != nil {
		return "", fmt.Errorf("workspace: create project root: %w", err)
	}
	path := w.Root
	if err := requireDir(path); err != nil {
		return "", err
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\`) {
			return "", ErrUnsafePath
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(path, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", fmt.Errorf("workspace: create directory: %w", err)
			}
			info, err = os.Lstat(path)
		}
		if err != nil {
			return "", fmt.Errorf("workspace: inspect directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", ErrUnsafePath
		}
		if err := os.Chmod(path, dirMode); err != nil {
			return "", fmt.Errorf("workspace: secure directory: %w", err)
		}
	}
	return path, nil
}

func requireDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("workspace: inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafePath
	}
	if err := os.Chmod(path, dirMode); err != nil {
		return fmt.Errorf("workspace: secure directory: %w", err)
	}
	return nil
}
