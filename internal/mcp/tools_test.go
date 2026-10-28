package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSplitToolName(t *testing.T) {
	valid := []string{"mcp__server__tool", "mcp__a__b-c_d.e"}
	for _, name := range valid {
		ref, err := splitToolName(name)
		if err != nil {
			t.Fatalf("splitToolName(%q): %v", name, err)
		}
		if ref.full != name || ref.server == "" || ref.tool == "" {
			t.Fatalf("splitToolName(%q) = %+v", name, ref)
		}
	}
	invalid := []string{
		"", "server__tool", "mcp__server", "mcp__server__", "mcp____tool",
		"mcp__server__tool__extra", "mcp__bad/server__tool", "mcp__server__bad tool",
	}
	for _, name := range invalid {
		if _, err := splitToolName(name); err == nil {
			t.Fatalf("splitToolName(%q) succeeded", name)
		}
	}
}

func TestParseAllowlistDeduplicates(t *testing.T) {
	refs, err := parseAllowlist([]string{
		"mcp__server__a",
		"mcp__server__a",
		"mcp__server__b",
	})
	if err != nil {
		t.Fatalf("parseAllowlist: %v", err)
	}
	if len(refs) != 2 || refs[0].tool != "a" || refs[1].tool != "b" {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestInfoForRefusesMutationAnnotations(t *testing.T) {
	destructive := true
	raw := &sdk.Tool{
		Name:        "remove",
		Description: "remove",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive, ReadOnlyHint: false},
	}
	info, ok := infoFor(raw)
	if !ok || !info.Destructive || info.ReadOnly {
		t.Fatalf("info = %+v ok=%v", info, ok)
	}
	readonly := &sdk.Tool{Name: "read", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}
	info, ok = infoFor(readonly)
	if !ok || !info.ReadOnly || info.Destructive {
		t.Fatalf("readonly info = %+v ok=%v", info, ok)
	}
	if _, ok := infoFor(&sdk.Tool{Name: "bad__name"}); ok {
		t.Fatal("accepted a tool name containing __")
	}
}

func TestInputSchemaShape(t *testing.T) {
	if _, ok := inputSchema(nil); !ok {
		t.Fatal("nil schema should fall back to an object schema")
	}
	if _, ok := inputSchema(map[string]any{"type": "object"}); !ok {
		t.Fatal("object schema rejected")
	}
	if _, ok := inputSchema(map[string]any{"type": "array"}); ok {
		t.Fatal("array schema accepted")
	}
	if _, ok := inputSchema(json.RawMessage(`{"properties":{}}`)); ok {
		t.Fatal("schema without object type accepted")
	}
}

func TestCallErrorConnectionClosedEvictsEntry(t *testing.T) {
	previous := errors.New("watch connection failure")
	for _, tc := range []struct {
		name     string
		dead     bool
		previous error
	}{
		{name: "call detects closure"},
		{name: "watch marked entry dead first", dead: true, previous: previous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := entryKey{server: "local"}
			manager := &Manager{entries: make(map[entryKey]*serverEntry)}
			entry := &serverEntry{m: manager, key: key, dead: tc.dead, err: tc.previous}
			manager.entries[key] = entry
			cause := fmt.Errorf("transport: %w", sdk.ErrConnectionClosed)
			ctx := context.Background()
			if err := entry.callError(ctx, ctx, cause); !errors.Is(err, ErrConnectionClosed) {
				t.Fatalf("callError = %v, want ErrConnectionClosed", err)
			}
			if !entry.dead {
				t.Fatal("entry remained live after connection closed")
			}
			wantCause := cause
			if tc.previous != nil {
				wantCause = tc.previous
			}
			if entry.err != wantCause {
				t.Fatalf("stored cause = %v, want %v", entry.err, wantCause)
			}
			if _, ok := manager.entries[key]; ok {
				t.Fatal("closed entry remained in manager cache")
			}
		})
	}
}

func TestCallErrorDeadEntryReturnsConnectionClosed(t *testing.T) {
	previous := errors.New("watch connection failure")
	entry := &serverEntry{m: &Manager{}, key: entryKey{server: "local"}, dead: true, err: previous}
	ctx := context.Background()
	if err := entry.callError(ctx, ctx, errors.New("call failure")); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("callError = %v, want ErrConnectionClosed", err)
	}
	if entry.err != previous {
		t.Fatalf("stored cause = %v, want %v", entry.err, previous)
	}
}

func TestFormatCallResultBoundsText(t *testing.T) {
	res := &sdk.CallToolResult{Content: []sdk.Content{
		&sdk.TextContent{Text: "first line\n"},
		&sdk.TextContent{Text: "second line"},
	}}
	content, truncated := formatCallResult(res, 1000)
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if !strings.Contains(content, "first line") || !strings.Contains(content, "second line") {
		t.Fatalf("content = %q", content)
	}
}

func TestFormatCallResultTruncatesUTF8(t *testing.T) {
	res := &sdk.CallToolResult{Content: []sdk.Content{
		&sdk.TextContent{Text: strings.Repeat("界", 1000)},
	}}
	content, truncated := formatCallResult(res, 100)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(content) > 100 {
		t.Fatalf("content length = %d", len(content))
	}
	if !strings.HasSuffix(content, truncationMarker) {
		t.Fatalf("missing truncation marker: %q", content)
	}
}

func TestFormatCallResultStructured(t *testing.T) {
	res := &sdk.CallToolResult{StructuredContent: map[string]any{"answer": 42}}
	content, truncated := formatCallResult(res, 1000)
	if truncated || !strings.Contains(content, `"answer":42`) {
		t.Fatalf("content = %q truncated=%v", content, truncated)
	}
}
