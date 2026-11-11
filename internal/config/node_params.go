package config

import (
	"math"

	"github.com/LeXwDeX/tribunal/internal/llm/effort"
)

// Review node keys used by NodeParams and by stored profile overrides.
const (
	NodeExplore    = "explore"
	NodeCodeReview = "code_review"
	NodeReview     = "review"
	NodeReasoner   = "reasoner"
	NodeValidate   = "validate"
	NodeChallenge  = "challenge"
)

// NodeParams overrides request parameters for one review node. Only these four
// are allowed; they are sent as request values and take precedence over the
// model's own defaults. A nil field keeps the model's value.
type NodeParams struct {
	MaxTokens   *int             `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	Reasoning   *ReasoningConfig `json:"reasoning,omitempty"`
	Thinking    *ThinkingConfig  `json:"thinking,omitempty"`

	// Source records where the values came from ("override" for the node's
	// own, "override_default" for the profile default). Audit only.
	Source string `json:"-"`
}

// IsZero reports whether no parameter is set.
func (p NodeParams) IsZero() bool {
	return p.MaxTokens == nil && p.Temperature == nil &&
		(p.Reasoning == nil || p.Reasoning.Effort == "") &&
		(p.Thinking == nil || (p.Thinking.BudgetTokens == 0 && p.Thinking.Type == ""))
}

// Overlay returns p with every field p leaves unset taken from base.
func (p NodeParams) Overlay(base NodeParams) NodeParams {
	out := base
	if p.MaxTokens != nil {
		v := *p.MaxTokens
		out.MaxTokens = &v
	} else if out.MaxTokens != nil {
		v := *out.MaxTokens
		out.MaxTokens = &v
	}
	if p.Temperature != nil {
		v := *p.Temperature
		out.Temperature = &v
	} else if out.Temperature != nil {
		v := *out.Temperature
		out.Temperature = &v
	}
	if p.Reasoning != nil && p.Reasoning.Effort != "" {
		v := *p.Reasoning
		out.Reasoning = &v
	} else if out.Reasoning != nil {
		v := *out.Reasoning
		out.Reasoning = &v
	}
	if p.Thinking != nil && (p.Thinking.BudgetTokens != 0 || p.Thinking.Type != "") {
		v := *p.Thinking
		out.Thinking = &v
	} else if out.Thinking != nil {
		v := *out.Thinking
		out.Thinking = &v
	}
	out.Source = ""
	return out
}

// ValidateNodeParams applies the same rules as model parameters; field is the
// path of the params object in the error message.
func ValidateNodeParams(field string, p NodeParams) error {
	var p2 problems
	if p.MaxTokens != nil && *p.MaxTokens <= 0 {
		p2.add("%s.max_tokens must be positive", field)
	}
	if p.Temperature != nil && (math.IsNaN(*p.Temperature) || *p.Temperature < 0 || *p.Temperature > 2) {
		p2.add("%s.temperature must be between 0 and 2", field)
	}
	if p.Reasoning != nil {
		switch {
		case p.Reasoning.Effort == "":
			p2.add("%s.reasoning.effort must be low, medium, high or max; omit the field to inherit", field)
		case p.Reasoning.Effort != "none" && !effort.Valid(p.Reasoning.Effort):
			p2.add("%s.reasoning.effort must be low, medium, high or max", field)
		}
	}
	if p.Thinking != nil {
		if p.Thinking.BudgetTokens < 0 || (p.Thinking.BudgetTokens == 0 && p.Thinking.Type == "") {
			p2.add("%s.thinking.budget_tokens must be positive; omit the field to inherit", field)
		}
		switch p.Thinking.Type {
		case "", "enabled", "disabled":
		default:
			p2.add("%s.thinking.type must be enabled or disabled", field)
		}
	}
	return p2.err()
}
