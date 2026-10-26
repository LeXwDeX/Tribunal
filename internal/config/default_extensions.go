package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

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
	Skills     []DefaultSkill     `yaml:"skills" json:"skills"`
	MCPServers []DefaultMCPServer `yaml:"mcp_servers" json:"mcp_servers"`
}

// DefaultSkill names one bundled skill and the blocks it is mounted on.
type DefaultSkill struct {
	Name   string   `yaml:"name" json:"name"`
	Blocks []string `yaml:"blocks" json:"blocks"`
}

// DefaultMCPServer is one built-in platform MCP server. Config is a Claude
// Code mcpServers object, optionally with x-review; the built-in row follows
// it on every start while the administrator keeps the enabled state. Tools
// maps a block to tool names of this server, granted as mcp__<name>__<tool>.
type DefaultMCPServer struct {
	Name   string              `yaml:"name" json:"name"`
	Config map[string]any      `yaml:"config" json:"config"`
	Tools  map[string][]string `yaml:"tools" json:"tools"`
}

// ConfigJSON returns the server object as JSON with sorted object keys, so
// equal configurations always produce equal bytes.
func (d DefaultMCPServer) ConfigJSON() (json.RawMessage, error) {
	raw, err := json.Marshal(d.Config)
	if err != nil {
		return nil, fmt.Errorf("config: default MCP server %q: %w", d.Name, err)
	}
	return raw, nil
}

var (
	// defaultMCPNameRE matches the server names accepted by internal/mcp.
	defaultMCPNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// defaultMCPToolRE keeps a tool name inside one mcp__server__tool segment.
	defaultMCPToolRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*(_[A-Za-z0-9.-]+)*$`)
)

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
	servers := make(map[string]bool, len(cfg.MCPServers))
	for i, item := range cfg.MCPServers {
		field := fmt.Sprintf("default_extensions.mcp_servers[%d]", i)
		switch {
		case !defaultMCPNameRE.MatchString(item.Name) || strings.Contains(item.Name, "__"):
			p.add("%s.name %q is not a valid MCP server name", field, item.Name)
		case servers[item.Name]:
			p.add("%s.name %q is listed more than once", field, item.Name)
		}
		servers[item.Name] = true
		if len(item.Config) == 0 {
			p.add("%s.config must be a non-empty mapping", field)
		}
		for _, block := range sortedKeys(item.Tools) {
			if !containsBlock(block) {
				p.add("%s.tools.%s: block must be one of %v", field, block, BindableBlocks)
			}
			for j, tool := range item.Tools[block] {
				if !defaultMCPToolRE.MatchString(tool) {
					p.add("%s.tools.%s[%d] %q must be a plain tool name of this server", field, block, j, tool)
				}
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
