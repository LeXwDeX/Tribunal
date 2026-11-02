// Package skill loads Claude-compatible instruction skills from explicitly
// configured platform, project and optional repository snapshot directories.
//
// Only name and description are exposed until the model calls load_skill.
// Bodies and attached resources are read lazily, confined to each skill
// directory, and bounded. Scripts inside a skill are never executed.
package skill

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	// DefaultMaxOutputChars bounds one load_skill result.
	DefaultMaxOutputChars = 20000
	// MaxOutputChars is the largest accepted load_skill output limit.
	MaxOutputChars = 1 << 20

	maxSkillFileBytes = 1 << 20
	maxFrontmatter    = 64 << 10
	maxDescriptionRaw = 4096
	maxResources      = 200
	maxResourcePath   = 512
)

var (
	// ErrUnknownSkill reports a bound name that is not in the catalogue.
	ErrUnknownSkill = errors.New("skill: unknown skill")
	// ErrNoSkills reports an empty binding.
	ErrNoSkills = errors.New("skill: no skills bound")
	// ErrUnsafePath reports a skill directory or attachment that escapes its
	// sandbox or is a symlink.
	ErrUnsafePath = errors.New("skill: unsafe path")
	// ErrInvalidSkill reports unreadable or invalid SKILL.md content.
	ErrInvalidSkill = errors.New("skill: invalid skill")

	skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// ValidRepositoryName identifies names that are distinct from legacy skill
// names. It only checks syntax and does not grant access to the name.
func ValidRepositoryName(name string) bool {
	value, ok := strings.CutPrefix(name, "repo:")
	return ok && skillNameRE.MatchString(value) && !strings.Contains(value, "__")
}

// Skill is one catalogue entry. It deliberately omits the body and path.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Options configures a Loader. PlatformDirs are scanned in order, then
// RepoSkillsDir, then ProjectDir. Project skills override platform skills.
// Repository skills are named repo:<name> and cannot override either source.
// RepoSkillsDir must be an already verified, ignore-filtered snapshot; this
// loader does not authenticate its source or authorize any block binding.
type Options struct {
	PlatformDirs   []string
	ProjectDir     string
	RepoSkillsDir  string
	MaxOutputChars int
}

// Loader is an immutable catalogue of validated skills.
type Loader struct {
	skills   map[string]*entry
	warnings []string
	max      int
}

type entry struct {
	name        string
	description string
	dir         string
	source      int
}

// New scans the configured directories. Missing directories are ignored; an
// invalid individual skill is skipped with a warning. Structural problems
// such as a symlinked source directory fail the load.
func New(opts Options) (*Loader, error) {
	max := opts.MaxOutputChars
	if max == 0 {
		max = DefaultMaxOutputChars
	}
	if max < 1 || max > MaxOutputChars {
		return nil, fmt.Errorf("skill: max output chars must be between 1 and %d", MaxOutputChars)
	}
	l := &Loader{skills: make(map[string]*entry), max: max}
	if err := l.scanSources(opts); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Loader) scanSources(opts Options) error {
	for _, dir := range opts.PlatformDirs {
		if err := l.scanSource(dir, 0); err != nil {
			return err
		}
	}
	if opts.RepoSkillsDir != "" {
		if err := l.scanSource(opts.RepoSkillsDir, 1); err != nil {
			return err
		}
	}
	if opts.ProjectDir != "" {
		if err := l.scanSource(opts.ProjectDir, 2); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loader) scanSource(dir string, source int) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	absolute, err := realDir(dir)
	if err != nil {
		return err
	}
	if absolute == "" {
		return nil
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return fmt.Errorf("skill: open source directory: %w", err)
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("skill: read source directory: %w", err)
	}
	for _, dirEntry := range entries {
		name := dirEntry.Name()
		if !dirEntry.IsDir() || dirEntry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		child, err := root.OpenRoot(name)
		if err != nil {
			l.warn("skill directory %q could not be opened", name)
			continue
		}
		skill, err := readSkill(child, filepath.Join(absolute, name), source)
		child.Close()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			l.warn("%s %q: %s", sourceLabel(source), name, err)
			continue
		}
		if source == 1 {
			skill.name = "repo:" + skill.name
		}
		if previous, ok := l.skills[skill.name]; ok {
			if source == 1 && previous.source == 1 {
				return fmt.Errorf("%w: duplicate repository skill name", ErrInvalidSkill)
			}
			l.warn("skill %q from %s overrides %s", skill.name, sourceLabel(source), sourceLabel(previous.source))
		}
		l.skills[skill.name] = skill
	}
	return nil
}

func sourceLabel(source int) string {
	switch source {
	case 1:
		return "repository"
	case 2:
		return "project"
	default:
		return "platform"
	}
}

func (l *Loader) warn(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func readSkill(root *os.Root, dir string, source int) (*entry, error) {
	info, err := root.Lstat("SKILL.md")
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: SKILL.md must be a regular file, not a symlink", ErrInvalidSkill)
	}
	file, err := root.Open("SKILL.md")
	if err != nil {
		return nil, fmt.Errorf("%w: SKILL.md could not be opened", ErrInvalidSkill)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSkillFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: SKILL.md could not be read", ErrInvalidSkill)
	}
	if len(data) > maxSkillFileBytes {
		return nil, fmt.Errorf("%w: SKILL.md is too large", ErrInvalidSkill)
	}
	fm, _, err := parseSkill(data)
	if err != nil {
		return nil, err
	}
	return &entry{name: fm.Name, description: fm.Description, dir: dir, source: source}, nil
}

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

