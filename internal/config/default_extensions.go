package config

import (
	"fmt"

	"github.com/LeXwDeX/tribunal/internal/skill/builtin"
)

// BindableBlocks are the review blocks that accept MCP tool and skill
// bindings.
var BindableBlocks = []string{"code-review", "review", "reasoner", "verify", "synthesize", "onboarding"}

// DefaultExtensionsConfig lists platform extensions that Tribunal registers
// and binds once, on the first start that sees them. Later starts never
// restore a default the administrator deleted or unbound, and never change a
// block binding the administrator or the file configuration already owns.
// An empty list disables new defaults; it does not remove registered ones.
type DefaultExtensionsConfig struct {
	Skills []DefaultSkill `yaml:"skills" json:"skills"`
}

// DefaultSkill names one bundled skill and the blocks it is mounted on.
type DefaultSkill struct {
	Name   string   `yaml:"name" json:"name"`
	Blocks []string `yaml:"blocks" json:"blocks"`
}

func validateDefaultExtensions(p *problems, cfg DefaultExtensionsConfig) {
	seen := make(map[string]bool, len(cfg.Skills))
	for i, item := range cfg.Skills {
		field := fmt.Sprintf("default_extensions.skills[%d]", i)
		switch {
		case !builtin.Has(item.Name):
			p.add("%s.name must be a bundled skill (%v)", field, builtin.Names())
		case seen[item.Name]:
			p.add("%s.name %q is listed more than once", field, item.Name)
		}
		seen[item.Name] = true
		for j, block := range item.Blocks {
			if !containsBlock(block) {
				p.add("%s.blocks[%d] must be one of %v", field, j, BindableBlocks)
			}
		}
	}
}

func containsBlock(block string) bool {
	for _, candidate := range BindableBlocks {
		if candidate == block {
			return true
		}
	}
	return false
}
