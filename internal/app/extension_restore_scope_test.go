package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// shippedDefaults is the current release layout: the skill is only bound to
// code-review, while the MCP server grants tools to four blocks.
func shippedDefaults() ([]config.DefaultSkill, []config.DefaultMCPServer) {
	skills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review"}}}
	servers := graphDefault(analysisArgs)
	servers[0].Tools = map[string][]string{
		"code-review": {"search_graph"},
		"review":      {"search_graph"},
		"verify":      {"get_code_snippet"},
		"onboarding":  {"search_graph"},
	}
	return skills, servers
}

func newScopedRestoreFixture(t *testing.T) (*ExtensionService, *sqlite.Store, []config.DefaultSkill, []config.DefaultMCPServer) {
	t.Helper()
	skills, servers := shippedDefaults()
	service, st := newRestoreFixture(t, restoreConfig(skills, servers))
	applySkillDefaults(t, service, st, nil, skills)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)
	return service, st, skills, servers
}

func setPlatformBinding(t *testing.T, service *ExtensionService, block string, tools, skills []string) {
	t.Helper()
	if _, err := service.UpsertBlockBinding(context.Background(), BlockBindingInput{
		Scope: store.ScopePlatform, Block: block, MCPTools: tools, Skills: skills,
	}); err != nil {
		t.Fatalf("UpsertBlockBinding(%s): %v", block, err)
	}
}

// addExtraBuiltinGrants adds built-in grants to blocks the current defaults do
// not declare, plus custom grants that restore must keep.
func addExtraBuiltinGrants(t *testing.T, service *ExtensionService) {
	t.Helper()
	setPlatformBinding(t, service, "review",
		[]string{"mcp__codebase-memory__search_graph", "mcp__codebase-memory__old_tool", "mcp__team__lookup"},
		[]string{"code-review", "team-rules"})
	setPlatformBinding(t, service, "reasoner",
		[]string{"mcp__codebase-memory__old_tool"},
		[]string{"code-review"})
	setPlatformBinding(t, service, "synthesize",
		[]string{"mcp__codebase-memory__trace_path"},
		[]string{"code-review"})
}

// Restore removes built-in grants the current release does not declare, even
// on blocks with no default grant at all, while keeping custom grants.
func TestRestoreDefaultsCleansBuiltinGrantsOutsideDeclaredBlocks(t *testing.T) {
	service, st, _, _ := newScopedRestoreFixture(t)
	addExtraBuiltinGrants(t, service)

	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, DryRun: true})
	change := restoreChange(preview, "reasoner")
	if change.Status != restoreStatusChange {
		t.Fatalf("reasoner preview = %+v, want change", change)
	}
	if !reflect.DeepEqual(change.Remove.Skills, []string{"code-review"}) ||
		!reflect.DeepEqual(change.Remove.MCPTools, []string{"mcp__codebase-memory__old_tool"}) {
		t.Fatalf("reasoner remove = %+v", change.Remove)
	}

	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, PreviewToken: preview.PreviewToken})
	reasoner := platformBindings(t, st)["reasoner"]
	if len(reasoner.MCPTools) != 0 || len(reasoner.Skills) != 0 {
		t.Fatalf("reasoner kept built-in grants = %+v", reasoner)
	}
	if row := ledgerRows(t, st)["block:reasoner"]; row.Outcome != store.ExtensionDefaultApplied {
		t.Fatalf("reasoner snapshot = %+v, want applied", row)
	}

	// Review already has a declared MCP grant; only the removed skill and
	// extra tool go away, and custom grants stay.
	preview = restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change = restoreChange(preview, "review")
	if change.Status != restoreStatusChange {
		t.Fatalf("review preview = %+v, want change", change)
	}
	if !reflect.DeepEqual(change.Remove.Skills, []string{"code-review"}) ||
		!reflect.DeepEqual(change.Remove.MCPTools, []string{"mcp__codebase-memory__old_tool"}) {
		t.Fatalf("review remove = %+v", change.Remove)
	}
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})
	review := platformBindings(t, st)["review"]
	if got := sortedCopy(review.MCPTools); !reflect.DeepEqual(got, []string{"mcp__codebase-memory__search_graph", "mcp__team__lookup"}) {
		t.Fatalf("review tools = %v", got)
	}
	if got := sortedCopy(review.Skills); !reflect.DeepEqual(got, []string{"team-rules"}) {
		t.Fatalf("review skills = %v", got)
	}
}

