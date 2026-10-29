package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// Project residue kinds. The workspace root and the managed skills root use
// different layouts, so a project can have either or both.
const (
	ProjectResidueWorkspace     = "workspace"
	ProjectResidueManagedSkills = "managed_skills"
	ProjectResidueUnknown       = "unknown"
)

const (
	projectResidueUnregistered = "project_not_registered"
	projectResidueRegistered   = "project_registered"
	projectResidueUnknownOwner = "ownership_unproven"
)

// ProjectResidue is one path found under the configured workspace root that
// may belong to an unregistered project. Removable is true only when the path
// follows the platform's own layout and no project row owns it. Unknown paths
// are reported and never deleted.
type ProjectResidue struct {
	ProjectID int64  `json:"project_id"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Removable bool   `json:"removable"`
	Reason    string `json:"reason,omitempty"`
}

// ProjectResidueFailure reports one path that could not be removed. Error is
// a sanitized errclass category, never raw filesystem or store text.
type ProjectResidueFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// ProjectResidueCleanup is the outcome of one explicit cleanup pass.
type ProjectResidueCleanup struct {
	Removed []ProjectResidue        `json:"removed"`
	Kept    []ProjectResidue        `json:"kept"`
	Failed  []ProjectResidueFailure `json:"failed"`
}

// ListProjectResidues reports residue without changing any file. The project
// lock keeps the listing consistent with registration and deletion.
func (s *ProjectService) ListProjectResidues(ctx context.Context) ([]ProjectResidue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inspectProjectResidues(ctx)
}

// CleanupProjectResidues removes only paths that the workspace layout proves
// belong to an unregistered project. Unknown paths stay in place. Each removal
// takes the same lifecycle and content locks as project deletion so an old
// workspace job cannot race the delete.
func (s *ProjectService) CleanupProjectResidues(ctx context.Context) (*ProjectResidueCleanup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.inspectProjectResidues(ctx)
	if err != nil {
		return nil, err
	}
	result := &ProjectResidueCleanup{
		Removed: []ProjectResidue{},
		Kept:    []ProjectResidue{},
		Failed:  []ProjectResidueFailure{},
	}
	for _, item := range items {
		if !item.Removable {
			result.Kept = append(result.Kept, item)
			continue
		}
		kept, failure := s.removeProjectResidue(ctx, item)
		switch {
		case failure != nil:
			result.Failed = append(result.Failed, *failure)
		case kept != nil:
			result.Kept = append(result.Kept, *kept)
		default:
			result.Removed = append(result.Removed, item)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return result, nil
}

func (s *ProjectService) removeProjectResidue(ctx context.Context, item ProjectResidue) (*ProjectResidue, *ProjectResidueFailure) {
	ws := s.workspaces.Get(item.ProjectID)
	unlockLifecycle, err := ws.LifecycleLock(ctx)
	if err != nil {
		return nil, s.projectResidueFailure(item.Path, err)
	}
	unlock, err := ws.Lock(ctx)
	if err != nil {
		unlockLifecycle()
		return nil, s.projectResidueFailure(item.Path, err)
	}
	_, lookupErr := s.store.GetProject(ctx, item.ProjectID)
	if lookupErr == nil {
		unlock()
		unlockLifecycle()
		item.Removable = false
		item.Reason = projectResidueRegistered
		return &item, nil
	}
	if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
		unlock()
		unlockLifecycle()
		return nil, s.projectResidueFailure(item.Path, lookupErr)
	}
	if err := removePathUnder(s.workspace, item.Path); err != nil {
		unlock()
		unlockLifecycle()
		return nil, s.projectResidueFailure(item.Path, err)
	}
	unlock()
	unlockLifecycle()
	return nil, nil
}

func (s *ProjectService) projectResidueFailure(path string, err error) *ProjectResidueFailure {
	class := errclass.Of(err)
	s.logger.Warn("project residue cleanup failed", "error_class", class)
	return &ProjectResidueFailure{Path: path, Error: class}
}

// inspectProjectResidues must run under s.mu. It reads the registered project
// IDs once and classifies every direct entry below the workspace root.
func (s *ProjectService) inspectProjectResidues(ctx context.Context) ([]ProjectResidue, error) {
	if strings.TrimSpace(s.workspace) == "" {
		return nil, internalError()
	}
	info, err := os.Lstat(s.workspace)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return []ProjectResidue{}, nil
	case err != nil:
		return nil, s.residueInspectionError(err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return nil, s.residueInspectionError(errors.New("app: unsafe workspace root"))
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, s.residueInspectionError(err)
	}
	registered := make(map[int64]struct{}, len(projects))
	for i := range projects {
		registered[projects[i].ID] = struct{}{}
	}

	items := make([]ProjectResidue, 0)
	entries, err := os.ReadDir(s.workspace)
	if err != nil {
		return nil, s.residueInspectionError(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(s.workspace, name)
		if name == "skills" {
			skills, err := s.inspectManagedSkills(path, registered)
			if err != nil {
				return nil, err
			}
			items = append(items, skills...)
			continue
		}
		id, ok := parseProjectDirName(name)
		switch {
		case !ok:
			items = append(items, unknownProjectResidue(path))
		case entry.Type()&os.ModeSymlink != 0 || !entry.IsDir():
			items = append(items, unknownProjectResidue(path))
		case isRegisteredProject(registered, id):
		default:
			items = append(items, ProjectResidue{
				ProjectID: id,
				Path:      path,
				Kind:      ProjectResidueWorkspace,
				Removable: true,
				Reason:    projectResidueUnregistered,
			})
		}
	}
	sortProjectResidues(items)
	return items, nil
}

// inspectManagedSkills reads <workspace>/skills/project-<id>. The platform
// scope directory is expected and is not residue.
func (s *ProjectService) inspectManagedSkills(root string, registered map[int64]struct{}) ([]ProjectResidue, error) {
	info, err := os.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, s.residueInspectionError(err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return []ProjectResidue{unknownProjectResidue(root)}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, s.residueInspectionError(err)
	}
	items := make([]ProjectResidue, 0)
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(root, name)
		if name == "platform" {
			continue
		}
		id, ok := parseManagedSkillDirName(name)
		switch {
		case !ok:
			items = append(items, unknownProjectResidue(path))
		case entry.Type()&os.ModeSymlink != 0 || !entry.IsDir():
			items = append(items, unknownProjectResidue(path))
		case isRegisteredProject(registered, id):
		default:
			items = append(items, ProjectResidue{
				ProjectID: id,
				Path:      path,
				Kind:      ProjectResidueManagedSkills,
				Removable: true,
				Reason:    projectResidueUnregistered,
			})
		}
	}
	return items, nil
}

func (s *ProjectService) residueInspectionError(err error) error {
	s.logger.Warn("project residue inspection failed", "error_class", errclass.Of(err))
	return internalError()
}

func isRegisteredProject(registered map[int64]struct{}, id int64) bool {
	_, ok := registered[id]
	return ok
}

func unknownProjectResidue(path string) ProjectResidue {
	return ProjectResidue{
		Path:   path,
		Kind:   ProjectResidueUnknown,
		Reason: projectResidueUnknownOwner,
	}
}

// parseProjectDirName accepts only the canonical decimal form the workspace
// manager itself creates.
func parseProjectDirName(name string) (int64, bool) {
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != name {
		return 0, false
	}
	return id, true
}

func parseManagedSkillDirName(name string) (int64, bool) {
	raw, ok := strings.CutPrefix(name, "project-")
	if !ok {
		return 0, false
	}
	return parseProjectDirName(raw)
}

func sortProjectResidues(items []ProjectResidue) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		return items[i].Kind < items[j].Kind
	})
}
