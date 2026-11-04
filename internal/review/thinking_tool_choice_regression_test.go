package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// thinkingGatewayClient models a reasoning gateway (#255): any non-auto
// tool_choice is rejected with HTTP 400, and the model keeps exploring with
// tools until a trailing user message asks it to call submit_result.
type thinkingGatewayClient struct {
	mu       sync.Mutex
	submit   json.RawMessage
	requests int
	forced   []string
}

func (c *thinkingGatewayClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "thinking", Model: "thinking-model"}
}

func (c *thinkingGatewayClient) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	if req.ToolChoice != "" && req.ToolChoice != "auto" {
		c.forced = append(c.forced, req.ToolChoice)
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Status: 400}
	}
	usage := llm.Usage{InputTokens: 10, OutputTokens: 2}
	if n := len(req.Messages); n > 1 {
		last := req.Messages[n-1]
		if last.Role == llm.RoleUser && len(last.Parts) > 0 && strings.Contains(last.Parts[0].Text, agent.SubmitToolName) {
			return &llm.Response{StopReason: llm.StopToolUse, Usage: usage, Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
				Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "submit", Name: agent.SubmitToolName, Input: c.submit},
			}}}}, nil
		}
	}
	return &llm.Response{StopReason: llm.StopToolUse, Usage: usage, Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: fmt.Sprintf("explore-%d", c.requests), Name: "explore", Input: json.RawMessage(`{}`)},
	}}}}, nil
}

// The fake gateway takes no time and its input does not grow, so this test
// proves only the tool_choice and turn-limit behavior. In production the node
// timeout, token budget or context window can still end a run first; the loop
// then relies on its deadline-based submit prompt (agent package tests).
func TestOnboardingSubmitsOnFinalTurnBehindThinkingGateway(t *testing.T) {
	submit, err := json.Marshal(OnboardingOut{
		Overview:     "overview",
		Architecture: "architecture",
		Modules:      []Module{{Path: "internal", Purpose: "core"}},
		Conventions:  []string{"testing"},
		BuildAndTest: []string{"go test ./..."},
		RiskHotspots: []string{"none"},
		ReviewFocus:  []string{"contracts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &thinkingGatewayClient{submit: submit}
	executor := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), Clients: metadataClients{"thinking": client}})
	out, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf-255", Node: dag.NodeSpec{
		ID: KindOnboarding, Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"thinking"}`),
	}})
	client.mu.Lock()
	requests, forced := client.requests, append([]string(nil), client.forced...)
	client.mu.Unlock()
	if err != nil {
		t.Fatalf("onboarding failed behind a thinking gateway: err=%v requests=%d forced tool_choice=%q", err, requests, forced)
	}
	if len(forced) != 0 {
		t.Fatalf("forced tool_choice sent %d times: %q", len(forced), forced)
	}
	if requests != onboardingMaxTurns {
		t.Fatalf("requests = %d, want the onboarding turn limit %d", requests, onboardingMaxTurns)
	}
	decoded, err := DecodeStrict[*OnboardingOut](out.Data)
	if err != nil || decoded.Overview != "overview" {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
}

func TestAgentNodeTurnLimits(t *testing.T) {
	var onboardingTurns int
	onboarding := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		onboardingTurns = spec.MaxTurns
		return &agent.RunResult{Output: &OnboardingOut{Overview: "ok"}}, nil
	}})
	if _, err := onboarding.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf", Node: dag.NodeSpec{
		ID: KindOnboarding, Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`),
	}}); err != nil || onboardingTurns != onboardingMaxTurns {
		t.Fatalf("onboarding max turns = %d, want %d (err=%v)", onboardingTurns, onboardingMaxTurns, err)
	}

	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	adjudicated, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	wf := dag.WorkflowView{ID: "wf", Spec: spec, Nodes: map[string]dag.NodeView{
		NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicated},
		NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning},
	}, Order: []string{NodeAdjudicate, NodeSynth}}
	synthTurns := -1
	synth := newTestExecutor(t, ExecutorDeps{Dossier: dossier, Profile: profile, Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }, AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		synthTurns = spec.MaxTurns
		return nil, &llm.Error{Kind: llm.ErrBadRequest}
	}})
	_, _ = synth.Execute(context.Background(), dag.NodeInput{WorkflowID: wf.ID, Node: mustNode(t, spec, NodeSynth)})
	if synthTurns != 0 {
		t.Fatalf("synthesize max turns = %d, want 0 (agent.DefaultMaxTurns)", synthTurns)
	}
}
