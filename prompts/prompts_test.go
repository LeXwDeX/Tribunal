package prompts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLoadSelectsAllRolesAndIsDeterministic(t *testing.T) {
	first, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || len(first.Hash) != 64 {
		t.Fatalf("hash = %q, %q", first.Hash, second.Hash)
	}
	if len(RoleFiles) != 10 {
		t.Fatalf("role count = %d", len(RoleFiles))
	}
	for _, name := range RoleFiles {
		content, ok := first.Content(name)
		if !ok || strings.TrimSpace(content) == "" {
			t.Fatalf("missing role %s", name)
		}
		version := first.RoleVersion(name)
		if version == "" || !regexp.MustCompile(`^[0-9A-Za-z._-]+$`).MatchString(version) {
			t.Fatalf("bad version %s=%q", name, version)
		}
	}
	if len(first.Names()) != len(RoleFiles) || first.Names()[0] != PlatformRules {
		t.Fatalf("names = %v", first.Names())
	}
}

func TestOverrideOnlyUsesRegularVersionedFiles(t *testing.T) {
	dir := t.TempDir()
	override := "<!-- v7 -->\n# Custom code review\nCustom body.\n"
	if err := os.WriteFile(filepath.Join(dir, CodeReview), []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	embedded, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Hash == embedded.Hash {
		t.Fatal("override must change the bundle hash")
	}
	if selected.RoleVersion(CodeReview) != "7" {
		t.Fatalf("version = %q", selected.RoleVersion(CodeReview))
	}
	if content, _ := selected.Content(CodeReview); content != override {
		t.Fatalf("override content = %q", content)
	}
	if content, _ := selected.Content(Plan); content != embedded.Files[Plan] {
		t.Fatal("unrelated role was not preserved")
	}

	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, Verify), []byte("# no version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(badDir); err == nil {
		t.Fatal("missing version must fail")
	}

	dirPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirPath, Reasoner), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dirPath); err == nil {
		t.Fatal("override directory entry must fail")
	}

	largeDir := t.TempDir()
	large := "<!-- v1 -->\n" + strings.Repeat("x", MaxFileBytes)
	if err := os.WriteFile(filepath.Join(largeDir, Onboarding), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(largeDir); err == nil {
		t.Fatal("oversized override must fail")
	}
}

func TestCheckFilesRejectsZeroBundle(t *testing.T) {
	if err := CheckFiles(Bundle{}); err == nil {
		t.Fatal("zero bundle must fail")
	}
}

func TestPromptFilesContainNoSecrets(t *testing.T) {
	bundle, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	patterns := []string{"sk-", "glpat-", "AKIA", "ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "-----BEGIN", "xoxb-"}
	for name, content := range bundle.Files {
		for _, pattern := range patterns {
			if strings.Contains(content, pattern) {
				t.Fatalf("prompt %s contains secret-like pattern %q", name, pattern)
			}
		}
	}
}

// embeddedBundleHash pins the embedded prompt bundle. The hash is part of
// every review fingerprint, so changing any prompt invalidates pending
// publications and makes the next trigger review the MR again. Update this
// value only together with a role version bump and a note in the change.
const embeddedBundleHash = "1c2475c13fd8d3036eb440b6496252dddf58ac298dee03ad48ffb59890284664"

func TestEmbeddedBundleHashIsPinned(t *testing.T) {
	bundle, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Hash != embeddedBundleHash {
		t.Fatalf("embedded bundle hash = %s, want %s; bump the changed role version and update embeddedBundleHash", bundle.Hash, embeddedBundleHash)
	}
}

func TestPlatformRulesExplainGraphReflectsBase(t *testing.T) {
	bundle, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := bundle.RoleVersion(PlatformRules); got != "3" {
		t.Fatalf("platform rules version = %q, want 3", got)
	}
	rules, _ := bundle.Content(PlatformRules)
	for _, want := range []string{"codebase-memory", "`base`", "worktree", "`read_file`", "`grep`"} {
		if !strings.Contains(rules, want) {
			t.Fatalf("platform rules missing %q", want)
		}
	}
}

func TestPlatformRulesSetReportingBarAndCommentStyle(t *testing.T) {
	bundle, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := bundle.Content(PlatformRules)
	for _, want := range []string{"## Reporting bar", "This merge request introduced it", "Speculation that something may break is not a finding", "## Comment style", "one short paragraph", "Lead with the\n  trigger", "at most 3 lines", "`suggestion` fenced block"} {
		if !strings.Contains(rules, want) {
			t.Fatalf("platform rules missing %q", want)
		}
	}
	for _, name := range []string{CodeReview, ReviewStandards} {
		content, _ := bundle.Content(name)
		if strings.Contains(content, "structured Markdown") || strings.Contains(content, "Structured Markdown") {
			t.Fatalf("%s still asks for structured Markdown", name)
		}
	}
	synth, _ := bundle.Content(Synthesize)
	if !strings.Contains(synth, "1 to 3 sentences") {
		t.Fatal("synthesize summary is not bounded to 1 to 3 sentences")
	}
}
