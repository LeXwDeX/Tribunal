// Package prompts embeds the trusted role prompts used by review nodes.
//
// Prompt text is code-reviewed with the binary. An administrator may override
// the same file names from a trusted local directory. The bundle hash covers
// every selected file, including overrides, and belongs in the review
// fingerprint.
package prompts

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Role file names listed by docs/design/01-modules.md.
const (
	PlatformRules   = "_platform_rules.md"
	Plan            = "main.plan.md"
	Synthesize      = "main.synthesize.md"
	CodeReview      = "code-review.md"
	ReviewStandards = "review.standards.md"
	ReviewIntent    = "review.intent.md"
	ReviewGate      = "review.gate.md"
	Reasoner        = "reasoner.md"
	Verify          = "verify.md"
	Onboarding      = "onboarding.md"
)

// RoleFiles is the complete selected bundle, in stable hash order.
var RoleFiles = []string{
	PlatformRules,
	Plan,
	Synthesize,
	CodeReview,
	ReviewStandards,
	ReviewIntent,
	ReviewGate,
	Reasoner,
	Verify,
	Onboarding,
}

// MaxFileBytes bounds an override file. Embedded files are reviewed with the
// binary, but an override is an external input and needs a hard limit.
const MaxFileBytes = 256 << 10

// TaskRenderVersion identifies the task layout and coverage semantics that
// build prompts from these files. It is code-owned and cannot be overridden.
// Bump it whenever task rendering changes, so workflows created with the old
// identity are not resumed, retried, or published.
const TaskRenderVersion = "3"

var versionRE = regexp.MustCompile(`^<!-- v([0-9A-Za-z._-]+) -->$`)

//go:embed *.md
var embedded embed.FS

// Bundle is one selected prompt set after optional overrides. Hash covers the
// selected files and TaskRenderVersion, so an unchanged override cannot keep
// an old task-render identity.
type Bundle struct {
	Files    map[string]string // role file name -> content
	Versions map[string]string // role file name -> first-line version
	Hash     string            // SHA-256 of files plus the task-render version
}

// Load reads the embedded bundle and overlays regular files with matching
// names from overrideDir. An empty overrideDir selects the embedded bundle.
func Load(overrideDir string) (Bundle, error) {
	b := Bundle{
		Files:    make(map[string]string, len(RoleFiles)),
		Versions: make(map[string]string, len(RoleFiles)),
	}
	if overrideDir != "" {
		info, err := os.Stat(overrideDir)
		if err != nil {
			return Bundle{}, fmt.Errorf("prompt override directory %q: %w", overrideDir, err)
		}
		if !info.IsDir() {
			return Bundle{}, fmt.Errorf("prompt override path %q is not a directory", overrideDir)
		}
	}
	for _, name := range RoleFiles {
		content, err := embedded.ReadFile(name)
		if err != nil {
			return Bundle{}, fmt.Errorf("embedded prompt %q: %w", name, err)
		}
		selected := string(content)
		if overrideDir != "" {
			override, ok, err := readOverride(overrideDir, name)
			if err != nil {
				return Bundle{}, err
			}
			if ok {
				selected = override
			}
		}
		version, err := promptVersion(selected)
		if err != nil {
			return Bundle{}, fmt.Errorf("prompt %q: %w", name, err)
		}
		b.Files[name] = selected
		b.Versions[name] = version
	}
	b.Hash = bundleHash(b.Files, b.Versions)
	return b, nil
}

// Content returns a selected role file.
func (b Bundle) Content(name string) (string, bool) {
	content, ok := b.Files[name]
	return content, ok
}

// RoleVersion returns the first-line version of a role file.
func (b Bundle) RoleVersion(name string) string {
	return b.Versions[name]
}

// Names returns the selected role names in hash order.
func (b Bundle) Names() []string {
	names := make([]string, len(RoleFiles))
	copy(names, RoleFiles)
	return names
}

func readOverride(dir, name string) (string, bool, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("prompt override %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("prompt override %q is not a regular file", path)
	}
	if info.Size() > MaxFileBytes {
		return "", false, fmt.Errorf("prompt override %q exceeds %d bytes", path, MaxFileBytes)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read prompt override %q: %w", path, err)
	}
	if len(content) > MaxFileBytes {
		return "", false, fmt.Errorf("prompt override %q exceeds %d bytes", path, MaxFileBytes)
	}
	return string(content), true, nil
}

func promptVersion(content string) (string, error) {
	line := content
	if i := strings.IndexByte(content, '\n'); i >= 0 {
		line = content[:i]
	}
	line = strings.TrimSuffix(line, "\r")
	line = strings.TrimSpace(line)
	match := versionRE.FindStringSubmatch(line)
	if match == nil {
		return "", fmt.Errorf("first line must be a version comment like <!-- v1 -->")
	}
	return match[1], nil
}

func bundleHash(files, versions map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "task-render\x00%s\x00", TaskRenderVersion)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", name, versions[name], files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CheckFiles validates every file name and returns a stable error for a
// missing role. It is useful in tests and at startup.
func CheckFiles(b Bundle) error {
	for _, name := range RoleFiles {
		content, ok := b.Files[name]
		if !ok {
			return fmt.Errorf("missing prompt role %q", name)
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Errorf("empty prompt role %q", name)
		}
		if _, ok := b.Versions[name]; !ok {
			return fmt.Errorf("missing prompt version %q", name)
		}
	}
	return nil
}