// The all-blocks request applies the same cleanup to every selected block.
func TestRestoreDefaultsAllBlocksCleansBuiltinGrants(t *testing.T) {
	service, st, _, _ := newScopedRestoreFixture(t)
	addExtraBuiltinGrants(t, service)

	preview := restoreBlocks(t, service, RestoreDefaultsRequest{DryRun: true})
	for _, block := range []string{"review", "reasoner", "synthesize"} {
		if change := restoreChange(preview, block); change.Status != restoreStatusChange {
			t.Fatalf("%s preview = %+v, want change", block, change)
		}
	}
	restoreBlocks(t, service, RestoreDefaultsRequest{PreviewToken: preview.PreviewToken})

	for block, wantSkills := range map[string][]string{
		"review":     {"team-rules"},
		"reasoner":   {},
		"synthesize": {},
	} {
		if got := sortedCopy(platformBindings(t, st)[block].Skills); !reflect.DeepEqual(got, wantSkills) {
			t.Fatalf("%s skills = %v, want %v", block, got, wantSkills)
		}
	}
	for block, wantTools := range map[string][]string{
		"review":      {"mcp__codebase-memory__search_graph", "mcp__team__lookup"},
		"reasoner":    {},
		"synthesize":  {},
		"code-review": {"mcp__codebase-memory__search_graph"},
		"verify":      {"mcp__codebase-memory__get_code_snippet"},
		"onboarding":  {"mcp__codebase-memory__search_graph"},
	} {
		if got := sortedCopy(platformBindings(t, st)[block].MCPTools); !reflect.DeepEqual(got, wantTools) {
			t.Fatalf("%s tools = %v, want %v", block, got, wantTools)
		}
	}
	if got := sortedCopy(platformBindings(t, st)["code-review"].Skills); !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("code-review skills = %v", got)
	}
}

// A later release that grants the block again follows the restored snapshot.
func TestRestoreDefaultsFollowsGrantReturningToDifferentBlocks(t *testing.T) {
	service, st, skills, servers := newScopedRestoreFixture(t)
	addExtraBuiltinGrants(t, service)
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, DryRun: true})
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, PreviewToken: preview.PreviewToken})

	skills[0].Blocks = []string{"code-review", "reasoner"}
	servers[0].Tools["reasoner"] = []string{"search_graph"}
	applySkillDefaults(t, service, st, nil, skills)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)

	reasoner := platformBindings(t, st)["reasoner"]
	if got := sortedCopy(reasoner.Skills); !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("reasoner skills after re-add = %v", got)
	}
	if got := sortedCopy(reasoner.MCPTools); !reflect.DeepEqual(got, []string{"mcp__codebase-memory__search_graph"}) {
		t.Fatalf("reasoner tools after re-add = %v", got)
	}
}

// With no registered built-in defaults an empty row is not touched.
func TestRestoreDefaultsEmptyBindingWithoutRegisteredDefaults(t *testing.T) {
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	setPlatformBinding(t, service, "review", nil, nil)
	result := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(result, "review")
	if change.Status != restoreStatusSkipped || change.Reason != restoreReasonNoRegistered {
		t.Fatalf("empty unregistered change = %+v", change)
	}
	rows, err := st.ListBlockBindings(context.Background(), store.ScopePlatform, nil)
	if err != nil || len(rows) != 1 || rows[0].Block != "review" {
		t.Fatalf("empty row removed = %+v, %v", rows, err)
	}
}

// The preview token covers the shipped grant projection. A second process
// with a different configuration and the same storage must reject an old
// preview token without writing.
func TestRestoreDefaultsPreviewTokenCoversProcessDefaults(t *testing.T) {
	ctx := context.Background()
	cfgA := restoreConfig([]config.DefaultSkill{{
		Name: "code-review", Blocks: []string{"code-review", "review"},
	}}, nil)
	serviceA, st := newRestoreFixture(t, cfgA)
	applySkillDefaults(t, serviceA, st, nil, cfgA.DefaultExtensions.Skills)
	setPlatformBinding(t, serviceA, "review", nil, nil)
	previewA := restoreBlocks(t, serviceA, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if change := restoreChange(previewA, "review"); change.Status != restoreStatusChange {
		t.Fatalf("preview A = %+v, want change", change)
	}

	cfgB := restoreConfig([]config.DefaultSkill{{
		Name: "code-review", Blocks: []string{"code-review", "reasoner"},
	}}, nil)
	serviceB, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: workspace.NewManager(t.TempDir(), nil),
		Platform:  cfgB,
	})
	if err != nil {
		t.Fatal(err)
	}
	previewB := restoreBlocks(t, serviceB, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if previewB.PreviewToken == previewA.PreviewToken {
		t.Fatal("tokens are equal although the process defaults differ")
	}

	_, err = serviceB.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: previewA.PreviewToken,
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("stale token from another process = %v, want conflict", err)
	}
	if got := platformBindings(t, st)["review"]; len(got.MCPTools) != 0 || len(got.Skills) != 0 {
		t.Fatalf("stale token wrote the row: %+v", got)
	}

	// The same process accepts its own preview and applies its own grants.
	_, err = serviceB.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: previewB.PreviewToken,
	})
	if err != nil {
		t.Fatalf("current preview token rejected: %v", err)
	}
}

