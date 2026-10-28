package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCIWebDistMatchesCheckout(t *testing.T) {
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(name, filepath.Join(t.TempDir(), "foreign"))
	}
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Defaults struct {
				Run struct {
					WorkingDirectory string `yaml:"working-directory"`
				} `yaml:"run"`
			} `yaml:"defaults"`
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	web := workflow.Jobs["web"]
	if web.Defaults.Run.WorkingDirectory != "web" {
		t.Fatal("web job must run in web directory")
	}
	var check string
	for i, step := range web.Steps {
		if step.Name == "Check committed web build" {
			if i == 0 || web.Steps[i-1].Run != "npm run build" {
				t.Fatal("dist check must immediately follow the build")
			}
			check = step.Run
		}
	}
	if check == "" {
		t.Fatal("missing committed web build check")
	}

	for _, tc := range []struct {
		name  string
		path  string
		value string
		del   bool
		fail  bool
	}{
		{name: "clean"},
		{name: "modified asset", path: "web/dist/app.js", value: "changed", fail: true},
		{name: "deleted asset", path: "web/dist/old.js", del: true, fail: true},
		{name: "new asset", path: "web/dist/new/nested.js", value: "new", fail: true},
		{name: "modified outside dist", path: "other.txt", value: "changed"},
		{name: "untracked outside dist", path: "web/other.txt", value: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
			git := func(input string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				cmd.Env = env
				cmd.Stdin = strings.NewReader(input)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("", "init", "-q")
			blob := git("original", "hash-object", "-w", "--stdin")
			distTree := git("100644 blob "+blob+"\tapp.js\n100644 blob "+blob+"\told.js\n", "mktree")
			webTree := git("040000 tree "+distTree+"\tdist\n", "mktree")
			rootTree := git("100644 blob "+blob+"\tother.txt\n040000 tree "+webTree+"\tweb\n", "mktree")
			commit := git("tree "+rootTree+"\nauthor Fixture <fixture@example.invalid> 0 +0000\ncommitter Fixture <fixture@example.invalid> 0 +0000\n\nfixture\n", "hash-object", "-t", "commit", "-w", "--stdin")
			git("", "update-ref", "HEAD", commit)
			git("", "read-tree", rootTree)
			for _, path := range []string{"web/dist/app.js", "web/dist/old.js", "other.txt"} {
				writeFixture(t, dir, path, "original")
			}
			if tc.del {
				if err := os.Remove(filepath.Join(dir, tc.path)); err != nil {
					t.Fatal(err)
				}
			} else if tc.path != "" {
				writeFixture(t, dir, tc.path, tc.value)
			}
			cmd := exec.Command("bash", "-e", "-c", check)
			cmd.Dir = filepath.Join(dir, "web")
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("check failed = %v, want %v: %s", err != nil, tc.fail, out)
			}
		})
	}
}

func writeFixture(t *testing.T, dir, path, value string) {
	t.Helper()
	name := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}
