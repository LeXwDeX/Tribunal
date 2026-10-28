package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSandboxOptionalRootAliases(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "one", "skill"),
		filepath.Join(dir, "two", "skill"),
		filepath.Join(dir, "three", "skill-2"),
		filepath.Join(dir, "four", "head"),
		filepath.Join(dir, "five", "base"),
		filepath.Join(dir, "six", "skill-4"),
	}
	head, base := filepath.Join(dir, "checkout-head"), filepath.Join(dir, "checkout-base")
	for i, path := range append([]string{head, base}, paths...) {
		writeFile(t, filepath.Join(path, "marker"), string(rune('A'+i)))
	}
	sb, err := NewSandbox(head, base, paths...)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	want := []string{"base", "head", "skill", "skill-2", "skill-3", "skill-4", "skill-5", "skill-6"}
	if got := sb.RootNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("roots = %v, want %v", got, want)
	}
	again, err := NewSandbox(head, base, paths...)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.RootNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("repeated roots = %v, want %v", got, want)
	}
	read := toolByName(t, BuiltinTools(sb), "read_file")
	for root, marker := range map[string]string{"head": "A", "base": "B", "skill": "C", "skill-3": "D", "skill-2": "E", "skill-5": "F", "skill-6": "G", "skill-4": "H"} {
		res, err := invoke(t, read, `{"root":"`+root+`","path":"marker"}`)
		if err != nil || strings.TrimSpace(res.Content) != "1\t"+marker {
			t.Errorf("root %s: %q, %v; want %s", root, res.Content, err, marker)
		}
	}
	for _, path := range []string{"../marker", "/marker", ".git/config", "escape"} {
		if path == "escape" {
			if err := os.Symlink(filepath.Join(base, "marker"), filepath.Join(paths[1], path)); err != nil {
				t.Fatal(err)
			}
		}
		_, err := invoke(t, read, `{"root":"skill-3","path":"`+path+`"}`)
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("path %q: %v", path, err)
		}
	}
}

func TestSandboxOptionalRootFallbackAndUnsafeRoots(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "not a valid name")
	head := filepath.Join(dir, "head")
	normal := filepath.Join(dir, "normal")
	for _, path := range []string{invalid, head, normal} {
		writeFile(t, filepath.Join(path, "marker"), "ok")
	}
	sb, err := NewSandbox("", "", invalid, head, normal)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if got, want := sb.RootNames(), []string{"normal", "skill-1", "skill-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roots = %v, want %v", got, want)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(normal, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, "relative"} {
		if _, err := NewSandbox("", "", normal, path); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("optional root %q: %v", path, err)
		}
	}
}

func TestBuiltinDefinitionsExposeTheirSandboxRoots(t *testing.T) {
	dir := t.TempDir()
	head := filepath.Join(dir, "checkout")
	first := filepath.Join(dir, "first", "skill")
	second := filepath.Join(dir, "second", "skill")
	other := filepath.Join(dir, "other")
	for _, path := range []string{head, first, second, other} {
		writeFile(t, filepath.Join(path, "marker"), "ok")
	}
	writeFile(t, filepath.Join(head, "marker"), "head marker")
	one, err := NewSandbox(head, "", first, second)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := NewSandbox("", "", other)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	toolsOne, toolsTwo := BuiltinTools(one), BuiltinTools(two)
	for i, tool := range toolsOne {
		def := tool.Def()
		if !strings.Contains(def.Description, "Available roots: head, skill, skill-2. Default root: head.") {
			t.Errorf("%s: %q", def.Name, def.Description)
		}
		otherDef := toolsTwo[i].Def()
		if !strings.Contains(otherDef.Description, "Available roots: other. Default root: other.") || strings.Contains(otherDef.Description, "skill-2") {
			t.Errorf("%s other sandbox: %q", otherDef.Name, otherDef.Description)
		}
		if strings.Contains(def.Description, dir) || strings.Contains(otherDef.Description, dir) {
			t.Errorf("%s exposes a host path", def.Name)
		}
		def.InputSchema[0] = 'x'
		if !json.Valid(tool.Def().InputSchema) || !json.Valid(toolsTwo[i].Def().InputSchema) {
			t.Errorf("%s shares mutable schema", def.Name)
		}
	}
	read := toolByName(t, toolsOne, "read_file")
	for _, input := range []string{`{"path":"marker"}`, `{"root":"","path":"marker"}`} {
		res, err := invoke(t, read, input)
		if err != nil || !strings.Contains(res.Content, "head marker") {
			t.Errorf("default root input %s: %q, %v", input, res.Content, err)
		}
	}
}
