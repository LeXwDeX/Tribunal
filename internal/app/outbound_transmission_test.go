package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type blockedOutbound struct {
	entered, cancelled, release chan struct{}
	once                        sync.Once
}

func newBlockedOutbound(t *testing.T) *blockedOutbound {
	t.Helper()
	b := &blockedOutbound{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(b.unblock)
	return b
}

func (b *blockedOutbound) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *blockedOutbound) wait(ctx context.Context) {
	close(b.entered)
	select {
	case <-ctx.Done():
		close(b.cancelled)
		<-b.release // Simulate a client which reports success after cancellation.
	case <-b.release:
	}
}

func awaitOutbound(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

type blockedTurnClient struct {
	*blockedOutbound
	calls atomic.Int64
}

func (c *blockedTurnClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "fake", Model: "fixture"}
}

func (c *blockedTurnClient) Generate(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
	if c.calls.Add(1) == 1 {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "continue"}}}, StopReason: llm.StopEndTurn}, nil
	}
	c.wait(ctx)
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "submit", Name: "submit_result", Input: json.RawMessage(`{"summary":"ok","findings":[]}`)}}}}}, nil
}

func TestPolicyChangeCancelsRealSecondGenerate(t *testing.T) {
	f := newPolicyGateFixture(t)
	block := newBlockedOutbound(t)
	client := &blockedTurnClient{blockedOutbound: block}
	f.f.service.clients = staticProviderClients{"fake": client}
	f.f.service.agentRun = nil
	job := f.f.seedReview(t, "blocked-model")
	runDone := make(chan error, 1)
	go func() { runDone <- f.f.service.RunReview(context.Background(), job) }()
	t.Cleanup(func() { block.unblock(); <-runDone })
	awaitOutbound(t, block.entered, "second Generate")

	writeDone := make(chan error, 1)
	settings := json.RawMessage(`{"allow_repo_skills":false,"ignore":[]}`)
	go func() {
		_, err := f.projects.Update(context.Background(), 42, UpdateProjectInput{Settings: &settings})
		writeDone <- err
	}()
	select {
	case <-block.cancelled:
	case err := <-writeDone:
		t.Fatalf("policy write finished without cancelling Generate: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("policy write did not cancel Generate")
	}
	select {
	case err := <-writeDone:
		t.Fatalf("policy write finished before Generate returned: %v", err)
	default:
	}
	block.unblock()
	if err := <-writeDone; err != nil {
		t.Fatalf("policy update: %v", err)
	}
	runErr := <-runDone
	runDone <- runErr // Leave the result for cleanup.
	if !errors.Is(runErr, workspace.ErrOutboundInvalidated) {
		t.Fatalf("review returned %v", runErr)
	}
	if got := client.calls.Load(); got != 2 {
		t.Fatalf("Generate calls = %d, want 2", got)
	}
}

func TestPolicyChangeTimesOutWhileModelIgnoresCancellation(t *testing.T) {
	f := newPolicyGateFixture(t)
	block := newBlockedOutbound(t)
	client := &blockedTurnClient{blockedOutbound: block}
	f.f.service.clients = staticProviderClients{"fake": client}
	f.f.service.agentRun = nil
	job := f.f.seedReview(t, "unresponsive-model")
	runDone := make(chan error, 1)
	go func() { runDone <- f.f.service.RunReview(context.Background(), job) }()
	t.Cleanup(func() { block.unblock(); <-runDone })
	awaitOutbound(t, block.entered, "second Generate")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	settings := json.RawMessage(`{"allow_repo_skills":false,"ignore":[]}`)
	_, err := f.projects.Update(ctx, 42, UpdateProjectInput{Settings: &settings})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unresponsive model policy write = %v", err)
	}
	awaitOutbound(t, block.cancelled, "model cancellation")
	project, err := f.f.db.GetProject(context.Background(), 42)
	if err != nil || string(project.Settings) != `{"allow_repo_skills":true,"ignore":[]}` {
		t.Fatalf("timed-out update committed: %+v, %v", project, err)
	}
	block.unblock()
	runErr := <-runDone
	runDone <- runErr
	if !errors.Is(runErr, workspace.ErrOutboundInvalidated) || client.calls.Load() != 2 {
		t.Fatalf("review after timed-out update = %v, calls=%d", runErr, client.calls.Load())
	}
}

