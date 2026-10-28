package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestUnicodeIgnoreMatcherAndModelTools(t *testing.T) {
	for _, dir := range []string{"私密", "café"} {
		t.Run(dir, func(t *testing.T) {
			if !NewIgnoreMatcher([]string{dir + "/**"}).Match(dir + "/a.go") {
				t.Errorf("matcher missed %s", dir)
			}
			head := t.TempDir()
			if err := os.MkdirAll(filepath.Join(head, dir), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(head, dir, "a.go"), []byte("CANARY_UNICODE\n"), 0600); err != nil {
				t.Fatal(err)
			}
			d := validDossier(t)
			d.HeadDir = head
			d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{dir + "/**"}}
			calls := 0
			e := newTestExecutor(t, ExecutorDeps{Dossier: d, AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				calls++
				for _, b := range spec.System {
					if strings.Contains(b.Text, "CANARY_UNICODE") {
						t.Error("system leaked")
					}
				}
				if strings.Contains(spec.Prompt, "CANARY_UNICODE") {
					t.Error("prompt leaked")
				}
				for _, tool := range spec.Tools {
					if tool.Def().Name == "read_file" {
						raw, _ := json.Marshal(map[string]string{"path": dir + "/a.go"})
						res, err := tool.Call(ctx, raw)
						if err == nil || strings.Contains(res.Content, "CANARY_UNICODE") {
							t.Errorf("read = %q %v", res.Content, err)
						}
					}
				}
				out, err := spec.Submit.Decode(json.RawMessage(`{"summary":"ok","findings":[]}`))
				return &agent.RunResult{Output: out}, err
			}})
			if _, err := e.Execute(context.Background(), codeReviewInput(t)); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

type boundSkillTestProvider struct {
	binding     *skill.Binding
	description string
}

func (p boundSkillTestProvider) BindSkills(context.Context, []string) (SkillBinding, error) {
	return SkillBinding{Roots: p.binding.Roots(), Tool: p.binding.Tool(), Skills: []Skill{{Name: "demo", Description: p.description}}}, nil
}

func TestStrictIgnoreRejectsOverlappingSkillBeforePrompt(t *testing.T) {
	for _, inside := range []bool{true, false} {
		t.Run(map[bool]string{true: "repo", false: "external"}[inside], func(t *testing.T) {
			base := t.TempDir()
			container := t.TempDir()
			if inside {
				container = filepath.Join(base, "private-skills")
				if err := os.MkdirAll(container, 0700); err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.Join(container, "demo")
			if err := os.MkdirAll(filepath.Join(root, "resources"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"SKILL.md": "---\nname: demo\ndescription: CANARY_SKILL_DESCRIPTION\n---\nCANARY_SKILL_BODY\n", "resources/file.txt": "CANARY_RESOURCE"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			loader, err := skill.New(skill.Options{PlatformDirs: []string{container}})
			if err != nil {
				t.Fatal(err)
			}
			binding, err := loader.Bind([]string{"demo"})
			if err != nil {
				t.Fatal(err)
			}
			d := validDossier(t)
			d.BaseDir = base
			d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"private-skills/**"}}
			calls := 0
			e := newTestExecutor(t, ExecutorDeps{Dossier: d, Blocks: map[string]config.BlockBinding{BlockCodeReview: {Skills: []string{"demo"}}}, Skills: boundSkillTestProvider{binding, "CANARY_SKILL_DESCRIPTION"}, AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				calls++
				for _, tool := range spec.Tools {
					if tool.Def().Name == "load_skill" {
						res, err := tool.Call(ctx, json.RawMessage(`{"name":"demo"}`))
						if err != nil || !strings.Contains(res.Content, "CANARY_SKILL_BODY") {
							t.Errorf("load=%q %v", res.Content, err)
						}
					}
				}
				out, err := spec.Submit.Decode(json.RawMessage(`{"summary":"ok","findings":[]}`))
				return &agent.RunResult{Output: out}, err
			}})
			_, err = e.Execute(context.Background(), codeReviewInput(t))
			if inside && (err == nil || calls != 0) {
				t.Errorf("repo skill exposed: calls=%d err=%v", calls, err)
			}
			if !inside && (err != nil || calls != 1) {
				t.Errorf("external skill failed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestStrictIgnoreRejectsAliasedVerifyEvidence(t *testing.T) {
	head := t.TempDir()
	if err := os.MkdirAll(filepath.Join(head, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(head, "private", "secret.go"), []byte("CANARY_EVIDENCE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private/secret.go", filepath.Join(head, "alias.go")); err != nil {
		t.Fatal(err)
	}
	for _, strict := range []bool{true, false} {
		d := validDossier(t)
		d.HeadDir = head
		patterns := []string{}
		if strict {
			patterns = []string{"private/**"}
		}
		d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: patterns}
		out := &VerifyOut{Status: "CONFIRMED", Reason: "confirmed", Evidence: []CodeRef{{Path: "alias.go", StartLine: 1, EndLine: 1, Root: "head", Note: "ok"}}}
		e := newTestExecutor(t, ExecutorDeps{Dossier: d, AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{Output: out}, nil
		}})
		if got := e.ignoredAgentOutput(out); got != strict {
			t.Errorf("strict=%v ignored=%v", strict, got)
		}
		params, _ := json.Marshal(verifyParams{GroupID: "g1", Provider: "claude"})
		_, err := e.Execute(context.Background(), dag.NodeInput{WorkflowID: "verify", Node: dag.NodeSpec{ID: "verify.1", Kind: KindVerify, Params: params}})
		if (err != nil) != strict {
			t.Errorf("strict=%v verify error=%v", strict, err)
		}
	}
}

func TestStrictSkillOverlapChecksAliasesAndAncestors(t *testing.T) {
	parent := t.TempDir()
	head := filepath.Join(parent, "head")
	base := filepath.Join(parent, "base")
	for _, root := range []string{head, base} {
		if err := os.MkdirAll(filepath.Join(root, "inside"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(base, "inside"), alias); err != nil {
		t.Fatal(err)
	}
	d := validDossier(t)
	d.HeadDir = head
	d.BaseDir = base
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"private/**"}}
	e := newTestExecutor(t, ExecutorDeps{Dossier: d})
	for _, root := range []string{head, filepath.Join(head, "inside"), base, parent, alias} {
		if err := e.checkSkillRoots(SkillBinding{Roots: []string{root}}); err == nil {
			t.Errorf("overlapping root accepted: %s", root)
		}
	}
	if err := e.checkSkillRoots(SkillBinding{Skills: []Skill{{Name: "unknown"}}}); err == nil {
		t.Fatal("unknown skill source accepted")
	}
	if err := e.checkSkillRoots(SkillBinding{Roots: []string{t.TempDir()}}); err != nil {
		t.Fatalf("external root: %v", err)
	}
}
