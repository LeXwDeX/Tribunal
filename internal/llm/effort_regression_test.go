package llm

import "testing"

func TestCallAuditRetainsValidEffort(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "max"} {
		t.Run(effort, func(t *testing.T) {
			audit := NewCallAudit(&Request{Reasoning: &ReasoningOpts{Effort: effort}}, nil)
			if audit.Request.Reasoning.Effort != effort {
				t.Fatalf("effort = %q, want %q", audit.Request.Reasoning.Effort, effort)
			}
		})
	}
}
