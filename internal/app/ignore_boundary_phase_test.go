package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestIgnoreRenameEitherSide(t *testing.T) {
	diffs := []gitlab.FileDiff{
		{OldPath: "secret/a.go", NewPath: "public/a.go", RenamedFile: true, Diff: "-CANARY_OLD\n+new\n"},
		{OldPath: "public/b.go", NewPath: "secret/b.go", RenamedFile: true, Diff: "-old\n+CANARY_NEW\n"},
		{OldPath: "public/c.go", NewPath: "public/d.go", RenamedFile: true, Diff: "-old\n+new\n"},
	}
	files, ignored, summary, warnings := buildReviewFiles(diffs, []string{"secret/**"}, 10, 100)
	if len(files) != 1 || files[0].Path != "public/d.go" || strings.Contains(files[0].Diff, "CANARY") {
		t.Fatalf("unexpected accepted files: %+v", files)
	}
	if len(ignored) != 2 || summary.Truncated || len(warnings) != 0 {
		t.Fatalf("ignored=%v summary=%+v warnings=%v", ignored, summary, warnings)
	}
}

func TestReviewDossierOmitsIgnoredDocsAndUnprovenancedContext(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ignore      []string
		wantAgents  string
		wantContext string
	}{
		{name: "ignore", ignore: []string{"AGENTS.md"}},
		{name: "empty", wantAgents: "CANARY_AGENTS", wantContext: "existing context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			contexts := newContextService(t, f)
			seedWorkspaceContext(t, f, contexts, true)
			f.gitlab.version.StartSHA = onboardingContextBase
			f.defaults.Ignore = tc.ignore
			f.service.reviewDefaults = f.defaults
			f.publisher.reviewDefaults = f.defaults
			f.gitlab.docs["AGENTS.md"] = "CANARY_AGENTS"
			var mu sync.Mutex
			var prompts []string
			f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				mu.Lock()
				prompts = append(prompts, spec.Prompt)
				mu.Unlock()
				return f.runner.Run(ctx, spec)
			}
			job := f.seedReview(t, "job-ignore-context")
			if err := f.service.RunReview(context.Background(), job); err != nil {
				t.Fatalf("RunReview: %v", err)
			}
			workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			record, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
			if err != nil {
				t.Fatal(err)
			}
			var dossier review.Dossier
			if err := json.Unmarshal(record.Data, &dossier); err != nil {
				t.Fatal(err)
			}
			if dossier.RepoDocs.AGENTS != tc.wantAgents || dossier.ContextDocs != tc.wantContext {
				t.Fatalf("dossier agents=%q context=%q", dossier.RepoDocs.AGENTS, dossier.ContextDocs)
			}
			mu.Lock()
			joinedPrompts := strings.Join(prompts, "\n")
			calls := len(prompts)
			mu.Unlock()
			if calls == 0 {
				t.Fatal("agent was not called")
			}
			if len(tc.ignore) > 0 && (strings.Contains(joinedPrompts, "CANARY_AGENTS") || strings.Contains(joinedPrompts, "existing context")) {
				t.Fatal("ignored material reached the agent prompt")
			}
			current, err := f.db.GetCurrentContextDoc(context.Background(), 42, "agents")
			if err != nil || current.Content != "existing context" || !current.Locked {
				t.Fatalf("stored context changed: %+v, %v", current, err)
			}
		})
	}
}

func TestIgnoredRepoDocumentsAndRulesAreNotIncluded(t *testing.T) {
	docs := review.RepoDocs{GitLabReviewConfig: "ignore: [AGENTS.md, .gitlab-review.yml]\nrules: [CANARY_RULE]\nlanguage: CANARY_LANGUAGE\nCANARY_FIELD: true\n", AGENTS: "CANARY_AGENTS", CLAUDE: "allowed"}
	defaults := config.ProjectSettings{AllowRepoConfig: true, CustomRules: []string{"platform rule"}}
	cfg := resolveReviewConfig(&store.Project{}, docs, defaults, nil)
	filtered := filterRepoDocs(docs, cfg.Settings.Ignore)
	if filtered.GitLabReviewConfig != "" || filtered.AGENTS != "" || filtered.CLAUDE != "allowed" {
		t.Fatalf("filtered documents = %+v", filtered)
	}
	if strings.Join(cfg.Settings.CustomRules, ",") != "platform rule" {
		t.Fatalf("custom rules = %v", cfg.Settings.CustomRules)
	}
	if len(cfg.Settings.Ignore) != 2 {
		t.Fatalf("repository configuration was not applied locally: %v", cfg.Settings.Ignore)
	}
	if strings.Contains(cfg.Settings.Language, "CANARY") || strings.Contains(strings.Join(cfg.Warnings, " "), "CANARY") {
		t.Fatalf("ignored config text leaked: language=%q warnings=%v", cfg.Settings.Language, cfg.Warnings)
	}
}
