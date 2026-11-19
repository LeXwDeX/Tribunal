package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

const (
	gapToolA = "mcp__codebase-memory__search_graph"
	gapToolB = "mcp__codebase-memory__trace_path"
)

var interruptBlocks = map[string]config.BlockBinding{
	BlockCodeReview: {MCPTools: []string{gapToolA, gapToolB}},
}

// interruptedCallError is a call cut off by a dropped server connection that
// the server recovers from, the way the MCP layer reports it: an unavailable
// tool that carries a retry hint for the agent.
type interruptedCallError struct{}

func (interruptedCallError) Error() string              { return "secret server response" }
func (interruptedCallError) ToolUnavailable() bool      { return true }
func (interruptedCallError) ToolInterrupted() bool      { return true }
func (interruptedCallError) AgentRetryFeedback() string { return "retry once" }

// callOutcome is what the agent loop sees of one tool call.
type callOutcome string

const (
	outcomeOK      callOutcome = "ok"      // no error
	outcomeHint    callOutcome = "hint"    // an interruption that carries the retry hint
	outcomeGeneric callOutcome = "generic" // an unavailable tool without the hint: the loop shows its generic feedback
	outcomeOther   callOutcome = "other"   // anything else, such as a hint on an error that is not marked interrupted
)

func outcomeOf(err error) callOutcome {
	if err == nil {
		return outcomeOK
	}
	var unavailable interface{ ToolUnavailable() bool }
	var interrupted interface{ ToolInterrupted() bool }
	var retry interface{ AgentRetryFeedback() string }
	isUnavailable := errors.As(err, &unavailable) && unavailable.ToolUnavailable()
	hasMarker, hasHint := errors.As(err, &interrupted), errors.As(err, &retry)
	switch {
	case !isUnavailable || hasMarker != hasHint:
		return outcomeOther
	case hasHint:
		return outcomeHint
	default:
		return outcomeGeneric
	}
}

// scriptedGapTool answers its calls from a script. A nil entry, and every call
// past the end of the script, succeeds.
type scriptedGapTool struct {
	name   string
	script []error
	mu     *sync.Mutex
	calls  *int
}

func scripted(name string, script ...error) scriptedGapTool {
	return scriptedGapTool{name: name, script: script, mu: &sync.Mutex{}, calls: new(int)}
}

func (s scriptedGapTool) Def() llm.ToolDef { return llm.ToolDef{Name: s.name} }

func (s scriptedGapTool) Call(context.Context, json.RawMessage) (agent.Result, error) {
	s.mu.Lock()
	i := *s.calls
	*s.calls++
	s.mu.Unlock()
	if i < len(s.script) && s.script[i] != nil {
		return agent.Result{}, s.script[i]
	}
	return agent.Result{Content: "ok"}, nil
}

type scriptedGapTools []agent.Tool

func (p scriptedGapTools) Tools(context.Context, string) ([]agent.Tool, error) { return p, nil }

// gapRun is one callAgent invocation whose agent loop calls the named tools in
// order and then returns finish.
type gapRun struct {
	tools    []agent.Tool
	calls    []string
	finish   error
	canceled bool // the loop calls its tools with a context that is already canceled
	workers  int  // each of this many goroutines runs all calls; zero means one
}

type gapRunResult struct {
	midRun   []string      // gaps visible to the agent loop after its calls
	gaps     []string      // gaps once callAgent has returned
	outcomes []callOutcome // what the loop saw of each call, in call order for one worker
	err      error
}

func (r gapRun) execute(t *testing.T) gapRunResult {
	t.Helper()
	var out gapRunResult
	var gaps []string
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier: validDossier(t), Blocks: interruptBlocks, Tools: scriptedGapTools(r.tools),
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			byName := make(map[string]agent.Tool, len(spec.Tools))
			for _, tool := range spec.Tools {
				byName[tool.Def().Name] = tool
			}
			callCtx := ctx
			if r.canceled {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var mu sync.Mutex
			var wg sync.WaitGroup
			for range max(r.workers, 1) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for _, name := range r.calls {
						tool, ok := byName[name]
						if !ok {
							t.Errorf("agent did not receive tool %q", name)
							continue
						}
						_, err := tool.Call(callCtx, json.RawMessage(`{}`))
						mu.Lock()
						out.outcomes = append(out.outcomes, outcomeOf(err))
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			out.midRun = slices.Clone(gaps)
			return &agent.RunResult{}, r.finish
		},
	})
	role, _ := roleForKind(KindCodeReview)
	_, out.err = executor.callAgent(context.Background(), dag.NodeInput{}, agentOptions{
		Role: role, Provider: "test", Block: BlockCodeReview, BlockTools: true, NoRetry: true,
		Task:     "review",
		Submit:   agent.SubmitSpec{Schema: json.RawMessage(`{}`), Decode: func(json.RawMessage) (any, error) { return nil, nil }},
		ToolGaps: &gaps,
	})
	out.gaps = gaps
	return out
}