func TestRevocationDrainsRealGenerateWithoutContentLockInversion(t *testing.T) {
	f := newPolicyGateFixture(t)
	block := newBlockedOutbound(t)
	f.f.service.clients = staticProviderClients{"fake": &blockedTurnClient{blockedOutbound: block}}
	f.f.service.agentRun = nil
	job := f.f.seedReview(t, "revoke-blocked-model")
	runDone := make(chan error, 1)
	go func() { runDone <- f.f.service.RunReview(context.Background(), job) }()
	t.Cleanup(func() { block.unblock(); <-runDone })
	awaitOutbound(t, block.entered, "second Generate")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		grant := *f.grant
		grant.Skills = nil
		_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, grant.Revision, "", "", 7)
		done <- err
	}()
	awaitOutbound(t, block.cancelled, "model cancellation on revocation")
	select {
	case err := <-done:
		t.Fatalf("revocation finished before the send returned: %v", err)
	default:
	}
	block.unblock()
	runErr := <-runDone
	runDone <- runErr
	if !errors.Is(runErr, workspace.ErrOutboundInvalidated) {
		t.Fatalf("review returned %v", runErr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("revocation after drain: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("revocation deadlocked waiting for the review content lock")
	}
}

type mcpCallingClient struct{ calls atomic.Int64 }

func (c *mcpCallingClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "fake", Model: "fixture"}
}
func (c *mcpCallingClient) Generate(context.Context, *llm.Request) (*llm.Response, error) {
	c.calls.Add(1)
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "mcp", Name: "mcp__remote__search", Input: json.RawMessage(`{"query":"repository-derived"}`)}}}}}, nil
}

