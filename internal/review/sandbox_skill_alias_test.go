package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/skill"
)

func TestManagedSkillRootsThroughSandbox(t *testing.T) {
	dir := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var sources []string
	for i, name := range []string{"alpha", "beta"} {
		source := filepath.Join(dir, "project", string(rune('1'+i)))
		sources = append(sources, source)
		write(filepath.Join(source, "skill", "SKILL.md"), "---\nname: "+name+"\ndescription: test\n---\nbody "+name+"\n")
		write(filepath.Join(source, "skill", "resources", name+".txt"), "marker "+name)
	}
	head, base := filepath.Join(dir, "head"), filepath.Join(dir, "base")
	write(filepath.Join(head, "marker"), "head marker")
	write(filepath.Join(base, "marker"), "base marker")
	loader, err := skill.New(skill.Options{PlatformDirs: sources})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := loader.Bind([]string{"beta", "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{deps: ExecutorDeps{Dossier: &Dossier{HeadDir: head, BaseDir: base, IgnorePolicy: &IgnorePolicy{Version: 1, Patterns: []string{}}}}}
	sb, err := e.sandboxFor(binding.Roots())
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	var read agent.Tool
	for _, tool := range agent.BuiltinTools(sb) {
		if tool.Def().Name == "read_file" {
			read = tool
		}
	}
	if read == nil {
		t.Fatal("read_file missing")
	}
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(read.Def().InputSchema, &schema); err != nil || schema.Properties["root"].Type != "string" {
		t.Fatalf("read_file root schema: %s, %v", read.Def().InputSchema, err)
	}
	description := read.Def().Description
	_, rootsText, found := strings.Cut(description, "Available roots: ")
	if !found {
		t.Fatalf("root names absent from definition: %q", description)
	}
	rootsText, _, found = strings.Cut(rootsText, ". Default root: ")
	if !found {
		t.Fatalf("default root absent from definition: %q", description)
	}
	identified := make(map[string]string)
	for _, root := range strings.Split(rootsText, ", ") {
		input, err := json.Marshal(map[string]string{"root": root, "path": "SKILL.md"})
		if err != nil {
			t.Fatal(err)
		}
		identity, err := read.Call(context.Background(), input)
		if err != nil {
			continue
		} // head and base need not contain a skill.
		for _, name := range []string{"alpha", "beta"} {
			if strings.Contains(identity.Content, "name: "+name) {
				identified[name] = root
			}
		}
	}
	for _, name := range []string{"alpha", "beta"} {
		root := identified[name]
		if root == "" {
			t.Fatalf("no root found for %s in %q", name, description)
		}
		loaded, err := binding.Tool().Call(context.Background(), json.RawMessage(`{"name":"`+name+`"}`))
		if err != nil || !strings.Contains(loaded.Content, "resources/"+name+".txt") {
			t.Fatalf("load %s: %q %v", name, loaded.Content, err)
		}
		input, err := json.Marshal(map[string]string{"root": root, "path": "resources/" + name + ".txt"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := read.Call(context.Background(), input)
		if err != nil || !strings.Contains(result.Content, "marker "+name) {
			t.Fatalf("read %s: %q %v", name, result.Content, err)
		}
	}
}