func newRestoreServiceOn(t *testing.T, st *sqlite.Store, cfg *config.Config) *ExtensionService {
	t.Helper()
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: workspace.NewManager(t.TempDir(), nil),
		Platform:  cfg,
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	return service
}

// A grant removed by the restore leaves a removed ledger marker. A later
// release that grants the block again must follow, and another restore with
// the unchanged configuration must stay a no-op.
func TestRestoreDefaultsMarksRemovedGrantLedgerAndFollowsReAdd(t *testing.T) {
	oldSkills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review", "reasoner"}}}
	oldServers := graphDefault(analysisArgs)
	oldServers[0].Tools["reasoner"] = []string{"search_graph"}
	service, st := newRestoreFixture(t, restoreConfig(oldSkills, oldServers))
	applySkillDefaults(t, service, st, nil, oldSkills)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, oldServers)
	setPlatformBinding(t, service, "reasoner",
		[]string{"mcp__codebase-memory__search_graph", "mcp__codebase-memory__old_tool"},
		[]string{"code-review"})

	newSkills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review"}}}
	newServers := graphDefault(analysisArgs)
	current := newRestoreServiceOn(t, st, restoreConfig(newSkills, newServers))
	preview := restoreBlocks(t, current, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, DryRun: true})
	if change := restoreChange(preview, "reasoner"); change.Status != restoreStatusChange {
		t.Fatalf("reasoner preview = %+v, want change", change)
	}
	restoreBlocks(t, current, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, PreviewToken: preview.PreviewToken})

	for _, key := range []string{"skill:code-review@reasoner", "mcp:codebase-memory@reasoner"} {
		row := ledgerRows(t, st)[key]
		if row.Outcome != store.ExtensionDefaultSkipped || row.Detail != reasonGrantRemoved {
			t.Fatalf("%s ledger = %+v, want removed", key, row)
		}
	}
	if row := ledgerRows(t, st)["block:reasoner"]; row.Outcome != store.ExtensionDefaultApplied {
		t.Fatalf("reasoner snapshot = %+v", row)
	}

	// Running the unchanged configuration again does not rewrite the marker.
	applySkillDefaults(t, current, st, nil, newSkills)
	applyMCPDefaults(t, current, st, nil, mcp.Config{}, newServers)
	if row := ledgerRows(t, st)["skill:code-review@reasoner"]; row.Outcome != store.ExtensionDefaultSkipped || row.Detail != reasonGrantRemoved {
		t.Fatalf("unchanged configuration rewrote the removed marker: %+v", row)
	}

	// The grant returning to the shipped defaults adds it back automatically.
	applySkillDefaults(t, current, st, nil, oldSkills)
	applyMCPDefaults(t, current, st, nil, mcp.Config{}, oldServers)
	reasoner := platformBindings(t, st)["reasoner"]
	if got := sortedCopy(reasoner.Skills); !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("reasoner skills after re-add = %v", got)
	}
	if got := sortedCopy(reasoner.MCPTools); !reflect.DeepEqual(got, []string{"mcp__codebase-memory__search_graph"}) {
		t.Fatalf("reasoner tools after re-add = %v", got)
	}
}

// A block with no platform row and no grant for the current release stays
// absent: restore does not manufacture an empty row or snapshot.
func TestRestoreDefaultsDoesNotCreateEmptyRows(t *testing.T) {
	service, st, _, _ := newScopedRestoreFixture(t)
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, DryRun: true})
	change := restoreChange(preview, "reasoner")
	if change.Status != restoreStatusUnchanged {
		t.Fatalf("reasoner preview = %+v, want unchanged", change)
	}
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, PreviewToken: preview.PreviewToken})
	if _, ok := platformBindings(t, st)["reasoner"]; ok {
		t.Fatal("restore created an empty reasoner row")
	}
	if _, ok := ledgerRows(t, st)["block:reasoner"]; ok {
		t.Fatal("restore created an empty reasoner snapshot")
	}
}

