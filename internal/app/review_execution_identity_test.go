package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func identityRuntime(t *testing.T, f *reviewFixture, cfg *config.Config) *Runtime {
	t.Helper()
	api := &runtimeTestGitLab{reviewGitLab: f.gitlab, admin: defaultFakeGitLab()}
	r, err := NewRuntime(context.Background(), cfg, RuntimeDeps{
		Store: f.db, Cipher: f.service.cipher, GitLab: api, LLM: runtimeTestClients{},
		Git: f.git, Workspace: f.manager, Prompts: f.bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	r.reviews.agentRun = f.runner.Run
	r.reviews.clients = nil
	f.service, f.engine, f.publisher = r.reviews, r.engine, r.publisher
	return r
}

func TestDeletedSkillOnlyBlocksProfilesThatUseIt(t *testing.T) {
	for _, block := range []string{"onboarding", "code-review", "verify"} {
		t.Run(block, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "deleted-skill")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: deleted-skill\ndescription: test\n---\nbody\n"), 0600); err != nil {
				t.Fatal(err)
			}
			projectID := int64(42)
			registered, err := f.db.CreateSkill(ctx, store.Skill{Scope: store.ScopeProject, ProjectID: &projectID, Name: "deleted-skill", Description: "test", Dir: root, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopeProject, ProjectID: &projectID, Block: block, Skills: []string{"deleted-skill"}}); err != nil {
				t.Fatal(err)
			}
			cfg := identityConfig(f)
			r1 := identityRuntime(t, f, cfg)
			job := f.seedReview(t, "job-deleted-skill-"+block)
			oldID := interruptReview(t, f, job)
			before, err := f.db.GetWorkflowDossier(ctx, oldID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.DeleteSkill(ctx, registered.ID); err != nil {
				t.Fatal(err)
			}
			if err := r1.Close(); err != nil {
				t.Fatal(err)
			}
			identityRuntime(t, f, cfg)
			err = f.service.Handle(ctx, job)
			if block != "onboarding" {
				if !errors.Is(err, skill.ErrUnknownSkill) {
					t.Fatalf("active deleted skill error = %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unused deleted skill blocked retry: %v", err)
				}
				wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
				if err != nil || wf.ID != oldID || wf.Status != "completed" {
					t.Fatalf("unchanged workflow = %+v, %v", wf, err)
				}
				freshJob := f.seedReview(t, "job-fresh-unused-skill")
				if err := f.service.Handle(ctx, freshJob); err != nil {
					t.Fatalf("unused deleted skill blocked a new review: %v", err)
				}
				fresh, err := f.db.GetWorkflowByJob(ctx, freshJob.ID)
				if err != nil || fresh.Status != "completed" {
					t.Fatalf("fresh workflow = %+v, %v", fresh, err)
				}
			}
			after, err := f.db.GetWorkflowDossier(ctx, oldID)
			if err != nil || string(after.Data) != string(before.Data) {
				t.Fatal("original dossier changed")
			}
		})
	}
}

