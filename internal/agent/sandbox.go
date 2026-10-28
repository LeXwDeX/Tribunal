package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// ErrUnsafePath reports an absolute, escaping, symlinked or .git path.
	ErrUnsafePath = errors.New("agent: unsafe sandbox path")
	// ErrUnknownRoot reports a root name that NewSandbox did not register.
	ErrUnknownRoot = errors.New("agent: unknown sandbox root")
	// ErrIgnoredPath reports a path excluded by the repository policy.
	ErrIgnoredPath = errors.New("agent: path is ignored")
)

var rootNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Sandbox confines every builtin tool path to a set of named roots. The
// head worktree is the default root; base and optional skill roots are
// mounted read-only as well.
type Sandbox struct {
	roots map[string]*os.Root
	paths map[string]string
	names []string
	def   string
	deny  func(string) bool
	repos []string
}

// NewSandbox opens head (the MR worktree), base (the target-branch checkout)
// and optional skill roots. Empty head or base is allowed when at least one
// root remains. Optional roots use their basename when available; invalid,
// reserved or duplicate names receive an available skill-N alias. Natural
// optional names are reserved before aliases are assigned. All roots must be absolute,
// existing, non-symlink directories. Callers must Close the sandbox after
// all tool calls finish.
func NewSandbox(head, base string, optionalRoots ...string) (*Sandbox, error) {
	return NewSandboxWithIgnore(head, base, nil, optionalRoots...)
}

// NewSandboxWithIgnore applies deny to slash-separated repository-relative paths
// under head and base, including optional roots that overlap either checkout.
// It checks directory names both with and without a trailing slash; "." denotes
// a checkout root. Ignored directories exclude their descendants.
// A non-nil predicate also enables restricted git log output even if it never
// matches. Callers must keep the predicate immutable and safe for concurrent use.
// A nil predicate preserves NewSandbox behaviour. The checkout must not be
// changed by an untrusted concurrent process while tools run.
func NewSandboxWithIgnore(head, base string, deny func(string) bool, optionalRoots ...string) (*Sandbox, error) {
	sb := &Sandbox{roots: make(map[string]*os.Root), paths: make(map[string]string), deny: deny}
	add := func(name, path string) error {
		if path == "" {
			return nil
		}
		clean, err := cleanRoot(path)
		if err != nil {
			return err
		}
		if _, exists := sb.roots[name]; exists {
			return fmt.Errorf("agent: duplicate sandbox root %q", name)
		}
		root, err := os.OpenRoot(clean)
		if err != nil {
			return fmt.Errorf("agent: open sandbox root %q: %w", name, err)
		}
		sb.roots[name] = root
		sb.paths[name] = clean
		sb.names = append(sb.names, name)
		return nil
	}
	if err := add("head", head); err != nil {
		return nil, err
	}
	if err := add("base", base); err != nil {
		sb.Close()
		return nil, err
	}
	if deny != nil {
		for _, name := range []string{"head", "base"} {
			if root := sb.roots[name]; root != nil {
				real, err := filepath.EvalSymlinks(root.Name())
				if err != nil {
					sb.Close()
					return nil, errors.New("agent: repository root is not accessible")
				}
				sb.repos = append(sb.repos, real)
			}
		}
	}
	reserved := make(map[string]bool, len(optionalRoots))
	for _, path := range optionalRoots {
		if path == "" {
			continue
		}
		name := sanitizeRootName(filepath.Base(filepath.Clean(path)))
		if name != "" && name != "head" && name != "base" && !strings.EqualFold(name, ".git") {
			reserved[name] = true
		}
	}
	for i, path := range optionalRoots {
		if path == "" {
			continue
		}
		name := sanitizeRootName(filepath.Base(filepath.Clean(path)))
		if _, exists := sb.roots[name]; name == "" || strings.EqualFold(name, ".git") || name == "head" || name == "base" || exists {
			for n := i + 1; ; n++ {
				name = fmt.Sprintf("skill-%d", n)
				if _, exists := sb.roots[name]; !exists && !reserved[name] {
					break
				}
			}
		}
		if err := add(name, path); err != nil {
			sb.Close()
			return nil, err
		}
	}
	if len(sb.roots) == 0 {
		return nil, errors.New("agent: at least one sandbox root is required")
	}
	sort.Strings(sb.names)
	switch {
	case sb.roots["head"] != nil:
		sb.def = "head"
	case sb.roots["base"] != nil:
		sb.def = "base"
	default:
		sb.def = sb.names[0]
	}
	return sb, nil
}