// A stale snapshot with no row still needs the empty snapshot row pair so a
// later release can grant the block again.
func TestRestoreDefaultsResetsStaleSnapshotWithoutRow(t *testing.T) {
	ctx := context.Background()
	service, st, _, _ := newScopedRestoreFixture(t)
	if err := st.SetDefaultBlockBinding(ctx, "reasoner", nil, store.ExtensionDefault{
		Key: "block:reasoner", Outcome: store.ExtensionDefaultApplied,
		Detail: `{"mcp_tools":["mcp__codebase-memory__old"],"skills":["code-review"]}`,
	}); err != nil {
		t.Fatal(err)
	}
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, DryRun: true})
	if change := restoreChange(preview, "reasoner"); change.Status != restoreStatusChange {
		t.Fatalf("reasoner preview = %+v, want change", change)
	}
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, PreviewToken: preview.PreviewToken})
	if _, ok := platformBindings(t, st)["reasoner"]; !ok {
		t.Fatal("restore did not create the empty row needed to follow later grants")
	}
	if !sameDefaultBinding(store.BlockBinding{Scope: store.ScopePlatform, Block: "reasoner"}, ledgerRows(t, st)["block:reasoner"].Detail) {
		t.Fatalf("reasoner snapshot = %+v, want the empty binding", ledgerRows(t, st)["block:reasoner"])
	}
}

// A skipped file-binding grant can be the only ledger row for a block. A
// restore that only rewrites that grant must not create a row without a
// snapshot; a later release that grants the block again must still follow.
func TestRestoreDefaultsLedgerOnlyRemoveKeepsBlockFollowable(t *testing.T) {
	oldSkills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review", "review"}}}
	fileBlocks := map[string]config.BlockBinding{"review": {}}
	service, st := newRestoreFixture(t, restoreConfig(oldSkills, nil))
	applySkillDefaults(t, service, st, fileBlocks, oldSkills)

	if _, ok := platformBindings(t, st)["review"]; ok {
		t.Fatal("test setup wrote a review row")
	}
	if _, ok := ledgerRows(t, st)["block:review"]; ok {
		t.Fatal("test setup wrote a review snapshot")
	}
	if row := ledgerRows(t, st)["skill:code-review@review"]; row.Outcome != store.ExtensionDefaultSkipped || row.Detail != reasonFileBinding {
		t.Fatalf("review grant = %+v, want skipped file binding", row)
	}

	newSkills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review"}}}
	current := newRestoreServiceOn(t, st, restoreConfig(newSkills, nil))
	preview := restoreBlocks(t, current, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(preview, "review")
	if change.Status != restoreStatusChange || change.OwnershipChanged {
		t.Fatalf("review preview = %+v, want change without ownership change", change)
	}
	if len(change.Add.MCPTools)+len(change.Add.Skills)+len(change.Remove.MCPTools)+len(change.Remove.Skills) != 0 {
		t.Fatalf("ledger-only preview reported entries: %+v / %+v", change.Add, change.Remove)
	}
	restoreBlocks(t, current, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})

	if _, ok := platformBindings(t, st)["review"]; ok {
		t.Fatal("ledger-only restore created a row without a snapshot")
	}
	if _, ok := ledgerRows(t, st)["block:review"]; ok {
		t.Fatal("ledger-only restore created a snapshot without a row")
	}
	if row := ledgerRows(t, st)["skill:code-review@review"]; row.Outcome != store.ExtensionDefaultSkipped || row.Detail != reasonGrantRemoved {
		t.Fatalf("review grant after restore = %+v, want removed", row)
	}

	// The default granting review again must create the row and its snapshot.
	applySkillDefaults(t, current, st, nil, oldSkills)
	row, ok := platformBindings(t, st)["review"]
	if !ok || !reflect.DeepEqual(row.Skills, []string{"code-review"}) {
		t.Fatalf("review row after re-grant = %+v (present %v)", row, ok)
	}
	if !sameDefaultBinding(row, ledgerRows(t, st)["block:review"].Detail) {
		t.Fatalf("review snapshot does not match the re-granted row")
	}
}