func TestRecoveryAllowsDeletedUnusedSkill(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx := context.Background()
	id := int64(42)
	root := filepath.Join(t.TempDir(), "unused-skill")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: unused-skill\ndescription: test\n---\nbody\n"), 0600); err != nil {
		t.Fatal(err)
	}
	saved, err := f.db.CreateSkill(ctx, store.Skill{Scope: store.ScopeProject, ProjectID: &id, Name: "unused-skill", Description: "test", Dir: root, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopeProject, ProjectID: &id, Block: "onboarding", Skills: []string{"unused-skill"}}); err != nil {
		t.Fatal(err)
	}
	cfg := identityConfig(f)
	r1 := identityRuntime(t, f, cfg)
	job := f.seedReview(t, "job-unused-skill-recovery")
	wfID := interruptReview(t, f, job)
	before, err := f.db.GetWorkflowDossier(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.DeleteSkill(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := identityRuntime(t, f, cfg)
	if err := r2.reviews.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertReviewPublished(t, f, wfID)
	after, err := f.db.GetWorkflowDossier(ctx, wfID)
	if err != nil || string(before.Data) != string(after.Data) {
		t.Fatal("recovery changed original dossier")
	}
}

func TestReviewExecutionSkillBlocksFollowGraphExpansions(t *testing.T) {
	code := testReviewProfile()
	reasoned := testReviewProfile()
	reasoned.Reasoner = &config.ReasonerConfig{Provider: "fake", When: "always"}
	reviewOnly := testReviewProfile()
	reviewOnly.Propose = []config.ProposeConfig{{Block: "review", Provider: "fake"}}
	extended := reviewOnly
	extended.Synthesize = config.SynthesizeConfig{Mode: "agent", Provider: "fake"}
	extended.Budget.MaxExtends = 1
	onboarding := config.ProfileConfig{Agent: &config.AgentConfig{Provider: "fake"}}
	for _, tc := range []struct {
		name string
		cfg  config.ProfileConfig
		want []string
	}{
		{"code", code, []string{"code-review", "verify"}},
		{"reasoner", reasoned, []string{"code-review", "verify"}},
		{"review", reviewOnly, []string{"review", "verify"}},
		{"synthesis-extension", extended, []string{"review", "verify", "code-review"}},
		{"onboarding", onboarding, []string{"onboarding"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reviewExecutionSkillBlocks(review.Profile{Name: tc.name, Config: tc.cfg}, []string{"fake"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("blocks = %v, want %v", got, tc.want)
			}
			for _, name := range tc.want {
				if !got[name] {
					t.Fatalf("blocks = %v, missing %s", got, name)
				}
			}
		})
	}
}

func TestResolverRejectsMissingSkillsOnlyForPossibleGraphBlocks(t *testing.T) {
	code := testReviewProfile()
	reviewOnly := testReviewProfile()
	reviewOnly.Propose = []config.ProposeConfig{{Block: "review", Provider: "fake"}}
	extended := reviewOnly
	extended.Synthesize = config.SynthesizeConfig{Mode: "agent", Provider: "fake"}
	extended.Budget.MaxExtends = 1
	onboarding := config.ProfileConfig{Agent: &config.AgentConfig{Provider: "fake"}}
	for _, tc := range []struct {
		name    string
		profile config.ProfileConfig
		block   string
		active  bool
	}{
		{"review-unused", code, "review", false},
		{"review-used", reviewOnly, "review", true},
		{"code-unused", reviewOnly, "code-review", false},
		{"code-synthesis-extension", extended, "code-review", true},
		{"onboarding-used", onboarding, "onboarding", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			id := int64(42)
			if _, err := f.db.UpsertBlockBinding(context.Background(), store.BlockBinding{Scope: store.ScopeProject, ProjectID: &id, Block: tc.block, Skills: []string{"missing-skill"}}); err != nil {
				t.Fatal(err)
			}
			r := identityRuntime(t, f, identityConfig(f))
			consumerReadLock(t, r.workspace, id)
			project, err := f.db.GetProject(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.reviews.resolveReviewExecution(context.Background(), project, "", review.Profile{Name: tc.name, Config: tc.profile}, "main", nil)
			if tc.active && !errors.Is(err, skill.ErrUnknownSkill) {
				t.Fatalf("active skill error = %v", err)
			}
			if !tc.active && err != nil {
				t.Fatalf("unused skill error = %v", err)
			}
		})
	}
}

func TestRuntimeRetryUnchangedIdentityReusesWorkflow(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	cfg := identityConfig(f)
	r1 := identityRuntime(t, f, cfg)
	job := f.seedReview(t, "job-identity-unchanged")
	oldID := interruptReview(t, f, job)
	old, err := f.db.GetWorkflowDossier(context.Background(), oldID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	identityRuntime(t, f, cfg)
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil || wf.ID != oldID {
		t.Fatalf("unchanged identity did not resume: %+v %v", wf, err)
	}
	after, err := f.db.GetWorkflowDossier(context.Background(), oldID)
	if err != nil || string(after.Data) != string(old.Data) {
		t.Fatal("original dossier changed")
	}
}

func TestRetryRebuildsForSkillContentChange(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	dir := t.TempDir()
	root := filepath.Join(dir, "skill-a")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "SKILL.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(file, []byte("---\nname: skill-a\ndescription: test skill\n---\n"+body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("first version")
	projectID := int64(42)
	if _, err := f.db.UpsertBlockBinding(context.Background(), store.BlockBinding{Scope: store.ScopeProject, ProjectID: &projectID, Block: "code-review", Skills: []string{"skill-a"}}); err != nil {
		t.Fatal(err)
	}
	cfg := identityConfig(f)
	cfg.Skills.Dirs = []string{dir}
	r1 := identityRuntime(t, f, cfg)
	job := f.seedReview(t, "job-skill-content")
	oldID := interruptReview(t, f, job)
	write("second version")
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	identityRuntime(t, f, cfg)
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.ID == oldID {
		t.Fatal("changed skill content reused old workflow")
	}
}

func TestReviewRejectsSkillChangeDuringAgent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	dir := t.TempDir()
	root := filepath.Join(dir, "skill-a")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "SKILL.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(file, []byte("---\nname: skill-a\ndescription: test skill\n---\n"+body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("original")
	projectID := int64(42)
	if _, err := f.db.UpsertBlockBinding(context.Background(), store.BlockBinding{Scope: store.ScopeProject, ProjectID: &projectID, Block: "code-review", Skills: []string{"skill-a"}}); err != nil {
		t.Fatal(err)
	}
	cfg := identityConfig(f)
	cfg.Skills.Dirs = []string{dir}
	identityRuntime(t, f, cfg)
	job := f.seedReview(t, "job-skill-live-change")
	block := make(chan struct{})
	f.runner.block = block
	f.runner.started = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- f.service.Handle(context.Background(), job) }()
	select {
	case <-f.runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not start")
	}
	write("changed")
	close(block)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not finish")
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status == "completed" {
		t.Fatal("review accepted output after its bound skill changed")
	}
}

func TestRecoveryRejectsChangedResolverIdentity(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx := context.Background()
	projectID := int64(42)
	bind := func(tools []string) {
		t.Helper()
		if _, err := f.db.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopeProject, ProjectID: &projectID, Block: "code-review", MCPTools: tools}); err != nil {
			t.Fatal(err)
		}
	}
	bind(nil)
	cfg := identityConfig(f)
	r1 := identityRuntime(t, f, cfg)
	job := f.seedReview(t, "job-recovery-binding")
	oldID := interruptReview(t, f, job)
	old, err := f.db.GetWorkflowDossier(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	bind([]string{"mcp__missing__read"})
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := identityRuntime(t, f, cfg)
	before := f.runner.count()
	if err := r2.reviews.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflow(ctx, oldID)
	if err != nil || wf.Status != "cancelled" {
		t.Fatalf("recovery reused old identity: %+v, %v", wf, err)
	}
	after, err := f.db.GetWorkflowDossier(ctx, oldID)
	if err != nil || string(old.Data) != string(after.Data) || after.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("old snapshot changed: %v", err)
	}
	if f.runner.count() != before {
		t.Fatal("recovery ran old workflow after binding changed")
	}
}

