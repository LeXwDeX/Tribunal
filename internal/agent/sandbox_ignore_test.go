package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func ignoreTestRepo(t *testing.T) string {
	t.Helper()
	_, dir := newGitRepo(t)
	writeFile(t, filepath.Join(dir, "secret", "hidden.txt"), "CANARY_SECRET\n")
	writeFile(t, filepath.Join(dir, "nested", "private.txt"), "CANARY_PRIVATE\n")
	writeFile(t, filepath.Join(dir, "nested", "allowed.txt"), "ALLOWED_NONDIF\n")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "CANARY_REPO_SKILL\n")
	if err := os.Symlink("secret/hidden.txt", filepath.Join(dir, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secret", filepath.Join(dir, "aliasdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../nested", filepath.Join(dir, "secret", "back")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dir, "add", ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s %v", out, err)
	}
	cmd = exec.Command("git", "-C", dir, "commit", "-qm", "CANARY_SUBJECT", "-m", "CANARY_BODY")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s %v", out, err)
	}
	return dir
}

func TestSandboxIgnoreToolsAndRoots(t *testing.T) {
	head, base := ignoreTestRepo(t), ignoreTestRepo(t)
	skill := filepath.Join(t.TempDir(), "platform-skill")
	writeFile(t, filepath.Join(skill, "SKILL.md"), "PLATFORM_SKILL\n")
	if err := os.Symlink(filepath.Join(head, "secret", "hidden.txt"), filepath.Join(skill, "escape")); err != nil {
		t.Fatal(err)
	}
	deny := func(p string) bool {
		return strings.HasPrefix(p, "secret/") || p == "nested/private.txt" || p == "SKILL.md"
	}
	sb, err := NewSandboxWithIgnore(head, base, deny, skill, head, filepath.Join(head, "secret"), filepath.Join(head, ".git", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	tools := BuiltinTools(sb)
	for _, root := range []string{"head", "base", filepath.Base(head)} {
		t.Run(root, func(t *testing.T) {
			for _, p := range []string{"secret/hidden.txt", "nested/private.txt", "alias.txt", "aliasdir/hidden.txt", "aliasdir/back/allowed.txt", "nested/../secret/hidden.txt"} {
				input := fmt.Sprintf(`{"root":%q,"path":%q}`, root, p)
				if result, err := invoke(t, toolByName(t, tools, "read_file"), input); err == nil {
					t.Errorf("read %s = %q, want denial", p, result.Content)
				}
				if err := sb.ValidateLineRange(root, p, 1, 1); err == nil {
					t.Errorf("validate %s succeeded", p)
				}
			}
			for _, input := range []string{
				fmt.Sprintf(`{"root":%q,"pattern":"CANARY"}`, root),
				fmt.Sprintf(`{"root":%q,"pattern":"CANARY","path":"nested"}`, root),
			} {
				res, err := invoke(t, toolByName(t, tools, "grep"), input)
				if err != nil || strings.Contains(res.Content, "CANARY") {
					t.Errorf("grep = %q, %v", res.Content, err)
				}
			}
			for _, input := range []string{
				fmt.Sprintf(`{"root":%q,"pattern":"CANARY","path":"nested/private.txt"}`, root),
				fmt.Sprintf(`{"root":%q,"pattern":"CANARY","path":"secret"}`, root),
			} {
				if res, err := invoke(t, toolByName(t, tools, "grep"), input); err == nil {
					t.Errorf("direct grep = %q", res.Content)
				}
			}
			for _, pattern := range []string{"**/*", "*.md"} {
				res, err := invoke(t, toolByName(t, tools, "glob"), fmt.Sprintf(`{"root":%q,"pattern":%q}`, root, pattern))
				if err != nil || strings.Contains(res.Content, "secret/") || strings.Contains(res.Content, "private.txt") || strings.Contains(res.Content, "SKILL.md") {
					t.Errorf("glob = %q, %v", res.Content, err)
				}
			}
			for _, dir := range []string{".", "nested"} {
				res, err := invoke(t, toolByName(t, tools, "list_dir"), fmt.Sprintf(`{"root":%q,"path":%q}`, root, dir))
				if err != nil || strings.Contains(res.Content, "secret") || strings.Contains(res.Content, "private.txt") || strings.Contains(res.Content, "alias") || strings.Contains(res.Content, "SKILL.md") {
					t.Errorf("list = %q, %v", res.Content, err)
				}
			}
			res, err := invoke(t, toolByName(t, tools, "read_file"), fmt.Sprintf(`{"root":%q,"path":"nested/allowed.txt"}`, root))
			if err != nil || !strings.Contains(res.Content, "ALLOWED_NONDIF") {
				t.Errorf("allowed = %q, %v", res.Content, err)
			}
			if err := sb.ValidateLineRange(root, "nested/allowed.txt", 1, 1); err != nil {
				t.Errorf("allowed evidence: %v", err)
			}
			res, err = invoke(t, toolByName(t, tools, "git_blame"), fmt.Sprintf(`{"root":%q,"path":"nested/allowed.txt","start_line":1,"end_line":1}`, root))
			if err != nil || !strings.Contains(res.Content, "ALLOWED_NONDIF") {
				t.Errorf("allowed blame = %q, %v", res.Content, err)
			}
			if res, err := invoke(t, toolByName(t, tools, "list_dir"), fmt.Sprintf(`{"root":%q,"path":"aliasdir"}`, root)); err == nil {
				t.Errorf("list alias = %q", res.Content)
			}
			for _, name := range []string{"git_log", "git_blame"} {
				input := fmt.Sprintf(`{"root":%q,"path":"secret/hidden.txt"}`, root)
				if name == "git_blame" {
					input = fmt.Sprintf(`{"root":%q,"path":"secret/hidden.txt","start_line":1,"end_line":1}`, root)
				}
				if res, err := invoke(t, toolByName(t, tools, name), input); err == nil {
					t.Errorf("%s ignored = %q", name, res.Content)
				}
			}
		})
	}
	res, err := invoke(t, toolByName(t, tools, "read_file"), `{"root":"platform-skill","path":"SKILL.md"}`)
	if err != nil || !strings.Contains(res.Content, "PLATFORM_SKILL") {
		t.Fatalf("platform skill = %q, %v", res.Content, err)
	}
	for _, input := range []string{`{"root":"platform-skill","path":"escape"}`, `{"root":"secret","path":"hidden.txt"}`} {
		if res, err := invoke(t, toolByName(t, tools, "read_file"), input); err == nil {
			t.Errorf("alias bypass = %q", res.Content)
		}
	}
	if res, err := invoke(t, toolByName(t, tools, "list_dir"), `{"root":"objects"}`); err == nil {
		t.Errorf("git internals alias = %q", res.Content)
	}
}

func TestSandboxIgnoreOptionalAncestorRoot(t *testing.T) {
	head := ignoreTestRepo(t)
	parent := filepath.Dir(head)
	sb, err := NewSandboxWithIgnore(head, "", func(p string) bool { return strings.HasPrefix(p, "secret/") }, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	root := filepath.Base(parent)
	tools := BuiltinTools(sb)
	input := fmt.Sprintf(`{"root":%q,"path":%q}`, root, filepath.ToSlash(filepath.Join(filepath.Base(head), "secret", "hidden.txt")))
	if res, err := invoke(t, toolByName(t, tools, "read_file"), input); err == nil {
		t.Fatalf("ancestor bypass = %q", res.Content)
	}
	res, err := invoke(t, toolByName(t, tools, "grep"), fmt.Sprintf(`{"root":%q,"pattern":"CANARY_SECRET"}`, root))
	if err != nil || strings.Contains(res.Content, "CANARY_SECRET") {
		t.Fatalf("ancestor grep = %q, %v", res.Content, err)
	}
}

func TestSandboxIgnoreNestedRootUnderIgnoredDirectory(t *testing.T) {
	head := ignoreTestRepo(t)
	writeFile(t, filepath.Join(head, "secret", "deep", "file.txt"), "CANARY_NESTED\n")
	sb, err := NewSandboxWithIgnore(head, "", func(p string) bool { return p == "secret/" }, filepath.Join(head, "secret", "deep"))
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	for _, tool := range []string{"read_file", "grep", "glob", "list_dir"} {
		input := `{"root":"deep","path":"file.txt"}`
		switch tool {
		case "grep":
			input = `{"root":"deep","pattern":"CANARY"}`
		case "glob":
			input = `{"root":"deep","pattern":"**/*"}`
		case "list_dir":
			input = `{"root":"deep"}`
		}
		if res, err := invoke(t, toolByName(t, BuiltinTools(sb), tool), input); err == nil {
			t.Errorf("%s nested alias = %q", tool, res.Content)
		}
	}
}

func TestSandboxIgnoreGitLogStrictAndCompatibility(t *testing.T) {
	head := ignoreTestRepo(t)
	for _, args := range [][]string{{"notes", "add", "-m", "CANARY_NOTE"}, {"config", "log.showNotes", "true"}} {
		cmd := exec.Command("git", append([]string{"-C", head}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	for _, tc := range []struct {
		name        string
		deny        func(string) bool
		wantSubject bool
	}{
		{"nil", nil, true},
		{"strict-no-matches", func(string) bool { return false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb, err := NewSandboxWithIgnore(head, "", tc.deny)
			if err != nil {
				t.Fatal(err)
			}
			defer sb.Close()
			tool := toolByName(t, BuiltinTools(sb), "git_log")
			for _, input := range []string{`{}`, `{"path":"a.txt"}`} {
				res, err := invoke(t, tool, input)
				if err != nil {
					t.Fatal(err)
				}
				wantText := "CANARY_SUBJECT"
				if strings.Contains(input, "a.txt") {
					wantText = "initial commit"
				}
				if strings.Contains(res.Content, wantText) != tc.wantSubject {
					t.Fatalf("log = %q", res.Content)
				}
				if strings.Contains(res.Content, "CANARY_BODY") {
					t.Fatalf("body leaked: %q", res.Content)
				}
				if !tc.wantSubject && strings.Contains(res.Content, "CANARY_NOTE") {
					t.Fatalf("note leaked: %q", res.Content)
				}
				if !tc.wantSubject {
					for _, line := range strings.Split(res.Content, "\n") {
						if !regexp.MustCompile(`^[0-9a-f]{40} [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}[+-][0-9]{2}:[0-9]{2}$`).MatchString(line) {
							t.Fatalf("strict log = %q", res.Content)
						}
					}
				}
			}
			for _, tool := range BuiltinTools(sb) {
				if !strings.Contains(tool.Def().Description, "Available roots: head") || !json.Valid(tool.Def().InputSchema) {
					t.Fatalf("missing roots: %+v", tool.Def())
				}
			}
			read, err := invoke(t, toolByName(t, BuiltinTools(sb), "read_file"), `{"path":"secret/hidden.txt"}`)
			if err != nil || !strings.Contains(read.Content, "CANARY_SECRET") {
				t.Fatalf("nonmatching policy read = %q, %v", read.Content, err)
			}
		})
	}
}