func TestRestorePreviewConflictsAfterBindingReorder(t *testing.T) {
	skills, servers := shippedDefaults()
	servers[0].Tools["review"] = []string{"search_graph", "trace_path"}
	service, st := newRestoreFixture(t, restoreConfig(skills, servers))
	applySkillDefaults(t, service, st, nil, skills)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	before := restoreChange(preview, "review")
	if before.Status != restoreStatusUnchanged || before.OwnershipChanged {
		t.Fatalf("expected unchanged baseline: %+v", before)
	}
	binding := platformBindings(t, st)["review"]
	if len(binding.MCPTools) != 2 {
		t.Fatalf("expected two tools: %+v", binding)
	}
	binding.MCPTools[0], binding.MCPTools[1] = binding.MCPTools[1], binding.MCPTools[0]
	setPlatformBinding(t, service, "review", binding.MCPTools, binding.Skills)
	ledger := ledgerRows(t, st)
	result, err := service.RestoreDefaults(context.Background(), RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("expected conflict after reordering; result=%+v error=%v", result, err)
	}
	if got := platformBindings(t, st)["review"]; !reflect.DeepEqual(got.MCPTools, binding.MCPTools) {
		t.Fatalf("conflict changed the reordered binding: %+v", got)
	}
	if got := ledgerRows(t, st); !reflect.DeepEqual(got, ledger) {
		t.Fatalf("conflict changed the default ledger")
	}
}

func TestRestoreRemovesWhitespacePaddedBuiltinSkill(t *testing.T) {
	service, st, _, _ := newScopedRestoreFixture(t)
	setPlatformBinding(t, service, "reasoner", nil, []string{" code-review "})
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, DryRun: true})
	change := restoreChange(preview, "reasoner")
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, PreviewToken: preview.PreviewToken})
	got := platformBindings(t, st)["reasoner"].Skills
	for _, name := range got {
		if strings.TrimSpace(name) == "code-review" {
			t.Fatalf("restored binding still enables the built-in Skill: preview remove=%q binding=%q", change.Remove.Skills, got)
		}
	}
	if !reflect.DeepEqual(change.Remove.Skills, []string{" code-review "}) {
		t.Fatalf("preview remove = %q, want the original stored name", change.Remove.Skills)
	}
}

// A reorder of the Skill list is an administrator change even though the set
// is equal. It must invalidate the preview and leave the row and ledger alone.
func TestRestorePreviewConflictsAfterSkillReorder(t *testing.T) {
	service, st, _, _ := newScopedRestoreFixture(t)
	setPlatformBinding(t, service, "code-review", []string{"mcp__codebase-memory__search_graph"}, []string{"code-review", "team-rules"})
	baseline := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"code-review"}, DryRun: true})
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"code-review"}, PreviewToken: baseline.PreviewToken})
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"code-review"}, DryRun: true})
	if change := restoreChange(preview, "code-review"); change.Status != restoreStatusUnchanged {
		t.Fatalf("expected unchanged baseline: %+v", change)
	}
	binding := platformBindings(t, st)["code-review"]
	binding.Skills[0], binding.Skills[1] = binding.Skills[1], binding.Skills[0]
	setPlatformBinding(t, service, "code-review", binding.MCPTools, binding.Skills)
	ledger := ledgerRows(t, st)

	_, err := service.RestoreDefaults(context.Background(), RestoreDefaultsRequest{
		Blocks: []string{"code-review"}, PreviewToken: preview.PreviewToken,
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("expected conflict after reordering Skills; error=%v", err)
	}
	if got := platformBindings(t, st)["code-review"]; !reflect.DeepEqual(got.Skills, binding.Skills) {
		t.Fatalf("conflict changed the reordered binding: %+v", got)
	}
	if got := ledgerRows(t, st); !reflect.DeepEqual(got, ledger) {
		t.Fatalf("conflict changed the default ledger")
	}
}