func gapFor(tool string) string {
	return toolGapMessage(BlockCodeReview, fmt.Errorf("tool %q became unavailable", tool))
}

func repeat(name string, n int) []string {
	calls := make([]string, n)
	for i := range calls {
		calls[i] = name
	}
	return calls
}

// An interruption earns the retry hint once in a row, and at most twice in a
// node: the tool is expected to come back, not to be retried forever. A hint
// that is withheld leaves the gap behind at once, and a tool never gets more
// than one gap for its interruptions.
func TestInterruptedToolGapIsDeferredUntilNoLaterCallSucceeds(t *testing.T) {
	interrupted, unavailable := error(interruptedCallError{}), error(unavailableCallError{})
	for _, tc := range []struct {
		name         string
		tools        []agent.Tool
		calls        []string
		wantOutcomes []callOutcome
		wantMid      []string
		wantFinal    []string
	}{
		{
			name:         "interrupted then success of the same tool",
			tools:        []agent.Tool{scripted(gapToolA, interrupted)},
			calls:        []string{gapToolA, gapToolA},
			wantOutcomes: []callOutcome{outcomeHint, outcomeOK},
		},
		{
			name:         "interrupted and never retried",
			tools:        []agent.Tool{scripted(gapToolA, interrupted)},
			calls:        []string{gapToolA},
			wantOutcomes: []callOutcome{outcomeHint},
			wantFinal:    []string{gapFor(gapToolA)},
		},
		{
			name:         "the second interruption in a row loses the hint and records the gap at once",
			tools:        []agent.Tool{scripted(gapToolA, interrupted, interrupted)},
			calls:        []string{gapToolA, gapToolA},
			wantOutcomes: []callOutcome{outcomeHint, outcomeGeneric},
			wantMid:      []string{gapFor(gapToolA)},
			wantFinal:    []string{gapFor(gapToolA)},
		},
		{
			name:         "a tool that crashes on every call gets one hint and one gap",
			tools:        []agent.Tool{scripted(gapToolA, interrupted, interrupted, interrupted, interrupted)},
			calls:        repeat(gapToolA, 4),
			wantOutcomes: []callOutcome{outcomeHint, outcomeGeneric, outcomeGeneric, outcomeGeneric},
			wantMid:      []string{gapFor(gapToolA)},
			wantFinal:    []string{gapFor(gapToolA)},
		},
		{
			name:         "interrupted again after a success",
			tools:        []agent.Tool{scripted(gapToolA, interrupted, nil, interrupted)},
			calls:        repeat(gapToolA, 3),
			wantOutcomes: []callOutcome{outcomeHint, outcomeOK, outcomeHint},
			wantFinal:    []string{gapFor(gapToolA)},
		},
		{
			name:         "alternating interruptions stop earning the hint at the third",
			tools:        []agent.Tool{scripted(gapToolA, interrupted, nil, interrupted, nil, interrupted, nil, interrupted)},
			calls:        repeat(gapToolA, 7),
			wantOutcomes: []callOutcome{outcomeHint, outcomeOK, outcomeHint, outcomeOK, outcomeGeneric, outcomeOK, outcomeGeneric},
			wantMid:      []string{gapFor(gapToolA)},
			wantFinal:    []string{gapFor(gapToolA)},
		},
		{
			name:         "success of another tool does not clear the gap",
			tools:        []agent.Tool{scripted(gapToolA, interrupted), scripted(gapToolB)},
			calls:        []string{gapToolA, gapToolB},
			wantOutcomes: []callOutcome{outcomeHint, outcomeOK},
			wantFinal:    []string{gapFor(gapToolA)},
		},
		{
			name:         "each tool is counted on its own and gets one gap in the order it ran out of hints",
			tools:        []agent.Tool{scripted(gapToolA, interrupted, interrupted), scripted(gapToolB, interrupted, interrupted)},
			calls:        []string{gapToolB, gapToolA, gapToolB, gapToolA},
			wantOutcomes: []callOutcome{outcomeHint, outcomeHint, outcomeGeneric, outcomeGeneric},
			wantMid:      []string{gapFor(gapToolB), gapFor(gapToolA)},
			wantFinal:    []string{gapFor(gapToolB), gapFor(gapToolA)},
		},
		{
			name:         "an unavailable tool still records its gap at once and keeps it after a success",
			tools:        []agent.Tool{scripted(gapToolA, unavailable)},
			calls:        []string{gapToolA, gapToolA},
			wantOutcomes: []callOutcome{outcomeGeneric, outcomeOK},
			wantMid:      []string{gapFor(gapToolA)},
			wantFinal:    []string{gapFor(gapToolA)},
		},
		{
			name:         "an interruption followed by an unavailable call leaves one gap",
			tools:        []agent.Tool{scripted(gapToolA, interrupted, unavailable)},
			calls:        []string{gapToolA, gapToolA},
			wantOutcomes: []callOutcome{outcomeHint, outcomeGeneric},
			wantMid:      []string{gapFor(gapToolA)},
			wantFinal:    []string{gapFor(gapToolA)},
		},
		{
			name:         "an unavailable call followed by an interruption leaves one gap",
			tools:        []agent.Tool{scripted(gapToolA, unavailable, interrupted)},
			calls:        []string{gapToolA, gapToolA},
			wantOutcomes: []callOutcome{outcomeGeneric, outcomeHint},
			wantMid:      []string{gapFor(gapToolA)},
			wantFinal:    []string{gapFor(gapToolA)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gapRun{tools: tc.tools, calls: tc.calls}.execute(t)
			if got.err != nil {
				t.Fatalf("callAgent: %v", got.err)
			}
			if !slices.Equal(got.outcomes, tc.wantOutcomes) {
				t.Fatalf("what the loop saw = %q, want %q", got.outcomes, tc.wantOutcomes)
			}
			if !slices.Equal(got.midRun, tc.wantMid) {
				t.Fatalf("gaps during the run = %q, want %q", got.midRun, tc.wantMid)
			}
			if !slices.Equal(got.gaps, tc.wantFinal) {
				t.Fatalf("gaps after callAgent = %q, want %q", got.gaps, tc.wantFinal)
			}
		})
	}
}

