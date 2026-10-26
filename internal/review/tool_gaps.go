package review

import (
	"context"
	"encoding/json"
)

// toolGapSignals collects the MCP binding failures recorded by completed
// agent nodes. Nodes write tool_gaps only from executor state, never from
// model output, so the signals are code-owned.
func (e *Executor) toolGapSignals(ctx context.Context, workflowID string) ([]UnresolvedSignal, error) {
	if e == nil || e.deps.Snapshot == nil {
		return nil, nil
	}
	wf, err := e.snapshot(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	var signals []UnresolvedSignal
	for _, id := range wf.Order {
		node, ok := wf.Nodes[id]
		if !ok || len(node.Output) == 0 {
			continue
		}
		var out struct {
			ToolGaps []string `json:"tool_gaps"`
		}
		if json.Unmarshal(node.Output, &out) != nil {
			continue
		}
		for _, gap := range out.ToolGaps {
			signals = append(signals, UnresolvedSignal{
				Node: node.ID, Kind: UnresolvedEvidence, Message: oneLine(gap, maxUnresolvedMessage),
			})
		}
	}
	return mergeUnresolved(signals), nil
}

// applyToolGaps keeps a review that ran without its bound MCP tools from
// settling as a clean PASS. Every gap becomes a warning; PASS and INCOMPLETE
// results also carry the gaps as unresolved evidence. Existing BLOCKING
// findings stay blocking.
func applyToolGaps(result *SynthesisResult, gaps []UnresolvedSignal) {
	if result == nil || len(gaps) == 0 {
		return
	}
	for _, gap := range gaps {
		result.Warnings = appendUnique(result.Warnings, gap.Node+": "+gap.Message)
	}
	if result.Verdict == VerdictBlocking {
		return
	}
	applyUnresolved(result, mergeUnresolved(result.Unresolved, gaps))
}
