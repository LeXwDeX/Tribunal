package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func skillRepoFile(t *testing.T, dir, name, value string) {
	t.Helper()
	file := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitSkillRepo(t *testing.T, dir string) string {
	t.Helper()
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "-m", "fixture")
	return trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))
}

func TestRepositorySkillTreeReadsOnlyPinnedCommitAndFilteredPaths(t *testing.T) {
	dir := initRepo(t)
	skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "target body")
	skillRepoFile(t, dir, ".claude/skills/review/public.txt", "target resource")
	skillRepoFile(t, dir, ".claude/skills/review/private.txt", "secret")
	skillRepoFile(t, dir, ".claude/skills/ignored/SKILL.md", "hidden metadata")
	skillRepoFile(t, dir, "outside.txt", "outside")
	target := commitSkillRepo(t, dir)
	skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "head body")
	_ = commitSkillRepo(t, dir)
	skillRepoFile(t, dir, ".claude/skills/review/public.txt", "dirty worktree")

	denied := map[string]bool{
		".claude/skills/review/private.txt": true,
		".claude/skills/ignored":            true,
	}
	files, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, target, func(path string) bool { return denied[path] })
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || string(files["review/SKILL.md"]) != "target body" || string(files["review/public.txt"]) != "target resource" {
		t.Fatalf("snapshot = %#v", files)
	}
	for _, path := range []string{"review/private.txt", "ignored/SKILL.md", "outside.txt"} {
		if _, ok := files[path]; ok {
			t.Fatalf("ignored/outside path %q was exposed", path)
		}
	}
}

func TestRepositorySkillTreeRejectsUnprovenRevisionAndMissingPolicy(t *testing.T) {
	dir := initRepo(t)
	skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "body")
	sha := commitSkillRepo(t, dir)
	for _, revision := range []string{"HEAD", "main", "--help", sha[:12]} {
		if _, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, revision, func(string) bool { return false }); !errors.Is(err, ErrInvalidSHA) {
			t.Errorf("revision %q: %v", revision, err)
		}
	}
	if _, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, sha, nil); err == nil {
		t.Fatal("missing ignore policy was accepted")
	}
}

func TestRepositorySkillTreeRejectsUnsafeGitObjects(t *testing.T) {
	tests := []struct {
		name string
		add  func(t *testing.T, dir, commit string)
	}{
		{"symlink", func(t *testing.T, dir, _ string) {
			if err := os.Symlink("/etc/passwd", filepath.Join(dir, ".claude/skills/review/link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"gitlink", func(t *testing.T, dir, commit string) {
			gitCmd(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+commit+",.claude/skills/review/submodule")
		}},
		{"unsafe path", func(t *testing.T, dir, _ string) {
			skillRepoFile(t, dir, ".claude/skills/review/bad\nname", "secret")
		}},
		{"oversize", func(t *testing.T, dir, _ string) {
			skillRepoFile(t, dir, ".claude/skills/review/large", strings.Repeat("x", (1<<20)+1))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := initRepo(t)
			skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "body")
			base := commitSkillRepo(t, dir)
			if tt.name != "gitlink" {
				tt.add(t, dir, base)
			}
			gitCmd(t, dir, "add", "-A")
			if tt.name == "gitlink" {
				tt.add(t, dir, base)
			}
			gitCmd(t, dir, "commit", "-q", "-m", "unsafe")
			sha := trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))
			files, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, sha, func(string) bool { return false })
			if err == nil || files != nil {
				t.Fatalf("unsafe repository snapshot = %#v, %v", files, err)
			}
		})
	}
}

func TestRepositorySkillTreeIgnoredSourceAndRenamePath(t *testing.T) {
	dir := initRepo(t)
	skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "hidden")
	skillRepoFile(t, dir, ".claude/skills/review/rules.txt", "rules")
	sha := commitSkillRepo(t, dir)
	for _, ignored := range []string{".claude", ".claude/skills", ".claude/skills/review"} {
		files, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, sha, func(path string) bool { return path == ignored })
		if err != nil || len(files) != 0 {
			t.Fatalf("ignored ancestor %q: files=%v err=%v", ignored, files, err)
		}
	}
	files, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, sha, func(path string) bool {
		return path == ".claude/skills/review/SKILL.md"
	})
	if err != nil || len(files) != 1 || string(files["review/rules.txt"]) != "rules" {
		t.Fatalf("ignored rename counterpart: files=%v err=%v", files, err)
	}
}

func TestRepositorySkillTreeIgnoresGitReplaceRefs(t *testing.T) {
	dir := initRepo(t)
	skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "trusted target")
	sha := commitSkillRepo(t, dir)
	original := trimSHA(gitCmd(t, dir, "rev-parse", sha+":.claude/skills/review/SKILL.md"))
	skillRepoFile(t, dir, "replacement", "replacement from git replace")
	replacement := trimSHA(gitCmd(t, dir, "hash-object", "-w", "replacement"))
	gitCmd(t, dir, "replace", original, replacement)
	files, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, sha, func(string) bool { return false })
	if err != nil || string(files["review/SKILL.md"]) != "trusted target" {
		t.Fatalf("replaced target object was used: files=%v err=%v", files, err)
	}
}