// Two processes share storage but ship the same grants in a different order.
// The second process must reject the first process's preview token.
func TestRestorePreviewConflictsAfterProcessDefaultsReorder(t *testing.T) {
	ctx := context.Background()
	skills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review"}}}
	serversA := graphDefault(analysisArgs)
	serversA[0].Tools["review"] = []string{"search_graph", "trace_path"}
	cfgA := restoreConfig(skills, serversA)
	serviceA, st := newRestoreFixture(t, cfgA)
	applySkillDefaults(t, serviceA, st, nil, skills)
	applyMCPDefaults(t, serviceA, st, nil, mcp.Config{}, serversA)
	previewA := restoreBlocks(t, serviceA, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if change := restoreChange(previewA, "review"); change.Status != restoreStatusUnchanged {
		t.Fatalf("expected unchanged baseline: %+v", change)
	}

	serversB := graphDefault(analysisArgs)
	serversB[0].Tools["review"] = []string{"trace_path", "search_graph"}
	serviceB := newRestoreServiceOn(t, st, restoreConfig(skills, serversB))
	previewB := restoreBlocks(t, serviceB, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if previewB.PreviewToken == previewA.PreviewToken {
		t.Fatal("tokens are equal although the shipped grant order differs")
	}
	binding := platformBindings(t, st)["review"]
	ledger := ledgerRows(t, st)
	_, err := serviceB.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: previewA.PreviewToken,
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("stale token from the reordered process = %v, want conflict", err)
	}
	if got := platformBindings(t, st)["review"]; !reflect.DeepEqual(got.MCPTools, binding.MCPTools) || !reflect.DeepEqual(got.Skills, binding.Skills) {
		t.Fatalf("conflict changed the binding: %+v", got)
	}
	if got := ledgerRows(t, st); !reflect.DeepEqual(got, ledger) {
		t.Fatal("conflict changed the default ledger")
	}
}

// A file-declared block never gets a platform write, but its declaration is
// still part of the decision. Reordering the declared lists must conflict.
func TestRestorePreviewConflictsAfterFileBindingReorder(t *testing.T) {
	ctx := context.Background()
	skills, servers := shippedDefaults()
	cfgA := restoreConfig(skills, servers)
	cfgA.Blocks = map[string]config.BlockBinding{
		"review": {MCPTools: []string{"mcp__team__a", "mcp__team__b"}, Skills: []string{"a", "b"}},
	}
	serviceA, st := newRestoreFixture(t, cfgA)
	previewA := restoreBlocks(t, serviceA, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})

	cfgB := restoreConfig(skills, servers)
	cfgB.Blocks = map[string]config.BlockBinding{
		"review": {MCPTools: []string{"mcp__team__b", "mcp__team__a"}, Skills: []string{"b", "a"}},
	}
	serviceB := newRestoreServiceOn(t, st, cfgB)
	previewB := restoreBlocks(t, serviceB, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if previewB.PreviewToken == previewA.PreviewToken {
		t.Fatal("tokens are equal although the file declaration order differs")
	}
	_, err := serviceB.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: previewA.PreviewToken,
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("stale token from the reordered file declaration = %v, want conflict", err)
	}
	if _, ok := platformBindings(t, st)["review"]; ok {
		t.Fatal("conflict wrote a row for a file-declared block")
	}
}

// The runtime trims Skill and MCP tool names before binding them. Restore must
// clean padded built-in aliases from existing rows while keeping custom and
// blank entries with their original text and order.
func TestRestoreDefaultsRemovesWhitespacePaddedBuiltinGrantsWithDefaults(t *testing.T) {
	service, st, _, _ := newScopedRestoreFixture(t)
	setPlatformBinding(t, service, "review",
		[]string{
			" mcp__codebase-memory__search_graph",
			"mcp__codebase-memory__old_tool ",
			"mcp__team__lookup ",
		},
		[]string{" code-review ", " team-rules ", ""})
	beforeMCP := platformMCPServer(t, st, "codebase-memory")
	if beforeMCP == nil || !beforeMCP.Enabled {
		t.Fatalf("test setup lost the enabled built-in MCP server: %+v", beforeMCP)
	}

	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(preview, "review")
	if change.Status != restoreStatusChange {
		t.Fatalf("review preview = %+v, want change", change)
	}
	if want := []string{" mcp__codebase-memory__search_graph", "mcp__codebase-memory__old_tool "}; !reflect.DeepEqual(change.Remove.MCPTools, want) {
		t.Fatalf("preview remove tools = %q, want %q", change.Remove.MCPTools, want)
	}
	if want := []string{"mcp__codebase-memory__search_graph"}; !reflect.DeepEqual(change.Add.MCPTools, want) {
		t.Fatalf("preview add tools = %q, want %q", change.Add.MCPTools, want)
	}
	if want := []string{" code-review "}; !reflect.DeepEqual(change.Remove.Skills, want) {
		t.Fatalf("preview remove skills = %q, want %q", change.Remove.Skills, want)
	}
	if len(change.Add.Skills) != 0 {
		t.Fatalf("preview added an undeclared Skill default: %q", change.Add.Skills)
	}

	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})
	row := platformBindings(t, st)["review"]
	if want := []string{"mcp__team__lookup ", "mcp__codebase-memory__search_graph"}; !reflect.DeepEqual(row.MCPTools, want) {
		t.Fatalf("restored tools = %q, want %q", row.MCPTools, want)
	}
	if want := []string{" team-rules ", ""}; !reflect.DeepEqual(row.Skills, want) {
		t.Fatalf("restored skills = %q, want %q", row.Skills, want)
	}
	if !sameDefaultBinding(row, ledgerRows(t, st)["block:review"].Detail) {
		t.Fatalf("snapshot does not match the restored row")
	}
	if afterMCP := platformMCPServer(t, st, "codebase-memory"); afterMCP == nil || !afterMCP.Enabled {
		t.Fatalf("restore changed the MCP server enabled state: %+v", afterMCP)
	}
	skills, err := st.ListSkills(context.Background(), store.ScopePlatform, nil)
	if err != nil || len(skills) != 1 || !skills[0].Enabled {
		t.Fatalf("restore changed the Skill enabled state: %+v, %v", skills, err)
	}
}

