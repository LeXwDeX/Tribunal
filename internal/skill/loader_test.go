package skill

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillFile(t *testing.T, root, dir, name, description, body string) string {
	t.Helper()
	skillDir := filepath.Join(root, dir)
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return skillDir
}

func writeAttachment(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoaderCatalogueAndProjectOverride(t *testing.T) {
	platform := t.TempDir()
	project := t.TempDir()
	repo := t.TempDir()
	writeSkillFile(t, platform, "security", "security", "platform security", "platform body\n")
	writeSkillFile(t, platform, "coding", "coding", "coding standards", "coding body\n")
	writeSkillFile(t, project, "security", "security", "project security", "project body\n")
	writeSkillFile(t, repo, "repo-skill", "repo-skill", "repo skill", "repo body\n")

	l, err := New(Options{
		PlatformDirs:  []string{platform},
		ProjectDir:    project,
		RepoSkillsDir: repo,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	catalogue := l.Catalogue()
	if len(catalogue) != 3 {
		t.Fatalf("catalogue = %+v", catalogue)
	}
	byName := make(map[string]Skill, len(catalogue))
	for _, skill := range catalogue {
		byName[skill.Name] = skill
	}
	if byName["security"].Description != "project security" {
		t.Fatalf("project did not override platform: %+v", byName["security"])
	}
	if !strings.Contains(strings.Join(l.Warnings(), "\n"), "overrides") {
		t.Fatalf("warnings = %v", l.Warnings())
	}
}

func TestLoaderLazyBody(t *testing.T) {
	platform := t.TempDir()
	project := t.TempDir()
	writeSkillFile(t, project, "security", "security", "security", "old body\n")
	l, err := New(Options{PlatformDirs: []string{platform}, ProjectDir: project})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Changing the file after New must be observed by load_skill.
	writeSkillFile(t, project, "security", "security", "security", "new body\n")
	binding, err := l.Bind([]string{"security"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	content, _, err := binding.load("security", 20000)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(content, "new body") || strings.Contains(content, "old body") {
		t.Fatalf("lazy load content = %q", content)
	}
}

func TestLoaderInvalidSkillsAreSkipped(t *testing.T) {
	platform := t.TempDir()
	writeSkillFile(t, platform, "good", "good", "good description", "body\n")
	writeInvalid := func(dir, content string) {
		path := filepath.Join(platform, dir)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeInvalid("no-frontmatter", "# no frontmatter\n")
	writeInvalid("missing-description", "---\nname: missing-description\n---\nbody\n")
	writeInvalid("bad-name", "---\nname: Bad Name\ndescription: bad\n---\nbody\n")
	outside := t.TempDir()
	writeSkillFile(t, outside, "outside", "outside", "outside", "body\n")
	if err := os.Symlink(filepath.Join(outside, "outside"), filepath.Join(platform, "linked")); err != nil {
		t.Fatal(err)
	}

	l, err := New(Options{PlatformDirs: []string{platform}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	catalogue := l.Catalogue()
	if len(catalogue) != 1 || catalogue[0].Name != "good" {
		t.Fatalf("catalogue = %+v", catalogue)
	}
	if len(l.Warnings()) < 3 {
		t.Fatalf("warnings = %v", l.Warnings())
	}
}

func TestLoaderRejectsSymlinkedSource(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{PlatformDirs: []string{link}}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlinked source error = %v", err)
	}
}

func TestRepoSkillsAreOptIn(t *testing.T) {
	base := t.TempDir()
	repoSkills := filepath.Join(base, ".claude", "skills")
	writeSkillFile(t, repoSkills, "repo-skill", "repo-skill", "repo", "body\n")

	l, err := New(Options{PlatformDirs: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Catalogue()) != 0 {
		t.Fatalf("repository skills were read without opt-in: %+v", l.Catalogue())
	}

	l, err = New(Options{PlatformDirs: []string{t.TempDir()}, RepoSkillsDir: repoSkills})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Catalogue()) != 1 || l.Catalogue()[0].Name != "repo:repo-skill" {
		t.Fatalf("opt-in catalogue = %+v", l.Catalogue())
	}
}

func TestRepositorySkillsRequireSourceQualifiedBindings(t *testing.T) {
	platform, project, repo := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkillFile(t, platform, "shared", "shared", "platform", "platform body")
	writeSkillFile(t, project, "shared", "shared", "project", "project body")
	writeSkillFile(t, repo, "shared", "shared", "repository", "repository body")
	writeSkillFile(t, repo, "only-repo", "only-repo", "repository only", "repo only body")
	l, err := New(Options{PlatformDirs: []string{platform}, ProjectDir: project, RepoSkillsDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Bind([]string{"only-repo"}); !errors.Is(err, ErrUnknownSkill) {
		t.Fatalf("unqualified repository-only binding: %v", err)
	}
	bound, err := l.Bind([]string{"shared", "repo:shared", "repo:only-repo"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"shared": "project body", "repo:shared": "repository body", "repo:only-repo": "repo only body"} {
		content, _, err := bound.load(name, 20000)
		if err != nil || !strings.Contains(content, want) {
			t.Errorf("load %q = %q, %v; want %q", name, content, err, want)
		}
	}
	repoOnly, err := l.Bind([]string{"repo:shared"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repoOnly.Tool().Call(context.Background(), json.RawMessage(`{"name":"shared"}`)); !errors.Is(err, ErrUnknownSkill) {
		t.Fatalf("unbound platform skill exposed by repository binding: %v", err)
	}
	result, err := repoOnly.Tool().Call(context.Background(), json.RawMessage(`{"name":"repo:shared"}`))
	if err != nil || !strings.Contains(result.Content, "repository body") || strings.Contains(result.Content, "project body") {
		t.Fatalf("qualified tool call = %q, %v", result.Content, err)
	}
}

func TestRepositorySkillsRejectDuplicateManifestNames(t *testing.T) {
	repo := t.TempDir()
	writeSkillFile(t, repo, "first", "collision", "first", "first body")
	writeSkillFile(t, repo, "second", "collision", "second", "second body")
	if _, err := New(Options{RepoSkillsDir: repo}); err == nil {
		t.Fatal("ambiguous repository skill name was accepted")
	}
}

func TestRepositoryCatalogueUsesOnlyProvidedFilteredManifests(t *testing.T) {
	files := map[string][]byte{
		"allowed/SKILL.md":      []byte("---\nname: allowed\ndescription: visible\n---\nvisible body\n"),
		"allowed/private.txt":   []byte("not metadata"),
		"hidden/attachment.txt": []byte("ignored manifest is absent"),
		"nested/deep/SKILL.md":  []byte("---\nname: nested\ndescription: not a direct child\n---\n"),
		"invalid/SKILL.md":      []byte("---\nname: INVALID\ndescription: invalid\n---\n"),
	}
	items, err := RepositoryCatalogue(files)
	if err != nil || len(items) != 1 || items[0].Name != "repo:allowed" || items[0].Description != "visible" {
		t.Fatalf("repository catalogue = %+v, %v", items, err)
	}
	files["duplicate/SKILL.md"] = []byte("---\nname: allowed\ndescription: collision\n---\n")
	if _, err := RepositoryCatalogue(files); err == nil {
		t.Fatal("duplicate repository names accepted")
	}
}

func TestBindingValidation(t *testing.T) {
	platform := t.TempDir()
	writeSkillFile(t, platform, "security", "security", "security", "body\n")
	l, err := New(Options{PlatformDirs: []string{platform}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Bind(nil); !errors.Is(err, ErrNoSkills) {
		t.Fatalf("empty bind error = %v", err)
	}
	if _, err := l.Bind([]string{"missing"}); !errors.Is(err, ErrUnknownSkill) {
		t.Fatalf("unknown bind error = %v", err)
	}
	binding, err := l.Bind([]string{"security", "security"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if len(binding.Names()) != 1 {
		t.Fatalf("names = %v", binding.Names())
	}
	roots := binding.Roots()
	if len(roots) != 1 || !filepath.IsAbs(roots[0]) {
		t.Fatalf("roots = %v", roots)
	}
	if _, err := New(Options{MaxOutputChars: MaxOutputChars + 1}); err == nil {
		t.Fatal("New accepted an oversized output limit")
	}
}

func TestParseSkillFrontmatter(t *testing.T) {
	valid := "---\nname: review\nlicense: MIT\ndescription: review rules\n---\nBody\n"
	fm, body, err := parseSkill([]byte(valid))
	if err != nil {
		t.Fatalf("parseSkill: %v", err)
	}
	if fm.Name != "review" || fm.Description != "review rules" || !strings.Contains(body, "Body") {
		t.Fatalf("parsed = %+v body=%q", fm, body)
	}
	crlf := strings.ReplaceAll(valid, "\n", "\r\n")
	if _, _, err := parseSkill([]byte("\ufeff" + crlf)); err != nil {
		t.Fatalf("BOM/CRLF parse: %v", err)
	}

	cases := []string{
		"name: review\ndescription: rules\n",
		"---\nname: review\n",
		"---\nname: review\ndescription:\n---\nbody\n",
		"---\nname: \"Bad Name\"\ndescription: rules\n---\nbody\n",
		"---\n[not: valid\n---\nbody\n",
		"---\nname: review\ndescription: rules\n---\nbody\x00\n",
	}
	for _, data := range cases {
		if _, _, err := parseSkill([]byte(data)); err == nil {
			t.Fatalf("parseSkill accepted %q", data)
		}
	}
}
