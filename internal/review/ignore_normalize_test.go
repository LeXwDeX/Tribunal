package review

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreMatcherNormalizesGitignoreForms(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/secret/**", "secret/a.go", true},
		{"/secret/**", "nested/secret/a.go", false},
		{"./secret/**", "secret/a.go", true},
		{"secret/", "secret/a.go", true},
		{"secret/", "nested/secret/a.go", true},
		{"a/**/vendor/", "a/vendor/x.go", true},
		{"a/**/vendor/", "a/b/vendor/x.go", true},
		{"a/**/vendor/", "x/a/vendor/x.go", false},
		{"/private", "nested/private/a.go", false},
		{"/vendor/", "nested/vendor/a.go", false},
		{"vendor/", "nested/vendor/a.go", true},
		{"*.lock", "nested/a.lock", true},
	}
	for _, tc := range tests {
		got := NewIgnoreMatcher([]string{tc.pattern}).MatchPath(tc.path)
		if got != tc.want {
			t.Errorf("pattern %q path %q = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestValidateIgnorePatternsRejectsUnsupportedSyntax(t *testing.T) {
	tests := []struct {
		pattern string
		ok      bool
	}{
		{"/secret/**", true},
		{"./secret/**", true},
		{"a/**/vendor/", true},
		{"!keep", false},
		{"secret/[ab].go", false},
	}
	for _, tc := range tests {
		err := ValidateIgnorePatterns([]string{tc.pattern})
		if (err == nil) != tc.ok {
			t.Errorf("ValidateIgnorePatterns(%q) = %v, want ok=%v", tc.pattern, err, tc.ok)
		}
	}
}

func TestNewIgnoreMatcherFailsClosedForUnsupportedSyntax(t *testing.T) {
	for _, pattern := range []string{"!keep", "secret/[ab].go"} {
		if !NewIgnoreMatcher([]string{pattern}).MatchPath("anything/at/all.go") {
			t.Errorf("unsupported pattern %q did not fail closed", pattern)
		}
	}
}

func TestDossierIgnoresPathNormalizesCommonForms(t *testing.T) {
	d := &Dossier{IgnorePolicy: &IgnorePolicy{
		Version:  IgnorePolicyVersion,
		Patterns: []string{"/secret/**", "./private/**", "vendor/"},
	}}
	tests := []struct {
		path string
		want bool
	}{
		{"secret/a.go", true},
		{"nested/secret/a.go", false},
		{"private/a.go", true},
		{"nested/private/a.go", false},
		{"vendor/a.go", true},
		{"nested/vendor/a.go", true},
		{"allowed.go", false},
	}
	for _, tc := range tests {
		if got := d.IgnoresPath(tc.path); got != tc.want {
			t.Errorf("IgnoresPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSandboxReadDeniesNormalizedIgnoreForms(t *testing.T) {
	for _, pattern := range []string{"/private/**", "./private/**", "private/"} {
		t.Run(pattern, func(t *testing.T) {
			head := t.TempDir()
			if err := os.MkdirAll(filepath.Join(head, "private"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(head, "private", "a.go"), []byte("CANARY\n"), 0600); err != nil {
				t.Fatal(err)
			}
			e := &Executor{deps: ExecutorDeps{Dossier: &Dossier{
				HeadDir:      head,
				IgnorePolicy: &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{pattern}},
			}}}
			sb, err := e.sandboxFor(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer sb.Close()
			root := sb.RootNames()[0]
			if err := sb.ValidateLineRange(root, "private/a.go", 1, 1); err == nil {
				t.Fatalf("sandbox read allowed for %q", pattern)
			}
		})
	}
}