// A block with no shipped grant still loses padded copies of every registered
// built-in, and keeps custom and unknown entries untouched.
func TestRestoreDefaultsRemovesWhitespacePaddedBuiltinGrantsWithoutDefaults(t *testing.T) {
	service, st, _, _ := newScopedRestoreFixture(t)
	setPlatformBinding(t, service, "reasoner",
		[]string{"mcp__codebase-memory__trace_path ", "  mcp__team__lookup"},
		[]string{"code-review", " code-review ", "team-rules "})
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, DryRun: true})
	change := restoreChange(preview, "reasoner")
	if want := []string{"mcp__codebase-memory__trace_path "}; !reflect.DeepEqual(change.Remove.MCPTools, want) {
		t.Fatalf("preview remove tools = %q, want %q", change.Remove.MCPTools, want)
	}
	if want := []string{" code-review ", "code-review"}; !reflect.DeepEqual(change.Remove.Skills, want) {
		t.Fatalf("preview remove skills = %q, want %q", change.Remove.Skills, want)
	}
	if len(change.Add.MCPTools)+len(change.Add.Skills) != 0 {
		t.Fatalf("preview added entries without a default grant: %+v", change.Add)
	}

	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"reasoner"}, PreviewToken: preview.PreviewToken})
	row := platformBindings(t, st)["reasoner"]
	if want := []string{"  mcp__team__lookup"}; !reflect.DeepEqual(row.MCPTools, want) {
		t.Fatalf("restored tools = %q, want %q", row.MCPTools, want)
	}
	if want := []string{"team-rules "}; !reflect.DeepEqual(row.Skills, want) {
		t.Fatalf("restored skills = %q, want %q", row.Skills, want)
	}
}

// Duplicate and blank entries must appear in the preview exactly as the write
// changes them. The shipped grant is added back once, not once per alias.
func TestRestoreDefaultsDeduplicatesBuiltinAliasesAndKeepsBlankEntries(t *testing.T) {
	service, st, _, _ := newScopedRestoreFixture(t)
	setPlatformBinding(t, service, "code-review",
		[]string{"mcp__codebase-memory__search_graph", " mcp__codebase-memory__search_graph "},
		[]string{"code-review", " code-review ", "", "team-rules", "code-review"})
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"code-review"}, DryRun: true})
	change := restoreChange(preview, "code-review")
	if want := []string{" mcp__codebase-memory__search_graph "}; !reflect.DeepEqual(change.Remove.MCPTools, want) {
		t.Fatalf("preview remove tools = %q, want %q", change.Remove.MCPTools, want)
	}
	if want := []string{" code-review ", "code-review"}; !reflect.DeepEqual(change.Remove.Skills, want) {
		t.Fatalf("preview remove skills = %q, want %q", change.Remove.Skills, want)
	}
	if len(change.Add.MCPTools)+len(change.Add.Skills) != 0 {
		t.Fatalf("preview reported a false addition: %+v", change.Add)
	}

	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"code-review"}, PreviewToken: preview.PreviewToken})
	row := platformBindings(t, st)["code-review"]
	if want := []string{"mcp__codebase-memory__search_graph"}; !reflect.DeepEqual(row.MCPTools, want) {
		t.Fatalf("restored tools = %q, want %q", row.MCPTools, want)
	}
	if want := []string{"", "team-rules", "code-review"}; !reflect.DeepEqual(row.Skills, want) {
		t.Fatalf("restored skills = %q, want %q", row.Skills, want)
	}
}

