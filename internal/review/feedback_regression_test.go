package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const feedbackReviewCanary = "feedback-review-secret-canary"

type feedbackRegressionModel struct {
	inputs   []string
	requests []*llm.Request
}

func (m *feedbackRegressionModel) Info() llm.ProviderInfo { return llm.ProviderInfo{} }

func (m *feedbackRegressionModel) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.requests = append(m.requests, req)
	input := m.inputs[len(m.requests)-1]
	return &llm.Response{StopReason: llm.StopToolUse, Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "submit", Name: agent.SubmitToolName, Input: json.RawMessage(input)},
	}}}}, nil
}

func TestFeedbackRegressionReviewSubmitRecovery(t *testing.T) {
	validPlan := `{"risk":"low","risk_reasons":["risk"],"packages":[{"id":"p","blocks":["review"],"files":["a.go"],"focus":"focus","provider":"allowed"}],"run_reasoner":false,"challenge_only":["P1"]}`
	validReview := `{"summary":"ok","findings":[]}`
	validSynthesis := `{"verdict":"BLOCKING","summary":"ok","decisions":[],"new_findings":[],"extend_requests":[]}`
	policy := PlanPolicy{AllowedBlocks: []string{"review"}, AllowedProviders: []string{"allowed"}, ChangedFiles: []string{"a.go"}}
	synth := SynthesisContext{WorkflowID: "wf", NodeID: "synthesize.1", Groups: []Group{{
		ID: feedbackReviewCanary, Severity: SeverityP1, Sources: []Finding{{}}, Decision: &FindingDecision{Action: ActionPublish},
	}}}
	for _, tc := range []struct {
		name, bad, good, field, category string
		decode                           func(json.RawMessage) (any, error)
	}{
		{"required", `{"findings":[]}`, validReview, "summary", "required", func(raw json.RawMessage) (any, error) {
			return DecodeStrict[*CodeReviewOut](raw)
		}},
		{"nested-field", `{"summary":"ok","findings":[{"path":"","severity":"` + feedbackReviewCanary + `","category":"bug","title":"title","body":"body","evidence":"evidence","confidence":"high"}]}`, validReview, "findings[].severity", "value", func(raw json.RawMessage) (any, error) {
			return DecodeStrict[*CodeReviewOut](raw)
		}},
		{"type", `{"summary":"ok","findings":[{"path":"","line":"` + feedbackReviewCanary + `","severity":"P1","category":"bug","title":"title","body":"body","evidence":"evidence","confidence":"high"}]}`, validReview, "findings[].line", "type", func(raw json.RawMessage) (any, error) {
			return DecodeStrict[*CodeReviewOut](raw)
		}},
		{"null", `{"summary":null,"findings":[]}`, validReview, "summary", "null", func(raw json.RawMessage) (any, error) {
			return DecodeStrict[*CodeReviewOut](raw)
		}},
		{"unknown-field", `{"summary":"ok","findings":[],"` + feedbackReviewCanary + `":true}`, validReview, "", "schema", func(raw json.RawMessage) (any, error) {
			return DecodeStrict[*CodeReviewOut](raw)
		}},
		{"provider-policy", strings.Replace(validPlan, `"allowed"`, `"`+feedbackReviewCanary+`"`, 1), validPlan, "packages[].provider", "not authorized", func(raw json.RawMessage) (any, error) {
			out, err := DecodeStrict[*PlanOut](raw)
			if err != nil {
				return nil, err
			}
			return out, ValidatePlan(*out, policy)
		}},
		{"synthesis-policy", strings.Replace(validSynthesis, `"BLOCKING"`, `"PASS"`, 1), validSynthesis, "verdict", "PASS", func(raw json.RawMessage) (any, error) {
			out, err := DecodeStrict[*SynthesizeOut](raw)
			if err != nil {
				return nil, err
			}
			return out, ValidateSynthesis(*out, synth)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &feedbackRegressionModel{inputs: []string{tc.bad, tc.good}}
			result, err := agent.Run(context.Background(), agent.RunSpec{Client: model, Prompt: "review", Submit: agent.SubmitSpec{Decode: tc.decode}})
			if err != nil || result == nil || result.Turns != 2 {
				t.Fatalf("corrected submit failed: result=%+v err=%v", result, err)
			}
			got := model.requests[1].Messages[2].Parts[0].ToolResult
			if got == nil || !got.IsError || !strings.Contains(got.Content, tc.field) || !strings.Contains(got.Content, tc.category) || strings.Contains(got.Content, feedbackReviewCanary) {
				t.Fatalf("unsafe or unactionable submit feedback: %+v; want %q and %q", got, tc.field, tc.category)
			}
		})
	}
}

type feedbackRegressionMCPModel struct {
	t        *testing.T
	requests int
}

func (m *feedbackRegressionMCPModel) Info() llm.ProviderInfo { return llm.ProviderInfo{} }

func (m *feedbackRegressionMCPModel) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.requests++
	name, input := "mcp__remote__inspect", `{}`
	if m.requests == 2 {
		got := req.Messages[2].Parts[0].ToolResult
		if got == nil || !got.IsError || strings.Contains(got.Content, feedbackReviewCanary) {
			m.t.Fatalf("MCP error content leaked or lost error flag: %+v", got)
		}
		name, input = agent.SubmitToolName, `{"summary":"ok","findings":[]}`
	} else if m.requests != 1 {
		m.t.Fatal("unexpected request")
	}
	return &llm.Response{StopReason: llm.StopToolUse, Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: name, Name: name, Input: json.RawMessage(input)},
	}}}}, nil
}

func TestFeedbackRegressionActualMCPErrorResult(t *testing.T) {
	tool := localReviewMCP(t, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: feedbackReviewCanary}}}, nil
	}, time.Second)
	model := &feedbackRegressionMCPModel{t: t}
	result, err := agent.Run(context.Background(), agent.RunSpec{
		Client: model, Prompt: "review", Tools: []agent.Tool{tool}, Submit: agent.SubmitSpec{
			Decode: func(raw json.RawMessage) (any, error) { return DecodeStrict[*CodeReviewOut](raw) },
		},
	})
	if err != nil || result == nil || model.requests != 2 {
		t.Fatalf("MCP error recovery failed: result=%+v err=%v", result, err)
	}
}
