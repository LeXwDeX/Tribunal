package agent

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrictBlameDoesNotExposeIgnoredMailmap(t *testing.T) {
	_, dir := newGitRepo(t)
	writeFile(t, filepath.Join(dir, ".mailmap"), "CANARY_MAILMAP <mapped@example.invalid> Test <test@example.com>\n")
	if out, err := exec.Command("git", "-C", dir, "add", ".mailmap").CombinedOutput(); err != nil {
		t.Fatalf("add: %s %v", out, err)
	}
	for _, tc := range []struct {
		name string
		deny func(string) bool
		want bool
	}{{"empty", nil, true}, {"strict", func(path string) bool { return path == ".mailmap" }, false}} {
		t.Run(tc.name, func(t *testing.T) {
			sb, err := NewSandboxWithIgnore(dir, "", tc.deny)
			if err != nil {
				t.Fatal(err)
			}
			defer sb.Close()
			res, err := invoke(t, toolByName(t, BuiltinTools(sb), "git_blame"), `{"path":"a.txt","start_line":1,"end_line":1}`)
			if err != nil || strings.Contains(res.Content, "CANARY_MAILMAP") != tc.want || !strings.Contains(res.Content, "first line") {
				t.Fatalf("blame=%q err=%v", res.Content, err)
			}
		})
	}
}