// A project binding with the same padded built-in names is not in the restore
// scope and must keep its exact entries.
func TestRestoreDefaultsLeavesWhitespaceProjectBinding(t *testing.T) {
	ctx := context.Background()
	service, st, _, _ := newScopedRestoreFixture(t)
	setPlatformBinding(t, service, "review", nil, nil)
	projectID := int64(42)
	project := store.BlockBinding{
		Scope: store.ScopeProject, ProjectID: &projectID, Block: "review",
		MCPTools: []string{"mcp__codebase-memory__search_graph ", " mcp__team__lookup"},
		Skills:   []string{" code-review ", "project-rules"},
	}
	if _, err := st.UpsertBlockBinding(ctx, project); err != nil {
		t.Fatal(err)
	}
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})

	rows, err := st.ListBlockBindings(ctx, store.ScopeProject, &projectID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("project bindings = %+v, %v", rows, err)
	}
	if !reflect.DeepEqual(rows[0].MCPTools, project.MCPTools) || !reflect.DeepEqual(rows[0].Skills, project.Skills) {
		t.Fatalf("restore changed the project binding: %+v", rows[0])
	}
}

// Two registered built-ins grant the same block. The declaration order decides
// the order of the stored grants, so swapping their declarations in another
// process must invalidate the preview even when the set is equal.
func TestRestorePreviewConflictsAfterDefaultDeclarationReorder(t *testing.T) {
	ctx := context.Background()
	alpha := config.DefaultMCPServer{
		Name: "alpha", Config: map[string]any{"command": "/bin/true"},
		Tools: map[string][]string{"review": {"tool_a"}},
	}
	beta := config.DefaultMCPServer{
		Name: "beta", Config: map[string]any{"command": "/bin/true"},
		Tools: map[string][]string{"review": {"tool_b"}},
	}
	cfgA := restoreConfig(nil, []config.DefaultMCPServer{alpha, beta})
	serviceA, st := newRestoreFixture(t, cfgA)
	applyMCPDefaults(t, serviceA, st, nil, mcp.Config{}, cfgA.DefaultExtensions.MCPServers)
	// The administrator replaced the shipped grants with one custom tool.
	setPlatformBinding(t, serviceA, "review", []string{"mcp__team__lookup"}, nil)
	previewA := restoreBlocks(t, serviceA, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if want := []string{"mcp__alpha__tool_a", "mcp__beta__tool_b"}; !reflect.DeepEqual(restoreChange(previewA, "review").Add.MCPTools, want) {
		t.Fatalf("baseline add tools = %q, want the declaration order %q", restoreChange(previewA, "review").Add.MCPTools, want)
	}

	cfgB := restoreConfig(nil, []config.DefaultMCPServer{beta, alpha})
	serviceB := newRestoreServiceOn(t, st, cfgB)
	previewB := restoreBlocks(t, serviceB, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if previewB.PreviewToken == previewA.PreviewToken {
		t.Fatal("tokens are equal although the default declaration order differs")
	}
	// The order is not cosmetic: it is the order the plan writes.
	defaultsA, filesA, err := serviceA.restoreDefaultSet()
	if err != nil {
		t.Fatal(err)
	}
	defaultsB, filesB, err := serviceB.restoreDefaultSet()
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.ReadDefaultRestoreState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	planA, err := planDefaultRestore(defaultsA, filesA, []string{"review"}, state)
	if err != nil {
		t.Fatal(err)
	}
	planB, err := planDefaultRestore(defaultsB, filesB, []string{"review"}, state)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mcp__alpha__tool_a", "mcp__beta__tool_b"}; !reflect.DeepEqual(planA.plan.Bindings[0].MCPTools[1:], want) {
		t.Fatalf("plan A tools = %q, want %q", planA.plan.Bindings[0].MCPTools, want)
	}
	if want := []string{"mcp__beta__tool_b", "mcp__alpha__tool_a"}; !reflect.DeepEqual(planB.plan.Bindings[0].MCPTools[1:], want) {
		t.Fatalf("plan B tools = %q, want %q", planB.plan.Bindings[0].MCPTools, want)
	}
	binding := platformBindings(t, st)["review"]
	ledger := ledgerRows(t, st)
	_, err = serviceB.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: previewA.PreviewToken,
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("stale token from the reordered defaults = %v, want conflict", err)
	}
	if got := platformBindings(t, st)["review"]; !reflect.DeepEqual(got.MCPTools, binding.MCPTools) || !reflect.DeepEqual(got.Skills, binding.Skills) {
		t.Fatalf("conflict changed the binding: %+v", got)
	}
	if got := ledgerRows(t, st); !reflect.DeepEqual(got, ledger) {
		t.Fatal("conflict changed the default ledger")
	}
}
