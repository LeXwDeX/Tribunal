package builtin_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/skill/builtin"
)

const upstreamCommit = "3da8c01116264675eeefbd6e9d59c734de9095cc"

func TestNamesListsBundledSkills(t *testing.T) {
	if got, want := builtin.Names(), []string{"code-review"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if !builtin.Has("code-review") || builtin.Has("missing") {
		t.Fatal("Has() does not match Names()")
	}
	if _, err := builtin.Sub("missing"); err == nil {
		t.Fatal("Sub(missing) succeeded")
	}
}

// loadBody materializes the bundled skill the way the platform does and
// returns what load_skill hands to the model.
func loadBody(t *testing.T, name string) string {
	t.Helper()
	sub, err := builtin.Sub(name)
	if err != nil {
		t.Fatalf("Sub(%q): %v", name, err)
	}
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, name), sub); err != nil {
		t.Fatalf("CopyFS: %v", err)
	}
	loader, err := skill.New(skill.Options{PlatformDirs: []string{root}})
	if err != nil {
		t.Fatalf("skill.New: %v", err)
	}
	if warnings := loader.Warnings(); len(warnings) != 0 {
		t.Fatalf("loader warnings: %v", warnings)
	}
	binding, err := loader.Bind([]string{name})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	raw, _ := json.Marshal(map[string]string{"name": name})
	result, err := binding.Tool().Call(context.Background(), raw)
	if err != nil {
		t.Fatalf("load_skill: %v", err)
	}
	if result.Truncated {
		t.Fatal("load_skill output was truncated at the default limit")
	}
	return result.Content
}

func TestCodeReviewSkillIsNonInteractive(t *testing.T) {
	body := loadBody(t, "code-review")
	lower := strings.ToLower(body)
	for _, forbidden := range []string{
		"ask the user", "ask for it", "ask them", "tell the user", "the user said", "if they say",
		"sub-agent", "subagent", "spawn", "/setup-matt-pocock-skills", "tracker doc",
		"询问用户", "子 agent",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("load_skill body contains non-executable instruction %q", forbidden)
		}
	}
}

func TestCodeReviewSkillKeepsMethodAndRules(t *testing.T) {
	body := loadBody(t, "code-review")
	for _, want := range []string{
		// Fixed point and spec come from the merge request.
		"base", "head", "merge request description", "referenced issue",
		// Repository standards win; smells are judgement calls.
		"The repository overrides", "Always a judgement call",
		// Full Fowler smell baseline.
		"Mysterious Name", "Duplicated Code", "Feature Envy", "Data Clumps", "Primitive Obsession",
		"Repeated Switches", "Shotgun Surgery", "Divergent Change", "Speculative Generality",
		"Message Chains", "Middle Man", "Refused Bequest",
		// Channel mapping and output contract.
		"review.standards", "review.intent", "code-review", "submit_result",
		// Provenance.
		"mattpocock/skills", upstreamCommit, "MIT",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("load_skill body is missing %q", want)
		}
	}
	for _, resource := range []string{"- LICENSE", "- NOTICE.md"} {
		if !strings.Contains(body, resource) {
			t.Errorf("load_skill resources are missing %q", resource)
		}
	}
}

func TestCodeReviewSkillShipsLicenseAndProvenance(t *testing.T) {
	sub, err := builtin.Sub("code-review")
	if err != nil {
		t.Fatal(err)
	}
	license, err := fs.ReadFile(sub, "LICENSE")
	if err != nil {
		t.Fatalf("LICENSE: %v", err)
	}
	for _, want := range []string{"MIT License", "Copyright (c) 2026 Matt Pocock", "Permission is hereby granted, free of charge"} {
		if !strings.Contains(string(license), want) {
			t.Errorf("LICENSE is missing %q", want)
		}
	}
	notice, err := fs.ReadFile(sub, "NOTICE.md")
	if err != nil {
		t.Fatalf("NOTICE.md: %v", err)
	}
	for _, want := range []string{
		"https://github.com/mattpocock/skills",
		"skills/engineering/code-review/SKILL.md",
		upstreamCommit,
		"Copyright (c) 2026 Matt Pocock",
	} {
		if !strings.Contains(string(notice), want) {
			t.Errorf("NOTICE.md is missing %q", want)
		}
	}
}