func TestPolicyChangeCancelsActualMCPCallTool(t *testing.T) {
	f := newPolicyGateFixture(t)
	block := newBlockedOutbound(t)
	server := sdk.NewServer(&sdk.Implementation{Name: "outbound-test", Version: "1"}, nil)
	var calls atomic.Int64
	server.AddTool(&sdk.Tool{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.Add(1)
		if string(req.Params.Arguments) != `{"query":"repository-derived"}` {
			return nil, errors.New("unexpected MCP input")
		}
		block.wait(ctx)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "result"}}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	t.Cleanup(block.unblock)
	manager, err := mcp.New(mcp.Options{Workspace: t.TempDir(), Platform: mcp.Config{MCPServers: map[string]mcp.ServerConfig{"remote": {URL: httpServer.URL + "/mcp"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	blocks := map[string]config.BlockBinding{"code-review": {MCPTools: []string{"mcp__remote__search"}}}
	f.f.service.blocks = blocks
	f.f.service.tools = NewMCPToolProvider(manager, blocks)
	f.f.defaults.Ignore = []string{}
	f.f.service.reviewDefaults = f.f.defaults
	f.f.publisher.reviewDefaults = f.f.defaults
	if _, err := f.f.db.SaveWorkspace(context.Background(), store.Workspace{ProjectID: 42, State: "ready", IndexSHA: f.f.gitlab.version.StartSHA}); err != nil {
		t.Fatal(err)
	}
	client := &mcpCallingClient{}
	f.f.service.clients = staticProviderClients{"fake": client}
	f.f.service.agentRun = nil
	job := f.f.seedReview(t, "blocked-mcp")
	runDone := make(chan error, 1)
	go func() { runDone <- f.f.service.RunReview(context.Background(), job) }()
	t.Cleanup(func() { block.unblock(); <-runDone })
	awaitOutbound(t, block.entered, "MCP CallTool")
	settings := json.RawMessage(`{"allow_repo_skills":false,"ignore":[]}`)
	writeDone := make(chan error, 1)
	go func() {
		_, err := f.projects.Update(context.Background(), 42, UpdateProjectInput{Settings: &settings})
		writeDone <- err
	}()
	awaitOutbound(t, block.cancelled, "MCP request cancellation")
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	runErr := <-runDone
	runDone <- runErr
	if !errors.Is(runErr, workspace.ErrOutboundInvalidated) || calls.Load() != 1 || client.calls.Load() != 1 {
		t.Fatalf("review = %v, MCP calls=%d, model calls=%d", runErr, calls.Load(), client.calls.Load())
	}
}

type blockedSummaryGitLab struct {
	*reviewGitLab
	*blockedOutbound
	calls atomic.Int64
}

func (g *blockedSummaryGitLab) CreateMRNote(ctx context.Context, _ string, _, _ int64, _ string) (*gitlab.Note, error) {
	g.calls.Add(1)
	g.wait(ctx)
	return &gitlab.Note{ID: 123}, nil
}

func TestPolicyChangeCancelsPublicationEntrypoints(t *testing.T) {
	for _, entry := range []string{"publish", "republish", "pending", "queued-republish"} {
		t.Run(entry, func(t *testing.T) {
			f := newPolicyGateFixture(t)
			wf := reviewWithFailedNote(t, f.f, "blocked-publication")
			block := newBlockedOutbound(t)
			api := &blockedSummaryGitLab{reviewGitLab: f.f.gitlab, blockedOutbound: block}
			f.f.publisher.gitlab = api
			var job *store.Job
			if entry == "queued-republish" {
				id, err := f.dispatcher.RepublishReview(context.Background(), wf.ID)
				if err != nil {
					t.Fatal(err)
				}
				job, err = f.f.db.GetJob(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				switch entry {
				case "publish":
					done <- f.f.publisher.Publish(context.Background(), wf.ID)
				case "republish":
					done <- f.f.publisher.Republish(context.Background(), wf.ID)
				case "queued-republish":
					done <- f.f.service.RepublishReview(context.Background(), job)
				default:
					done <- f.f.service.PublishPending(context.Background())
				}
			}()
			t.Cleanup(func() { block.unblock(); <-done })
			awaitOutbound(t, block.entered, "GitLab summary send")
			settings := json.RawMessage(`{"allow_repo_skills":false,"ignore":[]}`)
			writeDone := make(chan error, 1)
			go func() {
				_, err := f.projects.Update(context.Background(), 42, UpdateProjectInput{Settings: &settings})
				writeDone <- err
			}()
			select {
			case <-block.cancelled:
			case err := <-writeDone:
				t.Fatalf("policy write finished without cancelling publication: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("policy write did not cancel publication")
			}
			select {
			case err := <-writeDone:
				t.Fatalf("policy write completed before GitLab returned: %v", err)
			default:
			}
			other := f.f.manager.Get(43)
			otherToken, err := other.OutboundToken()
			if err != nil || other.SendOutbound(context.Background(), otherToken, func(context.Context) error { return nil }) != nil {
				t.Fatalf("another project was blocked: %v", err)
			}
			block.unblock()
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			// The transport ignored cancellation and returned success. It must not
			// produce a successful publication record or another send.
			result := <-done
			done <- result // Leave the result for cleanup.
			if !errors.Is(result, workspace.ErrOutboundInvalidated) {
				t.Fatalf("publication returned %v", result)
			}
			if api.calls.Load() != 1 {
				t.Fatalf("publication attempts = %d", api.calls.Load())
			}
			record, err := f.f.db.GetWorkflow(context.Background(), wf.ID)
			if err != nil || record.PublicationState == store.WorkflowPublicationPublished {
				t.Fatalf("publication state after cancellation = %+v, %v", record, err)
			}
		})
	}
}

type retryBarrierGitLab struct {
	*reviewGitLab
	scans, creates atomic.Int64
	between        chan struct{}
	release        chan struct{}
}

func (g *retryBarrierGitLab) ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error) {
	if g.scans.Add(1) == 2 {
		close(g.between)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.reviewGitLab.ListMRNotes(ctx, token, projectID, iid)
}

func (g *retryBarrierGitLab) CreateMRNote(context.Context, string, int64, int64, string) (*gitlab.Note, error) {
	g.creates.Add(1)
	return nil, errSummaryTransport
}

func TestPublicationRetryRejectsPolicyChangedBetweenAttempts(t *testing.T) {
	f := newPolicyGateFixture(t)
	wf := reviewWithFailedNote(t, f.f, "policy-retry")
	api := &retryBarrierGitLab{reviewGitLab: f.f.gitlab, between: make(chan struct{}), release: make(chan struct{})}
	f.f.publisher.gitlab = api
	done := make(chan error, 1)
	go func() { done <- f.f.publisher.Publish(context.Background(), wf.ID) }()
	var once sync.Once
	release := func() { once.Do(func() { close(api.release) }) }
	t.Cleanup(func() { release(); <-done })
	awaitOutbound(t, api.between, "publication retry")
	settings := json.RawMessage(`{"allow_repo_skills":false,"ignore":[]}`)
	if _, err := f.projects.Update(context.Background(), 42, UpdateProjectInput{Settings: &settings}); err != nil {
		t.Fatal(err)
	}
	release()
	err := <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) || api.creates.Load() != 1 {
		t.Fatalf("publication retry = %v, sends=%d", err, api.creates.Load())
	}
}

type blockedPublicationGitLab struct {
	*reviewGitLab
	*blockedOutbound
	stage string
	calls atomic.Int64
}

func (g *blockedPublicationGitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	if g.stage != "update" {
		return g.reviewGitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
	}
	g.calls.Add(1)
	g.wait(ctx)
	return nil
}

func (g *blockedPublicationGitLab) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	if g.stage != "discussion" {
		return g.reviewGitLab.CreateDiscussion(ctx, token, projectID, iid, d)
	}
	g.calls.Add(1)
	g.wait(ctx)
	return &gitlab.Discussion{ID: "accepted"}, nil
}

func (g *blockedPublicationGitLab) SetCommitStatus(ctx context.Context, token string, projectID int64, sha string, status gitlab.CommitStatus) error {
	if g.stage != "status" {
		return g.reviewGitLab.SetCommitStatus(ctx, token, projectID, sha, status)
	}
	g.calls.Add(1)
	g.wait(ctx)
	return nil
}

func TestPolicyChangeCancelsOtherPublicationWrites(t *testing.T) {
	for _, stage := range []string{"update", "discussion", "status"} {
		t.Run(stage, func(t *testing.T) {
			f := newPolicyGateFixture(t)
			if stage == "status" {
				f.f.defaults.CommitStatus = true
				f.f.service.reviewDefaults = f.f.defaults
				f.f.publisher.reviewDefaults = f.f.defaults
			}
			wf := reviewWithFailedNote(t, f.f, "policy-"+stage)
			if stage == "update" {
				if err := f.f.publisher.Publish(context.Background(), wf.ID); err != nil {
					t.Fatal(err)
				}
			}
			block := newBlockedOutbound(t)
			api := &blockedPublicationGitLab{reviewGitLab: f.f.gitlab, blockedOutbound: block, stage: stage}
			f.f.publisher.gitlab = api
			done := make(chan error, 1)
			go func() { done <- f.f.publisher.Republish(context.Background(), wf.ID) }()
			t.Cleanup(func() { block.unblock(); <-done })
			awaitOutbound(t, block.entered, "GitLab "+stage)
			settings := json.RawMessage(`{"allow_repo_skills":false,"ignore":[]}`)
			writeDone := make(chan error, 1)
			go func() {
				_, err := f.projects.Update(context.Background(), 42, UpdateProjectInput{Settings: &settings})
				writeDone <- err
			}()
			awaitOutbound(t, block.cancelled, "GitLab "+stage+" cancellation")
			select {
			case err := <-writeDone:
				t.Fatalf("policy wrote before GitLab returned: %v", err)
			default:
			}
			block.unblock()
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			err := <-done
			done <- err
			if !errors.Is(err, workspace.ErrOutboundInvalidated) || api.calls.Load() != 1 {
				t.Fatalf("%s result = %v, calls=%d", stage, err, api.calls.Load())
			}
		})
	}
}

type blockedStatusGitLab struct {
	*reviewGitLab
	*blockedOutbound
	stage    string
	err      error
	addCalls atomic.Int64
}

func (g *blockedStatusGitLab) CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error) {
	if g.stage != "status" {
		return g.reviewGitLab.CreateMRNote(ctx, token, projectID, iid, body)
	}
	g.wait(ctx)
	return &gitlab.Note{ID: 123}, g.err
}

func (g *blockedStatusGitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	if g.stage != "status-update" {
		return g.reviewGitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
	}
	g.wait(ctx)
	return g.err
}

func (g *blockedStatusGitLab) AddNoteAward(ctx context.Context, token string, projectID, iid, noteID int64, name string) (*gitlab.Award, error) {
	g.addCalls.Add(1)
	if g.stage != "reaction" {
		return g.reviewGitLab.AddNoteAward(ctx, token, projectID, iid, noteID, name)
	}
	g.wait(ctx)
	return &gitlab.Award{ID: 123, Name: name}, g.err
}

func (g *blockedStatusGitLab) DeleteNoteAward(ctx context.Context, token string, projectID, iid, noteID, awardID int64) error {
	if g.stage != "reaction-delete" {
		return g.reviewGitLab.DeleteNoteAward(ctx, token, projectID, iid, noteID, awardID)
	}
	g.wait(ctx)
	return g.err
}

func TestPolicyChangeCancelsStatusAndReactionWrites(t *testing.T) {
	for _, tc := range []struct {
		name, kind, stage string
		err               error
	}{
		{"status-success", "status", "status", nil},
		{"status-not-found", "status", "status", &gitlab.Error{Op: "create_note", Status: 404}},
		{"status-update-not-found", "status", "status-update", &gitlab.Error{Op: "update_note", Status: 404}},
		{"reaction-success", "reaction", "reaction", nil},
		{"reaction-conflict", "reaction", "reaction", &gitlab.Error{Op: "add_award", Status: 409}},
		{"reaction-delete-not-found", "reaction", "reaction-delete", &gitlab.Error{Op: "delete_award", Status: 404}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := tc.kind
			f := newPolicyGateFixture(t)
			reviewJob := f.f.seedReview(t, "side-effect-policy")
			generation := jobGeneration(t, reviewJob)
			if stage == "status" {
				f.f.service.setReviewStatus(context.Background(), 42, 7, generation, 1, f.f.head, "default", reviewStatusPass, "")
			} else {
				f.f.service.setReviewReaction(context.Background(), 42, 7, 555, generation, reviewAwardPass)
			}
			job, err := f.f.db.Claim(context.Background(), "outbound-test", []string{"review." + stage}, time.Minute)
			if err != nil || job == nil {
				t.Fatalf("claim %s = %+v, %v", stage, job, err)
			}
			if tc.stage == "status-update" {
				f.f.gitlab.notes = []gitlab.Note{{ID: 88, AuthorID: f.f.gitlab.currentUser.ID, Body: reviewStatusMarkerLine + "\nold"}}
			}
			if tc.stage == "reaction-delete" {
				f.f.gitlab.awards[555] = []gitlab.Award{{ID: 88, UserID: f.f.gitlab.currentUser.ID, Name: reviewAwardEyes}}
			}
			block := newBlockedOutbound(t)
			api := &blockedStatusGitLab{reviewGitLab: f.f.gitlab, blockedOutbound: block, stage: tc.stage, err: tc.err}
			f.f.service.status = newStatusReporter(api, nil)
			done := make(chan error, 1)
			go func() { done <- f.f.service.Handle(context.Background(), job) }()
			t.Cleanup(func() { block.unblock(); <-done })
			awaitOutbound(t, block.entered, "GitLab "+stage)
			settings := json.RawMessage(`{"allow_repo_skills":false,"ignore":[]}`)
			writeDone := make(chan error, 1)
			go func() {
				_, err := f.projects.Update(context.Background(), 42, UpdateProjectInput{Settings: &settings})
				writeDone <- err
			}()
			select {
			case <-block.cancelled:
			case err := <-writeDone:
				t.Fatalf("policy wrote without cancelling %s: %v", stage, err)
			case <-time.After(3 * time.Second):
				t.Fatalf("policy did not cancel %s", stage)
			}
			select {
			case err := <-writeDone:
				t.Fatalf("policy wrote before %s returned: %v", stage, err)
			default:
			}
			block.unblock()
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			err = <-done
			done <- err
			if !errors.Is(err, workspace.ErrOutboundInvalidated) {
				t.Fatalf("%s reported success after cancellation: %v", stage, err)
			}
			if tc.stage == "reaction-delete" && api.addCalls.Load() != 0 {
				t.Fatal("reaction sent a second write after invalidation")
			}
			if stage == "status" {
				row, err := f.f.db.GetReviewStatus(context.Background(), 42, 7)
				if err != nil || row.AppliedVersion >= row.Version {
					t.Fatalf("canceled status marked applied: %+v, %v", row, err)
				}
			} else {
				row, err := f.f.db.GetReviewReaction(context.Background(), 42, 7, 555)
				if err != nil || row.AppliedVersion >= row.Version {
					t.Fatalf("canceled reaction marked applied: %+v, %v", row, err)
				}
			}
		})
	}
}
