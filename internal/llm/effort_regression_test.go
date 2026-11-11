package llm

import "testing"

func TestCallAuditRetainsValidEffort(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "max", "none"} {
		t.Run(effort, func(t *testing.T) {
			audit := NewCallAudit(&Request{Reasoning: &ReasoningOpts{Effort: effort}}, nil)
			if audit.Request.Reasoning.Effort != effort {
				t.Fatalf("effort = %q, want %q", audit.Request.Reasoning.Effort, effort)
			}
		})
	}
}

func TestCallAuditSanitizesThinkingType(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"", ""}, {"enabled", "enabled"}, {"disabled", "disabled"}, {"provider text", ""},
	} {
		audit := NewCallAudit(&Request{Reasoning: &ReasoningOpts{ThinkingType: tc.value}}, nil)
		if got := audit.Request.Reasoning.ThinkingType; got != tc.want {
			t.Errorf("thinking type %q: audit = %q, want %q", tc.value, got, tc.want)
		}
	}
}