func TestReviewExecutionIdentityIsRequired(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	_, err := NewReviewService(ReviewServiceConfig{Store: f.db, GitLab: f.gitlab, Cipher: f.service.cipher, Workspace: f.manager, Git: f.git, Prompts: f.bundle})
	if err == nil {
		t.Fatal("missing execution identity accepted")
	}
}

func TestResolvedExecutionPinsProjectBindings(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx := context.Background()
	dir := t.TempDir()
	for _, name := range []string{"skill-a", "skill-b"} {
		root := filepath.Join(dir, name)
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: test\n---\nbody"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id := int64(42)
	bind := func(name string) {
		t.Helper()
		if _, err := f.db.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopeProject, ProjectID: &id, Block: "code-review", Skills: []string{name}}); err != nil {
			t.Fatal(err)
		}
	}
	bind("skill-a")
	cfg := identityConfig(f)
	cfg.Skills.Dirs = []string{dir}
	r := identityRuntime(t, f, cfg)
	consumerReadLock(t, r.workspace, id)
	profile := review.Profile{Name: "default", Config: f.profiles["default"]}
	project, err := f.db.GetProject(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := r.reviews.resolveReviewExecution(ctx, project, "", profile, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	bind("skill-b")
	replacement, err := r.projectExts.build(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.projectExts.mu.Lock()
	previous := r.projectExts.sets[id]
	r.projectExts.sets[id] = replacement
	r.projectExts.mu.Unlock()
	t.Cleanup(func() { _ = previous.mcp.Close() })
	actual, err := execution.skills.BindSkills(ctx, execution.blocks["code-review"].Skills)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual.Skills) != 1 || actual.Skills[0].Name != "skill-a" {
		t.Fatalf("executor changed resolved set: %+v", actual.Skills)
	}
	newExecution, err := r.reviews.resolveReviewExecution(ctx, project, "", profile, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(newExecution.digest) == string(execution.digest) {
		t.Fatal("replacement identity did not change")
	}
}

func TestRetryRebuildsForMCPConfigWithoutLeakingSecrets(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore-%v", ignore), func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			cfg := identityConfig(f)
			if !ignore {
				cfg.ReviewDefaults.Ignore = nil
			}
			id := int64(42)
			server, err := f.db.CreateMCPServer(context.Background(), store.MCPServer{Scope: store.ScopeProject, ProjectID: &id, Name: "isolated", Enabled: true, Config: json.RawMessage(`{"type":"http","url":"https://example.invalid/mcp","headers":{"X-Secret":"secret-canary-a"}}`)})
			if err != nil {
				t.Fatal(err)
			}
			r1 := identityRuntime(t, f, cfg)
			job := f.seedReview(t, fmt.Sprintf("job-mcp-%v", ignore))
			oldID := interruptReview(t, f, job)
			old, err := f.db.GetWorkflowDossier(context.Background(), oldID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(old.Data), "secret-canary-a") {
				t.Fatal("secret was persisted in dossier")
			}
			if _, err := f.db.UpdateMCPServer(context.Background(), server.ID, store.MCPServerUpdate{Config: json.RawMessage(`{"type":"http","url":"https://example.invalid/mcp","headers":{"X-Secret":"secret-canary-b"}}`)}); err != nil {
				t.Fatal(err)
			}
			if err := r1.Close(); err != nil {
				t.Fatal(err)
			}
			identityRuntime(t, f, cfg)
			if err := f.service.Handle(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wf.ID == oldID {
				t.Fatal("changed MCP configuration reused old workflow")
			}
			fresh, err := f.db.GetWorkflowDossier(context.Background(), wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(fresh.Data), "secret-canary-b") {
				t.Fatal("new secret was persisted in dossier")
			}
		})
	}
}