func cleanRoot(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: root must be absolute", ErrUnsafePath)
	}
	clean := filepath.Clean(path)
	if filepath.Dir(clean) == clean {
		return "", fmt.Errorf("%w: filesystem root is not a sandbox root", ErrUnsafePath)
	}
	if strings.ContainsRune(clean, 0) {
		return "", fmt.Errorf("%w: root contains NUL", ErrUnsafePath)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return "", fmt.Errorf("agent: inspect sandbox root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: root is a symlink", ErrUnsafePath)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: root is not a directory", ErrUnsafePath)
	}
	if strings.EqualFold(filepath.Base(clean), ".git") {
		return "", fmt.Errorf("%w: .git root", ErrUnsafePath)
	}
	return clean, nil
}

func sanitizeRootName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) || !rootNameRE.MatchString(name) {
		return ""
	}
	return name
}

// Close releases every opened root.
func (s *Sandbox) Close() error {
	if s == nil {
		return nil
	}
	var first error
	for _, root := range s.roots {
		if err := root.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.roots = nil
	return first
}

// RootNames returns the mounted root names in stable order.
func (s *Sandbox) RootNames() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.names))
	copy(out, s.names)
	return out
}

// resolve returns the cleaned relative path and its root name.
func (s *Sandbox) resolve(rootName, rel string) (*os.Root, string, string, error) {
	if s == nil || len(s.roots) == 0 {
		return nil, "", "", errors.New("agent: sandbox is closed")
	}
	if rootName == "" {
		rootName = s.def
	}
	root, ok := s.roots[rootName]
	if !ok {
		return nil, "", "", fmt.Errorf("%w: %q (available: %s)", ErrUnknownRoot, rootName, strings.Join(s.names, ", "))
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return nil, "", "", err
	}
	if err := checkReal(root, clean); err != nil {
		return nil, "", "", err
	}
	if err := s.checkIgnore(root, clean); err != nil {
		return nil, "", "", err
	}
	return root, rootName, clean, nil
}

// checkIgnore checks the logical path and every resolved prefix. Checking
// prefixes also blocks a symlink that enters an ignored directory and exits it.
func (s *Sandbox) checkIgnore(root *os.Root, clean string) error {
	if s.deny == nil {
		return nil
	}
	rootPath, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return errors.New("agent: path is not accessible")
	}
	current := rootPath
	parts := []string{}
	if clean != "." {
		parts = strings.Split(clean, "/")
	}
	for i := 0; i <= len(parts); i++ {
		if i > 0 {
			current = filepath.Join(current, filepath.FromSlash(parts[i-1]))
		}
		real, err := filepath.EvalSymlinks(current)
		if err != nil {
			return errors.New("agent: path is not accessible")
		}
		info, err := os.Stat(real)
		if err != nil {
			return errors.New("agent: path is not accessible")
		}
		for _, candidate := range []string{current, real} {
			for _, repo := range s.repos {
				rel, err := filepath.Rel(repo, candidate)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					continue
				}
				rel = filepath.ToSlash(rel)
				if pathWithin(rel) {
					return fmt.Errorf("%w: .git internals are not readable", ErrUnsafePath)
				}
				if s.ignoredRepoPath(rel, info.IsDir()) {
					return ErrIgnoredPath
				}
			}
		}
	}
	return nil
}

func (s *Sandbox) ignoredRepoPath(rel string, isDir bool) bool {
	if rel == "." {
		return s.deny(rel)
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		if s.deny(prefix) || ((i < len(parts)-1 || isDir) && s.deny(prefix+"/")) {
			return true
		}
	}
	return false
}

