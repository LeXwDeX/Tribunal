package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRepositorySkillBindingsRejectUnprovedTarget(t *testing.T) {
	grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{"repo:shared"}, Revision: strings.Repeat("a", 64)}
	if _, _, err := applyRepositorySkillAuthorizations(nil, 42, "main", strings.Repeat("b", 40), []store.RepositorySkillAuthorization{grant}); err == nil {
		t.Fatal("repository skill without a reviewed target SHA was authorized")
	}
}

func TestRepositorySkillBindingsRequireIndependentExactGrant(t *testing.T) {
	blocks := map[string]config.BlockBinding{
		"review": {MCPTools: []string{"mcp__x__y"}, Skills: []string{"shared"}},
		"verify": {Skills: []string{"lint"}},
	}
	revision := strings.Repeat("a", 64)
	sha := strings.Repeat("b", 40)
	snapshot := strings.Repeat("d", 64)
	grants := []store.RepositorySkillAuthorization{{ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{"repo:shared"}, Revision: revision, SnapshotSHA256: snapshot}}
	got, identities, err := applyRepositorySkillAuthorizations(blocks, 42, "main", sha, grants)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["review"].Skills, []string{"shared", "repo:shared"}) || !reflect.DeepEqual(got["verify"].Skills, []string{"lint"}) || identities["review"] != revision {
		t.Fatalf("bound grants = %+v, identities=%v", got, identities)
	}
	if !reflect.DeepEqual(blocks["review"].Skills, []string{"shared"}) {
		t.Fatalf("cached binding mutated: %+v", blocks)
	}
	for _, bad := range []store.RepositorySkillAuthorization{
		{ProjectID: 43, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{"repo:shared"}, Revision: revision},
		{ProjectID: 42, TargetBranch: "other", TargetSHA: sha, Block: "review", Skills: []string{"repo:shared"}, Revision: revision},
		{ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{"shared"}, Revision: revision},
		{ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{"repo:shared"}, Revision: store.BlockBindingAbsentRevision},
		{ProjectID: 42, TargetBranch: "main", TargetSHA: strings.Repeat("c", 40), Block: "review", Skills: []string{"repo:shared"}, Revision: revision},
		{ProjectID: 42, TargetBranch: "main", TargetSHA: "invalid", Block: "review", Skills: []string{"repo:shared"}, Revision: revision},
		{ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{}, Revision: revision},
	} {
		if _, _, err := applyRepositorySkillAuthorizations(blocks, 42, "main", sha, []store.RepositorySkillAuthorization{bad}); err == nil {
			t.Errorf("unsafe grant accepted: %+v", bad)
		}
	}
	for _, current := range []string{"", "invalid", strings.Repeat("c", 40)} {
		if _, _, err := applyRepositorySkillAuthorizations(blocks, 42, "main", current, grants); err == nil {
			t.Errorf("grant accepted for current target %q", current)
		}
	}
	if _, _, err := applyRepositorySkillAuthorizations(blocks, 42, "main", strings.ToUpper(sha), grants); err != nil {
		t.Fatalf("same SHA in uppercase was rejected: %v", err)
	}
	empty := []store.RepositorySkillAuthorization{{ProjectID: 42, TargetBranch: "main", Block: "review", Skills: []string{}, Revision: revision}}
	got, identities, err = applyRepositorySkillAuthorizations(blocks, 42, "main", "", empty)
	if err != nil || !reflect.DeepEqual(got["review"].Skills, []string{"shared"}) || identities["review"] != revision {
		t.Fatalf("revoked grant applied repository skills: %+v identities=%v err=%v", got, identities, err)
	}
}

func TestCombinedRepositorySkillProviderSeparatesNamesAndMounts(t *testing.T) {
	platform, repository := t.TempDir(), t.TempDir()
	for dir, body := range map[string]string{platform: "platform instructions", repository: "repository instructions"} {
		child := filepath.Join(dir, "security")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatal(err)
		}
		baseSecurityWrite(t, filepath.Join(child, "SKILL.md"), "---\nname: security\ndescription: example\n---\n"+body)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	scripts := filepath.Join(repository, "security", "scripts")
	if err := os.Mkdir(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "check.sh"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	base, err := skill.New(skill.Options{PlatformDirs: []string{platform}})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := skill.New(skill.Options{RepoSkillsDir: repository})
	if err != nil {
		t.Fatal(err)
	}
	provider := &combinedRepositorySkillProvider{base: NewSkillProvider(base), repository: NewSkillProvider(repo)}
	bound, err := provider.BindSkills(context.Background(), []string{"security", "repo:security"})
	if err != nil || len(bound.Skills) != 2 || len(bound.Roots) != 2 || bound.Tool == nil {
		t.Fatalf("combined binding = %+v, %v", bound, err)
	}
	for name, want := range map[string]string{"security": "platform instructions", "repo:security": "repository instructions"} {
		result, err := bound.Tool.Call(context.Background(), json.RawMessage(`{"name":"`+name+`"}`))
		if err != nil || !strings.Contains(result.Content, want) {
			t.Errorf("%s = %q, %v", name, result.Content, err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository script ran: %v", err)
	}
	onlyBase, err := provider.BindSkills(context.Background(), []string{"security"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onlyBase.Tool.Call(context.Background(), json.RawMessage(`{"name":"repo:security"}`)); !errors.Is(err, skill.ErrUnknownSkill) {
		t.Fatalf("repository skill escaped its block binding: %v", err)
	}
}

func TestRepositorySkillBindingsNeverAcceptLegacyRepoStrings(t *testing.T) {
	for _, legacy := range []string{"repo:shared", " repo:shared "} {
		blocks := map[string]config.BlockBinding{"review": {Skills: []string{legacy}}}
		sha := strings.Repeat("b", 40)
		grant := store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", TargetSHA: sha, Block: "review", Skills: []string{"repo:shared"}, Revision: strings.Repeat("a", 64), SnapshotSHA256: strings.Repeat("d", 64)}
		if _, _, err := applyRepositorySkillAuthorizations(blocks, 42, "main", sha, []store.RepositorySkillAuthorization{grant}); err == nil {
			t.Errorf("legacy binding %q became repository authority", legacy)
		}
	}
}
