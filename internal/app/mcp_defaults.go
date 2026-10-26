package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// MCPDefaults turns the configured built-in MCP servers into extension
// defaults. Install creates an enabled platform row marked built-in; Sync
// rewrites its config whenever the release ships a different one and keeps
// the enabled state. A server of the same name that the administrator owns,
// stored or in mcp.config, is never replaced or bound.
func (s *ExtensionService) MCPDefaults(specs []config.DefaultMCPServer, fileMCP mcp.Config) ([]ExtensionDefault, error) {
	out := make([]ExtensionDefault, 0, len(specs))
	for _, spec := range specs {
		name := spec.Name
		raw, err := spec.ConfigJSON()
		if err != nil {
			return nil, err
		}
		if err := mcp.ValidateServerJSON(name, raw, s.deniedEnv...); err != nil {
			return nil, fmt.Errorf("app: default MCP server %q: %w", name, err)
		}
		want, err := canonicalJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("app: default MCP server %q: %w", name, err)
		}
		bindings := make(map[string]DefaultGrant, len(spec.Tools))
		for block, tools := range spec.Tools {
			grant := DefaultGrant{MCPTools: make([]string, 0, len(tools))}
			for _, tool := range tools {
				grant.MCPTools = append(grant.MCPTools, mcp.ToolName(name, tool))
			}
			bindings[block] = grant
		}
		_, inFile := fileMCP.MCPServers[name]
		out = append(out, ExtensionDefault{
			Kind: "mcp",
			Name: name,
			Exists: func(ctx context.Context) (bool, error) {
				if inFile {
					return true, nil
				}
				row, err := s.platformMCPServerNamed(ctx, name)
				return row != nil && row.Builtin == "", err
			},
			Install: func(ctx context.Context) error {
				row, err := s.platformMCPServerNamed(ctx, name)
				if err != nil || row != nil {
					return err
				}
				_, err = s.store.CreateMCPServer(ctx, store.MCPServer{
					Scope: store.ScopePlatform, Name: name, Config: raw, Enabled: true, Builtin: name,
				})
				return err
			},
			Sync: func(ctx context.Context) error {
				row, err := s.platformMCPServerNamed(ctx, name)
				if err != nil || row == nil || row.Builtin != name {
					return err
				}
				have, err := canonicalJSON(row.Config)
				if err == nil && bytes.Equal(have, want) {
					return nil
				}
				if _, err := s.store.UpdateMCPServer(ctx, row.ID, store.MCPServerUpdate{Config: raw}); err != nil {
					return err
				}
				s.logger.Info("built-in MCP server updated", "name", name)
				return nil
			},
			Bindings: bindings,
		})
	}
	return out, nil
}

func (s *ExtensionService) platformMCPServerNamed(ctx context.Context, name string) (*store.MCPServer, error) {
	rows, err := s.store.ListMCPServers(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// canonicalJSON re-encodes a JSON value with sorted keys and exact numbers,
// so formatting differences do not count as a config change.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