// checkReal rejects paths whose symlink-resolved target leaves the root or
// reaches .git internals. os.Root already blocks escaping links, but a
// symlink inside the root can still point at .git, which must stay hidden.
func checkReal(root *os.Root, clean string) error {
	realRoot, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return errors.New("agent: path is not accessible")
	}
	real, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(clean)))
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("agent: path does not exist")
		}
		return errors.New("agent: path is not accessible")
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil {
		return fmt.Errorf("%w: path escapes the sandbox", ErrUnsafePath)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%w: path escapes the sandbox", ErrUnsafePath)
	}
	if pathWithin(rel) {
		return fmt.Errorf("%w: .git internals are not readable", ErrUnsafePath)
	}
	return nil
}

func cleanRel(rel string) (string, error) {
	if strings.ContainsAny(rel, "\x00\r\n") {
		return "", fmt.Errorf("%w: path contains a control character", ErrUnsafePath)
	}
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("%w: absolute paths are not allowed", ErrUnsafePath)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path escapes the sandbox", ErrUnsafePath)
	}
	for _, segment := range strings.Split(filepath.ToSlash(clean), "/") {
		if segment == ".." {
			return "", fmt.Errorf("%w: path escapes the sandbox", ErrUnsafePath)
		}
		if strings.EqualFold(segment, ".git") {
			return "", fmt.Errorf("%w: .git internals are not readable", ErrUnsafePath)
		}
	}
	if clean == "." {
		return ".", nil
	}
	return filepath.ToSlash(clean), nil
}

func pathWithin(name string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(name), "/") {
		if strings.EqualFold(segment, ".git") {
			return true
		}
	}
	return false
}

// ValidateLineRange verifies that rel is a readable regular file inside
// rootName and contains every line in [start, end]. It uses the same escape
// checks as builtin reads and reports unknown roots, missing files, symlink
// escapes, and short files as errors. Callers use the error as evidence that a
// refutation is not proven; it must not become a successful refutation.
func (s *Sandbox) ValidateLineRange(rootName, rel string, start, end int) error {
	if start <= 0 {
		return errors.New("agent: start line must be positive")
	}
	if end < start {
		return errors.New("agent: end line must not be before start line")
	}
	file, clean, err := s.open(rootName, rel)
	if err != nil {
		return err
	}
	defer file.Close()
	lines, err := countLines(file)
	if err != nil {
		return fmt.Errorf("agent: read %s: %w", clean, err)
	}
	if end > lines {
		return fmt.Errorf("agent: %s has %d lines; range %d-%d is outside the file", clean, lines, start, end)
	}
	return nil
}

func countLines(reader io.Reader) (int, error) {
	buf := make([]byte, 32*1024)
	lines := 0
	nonEmpty := false
	last := byte('\n')
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			nonEmpty = true
			lines += bytes.Count(buf[:n], []byte{'\n'})
			last = buf[n-1]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if nonEmpty && last != '\n' {
		lines++
	}
	return lines, nil
}

// open opens a sandboxed file for reading. os.Root rejects symlinks that
// escape and path traversal beyond the root.
func (s *Sandbox) open(rootName, rel string) (*os.File, string, error) {
	root, _, clean, err := s.resolve(rootName, rel)
	if err != nil {
		return nil, "", err
	}
	info, err := root.Stat(filepath.FromSlash(clean))
	if err != nil {
		return nil, "", fmt.Errorf("agent: stat %s: %w", clean, err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("agent: %s is not a regular file", clean)
	}
	file, err := root.Open(filepath.FromSlash(clean))
	if err != nil {
		return nil, "", fmt.Errorf("agent: open %s: %w", clean, err)
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, "", fmt.Errorf("agent: stat %s: %w", clean, err)
	}
	if !opened.Mode().IsRegular() {
		file.Close()
		return nil, "", fmt.Errorf("agent: %s is not a regular file", clean)
	}
	return file, clean, nil
}
