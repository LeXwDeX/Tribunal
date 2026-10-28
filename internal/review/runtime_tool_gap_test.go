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
)

type unavailableCallError struct{}

func (unavailableCallError) Error() string         { return "secret server response" }
func (unavailableCallError) ToolUnavailable() bool { return true }

type canceledCallError struct{ error }

func (canceledCallError) ToolUnavailable() bool { return true }
func (e canceledCallError) Unwrap() error       { return e.error }

type unavailableTool struct{}

func (unavailableTool) Def() llm.ToolDef {
	return llm.ToolDef{Name: "mcp__codebase-memory__search_graph"}
}
func (unavailableTool) Call(context.Context, json.RawMessage) (agent.Result, error) {
	return agent.Result{}, unavailableCallError{}
}

type canceledTool struct{ unavailableTool }

func (canceledTool) Call(context.Context, json.RawMessage) (agent.Result, error) {
	return agent.Result{}, canceledCallError{fmt.Errorf("canceled: %w", context.Canceled)}
}

func TestRuntimeToolGapDoesNotRecordCancellation(t *testing.T) {
	var gaps []string
	tool := gapRecordingTool{Tool: canceledTool{}, block: BlockCodeReview, gaps: &gaps, mu: &sync.Mutex{}}
	if _, err := tool.Call(context.Background(), nil); err == nil || len(gaps) != 0 {
		t.Fatalf("cancellation: err=%v gaps=%v", err, gaps)
	}
}

type runtimeGapTools struct{ partial bool }

func (p runtimeGapTools) Tools(context.Context, string) ([]agent.Tool, error) {
	if p.partial {
		return nil, &PartialToolsError{Tools: []agent.Tool{unavailableTool{}}, Gaps: []string{"another tool missing"}}
	}
	return []agent.Tool{unavailableTool{}}, nil
}

func TestCodeReviewRecordsRuntimeToolGap(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			executor := newTestExecutor(t, ExecutorDeps{
				Dossier: validDossier(t), Blocks: graphBlocks, Tools: runtimeGapTools{partial: partial},
				AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
					for _, tool := range spec.Tools {
						if tool.Def().Name == "mcp__codebase-memory__search_graph" {
							if _, err := tool.Call(ctx, json.RawMessage(`{}`)); err == nil {
								t.Fatal("expected unavailable tool")
							}
						}
					}
					out, err := spec.Submit.Decode(json.RawMessage(`{"summary":"no issues","findings":[]}`))
					return &agent.RunResult{Output: out}, err
				},
			})
			out, err := executor.Execute(context.Background(), codeReviewInput(t))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeStrict[*CodeReviewOut](out.Data)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if partial {
				want++
			}
			if len(decoded.ToolGaps) != want || !strings.Contains(strings.Join(decoded.ToolGaps, "\n"), "search_graph") || strings.Contains(strings.Join(decoded.ToolGaps, "\n"), "secret") {
				t.Fatalf("runtime gaps = %q", decoded.ToolGaps)
			}
		})
	}
}
