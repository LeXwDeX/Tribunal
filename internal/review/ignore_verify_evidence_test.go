package review

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoredAgentOutputVerifyEvidenceErrors(t *testing.T) {
	head := t.TempDir()
	if err := os.MkdirAll(filepath.Join(head, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(head, "private", "secret.go"), []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(head, "ok.go"), []byte("a\nb\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		ref  CodeRef
		want bool
	}{
		{"out of range line", CodeRef{Path: "ok.go", StartLine: 50, EndLine: 60, Root: "head", Note: "n"}, false},
		{"missing file", CodeRef{Path: "nope.go", StartLine: 1, EndLine: 1, Root: "head", Note: "n"}, false},
		{"ignored path", CodeRef{Path: "private/secret.go", StartLine: 1, EndLine: 1, Root: "head", Note: "n"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDossier(t)
			d.HeadDir = head
			d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"private/**"}}
			e := newTestExecutor(t, ExecutorDeps{Dossier: d})
			out := &VerifyOut{Status: "CONFIRMED", Reason: "r", Evidence: []CodeRef{tc.ref}}
			if got := e.ignoredAgentOutput(out); got != tc.want {
				t.Errorf("ignored=%v want %v", got, tc.want)
			}
		})
	}
}