func TestRepositorySkillTreeRejectsAncestorSymlinksAndGitlinks(t *testing.T) {
	for _, ancestor := range []string{".claude", ".claude/skills"} {
		for _, mode := range []string{"symlink", "gitlink"} {
			t.Run(ancestor+"/"+mode, func(t *testing.T) {
				dir := initRepo(t)
				skillRepoFile(t, dir, "base", "base")
				base := commitSkillRepo(t, dir)
				if ancestor == ".claude/skills" {
					if err := os.Mkdir(filepath.Join(dir, ".claude"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "symlink" {
					if err := os.Symlink("/tmp", filepath.Join(dir, filepath.FromSlash(ancestor))); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, dir, "add", "-A")
				} else {
					gitCmd(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+base+","+ancestor)
				}
				gitCmd(t, dir, "commit", "-q", "-m", "unsafe ancestor")
				sha := trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))
				files, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, sha, func(string) bool { return false })
				if err == nil || files != nil {
					t.Fatalf("unsafe ancestor returned files=%v err=%v", files, err)
				}
			})
		}
	}
}

func TestRepositorySkillTreeDoesNotInspectIgnoredBlob(t *testing.T) {
	dir := initRepo(t)
	skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "allowed")
	skillRepoFile(t, dir, ".claude/skills/review/ignored.txt", "ignored content")
	sha := commitSkillRepo(t, dir)
	oid := trimSHA(gitCmd(t, dir, "rev-parse", sha+":.claude/skills/review/ignored.txt"))
	if err := os.Remove(filepath.Join(dir, ".git", "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitWrapper := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nfor arg in \"$@\"; do [ \"$arg\" = -l ] && exit 91; done\nexec " + strconv.Quote(gitPath) + " \"$@\"\n"
	if err := os.WriteFile(gitWrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := (&Git{Bin: gitWrapper}).ReadRepositorySkills(context.Background(), dir, sha, func(name string) bool {
		return name == ".claude/skills/review/ignored.txt"
	})
	if err != nil || len(files) != 1 || string(files["review/SKILL.md"]) != "allowed" {
		t.Fatalf("ignored blob influenced discovery: files=%v err=%v", files, err)
	}
}

func TestRepositorySkillTreeBoundsEntryCountAndTotalSize(t *testing.T) {
	for _, tt := range []struct {
		name  string
		count int
		size  int64
	}{
		{"entry count", maxRepositorySkillEntries + 1, 0},
		{"total size", maxRepositorySkillsBytes/maxRepositorySkillFile + 1, maxRepositorySkillFile},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := initRepo(t)
			skillDir := filepath.Join(dir, ".claude", "skills", "review")
			if err := os.MkdirAll(skillDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tt.count; i++ {
				file, err := os.Create(filepath.Join(skillDir, fmt.Sprintf("file-%05d", i)))
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(tt.size); err != nil {
					file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			sha := commitSkillRepo(t, dir)
			files, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, sha, func(string) bool { return false })
			if err == nil || files != nil {
				t.Fatalf("over-limit tree = %#v, %v", files, err)
			}
		})
	}
}

func TestRepositorySkillTreePreservesBinaryBlobBoundaries(t *testing.T) {
	dir := initRepo(t)
	skillRepoFile(t, dir, ".claude/skills/review/a", "a\n\x00b\n")
	skillRepoFile(t, dir, ".claude/skills/review/b", "b\nblob 9999\n")
	sha := commitSkillRepo(t, dir)
	files, err := (&Git{}).ReadRepositorySkills(context.Background(), dir, sha, func(string) bool { return false })
	if err != nil || string(files["review/a"]) != "a\n\x00b\n" || string(files["review/b"]) != "b\nblob 9999\n" {
		t.Fatalf("binary files = %#v, %v", files, err)
	}
	if cap(files["review/a"]) != len(files["review/a"]) {
		t.Fatal("one blob exposes another through its backing slice")
	}
}

func TestResolveCommitRefPinsRawCommitAndRejectsTagOrReplacement(t *testing.T) {
	dir := initRepo(t)
	skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "target")
	target := commitSkillRepo(t, dir)
	gitCmd(t, dir, "branch", "target", target)
	gitCmd(t, dir, "tag", "-a", "other", "-m", "tag", target)
	skillRepoFile(t, dir, ".claude/skills/review/SKILL.md", "head")
	head := commitSkillRepo(t, dir)
	gitCmd(t, dir, "replace", target, head)
	got, err := (&Git{}).ResolveCommitRef(context.Background(), dir, "refs/heads/target")
	if err != nil || got != target {
		t.Fatalf("target = %q, %v; want original %q", got, err, target)
	}
	if _, err := (&Git{}).ResolveCommitRef(context.Background(), dir, "refs/tags/other"); err == nil {
		t.Fatal("annotated tag was accepted as a raw commit ref")
	}
	for _, ref := range []string{"HEAD", "target", "--help", "refs/heads/../target"} {
		if _, err := (&Git{}).ResolveCommitRef(context.Background(), dir, ref); err == nil {
			t.Errorf("unsafe ref %q accepted", ref)
		}
	}
}

func TestMaterializeRepositorySkillsIsPrivateAndNeverMakesScriptsExecutable(t *testing.T) {
	parent := t.TempDir()
	files := map[string][]byte{
		"review/SKILL.md":         []byte("instructions"),
		"review/scripts/check.sh": []byte("#!/bin/sh\nexit 99\n"),
	}
	dir, cleanup, err := MaterializeRepositorySkills(context.Background(), parent, files)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if filepath.Dir(dir) != parent {
		t.Fatalf("snapshot is not isolated under parent: %q", dir)
	}
	for name, expected := range files {
		filename := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Lstat(filename)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o111 != 0 {
			t.Fatalf("unsafe snapshot file %q: %v %v", name, info, err)
		}
		got, err := os.ReadFile(filename)
		if err != nil || string(got) != string(expected) {
			t.Fatalf("snapshot file %q = %q, %v", name, got, err)
		}
	}
	files["review/SKILL.md"][0] = 'X'
	body, err := os.ReadFile(filepath.Join(dir, "review", "SKILL.md"))
	if err != nil || string(body) != "instructions" {
		t.Fatalf("snapshot was modified through input: %q %v", body, err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot not removed: %v", err)
	}
}

func TestMaterializeRepositorySkillsRejectsUnsafePathsAndCleansUp(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a/../escape", "a\\escape", ".git/config", "bad\nname", "a:stream"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir, cleanup, err := MaterializeRepositorySkills(context.Background(), parent, map[string][]byte{name: []byte("unsafe")})
			if err == nil || dir != "" || cleanup != nil {
				t.Fatalf("unsafe materialization succeeded: %q %v", dir, err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed materialization left files: %v %v", entries, err)
			}
		})
	}
	parent := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if dir, cleanup, err := MaterializeRepositorySkills(ctx, parent, map[string][]byte{"x": []byte("body")}); dir != "" || cleanup != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled materialization: %q %v", dir, err)
	}
}

