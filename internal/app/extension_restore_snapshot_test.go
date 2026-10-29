package app

import (
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
)

// When the shipped content is already present in a different order, restore
// must keep the stored order in both the row and the snapshot. A later upgrade
// must still recognise the row as default-owned.
func TestRestoreDefaultsKeepsRowOrderInSnapshot(t *testing.T) {
	skills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review", "review"}}}
	servers := graphDefault(analysisArgs, "search_graph")
	servers[0].Tools["review"] = []string{"search_graph"}
	service, st := newRestoreFixture(t, restoreConfig(skills, servers))
	applySkillDefaults(t, service, st, nil, skills)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)

	// The administrator adds custom entries after the built-in ones.
	setPlatformBinding(t, service, "review",
		[]string{"mcp__codebase-memory__search_graph", "mcp__team__lookup"},
		[]string{"code-review", "team-rules"})
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})

	row := platformBindings(t, st)["review"]
	if !reflect.DeepEqual(row.Skills, []string{"code-review", "team-rules"}) ||
		!reflect.DeepEqual(row.MCPTools, []string{"mcp__codebase-memory__search_graph", "mcp__team__lookup"}) {
		t.Fatalf("restore rewrote the order: %+v", row)
	}
	if !sameDefaultBinding(row, ledgerRows(t, st)["block:review"].Detail) {
		t.Fatalf("snapshot order differs from the row: %+v", ledgerRows(t, st)["block:review"])
	}

	again := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if change := restoreChange(again, "review"); change.Status != restoreStatusUnchanged {
		t.Fatalf("second preview = %+v, want unchanged", change)
	}

	// A later release adds a tool. The row is still default-owned, so the
	// upgrade follows and keeps the custom grants.
	servers[0].Tools["review"] = []string{"search_graph", "trace_path"}
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)
	upgraded := platformBindings(t, st)["review"]
	if got := sortedCopy(upgraded.MCPTools); !reflect.DeepEqual(got, []string{
		"mcp__codebase-memory__search_graph", "mcp__codebase-memory__trace_path", "mcp__team__lookup",
	}) {
		t.Fatalf("upgrade tools = %v", got)
	}
	if got := sortedCopy(upgraded.Skills); !reflect.DeepEqual(got, []string{"code-review", "team-rules"}) {
		t.Fatalf("upgrade skills = %v", got)
	}
	if !sameDefaultBinding(upgraded, ledgerRows(t, st)["block:review"].Detail) {
		t.Fatalf("upgraded snapshot differs from the row")
	}
}
