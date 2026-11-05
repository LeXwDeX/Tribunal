package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRevisionReadAndGrepChooseRequestedSnapshot(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	tools := BuiltinTools(sb)
	for _, tc := range []struct {
		tool, input, want, absent string
	}{
		{"read_file", `{"path":"main.go","rev":"head"}`, "package head", "package base"},
		{"read_file", `{"path":"main.go","rev":"base"}`, "package base", "package head"},
		{"grep", `{"pattern":"package","path":"main.go","rev":"head"}`, "package head", "package base"},
		{"grep", `{"pattern":"package","path":"main.go","rev":"base"}`, "package base", "package head"},
	} {
		t.Run(tc.tool+"/"+tc.want, func(t *testing.T) {
			result, err := invoke(t, toolByName(t, tools, tc.tool), tc.input)
			if err != nil {
				t.Fatalf("revision read failed: %v", err)
			}
			if !strings.Contains(result.Content, tc.want) || strings.Contains(result.Content, tc.absent) {
				t.Fatalf("revision returned wrong snapshot: %q", result.Content)
			}
		})
	}
}

func TestRevisionToolsRejectIgnoredPaths(t *testing.T) {
	parent := t.TempDir()
	head, base := filepath.Join(parent, "head"), filepath.Join(parent, "base")
	writeFile(t, filepath.Join(head, "private.txt"), "head secret\n")
	writeFile(t, filepath.Join(base, "private.txt"), "base secret\n")
	sb, err := NewSandboxWithIgnore(head, base, func(path string) bool { return path == "private.txt" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	for _, name := range []string{"read_file", "grep"} {
		for _, rev := range []string{"head", "base"} {
			input := `{"path":"private.txt","rev":"` + rev + `"`
			if name == "grep" {
				input += `,"pattern":"secret"`
			}
			input += `}`
			_, err := invoke(t, toolByName(t, BuiltinTools(sb), name), input)
			if err == nil || !strings.Contains(err.Error(), ErrIgnoredPath.Error()) {
				t.Errorf("%s rev=%s: expected ignored path error, got %v", name, rev, err)
			}
		}
	}
}
