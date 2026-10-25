package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testModule struct {
	Path    string `json:"Path"`
	Version string `json:"Version,omitempty"`
	Dir     string `json:"Dir,omitempty"`
	Main    bool   `json:"Main,omitempty"`
}

type testPackage struct {
	ImportPath string      `json:"ImportPath"`
	Module     *testModule `json:"Module,omitempty"`
}

type testLockPackage struct {
	Version   string `json:"version"`
	Resolved  string `json:"resolved,omitempty"`
	Integrity string `json:"integrity,omitempty"`
	Dev       bool   `json:"dev,omitempty"`
}

func TestBuildIncludesRequiredTexts(t *testing.T) {
	root, list := newFixture(t)

	content, err := build(root, list)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.HasPrefix(text, marker) {
		t.Fatalf("bundle does not start with generated marker:\n%s", text[:min(120, len(text))])
	}
	for _, want := range []string{
		"Go standard library",
		"example.com/dep", "v1.0.0", "dep notice text",
		"modernc.org/sqlite", "LICENSE-3RD-PARTY.md", "LICENSE-SQLITE",
		"react", "19.1.1", "react license text",
		"@scope/pkg", "2.0.0", "@scope/pkg license text",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bundle is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "dev-only") {
		t.Fatal("bundle contains a dev-only package")
	}
}

func TestBuildRejectsRuntimePackageWithoutIntegrity(t *testing.T) {
	root, list := newFixture(t)
	writeLock(t, root, map[string]testLockPackage{
		"":                    {Version: "0.0.0"},
		"node_modules/react":  {Version: "19.1.1", Resolved: "https://registry.npmjs.org/react/-/react-19.1.1.tgz"},
		"node_modules/cookie": {Version: "1.1.1", Resolved: "https://registry.npmjs.org/cookie/-/cookie-1.1.1.tgz", Integrity: "sha512-cookie"},
	})

	_, err := build(root, list)
	if err == nil {
		t.Fatal("build succeeded, want error")
	}
	if !strings.Contains(err.Error(), "react") || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("error = %q, want react and integrity", err)
	}
}