func TestReadRepositorySkillConfigIsPinnedRegularAndBounded(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		link    bool
		wantErr bool
	}{
		{"regular", "ignore:\n  - secret/**\n", false, false},
		{"symlink", "/etc/passwd", true, true},
		{"oversize", strings.Repeat("x", (1<<20)+1), false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := initRepo(t)
			filename := filepath.Join(dir, ".gitlab-review.yml")
			if tt.link {
				if err := os.Symlink(tt.content, filename); err != nil {
					t.Fatal(err)
				}
			} else {
				skillRepoFile(t, dir, ".gitlab-review.yml", tt.content)
			}
			sha := commitSkillRepo(t, dir)
			if !tt.link {
				skillRepoFile(t, dir, ".gitlab-review.yml", "dirty worktree")
			}
			data, exists, err := (&Git{}).ReadRepositorySkillConfig(context.Background(), dir, sha)
			if tt.wantErr {
				if err == nil || data != nil {
					t.Fatalf("unsafe config = %q exists=%v err=%v", data, exists, err)
				}
			} else if err != nil || !exists || string(data) != tt.content {
				t.Fatalf("pinned config = %q exists=%v err=%v", data, exists, err)
			}
		})
	}
	dir := initRepo(t)
	skillRepoFile(t, dir, "README.md", "no config")
	sha := commitSkillRepo(t, dir)
	if data, exists, err := (&Git{}).ReadRepositorySkillConfig(context.Background(), dir, sha); err != nil || exists || data != nil {
		t.Fatalf("absent config = %q exists=%v err=%v", data, exists, err)
	}
}

func TestMaterializeRepositorySkillsRejectsPathAliases(t *testing.T) {
	for name, files := range map[string]map[string][]byte{
		"file-and-child":  {"skill": []byte("file"), "skill/SKILL.md": []byte("nested")},
		"duplicate-slash": {"skill//SKILL.md": []byte("alias")},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir, cleanup, err := MaterializeRepositorySkills(context.Background(), parent, files)
			if err == nil || dir != "" || cleanup != nil {
				t.Fatalf("alias materialization succeeded: %q %v", dir, err)
			}
			entries, readErr := os.ReadDir(parent)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("failed materialization left files: %v %v", entries, readErr)
			}
		})
	}
}