func identityConfig(f *reviewFixture) *config.Config {
	return &config.Config{
		Workspace: config.WorkspaceConfig{Root: f.manager.Root(), RunCleanupAfter: time.Hour},
		Storage:   config.StorageConfig{LLMCallRetention: time.Hour, DeliveryRetention: time.Hour, JobRetention: time.Hour, ContextVersions: 1},
		Engine:    config.EngineConfig{MaxConcurrentNodes: 2, DefaultNodeTimeout: time.Minute},
		Profiles:  f.profiles, ReviewDefaults: f.defaults,
		Providers: map[string]config.ProviderConfig{"fake": {Protocol: "openai", BaseURL: "https://example.invalid", Model: "model-a"}},
	}
}

func TestRetryRebuildsForActualProjectBinding(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx := context.Background()
	dir := t.TempDir()
	for _, name := range []string{"skill-a", "skill-b"} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(fmt.Sprintf("---\nname: %s\ndescription: test skill\n---\nbody\n", name)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	projectID := int64(42)
	bind := func(name string) {
		t.Helper()
		if _, err := f.db.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopeProject, ProjectID: &projectID, Block: "code-review", Skills: []string{name}}); err != nil {
			t.Fatal(err)
		}
	}
	bind("skill-a")
	cfg := identityConfig(f)
	cfg.Skills.Dirs = []string{dir}
	r1 := identityRuntime(t, f, cfg)
	job := f.seedReview(t, "job-real-binding")
	oldID := interruptReview(t, f, job)
	old, err := f.db.GetWorkflowDossier(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	bind("skill-b")
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	identityRuntime(t, f, cfg)
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.ID == oldID {
		t.Fatal("changed project binding reused old completed nodes")
	}
	after, err := f.db.GetWorkflowDossier(ctx, oldID)
	if err != nil || string(after.Data) != string(old.Data) || after.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("old dossier not preserved: %v", err)
	}
}

func TestRetryRebuildsForProviderConfiguration(t *testing.T) {
	for _, change := range []string{"model", "temperature", "protocol", "endpoint", "reasoning", "max-tokens"} {
		t.Run(change, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			cfg := identityConfig(f)
			r1 := identityRuntime(t, f, cfg)
			job := f.seedReview(t, "job-provider-"+change)
			oldID := interruptReview(t, f, job)
			old, err := f.db.GetWorkflowDossier(context.Background(), oldID)
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Providers["fake"]
			switch change {
			case "model":
				p.Model = "model-b"
			case "temperature":
				temp := 0.7
				p.Temperature = &temp
			case "protocol":
				p.Protocol = "anthropic"
			case "endpoint":
				p.BaseURL = "https://other.invalid"
			case "reasoning":
				p.Reasoning = &config.ReasoningConfig{Effort: "high"}
			case "max-tokens":
				p.MaxTokens = 1234
			}
			cfg.Providers["fake"] = p
			if err := r1.Close(); err != nil {
				t.Fatal(err)
			}
			identityRuntime(t, f, cfg)
			if err := f.service.Handle(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wf.ID == oldID {
				t.Fatal("changed provider configuration reused old completed nodes")
			}
			after, err := f.db.GetWorkflowDossier(context.Background(), oldID)
			if err != nil || string(after.Data) != string(old.Data) || after.PublicationState != store.WorkflowPublicationSkipped {
				t.Fatalf("old dossier not preserved: %v", err)
			}
		})
	}
}

func TestProviderCredentialEnvIsExcludedFromIdentity(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	cfg := identityConfig(f)
	p := cfg.Providers["fake"]
	p.APIKeyEnv = "FIRST_SECRET_ENV"
	cfg.Providers["fake"] = p
	r1 := identityRuntime(t, f, cfg)
	job := f.seedReview(t, "job-provider-key-env")
	oldID := interruptReview(t, f, job)
	p.APIKeyEnv = "SECOND_SECRET_ENV"
	cfg.Providers["fake"] = p
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	identityRuntime(t, f, cfg)
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil || wf.ID != oldID {
		t.Fatalf("credential env changed execution identity: %+v %v", wf, err)
	}
}
