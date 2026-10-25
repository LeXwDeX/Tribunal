package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ContextMeta describes one generated context document version. The platform
// database remains the source of truth; these files let the workspace carry
// the version that matches a base checkout.
type ContextMeta struct {
	SourceSHA   string    `json:"source_sha"`
	Model       string    `json:"model,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
	Locked      bool      `json:"locked"`
	Stale       bool      `json:"stale"`
}

var contextDocs = map[string]bool{
	"AGENTS.md": true,
	"CLAUDE.md": true,
}

// ReadContext returns one generated document for the base SHA.
func (w *Workspace) ReadContext(sha, name string) ([]byte, error) {
	dir, err := w.existingContextDir(sha)
	if err != nil {
		return nil, err
	}
	if !contextDocs[name] {
		return nil, ErrInvalidContextName
	}
	return readRegularFile(filepath.Join(dir, name))
}

// WriteContext writes generated documents and their metadata. A locked
// version is never overwritten; callers must unlock it explicitly first.
func (w *Workspace) WriteContext(sha string, docs map[string][]byte, meta ContextMeta) error {
	return w.writeContext(sha, docs, meta, true)
}

// WriteActiveContext writes the database-active version. The database decides
// which revision is current, so a stale file lock must not block the
// projection; ordinary writers still use WriteContext and keep the file lock.
func (w *Workspace) WriteActiveContext(sha string, docs map[string][]byte, meta ContextMeta) error {
	return w.writeContext(sha, docs, meta, false)
}

func (w *Workspace) writeContext(sha string, docs map[string][]byte, meta ContextMeta, respectLock bool) error {
	if _, err := w.contextDirPath(sha); err != nil {
		return err
	}
	for name := range docs {
		if !contextDocs[name] {
			return ErrInvalidContextName
		}
	}
	if meta.SourceSHA == "" {
		meta.SourceSHA = strings.ToLower(sha)
	} else if !strings.EqualFold(meta.SourceSHA, sha) {
		return ErrInvalidSHA
	} else {
		meta.SourceSHA = strings.ToLower(meta.SourceSHA)
	}

	dir, err := w.ensureContextDir(sha)
	if err != nil {
		return err
	}

	if respectLock {
		existing, err := readContextMeta(dir)
		switch {
		case err == nil && existing.Locked:
			return ErrContextLocked
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}

	for name, body := range docs {
		if err := writeFileAtomic(filepath.Join(dir, name), body, fileMode); err != nil {
			return err
		}
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: encode context metadata: %w", err)
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(filepath.Join(dir, "meta.json"), encoded, fileMode)
}

// ContextMeta returns the metadata of the generated document version.
func (w *Workspace) ContextMeta(sha string) (ContextMeta, error) {
	dir, err := w.existingContextDir(sha)
	if err != nil {
		return ContextMeta{}, err
	}
	return readContextMeta(dir)
}

// CleanupContextDirs removes context version directories older than before
// when no database revision protects that SHA. The caller must hold the
// workspace write lock. Locked metadata, unreadable or malformed metadata,
// and unsafe paths are kept; one bad version does not block the others.
func (w *Workspace) CleanupContextDirs(protected map[string]struct{}, before time.Time) (int, error) {
	if w == nil || w.ProjectID <= 0 || !filepath.IsAbs(w.Root) {
		return 0, ErrUnsafePath
	}
	keep := make(map[string]struct{}, len(protected))
	for sha := range protected {
		keep[strings.ToLower(strings.TrimSpace(sha))] = struct{}{}
	}
	root := filepath.Join(w.Root, "context")
	info, err := os.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("workspace: inspect context directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return 0, ErrUnsafePath
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("workspace: read context directory: %w", err)
	}

	var (
		errs    []error
		removed int
	)
	for _, entry := range entries {
		name := entry.Name()
		if !shaRE.MatchString(name) {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := keep[key]; ok {
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
			errs = append(errs, fmt.Errorf("workspace: context %s: inspect: %w", key, err))
			continue
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			continue
		case !info.ModTime().Before(before):
			continue
		}
		meta, err := readContextMeta(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("workspace: context %s: read metadata: %w", key, err))
			continue
		}
		if meta.Locked {
			continue
		}
		if meta.SourceSHA == "" || !strings.EqualFold(meta.SourceSHA, key) {
			errs = append(errs, fmt.Errorf("workspace: context %s: metadata source is missing or mismatched", key))
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("workspace: context %s: remove: %w", key, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// SetContextLocked changes only the lock flag, preserving documents.
func (w *Workspace) SetContextLocked(sha string, locked bool) error {
	dir, err := w.existingContextDir(sha)
	if err != nil {
		return err
	}
	meta, err := readContextMeta(dir)
	if err != nil {
		return err
	}
	meta.Locked = locked
	return writeContextMeta(dir, meta)
}

// SetContextStale changes only the stale flag, which is allowed while locked.
func (w *Workspace) SetContextStale(sha string, stale bool) error {
	dir, err := w.existingContextDir(sha)
	if err != nil {
		return err
	}
	meta, err := readContextMeta(dir)
	if err != nil {
		return err
	}
	meta.Stale = stale
	return writeContextMeta(dir, meta)
}

func (w *Workspace) contextDirPath(sha string) (string, error) {
	if !shaRE.MatchString(sha) {
		return "", ErrInvalidSHA
	}
	return filepath.Join(w.Root, "context", strings.ToLower(sha)), nil
}

func (w *Workspace) ensureContextDir(sha string) (string, error) {
	normalized := strings.ToLower(sha)
	if !shaRE.MatchString(normalized) {
		return "", ErrInvalidSHA
	}
	return w.ensureDir("context", normalized)
}

func (w *Workspace) existingContextDir(sha string) (string, error) {
	dir, err := w.contextDirPath(sha)
	if err != nil {
		return "", err
	}
	for _, path := range []string{filepath.Join(w.Root, "context"), dir} {
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", ErrUnsafePath
		}
	}
	return dir, nil
}

func readContextMeta(dir string) (ContextMeta, error) {
	data, err := readRegularFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return ContextMeta{}, err
	}
	var meta ContextMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return ContextMeta{}, fmt.Errorf("workspace: decode context metadata: %w", err)
	}
	return meta, nil
}

func writeContextMeta(dir string, meta ContextMeta) error {
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: encode context metadata: %w", err)
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(filepath.Join(dir, "meta.json"), encoded, fileMode)
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafePath
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workspace: not a regular file")
	}
	return os.ReadFile(path)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("workspace: not a regular file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("workspace: inspect file: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("workspace: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("workspace: secure temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("workspace: write temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("workspace: close temporary file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("workspace: replace file: %w", err)
	}
	return nil
}
