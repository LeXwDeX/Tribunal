package repo

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// MaterializeRepositorySkills writes an isolated, non-executable snapshot.
// parent must be a trusted, project-owned directory outside every checkout;
// the caller must remove the returned directory on every exit path and must
// not reuse it after a process restart. The returned files are not protected
// from another process with permission to mutate the parent or snapshot.
func MaterializeRepositorySkills(ctx context.Context, parent string, files map[string][]byte) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err := validateAbsPath(parent); err != nil || filepath.Dir(filepath.Clean(parent)) == filepath.Clean(parent) {
		return "", nil, errRepositorySkills
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", nil, errRepositorySkills
	}
	names := make([]string, 0, len(files))
	parents := make(map[string]bool)
	var total int64
	for name, data := range files {
		if !safeRepositorySkillPath(repositorySkillsRoot+"/"+name) || len(data) > maxRepositorySkillFile {
			return "", nil, errRepositorySkills
		}
		total += int64(len(data))
		if total > maxRepositorySkillsBytes {
			return "", nil, errRepositorySkills
		}
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			parents[dir] = true
		}
		names = append(names, name)
	}
	if len(names)+len(parents) > maxRepositorySkillEntries {
		return "", nil, errRepositorySkills
	}
	sort.Strings(names)
	dir, err := os.MkdirTemp(parent, ".repo-skills-")
	if err != nil {
		return "", nil, errRepositorySkills
	}
	remove := func() error { return os.RemoveAll(dir) }
	ready := false
	defer func() {
		if !ready {
			_ = remove()
		}
	}()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", nil, errRepositorySkills
	}
	defer root.Close()
	created := make(map[string]bool, len(parents))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			part := strings.Join(parts[:i], "/")
			if created[part] {
				continue
			}
			// Mkdir, not MkdirAll, rejects names that alias a different path
			// on case-insensitive or Unicode-normalizing filesystems.
			if err := root.Mkdir(filepath.FromSlash(part), 0o700); err != nil {
				return "", nil, errRepositorySkills
			}
			created[part] = true
		}
		file, err := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", nil, errRepositorySkills
		}
		written, writeErr := file.Write(files[name])
		modeErr := file.Chmod(0o400)
		closeErr := file.Close()
		if writeErr != nil || written != len(files[name]) || modeErr != nil || closeErr != nil {
			return "", nil, errRepositorySkills
		}
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	ready = true
	return dir, remove, nil
}