func TestInterruptedToolGapIsFlushedWhenTheRunFails(t *testing.T) {
	got := gapRun{
		tools:  []agent.Tool{scripted(gapToolA, interruptedCallError{})},
		calls:  []string{gapToolA},
		finish: errors.New("model request failed"),
	}.execute(t)
	if got.err == nil {
		t.Fatal("callAgent succeeded, want the run error")
	}
	if want := []string{gapFor(gapToolA)}; !slices.Equal(got.gaps, want) {
		t.Fatalf("gaps after a failed run = %q, want %q", got.gaps, want)
	}
}

func TestInterruptedToolGapIgnoresCanceledCaller(t *testing.T) {
	got := gapRun{
		tools:    []agent.Tool{scripted(gapToolA, interruptedCallError{})},
		calls:    []string{gapToolA},
		canceled: true,
	}.execute(t)
	if got.err != nil || len(got.gaps) != 0 {
		t.Fatalf("canceled caller: err=%v gaps=%q, want none", got.err, got.gaps)
	}
}

func TestInterruptedToolGapTrackerIsSharedAcrossConcurrentCalls(t *testing.T) {
	const workers, rounds = 8, 25
	always := make([]error, workers*rounds)
	for i := range always {
		always[i] = interruptedCallError{}
	}
	var calls []string
	for range rounds {
		calls = append(calls, gapToolA, gapToolB)
	}
	got := gapRun{
		tools:   []agent.Tool{scripted(gapToolA, always...), scripted(gapToolB)},
		calls:   calls,
		workers: workers,
	}.execute(t)
	if got.err != nil {
		t.Fatalf("callAgent: %v", got.err)
	}
	// Tool A is interrupted on every call and tool B always succeeds, so
	// exactly one gap remains however the calls interleave.
	if want := []string{gapFor(gapToolA)}; !slices.Equal(got.gaps, want) {
		t.Fatalf("gaps = %q, want %q", got.gaps, want)
	}
}