func TestBuildRejectsMissingGoLicense(t *testing.T) {
	root, list := newFixture(t)
	depDir := filepath.Join(root, "mods", "example.com", "dep")
	for _, name := range []string{"LICENSE", "NOTICE"} {
		if err := os.Remove(filepath.Join(depDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	_, err := build(root, list)
	if err == nil {
		t.Fatal("build succeeded, want error")
	}
	if !strings.Contains(err.Error(), "example.com/dep") || !strings.Contains(err.Error(), "no LICENSE or COPYING") {
		t.Fatalf("error = %q", err)
	}
}

func TestBuildRejectsNoticeOnlyGoModule(t *testing.T) {
	root, list := newFixture(t)
	if err := os.Remove(filepath.Join(root, "mods", "example.com", "dep", "LICENSE")); err != nil {
		t.Fatal(err)
	}

	_, err := build(root, list)
	if err == nil {
		t.Fatal("build succeeded, want error")
	}
	if !strings.Contains(err.Error(), "example.com/dep") || !strings.Contains(err.Error(), "LICENSE or COPYING") {
		t.Fatalf("error = %q", err)
	}
}

func TestBuildRejectsMissingNpmLicense(t *testing.T) {
	root, list := newFixture(t)
	reactDir := filepath.Join(root, "web", "node_modules", "react")
	if err := os.Remove(filepath.Join(reactDir, "LICENSE")); err != nil {
		t.Fatal(err)
	}

	_, err := build(root, list)
	if err == nil {
		t.Fatal("build succeeded, want error")
	}
	if !strings.Contains(err.Error(), "react") || !strings.Contains(err.Error(), "no LICENSE or COPYING") {
		t.Fatalf("error = %q", err)
	}
}

func TestBuildRejectsNoticeOnlyNpmPackage(t *testing.T) {
	root, list := newFixture(t)
	reactDir := filepath.Join(root, "web", "node_modules", "react")
	if err := os.Remove(filepath.Join(reactDir, "LICENSE")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, reactDir, "NOTICE", "notice only\n")

	_, err := build(root, list)
	if err == nil {
		t.Fatal("build succeeded, want error")
	}
	if !strings.Contains(err.Error(), "react") || !strings.Contains(err.Error(), "LICENSE or COPYING") {
		t.Fatalf("error = %q", err)
	}
}

func TestBuildRejectsInstalledNpmVersionMismatch(t *testing.T) {
	root, list := newFixture(t)
	writePackageJSON(t, root, "react", "0.0.0")

	_, err := build(root, list)
	if err == nil {
		t.Fatal("build succeeded, want error")
	}
	if !strings.Contains(err.Error(), "react") || !strings.Contains(err.Error(), "does not match lock") {
		t.Fatalf("error = %q", err)
	}
}

func TestBuildRejectsNpmLockPathEscape(t *testing.T) {
	root, list := newFixture(t)
	writeLock(t, root, map[string]testLockPackage{
		"":                                {Version: "0.0.0"},
		"node_modules/react":              {Version: "19.1.1", Resolved: "https://registry.npmjs.org/react/-/react-19.1.1.tgz", Integrity: "sha512-react"},
		"node_modules/../../sentinel-pkg": {Version: "1.0.0", Resolved: "https://registry.npmjs.org/sentinel/-/sentinel-1.0.0.tgz", Integrity: "sha512-sentinel"},
	})

	_, err := build(root, list)
	if err == nil {
		t.Fatal("build succeeded, want error")
	}
	if !strings.Contains(err.Error(), "unsafe") || !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("error = %q, want unsafe path rejection", err)
	}
}

func TestBuildRejectsSymlinkLicenseFile(t *testing.T) {
	root, list := newFixture(t)
	depDir := filepath.Join(root, "mods", "example.com", "dep")
	licensePath := filepath.Join(depDir, "LICENSE")
	if err := os.Remove(licensePath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-license")
	if err := os.WriteFile(outside, []byte("outside license\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, licensePath); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	_, err := build(root, list)
	if err == nil {
		t.Fatal("build succeeded, want error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %q, want symlink rejection", err)
	}
}

func TestWriteBundleIsDeterministicAndCheckable(t *testing.T) {
	root, list := newFixture(t)
	first, err := build(root, list)
	if err != nil {
		t.Fatal(err)
	}
	second, err := build(root, list)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two builds differ")
	}
	if err := writeBundle(root, first); err != nil {
		t.Fatal(err)
	}
	if err := checkBundle(root, first); err != nil {
		t.Fatalf("checkBundle after write: %v", err)
	}
	info, err := os.Stat(filepath.Dir(noticesPath(root)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("bundle directory mode = %o, want 755", info.Mode().Perm())
	}

	path := noticesPath(root)
	altered := append(append([]byte{}, first...), []byte("changed\n")...)
	if err := os.WriteFile(path, altered, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkBundle(root, first); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("checkBundle = %v, want content difference", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := checkBundle(root, first); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("checkBundle = %v, want missing file", err)
	}
}

func TestRunWritesAndChecksBundle(t *testing.T) {
	root, list := newFixture(t)
	args := []string{"-root", root}
	code, stdout, stderr := runWithList(args, list)
	if code != 0 {
		t.Fatalf("run = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "wrote") {
		t.Fatalf("stdout = %q", stdout)
	}

	code, stdout, stderr = runWithList(append(args, "-check"), list)
	if code != 0 {
		t.Fatalf("check = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "up to date") {
		t.Fatalf("stdout = %q", stdout)
	}

	if err := os.Remove(noticesPath(root)); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runWithList(append(args, "-check"), list)
	if code != 1 || !strings.Contains(stderr, "missing") {
		t.Fatalf("check = %d, stderr = %q, want missing file failure", code, stderr)
	}
}

func TestRunRejectsOutFlagAndPreservesSentinel(t *testing.T) {
	root, list := newFixture(t)
	sentinelDir := t.TempDir()
	sentinel := filepath.Join(sentinelDir, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, _ := runWithList([]string{"-root", root, "-out", sentinelDir}, list)
	if code != 2 {
		t.Fatalf("run = %d, want flag parse error 2", code)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me\n" {
		t.Fatalf("sentinel changed to %q", data)
	}
}

func TestRunRejectsRootWithoutModuleAndPreservesSentinel(t *testing.T) {
	_, list := newFixture(t)
	root := t.TempDir()
	sentinel := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runWithList([]string{"-root", root}, list)
	if code != 1 {
		t.Fatalf("run = %d, stderr = %s, want invalid root failure", code, stderr)
	}
	if !strings.Contains(stderr, "go.mod") {
		t.Fatalf("stderr = %q, want go.mod error", stderr)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me\n" {
		t.Fatalf("sentinel changed to %q", data)
	}
}

func TestBuildRejectsSymlinkedRoot(t *testing.T) {
	realRoot := t.TempDir()
	writeFile(t, realRoot, "go.mod", "module example.com/main\n")
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	_, err := build(link, func(string) ([]byte, error) { return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("build = %v, want symlink root rejection", err)
	}
}

func TestWriteBundleRejectsFilesystemRoot(t *testing.T) {
	err := writeBundle(string(filepath.Separator), []byte(marker))
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("writeBundle = %v, want filesystem root rejection", err)
	}
}

func TestWriteBundleRejectsSymlinkedOutputDir(t *testing.T) {
	root, list := newFixture(t)
	content, err := build(root, list)
	if err != nil {
		t.Fatal(err)
	}
	sentinelDir := t.TempDir()
	sentinel := filepath.Join(sentinelDir, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinelDir, filepath.Join(root, "licenses")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	if err := writeBundle(root, content); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("writeBundle = %v, want symlink rejection", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me\n" {
		t.Fatalf("sentinel changed to %q", data)
	}
}

func TestWriteBundleRejectsUnknownContents(t *testing.T) {
	root, list := newFixture(t)
	content, err := build(root, list)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "licenses")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeBundle(root, content); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("writeBundle = %v, want unknown-content rejection", err)
	}
	data, err := os.ReadFile(keep)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me\n" {
		t.Fatalf("unknown file changed to %q", data)
	}
}

func TestWriteBundleRejectsUnownedNotices(t *testing.T) {
	root, list := newFixture(t)
	content, err := build(root, list)
	if err != nil {
		t.Fatal(err)
	}
	path := noticesPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeBundle(root, content); err == nil || !strings.Contains(err.Error(), "not generated") {
		t.Fatalf("writeBundle = %v, want ownership rejection", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "user content\n" {
		t.Fatalf("unowned file changed to %q", data)
	}
}

func TestWriteBundleKeepsPriorBundleOnWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	root, list := newFixture(t)
	content, err := build(root, list)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBundle(root, content); err != nil {
		t.Fatal(err)
	}
	path := noticesPath(root)
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	changed := append(append([]byte{}, content...), []byte("changed\n")...)
	if err := writeBundle(root, changed); err == nil {
		t.Fatal("writeBundle succeeded in a read-only directory")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prior, after) {
		t.Fatal("failed write changed the prior bundle")
	}
}

func TestFailedBuildLeavesPriorBundle(t *testing.T) {
	root, list := newFixture(t)
	content, err := build(root, list)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBundle(root, content); err != nil {
		t.Fatal(err)
	}
	path := noticesPath(root)
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	writePackageJSON(t, root, "react", "0.0.0")
	if _, err := build(root, list); err == nil {
		t.Fatal("build succeeded after the installed version changed")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prior, after) {
		t.Fatal("failed build changed the prior bundle")
	}
}

func TestLicenseFilesRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-license")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "LICENSE")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	_, err := licenseFiles(dir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("licenseFiles = %v, want symlink rejection", err)
	}
}

func TestHasActualLicense(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  bool
	}{
		{name: "notice only", names: []string{"NOTICE"}, want: false},
		{name: "license", names: []string{"LICENSE"}, want: true},
		{name: "license and notice", names: []string{"LICENSE", "NOTICE"}, want: true},
		{name: "copying", names: []string{"COPYING"}, want: true},
		{name: "bundled license", names: []string{"LICENSE-SQLITE"}, want: true},
		{name: "source file", names: []string{"license.go"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasActualLicense(tc.names); got != tc.want {
				t.Fatalf("hasActualLicense(%v) = %v, want %v", tc.names, got, tc.want)
			}
		})
	}
}

func newFixture(t *testing.T) (string, func(string) ([]byte, error)) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/main\n\ngo 1.26.0\n")

	depDir := filepath.Join(root, "mods", "example.com", "dep")
	writeFile(t, depDir, "LICENSE", "dep license text\n")
	writeFile(t, depDir, "NOTICE", "dep notice text\n")
	moderncDir := filepath.Join(root, "mods", "modernc.org", "sqlite")
	writeFile(t, moderncDir, "LICENSE", "sqlite license text\n")
	writeFile(t, moderncDir, "LICENSE-SQLITE", "sqlite public domain text\n")
	writeFile(t, moderncDir, "LICENSE-3RD-PARTY.md", "third party notices\n")

	writeLock(t, root, map[string]testLockPackage{
		"":                        {Version: "0.0.0"},
		"node_modules/react":      {Version: "19.1.1", Resolved: "https://registry.npmjs.org/react/-/react-19.1.1.tgz", Integrity: "sha512-react"},
		"node_modules/@scope/pkg": {Version: "2.0.0", Resolved: "https://registry.npmjs.org/@scope/pkg/-/pkg-2.0.0.tgz", Integrity: "sha512-scope"},
		"node_modules/dev-only":   {Version: "1.0.0", Dev: true},
	})
	writeNpmPackage(t, root, "react", "19.1.1")
	writeNpmPackage(t, root, "@scope/pkg", "2.0.0")

	data := encodePackages(
		testPackage{
			ImportPath: "example.com/main",
			Module:     &testModule{Path: "example.com/main", Main: true, Dir: root},
		},
		testPackage{
			ImportPath: "example.com/dep",
			Module:     &testModule{Path: "example.com/dep", Version: "v1.0.0", Dir: depDir},
		},
		testPackage{
			ImportPath: "example.com/dep/sub",
			Module:     &testModule{Path: "example.com/dep", Version: "v1.0.0", Dir: depDir},
		},
		testPackage{
			ImportPath: "modernc.org/sqlite",
			Module:     &testModule{Path: "modernc.org/sqlite", Version: "v1.61.0", Dir: moderncDir},
		},
	)
	return root, func(string) ([]byte, error) { return data, nil }
}

func writeNpmPackage(t *testing.T, root, name, version string) {
	t.Helper()
	dir := filepath.Join(root, "web", "node_modules", filepath.FromSlash(name))
	writeFile(t, dir, "LICENSE", name+" license text\n")
	writePackageJSON(t, root, name, version)
}

func writePackageJSON(t *testing.T, root, name, version string) {
	t.Helper()
	dir := filepath.Join(root, "web", "node_modules", filepath.FromSlash(name))
	writeFile(t, dir, "package.json", `{"name":"`+name+`","version":"`+version+`"}`+"\n")
}

func writeLock(t *testing.T, root string, packages map[string]testLockPackage) {
	t.Helper()
	data, err := json.MarshalIndent(map[string]any{
		"name":            "fixture",
		"version":         "0.0.0",
		"lockfileVersion": 3,
		"packages":        packages,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "web"), "package-lock.json", string(data)+"\n")
}

func encodePackages(packages ...testPackage) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, pkg := range packages {
		if err := enc.Encode(pkg); err != nil {
			panic(err)
		}
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runWithList(args []string, list func(string) ([]byte, error)) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, list)
	return code, stdout.String(), stderr.String()
}
