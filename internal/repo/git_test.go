package repo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateRemoteURL(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:8080/group/repo.git",
		"https://gitlab.example.com/group/sub/repo.git",
	}
	for _, raw := range valid {
		if err := validateRemoteURL(raw); err != nil {
			t.Errorf("validateRemoteURL(%q) = %v, want nil", raw, err)
		}
	}

	invalid := []string{
		"",
		" file:///tmp/repo.git",
		"file:///tmp/repo.git",
		"ssh://git@example.com/group/repo.git",
		"git@example.com:group/repo.git",
		"https://user:secret@example.com/group/repo.git",
		"https://example.com/group/repo.git?token=x",
		"https://example.com/group/repo.git#fragment",
		"https:///group/repo.git",
		"https://example.com",
		"http://example.com/",
		"http://example.com/group/repo.git\\evil",
	}
	for _, raw := range invalid {
		if err := validateRemoteURL(raw); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("validateRemoteURL(%q) = %v, want ErrInvalidURL", raw, err)
		}
	}
}

func TestValidateRefspecRejectsInjection(t *testing.T) {
	valid := []string{
		"+refs/heads/main:refs/workspace/base",
		"refs/merge-requests/12/head:refs/mr/12",
	}
	for _, spec := range valid {
		if err := validateRefspec(spec); err != nil {
			t.Errorf("validateRefspec(%q) = %v, want nil", spec, err)
		}
	}
	invalid := []string{
		"",
		"--upload-pack=/bin/sh",
		"-c",
		"refs/heads/main",
		"refs/heads/*:refs/remotes/origin/*",
		"refs/heads/main:--evil",
		"HEAD:refs/workspace/base",
		"refs/heads/../secret:refs/workspace/base",
		"refs/heads/main:refs/workspace/base\n",
	}
	for _, spec := range invalid {
		if err := validateRefspec(spec); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("validateRefspec(%q) = %v, want ErrInvalidRef", spec, err)
		}
	}
}

func TestNormalizeRef(t *testing.T) {
	got, err := NormalizeRef("main")
	if err != nil || got != "refs/heads/main" {
		t.Fatalf("NormalizeRef(main) = %q, %v", got, err)
	}
	if _, err := NormalizeRef("-c"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("NormalizeRef(-c) = %v, want ErrInvalidRef", err)
	}
	if _, err := NormalizeRef("refs/heads/a..b"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("NormalizeRef(dot-dot) = %v, want ErrInvalidRef", err)
	}
	if _, err := NormalizeRef("HEAD"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("NormalizeRef(HEAD) = %v, want ErrInvalidRef", err)
	}
}

func TestFetchUsesWhitelistedEnvAndNoArgvToken(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	envFile := filepath.Join(dir, "env")
	g := &Git{
		Bin: fakeGit(t),
		BaseEnv: []string{
			"FAKE_ARGS=" + argsFile,
			"FAKE_ENV=" + envFile,
			"GIT_DIR=/evil",
			"GIT_CONFIG_GLOBAL=/evil",
		},
	}
	t.Setenv("GIT_DIR", "/evil-process")
	t.Setenv("HOME", "/evil-home")
	t.Setenv("GIT_CONFIG_COUNT", "9")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	token := "sekret-token-value"
	if err := g.Fetch(ctx, t.TempDir(), Auth{Token: token}, "+refs/heads/main:refs/workspace/base"); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), token) {
		t.Fatalf("token leaked into argv:\n%s", args)
	}
	if !strings.Contains(string(args), "http.followRedirects=false") {
		t.Errorf("redirect protection missing from argv:\n%s", args)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(env)
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("environment missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"GIT_DIR=/evil",
		"HOME=/evil-home",
		"GIT_CONFIG_GLOBAL=/evil",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("environment inherited %q", forbidden)
		}
	}
	if strings.Contains(text, token) && strings.Count(text, token) != 1 {
		t.Errorf("token must appear only inside the header value")
	}
	if !strings.Contains(text, "Authorization: Basic ") {
		t.Errorf("authorization header missing")
	}
}

