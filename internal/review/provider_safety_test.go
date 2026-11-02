package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type truncatedSynthesisClient struct{}

func (truncatedSynthesisClient) Info() llm.ProviderInfo { return llm.ProviderInfo{} }
func (truncatedSynthesisClient) Generate(context.Context, *llm.Request) (*llm.Response, error) {
	return &llm.Response{StopReason: llm.StopMaxTokens, Message: llm.Message{Role: llm.RoleAssistant}, Usage: llm.Usage{InputTokens: 11, OutputTokens: 7}}, nil
}

func safetySynthesisInput() dag.NodeInput {
	params, _ := json.Marshal(synthesizeParams{Mode: "agent", Provider: "claude", OnUncertain: string(OnUncertainMark)})
	return dag.NodeInput{WorkflowID: "safety", Node: dag.NodeSpec{ID: NodeSynth, Kind: KindSynthesize, Params: params}, Attempt: 1,
		Inputs: map[string]json.RawMessage{"adjudicate": json.RawMessage(`{"groups":[]}`)}}
}

func TestSynthesisSafetyFailureDoesNotBecomeFallback(t *testing.T) {
	executor := evidenceAgentSynthesizer(t, func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		spec.Client = truncatedSynthesisClient{}
		return agent.Run(ctx, spec)
	})
	out, err := executor.Execute(context.Background(), safetySynthesisInput())
	if !errors.Is(err, agent.ErrUnsafeResponse) {
		t.Fatalf("truncated synthesis became a result: output=%s error=%v", out.Data, err)
	}
	var classified interface{ ErrorClass() string }
	if !errors.As(err, &classified) || classified.ErrorClass() != "unsafe_model_response" {
		t.Fatalf("unsafe error classification missing: %v", err)
	}
	if out.Usage.InputTokens != 11 || out.Usage.OutputTokens != 7 {
		t.Fatalf("synthesis lost usage: %v", err)
	}
}

func TestSynthesisFallbackDoesNotExposeValidationInput(t *testing.T) {
	const secret = "validation-secret-canary"
	executor := evidenceAgentSynthesizer(t, func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		_, err := spec.Submit.Decode(json.RawMessage(`{"` + secret + `":1}`))
		return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
	})
	out, err := executor.Execute(context.Background(), safetySynthesisInput())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out.Data), secret) {
		t.Fatal("synthesis fallback leaked input through validation error")
	}
}