func parseSkill(data []byte) (frontmatter, string, error) {
	if !utf8.Valid(data) {
		return frontmatter{}, "", fmt.Errorf("%w: SKILL.md is not valid UTF-8", ErrInvalidSkill)
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	if strings.ContainsRune(text, 0) {
		return frontmatter{}, "", fmt.Errorf("%w: SKILL.md contains NUL", ErrInvalidSkill)
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return frontmatter{}, "", fmt.Errorf("%w: SKILL.md must start with a frontmatter block", ErrInvalidSkill)
	}
	closeAt := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeAt = i
			break
		}
	}
	if closeAt < 0 {
		return frontmatter{}, "", fmt.Errorf("%w: frontmatter is not closed", ErrInvalidSkill)
	}
	header := strings.Join(lines[1:closeAt], "\n")
	if len(header) > maxFrontmatter {
		return frontmatter{}, "", fmt.Errorf("%w: frontmatter is too large", ErrInvalidSkill)
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(header), &fm); err != nil {
		return frontmatter{}, "", fmt.Errorf("%w: frontmatter is invalid YAML", ErrInvalidSkill)
	}
	fm.Name = strings.TrimSpace(fm.Name)
	fm.Description = strings.TrimSpace(fm.Description)
	if !skillNameRE.MatchString(fm.Name) || strings.Contains(fm.Name, "__") {
		return frontmatter{}, "", fmt.Errorf("%w: name must match %s", ErrInvalidSkill, skillNameRE)
	}
	if fm.Description == "" {
		return frontmatter{}, "", fmt.Errorf("%w: description is required", ErrInvalidSkill)
	}
	if strings.ContainsRune(fm.Description, 0) || len(fm.Description) > maxDescriptionRaw {
		return frontmatter{}, "", fmt.Errorf("%w: description is invalid or too long", ErrInvalidSkill)
	}
	body := strings.Join(lines[closeAt+1:], "\n")
	body = strings.TrimPrefix(body, "\n")
	return fm, body, nil
}

// Catalogue returns the validated skills sorted by name. It never includes
// bodies.
func (l *Loader) Catalogue() []Skill {
	if l == nil {
		return nil
	}
	out := make([]Skill, 0, len(l.skills))
	for _, entry := range l.skills {
		out = append(out, Skill{Name: entry.name, Description: entry.description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RepositoryCatalogue reads metadata only from an already verified and
// ignore-filtered snapshot. It does not authorize a block or verify the
// snapshot's project, target branch or provenance.
func RepositoryCatalogue(files map[string][]byte) ([]Skill, error) {
	items := make(map[string]Skill)
	for path, data := range files {
		dir, file, ok := strings.Cut(path, "/")
		if !ok || dir == "" || file != "SKILL.md" || strings.Contains(path, "\\") || !safeResourcePath(path) || len(data) > maxSkillFileBytes {
			continue
		}
		fm, _, err := parseSkill(data)
		if err != nil {
			continue
		}
		name := "repo:" + fm.Name
		if _, exists := items[name]; exists {
			return nil, fmt.Errorf("%w: duplicate repository skill name", ErrInvalidSkill)
		}
		items[name] = Skill{Name: name, Description: fm.Description}
	}
	out := make([]Skill, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Warnings returns skipped-skill diagnostics in scan order.
func (l *Loader) Warnings() []string {
	if l == nil {
		return nil
	}
	out := make([]string, len(l.warnings))
	copy(out, l.warnings)
	return out
}

// Binding is a fixed, explicitly mounted set of skills.
type Binding struct {
	loader *Loader
	names  []string
	dirs   map[string]string
	schema json.RawMessage
}

// Bind selects skills for one block. Unknown names are an error; an empty
// list is an error because load_skill would be unusable.
func (l *Loader) Bind(names []string) (*Binding, error) {
	if l == nil {
		return nil, ErrNoSkills
	}
	seen := make(map[string]bool, len(names))
	selected := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if _, ok := l.skills[name]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownSkill, name)
		}
		seen[name] = true
		selected = append(selected, name)
	}
	if len(selected) == 0 {
		return nil, ErrNoSkills
	}
	sort.Strings(selected)
	b := &Binding{
		loader: l,
		names:  selected,
		dirs:   make(map[string]string, len(selected)),
		schema: loadSchema(selected),
	}
	for _, name := range selected {
		b.dirs[name] = l.skills[name].dir
	}
	return b, nil
}

// Roots returns absolute skill directories for agent.NewSandbox. Only the
// skills in this binding are mounted.
func (b *Binding) Roots() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.names))
	for _, name := range b.names {
		out = append(out, b.dirs[name])
	}
	return out
}

// Names returns the bound names in stable order.
func (b *Binding) Names() []string {
	if b == nil {
		return nil
	}
	out := make([]string, len(b.names))
	copy(out, b.names)
	return out
}

func realDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: invalid directory", ErrUnsafePath)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("skill: inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: source must be a real directory", ErrUnsafePath)
	}
	return absolute, nil
}

func safeResourcePath(name string) bool {
	if name == "" || name == "." || len(name) > maxResourcePath {
		return false
	}
	if strings.ContainsRune(name, 0) || strings.ContainsAny(name, "\r\n") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func readFileBounded(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrUnsafePath)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSkillFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSkillFileBytes {
		return nil, fmt.Errorf("%w: file is too large", ErrInvalidSkill)
	}
	return data, nil
}