func TestRemoteHEADReadsHeadWithoutArgvToken(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	envFile := filepath.Join(dir, "env")
	sha := strings.Repeat("a", 40)
	token := "sekret-token-value"
	g := &Git{
		Bin: fakeGit(t),
		BaseEnv: []string{
			"FAKE_ARGS=" + argsFile,
			"FAKE_ENV=" + envFile,
			"FAKE_STDOUT=" + sha + "\tHEAD",
		},
	}

	got, err := g.RemoteHEAD(context.Background(),
		"https://gitlab.example.com/group/repo.git", Auth{Token: token})
	if err != nil {
		t.Fatalf("RemoteHEAD() error = %v", err)
	}
	if got != sha {
		t.Fatalf("RemoteHEAD() = %q, want %q", got, sha)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(args)
	for _, want := range []string{"ls-remote", "--exit-code", "--", "HEAD", "https://gitlab.example.com/group/repo.git", "http.followRedirects=false"} {
		if !strings.Contains(text, want) {
			t.Errorf("argv missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, token) {
		t.Fatalf("token leaked into argv:\n%s", text)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "http.extraHeader") {
		t.Fatalf("token header missing from environment:\n%s", env)
	}
}

func TestRemoteHEADRejectsInvalidURLBeforeRunning(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	g := &Git{
		Bin:     fakeGit(t),
		BaseEnv: []string{"FAKE_ARGS=" + argsFile},
	}
	if _, err := g.RemoteHEAD(context.Background(), "ssh://git@example.com/group/repo.git", Auth{}); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("RemoteHEAD(ssh URL) = %v, want ErrInvalidURL", err)
	}
	if _, err := os.Stat(argsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git ran for an invalid URL: %v", err)
	}
}

func TestRemoteHEADRejectsMalformedOutput(t *testing.T) {
	for name, stdout := range map[string]string{
		"missing":   "",
		"not sha":   "not-a-sha\tHEAD",
		"other ref": strings.Repeat("a", 40) + "\trefs/heads/main",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			g := &Git{
				Bin: fakeGit(t),
				BaseEnv: []string{
					"FAKE_ARGS=" + filepath.Join(dir, "args"),
					"FAKE_ENV=" + filepath.Join(dir, "env"),
					"FAKE_STDOUT=" + stdout,
				},
			}
			if _, err := g.RemoteHEAD(context.Background(),
				"https://gitlab.example.com/group/repo.git", Auth{}); err == nil {
				t.Fatal("RemoteHEAD() = nil, want error")
			}
		})
	}
}

