package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestIgnoreEffectiveProjectBindingsWithoutMCPConnection(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	project := int64(42)
	for _, server := range []struct {
		name    string
		enabled bool
	}{{"active", true}, {"disabled", false}} {
		if _, err := service.CreateMCPServer(ctx, MCPServerInput{Scope: "platform", Name: server.name, Enabled: server.enabled, Config: json.RawMessage(`{"command":"/definitely/not/a/real/server"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []BlockBindingInput{
		{Scope: "project", ProjectID: &project, Block: "review", MCPTools: []string{"mcp__active__project", "mcp__disabled__blocked"}},
		{Scope: "project", ProjectID: &project, Block: "verify", MCPTools: []string{}},
	} {
		if _, err := service.UpsertBlockBinding(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{Store: service.store, Workspace: service.workspace,
		Blocks: map[string]config.BlockBinding{
			"code-review": {MCPTools: []string{"mcp__active__platform"}},
			"review":      {MCPTools: []string{"mcp__active__inherited"}},
			"verify":      {MCPTools: []string{"mcp__active__inherited"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	p := &projectAwareToolProvider{resolver: resolver}
	projectCtx := context.WithValue(ctx, projectIDContextKey{}, project)
	for block, want := range map[string][]string{"code-review": {"mcp__active__platform"}, "review": {"mcp__active__project"}, "verify": nil} {
		got, err := p.BoundToolNames(projectCtx, block)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s got=%v want=%v err=%v", block, got, want, err)
		}
	}
}

type countingIgnoreBindingProvider struct {
	*projectAwareToolProvider
	calls atomic.Int32
}

func (p *countingIgnoreBindingProvider) Tools(ctx context.Context, block string) ([]agent.Tool, error) {
	p.calls.Add(1)
	return p.projectAwareToolProvider.Tools(ctx, block)
}

func TestIgnoreProjectBindingStubsDoNotCreateFalseGaps(t *testing.T) {
	target := strings.Repeat("2", 40)
	for _, tc := range []struct {
		name      string
		platform  bool
		override  bool
		enabled   bool
		ignore    bool
		wantGap   bool
		wantTools int32
		state     *store.Workspace
	}{
		{"no-bindings", false, false, true, true, false, 0, nil},
		{"platform-inherited", true, false, true, true, false, 0, nil},
		{"project-empty-override", true, true, true, true, false, 0, nil},
		{"project-only-binding", false, true, true, true, false, 0, nil},
		{"disabled-server", true, false, false, true, false, 0, nil},
		{"empty-policy-allows-mcp", true, false, true, false, false, 1, &store.Workspace{State: "ready", BaseSHA: strings.Repeat("1", 40), IndexSHA: target}},
		{"index-mismatch", true, false, true, false, true, 0, &store.Workspace{State: "ready", IndexSHA: strings.Repeat("1", 40)}},
		{"index-invalid", true, false, true, false, true, 0, &store.Workspace{State: "ready", IndexSHA: "bad"}},
		{"index-missing", true, false, true, false, true, 0, nil},
		{"index-error", true, false, true, false, true, 0, &store.Workspace{State: "ready", IndexSHA: target, LastError: store.WorkspaceIndexFailed}},
		{"workspace-error", true, false, true, false, true, 0, &store.Workspace{State: "error", IndexSHA: target}},
		{"disabled-server-missing-index", true, false, false, false, false, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			if tc.state != nil {
				state := *tc.state
				state.ProjectID = 42
				if _, err := f.db.SaveWorkspace(ctx, state); err != nil {
					t.Fatal(err)
				}
			}
			service, _ := newExtensionFixture(t)
			inner := newHTTPMCPTestServer(t)
			var requests atomic.Int32
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				inner.Config.Handler.ServeHTTP(w, r)
			}))
			defer httpServer.Close()
			if _, err := service.CreateMCPServer(ctx, MCPServerInput{Scope: "platform", Name: "remote", Enabled: tc.enabled, Config: json.RawMessage(`{"url":"` + httpServer.URL + `"}`)}); err != nil {
				t.Fatal(err)
			}
			blocks := map[string]config.BlockBinding{}
			if tc.platform {
				blocks[review.BlockCodeReview] = config.BlockBinding{MCPTools: []string{"mcp__remote__echo"}}
			}
			if tc.override {
				project := int64(42)
				var tools []string
				if !tc.platform {
					tools = []string{"mcp__remote__echo"}
				}
				if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{Scope: "project", ProjectID: &project, Block: review.BlockCodeReview, MCPTools: tools}); err != nil {
					t.Fatal(err)
				}
			}
			resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{Store: service.store, Workspace: service.workspace, Blocks: blocks})
			if err != nil {
				t.Fatal(err)
			}
			defer resolver.Close()
			provider := &countingIgnoreBindingProvider{projectAwareToolProvider: &projectAwareToolProvider{resolver: resolver}}
			f.executors.SetProjectLookup(func(ctx context.Context, id string) (int64, error) {
				wf, err := f.db.GetWorkflow(ctx, id)
				if err != nil {
					return 0, err
				}
				return wf.ProjectID, nil
			})
			f.service.tools = provider
			f.service.blocks = projectBindingStubs(blocks)
			f.defaults.Ignore = []string{}
			if tc.ignore {
				f.defaults.Ignore = []string{"private.txt"}
			}
			f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
			job := f.seedReview(t, "actual-ignore-bindings")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Workspace *struct {
					Version     int    `json:"version"`
					TargetSHA   string `json:"target_sha"`
					IndexSHA    string `json:"index_sha"`
					MCPEligible bool   `json:"mcp_eligible"`
				} `json:"workspace_provenance"`
			}
			if err := json.Unmarshal(record.Data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Workspace == nil || saved.Workspace.Version != 1 || saved.Workspace.TargetSHA != target || saved.Workspace.MCPEligible != (tc.wantTools > 0) {
				t.Fatalf("workspace provenance: %+v", saved.Workspace)
			}
			wantIndex := ""
			if tc.state != nil {
				wantIndex = tc.state.IndexSHA
			}
			if saved.Workspace.IndexSHA != wantIndex {
				t.Fatalf("index source = %q, want %q", saved.Workspace.IndexSHA, wantIndex)
			}
			wantVerdict := review.VerdictPass
			if tc.wantGap {
				wantVerdict = review.VerdictIncomplete
			}
			if wf.Verdict != wantVerdict || provider.calls.Load() != tc.wantTools || (requests.Load() > 0) != (tc.wantTools > 0) {
				t.Fatalf("verdict=%s Tools=%d HTTP=%d", wf.Verdict, provider.calls.Load(), requests.Load())
			}
			snapshot, err := f.engine.Snapshot(ctx, wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			gaps := 0
			for _, node := range snapshot.Nodes {
				if node.Spec.Kind != review.KindCodeReview {
					continue
				}
				var out review.CodeReviewOut
				if err := json.Unmarshal(node.Output, &out); err != nil {
					t.Fatal(err)
				}
				gaps += len(out.ToolGaps)
				for _, gap := range out.ToolGaps {
					if strings.Contains(gap, projectMCPStub) || !strings.Contains(gap, "mcp__remote__echo") {
						t.Fatalf("gap does not name an effective binding: %s", gap)
					}
				}
			}
			if (gaps == 1) != tc.wantGap || (!tc.wantGap && gaps != 0) {
				t.Fatalf("gaps=%d, wantGap=%v", gaps, tc.wantGap)
			}
		})
	}
}
