package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxRemoteTools   = 512
	maxSchemaBytes   = 64 << 10
	maxToolInput     = 1 << 20
	maxDescription   = 1024
	truncationMarker = "\n[truncated: output limit reached]"
)

// ErrListTools reports a tools/list failure.
var ErrListTools = errors.New("mcp: list tools failed")

// ToolInfo describes one advertised tool for the extension API. It does not
// grant access; only Tools with an explicit allowlist creates callable tools.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"read_only,omitempty"`
	Destructive bool   `json:"destructive,omitempty"`
}

// ToolSet is the explicitly allowlisted, read-only MCP tool set for one block.
// Tool results are untrusted data; agent.Run wraps them before model input.
// Warnings name skipped tools without exposing configuration values.
type ToolSet struct {
	Tools    []agent.Tool
	Warnings []string
}

// ToolName returns the model-visible name for one server tool.
func ToolName(server, tool string) string { return "mcp__" + server + "__" + tool }

type toolRef struct {
	full   string
	server string
	tool   string
}

// ListTools connects to server and returns its advertised tools for status and
// test APIs. It does not grant model access.
func (m *Manager) ListTools(ctx context.Context, server string) ([]ToolInfo, error) {
	if m == nil {
		return nil, ErrClosed
	}
	entry, err := m.acquire(ctx, server)
	if err != nil {
		return nil, err
	}
	defer entry.end()

	callCtx, cancel := context.WithTimeout(ctx, entry.cfg.XReview.CallTimeout.AsDuration())
	defer cancel()
	raws, err := entry.list(callCtx)
	if err != nil {
		return nil, entry.listError(ctx, callCtx, err)
	}
	infos := make([]ToolInfo, 0, len(raws))
	for _, raw := range raws {
		if info, ok := infoFor(raw); ok {
			infos = append(infos, info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

// Tools returns only the explicitly allowlisted mcp__server__tool entries.
// Read-only annotations never grant access; a tool with a destructive hint is
// refused even when allowlisted. Missing tools become warnings.
func (m *Manager) Tools(ctx context.Context, allowed []string) (*ToolSet, error) {
	if m == nil {
		return nil, ErrClosed
	}
	refs, err := parseAllowlist(allowed)
	if err != nil {
		return nil, err
	}
	set := &ToolSet{}
	if len(refs) == 0 {
		return set, nil
	}
	for _, ref := range refs {
		if _, ok := m.servers[ref.server]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownServer, ref.server)
		}
	}

	byServer := make(map[string][]toolRef)
	servers := make([]string, 0)
	for _, ref := range refs {
		if _, ok := byServer[ref.server]; !ok {
			servers = append(servers, ref.server)
		}
		byServer[ref.server] = append(byServer[ref.server], ref)
	}

	advertised := make(map[string]map[string]*sdk.Tool, len(servers))
	for _, server := range servers {
		entry, err := m.acquire(ctx, server)
		if err != nil {
			return nil, err
		}
		callCtx, cancel := context.WithTimeout(ctx, entry.cfg.XReview.CallTimeout.AsDuration())
		raws, err := entry.list(callCtx)
		var listErr error
		if err != nil {
			listErr = entry.listError(ctx, callCtx, err)
		}
		cancel()
		entry.end()
		if err != nil {
			return nil, listErr
		}
		index := make(map[string]*sdk.Tool, len(raws))
		for _, raw := range raws {
			if raw == nil {
				continue
			}
			index[raw.Name] = raw
		}
		advertised[server] = index
	}

	for _, ref := range refs {
		raw, ok := advertised[ref.server][ref.tool]
		if !ok {
			set.Warnings = append(set.Warnings, fmt.Sprintf("server %q: tool %q is not advertised", ref.server, ref.tool))
			continue
		}
		info, ok := infoFor(raw)
		if !ok || info.Destructive {
			if ok && info.Destructive {
				set.Warnings = append(set.Warnings, fmt.Sprintf("server %q: tool %q has mutation annotations and was skipped", ref.server, ref.tool))
			} else {
				set.Warnings = append(set.Warnings, fmt.Sprintf("server %q: tool %q has an unusable definition", ref.server, ref.tool))
			}
			continue
		}
		schema, ok := inputSchema(raw.InputSchema)
		if !ok {
			set.Warnings = append(set.Warnings, fmt.Sprintf("server %q: tool %q has an invalid input schema", ref.server, ref.tool))
			continue
		}
		description := descriptionFor(ref.server, raw)
		set.Tools = append(set.Tools, &mcpTool{
			m:      m,
			server: ref.server,
			remote: ref.tool,
			def: llm.ToolDef{
				Name:        ToolName(ref.server, ref.tool),
				Description: description,
				InputSchema: schema,
			},
		})
	}
	return set, nil
}

func parseAllowlist(allowed []string) ([]toolRef, error) {
	refs := make([]toolRef, 0, len(allowed))
	seen := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		ref, err := splitToolName(name)
		if err != nil {
			return nil, err
		}
		if seen[ref.full] {
			continue
		}
		seen[ref.full] = true
		refs = append(refs, ref)
	}
	return refs, nil
}

func splitToolName(name string) (toolRef, error) {
	name = strings.TrimSpace(name)
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return toolRef{}, fmt.Errorf("%w: tool allowlist entries need the mcp__server__tool form", ErrInvalidConfig)
	}
	sep := strings.Index(rest, "__")
	if sep <= 0 || sep+2 >= len(rest) {
		return toolRef{}, fmt.Errorf("%w: tool allowlist entries need the mcp__server__tool form", ErrInvalidConfig)
	}
	server, tool := rest[:sep], rest[sep+2:]
	if strings.Contains(tool, "__") || strings.Contains(server, "__") {
		return toolRef{}, fmt.Errorf("%w: tool allowlist entries need the mcp__server__tool form", ErrInvalidConfig)
	}
	if !serverNameRE.MatchString(server) || !toolNameRE.MatchString(tool) {
		return toolRef{}, fmt.Errorf("%w: invalid tool allowlist entry", ErrInvalidConfig)
	}
	return toolRef{full: name, server: server, tool: tool}, nil
}

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (entry *serverEntry) list(ctx context.Context) ([]*sdk.Tool, error) {
	session, err := entry.current()
	if err != nil {
		return nil, err
	}
	var tools []*sdk.Tool
	for tool, err := range session.Tools(ctx, &sdk.ListToolsParams{}) {
		if err != nil {
			return nil, err
		}
		if tool == nil {
			continue
		}
		if len(tools) >= maxRemoteTools {
			return nil, fmt.Errorf("mcp: server %q advertises too many tools", entry.key.server)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (entry *serverEntry) listError(parent, callCtx context.Context, err error) error {
	if errors.Is(err, sdk.ErrConnectionClosed) {
		entry.markDead(err)
		return fmt.Errorf("%w: %q", ErrConnectionClosed, entry.key.server)
	}
	if parent.Err() != nil {
		return fmt.Errorf("%w: server %q", parent.Err(), entry.key.server)
	}
	if callCtx != nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: server %q: timed out", ErrListTools, entry.key.server)
	}
	return fmt.Errorf("%w: %q", ErrListTools, entry.key.server)
}

func infoFor(raw *sdk.Tool) (ToolInfo, bool) {
	if raw == nil || raw.Name == "" || !toolNameRE.MatchString(raw.Name) || strings.Contains(raw.Name, "__") {
		return ToolInfo{}, false
	}
	info := ToolInfo{
		Name:        raw.Name,
		Description: truncateUTF8(oneLine(raw.Description), maxDescription),
	}
	if raw.Annotations != nil {
		info.ReadOnly = raw.Annotations.ReadOnlyHint
		info.Destructive = raw.Annotations.DestructiveHint != nil && *raw.Annotations.DestructiveHint
	}
	return info, true
}

func descriptionFor(server string, raw *sdk.Tool) string {
	description := truncateUTF8(oneLine(raw.Description), maxDescription)
	if description == "" {
		description = fmt.Sprintf("MCP tool %s on server %s.", raw.Name, server)
	}
	return description
}

func inputSchema(schema any) (json.RawMessage, bool) {
	if schema == nil {
		return json.RawMessage(`{"type":"object"}`), true
	}
	data, err := json.Marshal(schema)
	if err != nil || len(data) == 0 || len(data) > maxSchemaBytes {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, false
	}
	var kind string
	if raw, ok := object["type"]; !ok || json.Unmarshal(raw, &kind) != nil || kind != "object" {
		return nil, false
	}
	return json.RawMessage(data), true
}

type mcpTool struct {
	m      *Manager
	server string
	remote string
	def    llm.ToolDef
}

func (t *mcpTool) Def() llm.ToolDef { return t.def }

func (t *mcpTool) Call(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
	if len(raw) > maxToolInput {
		return agent.Result{}, errors.New("mcp: tool input is too large")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return agent.Result{}, errors.New("mcp: tool input must be a JSON object")
	}
	result, err := t.m.call(ctx, t.server, t.remote, json.RawMessage(trimmed))
	if err != nil {
		return agent.Result{}, err
	}
	return result, nil
}

func (m *Manager) call(ctx context.Context, server, tool string, args json.RawMessage) (agent.Result, error) {
	entry, err := m.acquire(ctx, server)
	if err != nil {
		return agent.Result{}, err
	}
	defer entry.end()

	session, err := entry.current()
	if err != nil {
		return agent.Result{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, entry.cfg.XReview.CallTimeout.AsDuration())
	defer cancel()

	res, err := session.CallTool(callCtx, &sdk.CallToolParams{
		Name:      tool,
		Arguments: args,
	})
	if err != nil {
		return agent.Result{}, entry.callError(ctx, callCtx, err)
	}
	if res == nil {
		return agent.Result{}, fmt.Errorf("%w: %q: empty result", ErrCallFailed, server)
	}
	if res.NeedsInput() {
		return agent.Result{}, fmt.Errorf("%w: %q: server requested interactive input", ErrCallFailed, server)
	}
	content, truncated := formatCallResult(res, entry.cfg.XReview.MaxOutputChars)
	return agent.Result{Content: content, IsError: res.IsError, Truncated: truncated}, nil
}

func (entry *serverEntry) callError(parent, callCtx context.Context, err error) error {
	if errors.Is(err, sdk.ErrConnectionClosed) {
		entry.markDead(err)
		return fmt.Errorf("%w: %q", ErrConnectionClosed, entry.key.server)
	}
	entry.mu.Lock()
	dead := entry.dead
	entry.mu.Unlock()
	if dead {
		return fmt.Errorf("%w: %q", ErrConnectionClosed, entry.key.server)
	}
	if parent.Err() != nil {
		return fmt.Errorf("%w: server %q", parent.Err(), entry.key.server)
	}
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: server %q: call timed out", ErrCallFailed, entry.key.server)
	}
	return fmt.Errorf("%w: %q", ErrCallFailed, entry.key.server)
}

func formatCallResult(res *sdk.CallToolResult, max int) (string, bool) {
	if max <= 0 {
		max = DefaultMaxOutputChars
	}
	limit := max - len(truncationMarker)
	if limit < 1 {
		return truncateUTF8(truncationMarker, max), true
	}
	b := boundedBuilder{limit: limit}
	for _, content := range res.Content {
		switch value := content.(type) {
		case *sdk.TextContent:
			b.append(value.Text)
			b.append("\n")
		case *sdk.EmbeddedResource:
			if value.Resource != nil && value.Resource.Text != "" {
				b.append(value.Resource.Text)
				b.append("\n")
			} else {
				b.append("[resource content omitted]\n")
			}
		case *sdk.ResourceLink:
			b.append("[resource: " + value.URI + "]\n")
		default:
			b.append("[unsupported MCP content]\n")
		}
	}
	if res.StructuredContent != nil {
		if data, err := json.Marshal(res.StructuredContent); err == nil {
			b.append("\n[structured]\n")
			b.append(string(data))
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "(no output)", false
	}
	if b.truncated {
		return out + truncationMarker, true
	}
	return out, false
}

type boundedBuilder struct {
	b         strings.Builder
	limit     int
	truncated bool
}

func (b *boundedBuilder) String() string { return b.b.String() }

func (b *boundedBuilder) append(s string) {
	if b.truncated || s == "" {
		return
	}
	remaining := b.limit - b.b.Len()
	if remaining <= 0 {
		b.truncated = true
		return
	}
	if len(s) <= remaining {
		b.b.WriteString(s)
		return
	}
	b.b.WriteString(truncateUTF8(s, remaining))
	b.truncated = true
}

func truncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if !strings.ContainsAny(s, "\r\n\t") {
		return s
	}
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