func TestRemoteHEADHonorsContextDeadline(t *testing.T) {
	dir := t.TempDir()
	g := &Git{
		Bin: fakeGit(t),
		BaseEnv: []string{
			"FAKE_ARGS=" + filepath.Join(dir, "args"),
			"FAKE_ENV=" + filepath.Join(dir, "env"),
			"FAKE_SLEEP=5",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := g.RemoteHEAD(ctx, "https://gitlab.example.com/group/repo.git", Auth{})
	if err == nil {
		t.Fatal("RemoteHEAD() = nil, want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RemoteHEAD() error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("RemoteHEAD() took %v, want bounded by ctx", elapsed)
	}
}

func TestFetchErrorDoesNotLeakStderr(t *testing.T) {
	dir := t.TempDir()
	secret := "sekret-token-value"
	g := &Git{
		Bin: fakeGit(t),
		BaseEnv: []string{
			"FAKE_ARGS=" + filepath.Join(dir, "args"),
			"FAKE_ENV=" + filepath.Join(dir, "env"),
			"FAKE_STDERR=Authorization: Basic " + secret,
			"FAKE_EXIT=7",
		},
	}
	err := g.Fetch(context.Background(), t.TempDir(), Auth{Token: secret}, "+refs/heads/main:refs/workspace/base")
	if err == nil {
		t.Fatal("Fetch() = nil, want error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "Authorization") {
		t.Fatalf("error leaked stderr: %v", err)
	}
	var ge *Error
	if !errors.As(err, &ge) || ge.ExitCode != 7 {
		t.Fatalf("error = %#v, want sanitized Error with exit 7", err)
	}
}

func TestEnsureMirrorRejectsFileURLAndUnknownDirectory(t *testing.T) {
	g := &Git{}
	dir := filepath.Join(t.TempDir(), "mirror.git")
	if err := g.EnsureMirror(context.Background(), dir, "file:///tmp/repo.git", Auth{}); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("EnsureMirror(file URL) = %v, want ErrInvalidURL", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mirror directory was created for an invalid URL")
	}

	unknown := filepath.Join(t.TempDir(), "unknown")
	if err := os.Mkdir(unknown, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unknown, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.EnsureMirror(context.Background(), unknown, "https://example.com/group/repo.git", Auth{}); !errors.Is(err, ErrNotMirror) {
		t.Fatalf("EnsureMirror(unknown) = %v, want ErrNotMirror", err)
	}
	if data, err := os.ReadFile(filepath.Join(unknown, "keep.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("unknown directory content changed: %q, %v", data, err)
	}
}

func TestWorktreeLifecycle(t *testing.T) {
	ctx := context.Background()
	source := initRepo(t)
	first := commitFile(t, source, "a.txt", "one\n", "first")
	second := commitFile(t, source, "b.txt", "two\n", "second")

	root := t.TempDir()
	mirror := filepath.Join(root, "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, "https://example.com/group/repo.git", Auth{}); err != nil {
		t.Fatalf("EnsureMirror() error = %v", err)
	}
	pushToMirror(t, source, mirror)

	sha, err := g.Resolve(ctx, mirror, "refs/heads/main")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if sha != second {
		t.Fatalf("Resolve() = %s, want %s", sha, second)
	}

	base := filepath.Join(root, "base")
	if err := g.AddWorktree(ctx, mirror, base, first); err != nil {
		t.Fatalf("AddWorktree() error = %v", err)
	}
	if info, err := os.Stat(base); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("worktree permissions = %v, %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(base, "a.txt")); err != nil || string(data) != "one\n" {
		t.Fatalf("worktree content = %q, %v", data, err)
	}
	if err := g.Checkout(ctx, base, second); err != nil {
		t.Fatalf("Checkout() error = %v", err)
	}
	if sha, err := g.Resolve(ctx, base, "HEAD"); err != nil || sha != second {
		t.Fatalf("Resolve(HEAD) = %q, %v", sha, err)
	}
	files, err := g.ChangedFiles(ctx, base, first, second)
	if err != nil {
		t.Fatalf("ChangedFiles() error = %v", err)
	}
	if len(files) != 1 || files[0] != "b.txt" {
		t.Fatalf("ChangedFiles() = %v, want [b.txt]", files)
	}
	if err := g.RemoveWorktree(ctx, mirror, base); err != nil {
		t.Fatalf("RemoveWorktree() error = %v", err)
	}
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still exists after removal")
	}
	if err := g.RemoveWorktree(ctx, mirror, base); err != nil {
		t.Fatalf("RemoveWorktree(missing) = %v, want nil", err)
	}
}

func TestEnsureMirrorUpdatesOriginWithoutReclone(t *testing.T) {
	ctx := context.Background()
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, "https://example.com/one/repo.git", Auth{}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(mirror, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.EnsureMirror(ctx, mirror, "https://example.com/two/repo.git", Auth{}); err != nil {
		t.Fatalf("second EnsureMirror() error = %v", err)
	}
	out, err := exec.Command("git", "--git-dir="+mirror, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "https://example.com/two/repo.git" {
		t.Fatalf("origin = %q", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("existing mirror was recreated: %v", err)
	}
}

func TestFetchOverDumbHTTP(t *testing.T) {
	ctx := context.Background()
	source := initRepo(t)
	sha := commitFile(t, source, "a.txt", "one\n", "first")
	serverRoot := t.TempDir()
	bare := filepath.Join(serverRoot, "repo.git")
	gitCmd(t, "", "clone", "--bare", source, bare)
	gitCmd(t, bare, "update-server-info")

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		http.FileServer(http.Dir(serverRoot)).ServeHTTP(w, r)
	}))
	defer server.Close()

	mirror := filepath.Join(t.TempDir(), "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, server.URL+"/repo.git", Auth{Token: "token-123"}); err != nil {
		t.Fatalf("EnsureMirror() error = %v", err)
	}
	if err := g.Fetch(ctx, mirror, Auth{Token: "token-123"}, "+refs/heads/main:refs/workspace/base"); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	got, err := g.Resolve(ctx, mirror, "refs/workspace/base")
	if err != nil || got != sha {
		t.Fatalf("Resolve() = %q, %v, want %q", got, err, sha)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
}

func TestFetchDoesNotFollowRedirectWithCredentials(t *testing.T) {
	ctx := context.Background()
	var targetHit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/repo.git/info/refs", http.StatusFound)
	}))
	defer redirect.Close()

	mirror := filepath.Join(t.TempDir(), "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, redirect.URL+"/repo.git", Auth{}); err != nil {
		t.Fatalf("EnsureMirror() error = %v", err)
	}
	err := g.Fetch(ctx, mirror, Auth{Token: "token-123"}, "+refs/heads/main:refs/workspace/base")
	if err == nil {
		t.Fatal("Fetch() = nil, want redirect failure")
	}
	if targetHit {
		t.Fatal("redirect target was contacted")
	}
}

func TestCanceledCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &Git{}
	if err := g.Fetch(ctx, t.TempDir(), Auth{}, "+refs/heads/main:refs/workspace/base"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch(canceled) = %v, want context.Canceled", err)
	}
}

func fakeGit(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-git.sh")
	script := `#!/bin/sh
: > "$FAKE_ARGS"
for arg in "$@"; do printf '%s\n' "$arg" >> "$FAKE_ARGS"; done
env > "$FAKE_ENV"
if [ -n "$FAKE_SLEEP" ]; then sleep "$FAKE_SLEEP"; fi
if [ -n "$FAKE_STDOUT" ]; then printf '%s\n' "$FAKE_STDOUT"; fi
if [ -n "$FAKE_STDERR" ]; then printf '%s\n' "$FAKE_STDERR" >&2; fi
exit "${FAKE_EXIT:-0}"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q", "-b", "main")
	gitCmd(t, dir, "config", "user.email", "test@example.com")
	gitCmd(t, dir, "config", "user.name", "Test")
	return dir
}

func commitFile(t *testing.T, dir, name, body, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", name)
	gitCmd(t, dir, "commit", "-q", "-m", message)
	out := gitCmd(t, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out)
}

func pushToMirror(t *testing.T, source, mirror string) {
	t.Helper()
	gitCmd(t, source, "remote", "add", "mirror", mirror)
	gitCmd(t, source, "push", "-q", "mirror", "refs/heads/main:refs/heads/main")
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = fixtureEnv()
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func fixtureEnv() []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
	)
}

func TestRemoveWorktreePrunesMissingRegistration(t *testing.T) {
	ctx := context.Background()
	source := initRepo(t)
	sha := commitFile(t, source, "a.txt", "one\n", "first")
	root := t.TempDir()
	mirror := filepath.Join(root, "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, "https://example.com/group/repo.git", Auth{}); err != nil {
		t.Fatal(err)
	}
	pushToMirror(t, source, mirror)

	dir := filepath.Join(root, "run")
	if err := g.AddWorktree(ctx, mirror, dir, sha); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveWorktree(ctx, mirror, dir); err != nil {
		t.Fatalf("RemoveWorktree(missing directory) = %v", err)
	}
	if err := g.AddWorktree(ctx, mirror, dir, sha); err != nil {
		t.Fatalf("AddWorktree() after prune = %v", err)
	}
}

func TestRemoveWorktreeAcceptsMissingMirror(t *testing.T) {
	ctx := context.Background()
	source := initRepo(t)
	sha := commitFile(t, source, "a.txt", "one\n", "first")
	root := t.TempDir()
	mirror := filepath.Join(root, "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, "https://example.com/group/repo.git", Auth{}); err != nil {
		t.Fatal(err)
	}
	pushToMirror(t, source, mirror)
	dir := filepath.Join(root, "run")
	if err := g.AddWorktree(ctx, mirror, dir, sha); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(mirror); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveWorktree(ctx, mirror, dir); err != nil {
		t.Fatalf("RemoveWorktree(missing mirror) = %v, want nil", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("RemoveWorktree touched the caller-owned directory: %v", err)
	}
}

func TestRemoveWorktreeMissingMirrorHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	dir := filepath.Join(root, "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	g := &Git{}
	if err := g.RemoveWorktree(ctx, filepath.Join(root, "missing.git"), dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("RemoveWorktree(cancelled) = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("RemoveWorktree touched the directory after cancellation: %v", err)
	}
}

func TestRemoveWorktreeRejectsUnsafeMirror(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file.git")
	if err := os.WriteFile(file, []byte("not a mirror"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.git")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	g := &Git{}
	for name, mirror := range map[string]string{"file": file, "symlink": link} {
		t.Run(name, func(t *testing.T) {
			if err := g.RemoveWorktree(context.Background(), mirror, dir); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("RemoveWorktree(%s mirror) = %v, want ErrUnsafePath", name, err)
			}
			if _, err := os.Lstat(dir); err != nil {
				t.Fatalf("RemoveWorktree touched the directory: %v", err)
			}
		})
	}
}

func TestEnsureMirrorRejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(dir, "mirror.git")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	g := &Git{}
	if err := g.EnsureMirror(context.Background(), link, "https://example.com/group/repo.git", Auth{}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("EnsureMirror(symlink) = %v, want ErrUnsafePath", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("symlink target changed: %v, %v", entries, err)
	}
}

func TestAddWorktreeRejectsSymlinkTargetAndMissingMirror(t *testing.T) {
	g := &Git{}
	root := t.TempDir()
	link := filepath.Join(root, "base")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := g.AddWorktree(context.Background(), root, link, strings.Repeat("a", 40)); !errors.Is(err, ErrUnsafePath) && !errors.Is(err, ErrNotMirror) {
		t.Fatalf("AddWorktree(symlink target) = %v, want unsafe path or invalid mirror", err)
	}

	missing := filepath.Join(root, "missing.git")
	if err := g.Fetch(context.Background(), missing, Auth{}, "+refs/heads/main:refs/workspace/base"); err == nil {
		t.Fatal("Fetch(missing mirror) = nil, want error")
	}
	if output, err := g.Resolve(context.Background(), missing, "HEAD"); err == nil {
		t.Fatalf("Resolve(missing) = %q, want error", output)
	}
}

func TestChangedFilesRenameShowsBothPaths(t *testing.T) {
	ctx := context.Background()
	source := initRepo(t)
	first := commitFile(t, source, "old.txt", "body\n", "first")
	gitCmd(t, source, "mv", "old.txt", "new.txt")
	gitCmd(t, source, "commit", "-q", "-m", "rename")
	second := strings.TrimSpace(gitCmd(t, source, "rev-parse", "HEAD"))

	root := t.TempDir()
	mirror := filepath.Join(root, "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, "https://example.com/group/repo.git", Auth{}); err != nil {
		t.Fatal(err)
	}
	pushToMirror(t, source, mirror)
	worktree := filepath.Join(root, "worktree")
	if err := g.AddWorktree(ctx, mirror, worktree, second); err != nil {
		t.Fatal(err)
	}
	files, err := g.ChangedFiles(ctx, worktree, first, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != "new.txt" || files[1] != "old.txt" {
		t.Fatalf("ChangedFiles() = %v, want [new.txt old.txt]", files)
	}
}

func TestCheckoutAndResolveRejectInvalidInput(t *testing.T) {
	dir := t.TempDir()
	g := &Git{}
	if err := g.Checkout(context.Background(), dir, "main"); !errors.Is(err, ErrInvalidSHA) {
		t.Fatalf("Checkout(bad sha) = %v, want ErrInvalidSHA", err)
	}
	if _, err := g.Resolve(context.Background(), dir, "-c"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Resolve(-c) = %v, want ErrInvalidRef", err)
	}
}

func TestGlobalArgsDisableHooksAndFilters(t *testing.T) {
	args := strings.Join(globalArgs(), " ")
	for _, want := range []string{
		"core.hooksPath=",
		"protocol.file.allow=never",
		"submodule.recurse=false",
		"fetch.recurseSubmodules=false",
		"credential.helper=",
		"filter.lfs.required=false",
		"http.followRedirects=false",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("globalArgs() missing %q", want)
		}
	}
}
