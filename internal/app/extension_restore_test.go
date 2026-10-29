package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func newRestoreFixture(t *testing.T, cfg *config.Config) (*ExtensionService, *sqlite.Store) {
	t.Helper()
	st := openTestStore(t)
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: workspace.NewManager(t.TempDir(), nil),
		Platform:  cfg,
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	return service, st
}

func restoreConfig(skills []config.DefaultSkill, servers []config.DefaultMCPServer) *config.Config {
	return &config.Config{DefaultExtensions: config.DefaultExtensionsConfig{Skills: skills, MCPServers: servers}}
}

func restoreBlocks(t *testing.T, service *ExtensionService, req RestoreDefaultsRequest) *RestoreDefaultsResult {
	t.Helper()
	result, err := service.RestoreDefaults(context.Background(), req)
	if err != nil {
		t.Fatalf("RestoreDefaults: %v", err)
	}
	return result
}

func restoreChange(result *RestoreDefaultsResult, block string) RestoreDefaultsChange {
	for _, change := range result.Changes {
		if change.Block == block {
			return change
		}
	}
	return RestoreDefaultsChange{}
}

// Restore brings a block an administrator changed back to the shipped
// defaults while keeping grants the defaults do not own.
func TestRestoreDefaultsRestoresChangedBlockAndKeepsCustomGrants(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	custom := []string{"mcp__team__lookup"}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review", MCPTools: custom,
	}); err != nil {
		t.Fatal(err)
	}

	beforeLedger := ledgerRows(t, st)
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(preview, "review")
	if change.Status != "change" || !change.OwnershipChanged {
		t.Fatalf("preview change = %+v", change)
	}
	if !reflect.DeepEqual(change.Add.Skills, []string{"code-review"}) || len(change.Remove.Skills) != 0 {
		t.Fatalf("preview add/remove = %+v / %+v", change.Add, change.Remove)
	}
	if preview.PreviewToken == "" || !preview.RestartRequired {
		t.Fatalf("preview metadata = %+v", preview)
	}
	if got := platformBindings(t, st)["review"].Skills; len(got) != 0 {
		t.Fatalf("dry-run wrote the binding: %v", got)
	}
	if afterLedger := ledgerRows(t, st); !reflect.DeepEqual(afterLedger, beforeLedger) {
		t.Fatalf("dry-run wrote ledger rows")
	}

	result := restoreBlocks(t, service, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: preview.PreviewToken,
	})
	if got := restoreChange(result, "review"); got.Status != "change" {
		t.Fatalf("execute change = %+v", got)
	}
	got := platformBindings(t, st)["review"]
	if !reflect.DeepEqual(got.Skills, []string{"code-review"}) || !reflect.DeepEqual(got.MCPTools, custom) {
		t.Fatalf("restored binding = %+v", got)
	}
	if row := ledgerRows(t, st)["skill:code-review@review"]; row.Outcome != "applied" {
		t.Fatalf("grant ledger = %+v", row)
	}
	if !sameDefaultBinding(got, ledgerRows(t, st)["block:review"].Detail) {
		t.Fatalf("block snapshot does not match the restored row")
	}
}

// Restore removes extra grants of the shipped built-in MCP server and keeps
// grants of other servers.
func TestRestoreDefaultsRemovesExtraBuiltinMCPGrants(t *testing.T) {
	ctx := context.Background()
	servers := graphDefault(analysisArgs)
	servers[0].Tools["review"] = []string{"search_graph"}
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, servers))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review",
		MCPTools: []string{
			"mcp__codebase-memory__search_graph",
			"mcp__codebase-memory__old_tool",
			"mcp__team__lookup",
		},
		Skills: []string{"code-review"},
	}); err != nil {
		t.Fatal(err)
	}

	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(preview, "review")
	if !reflect.DeepEqual(change.Remove.MCPTools, []string{"mcp__codebase-memory__old_tool"}) {
		t.Fatalf("preview remove = %+v", change.Remove)
	}
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})

	if got := sortedCopy(platformBindings(t, st)["review"].MCPTools); !reflect.DeepEqual(got, []string{"mcp__codebase-memory__search_graph", "mcp__team__lookup"}) {
		t.Fatalf("restored tools = %v", got)
	}
}

// A preview becomes stale when the target row changes before execution.
func TestRestoreDefaultsConflictsAfterPreview(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review", MCPTools: []string{"mcp__team__lookup"},
	}); err != nil {
		t.Fatal(err)
	}
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})

	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review", MCPTools: []string{"mcp__new__value"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := service.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: preview.PreviewToken,
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("stale restore = %v, want conflict", err)
	}
	if got := platformBindings(t, st)["review"].MCPTools; !reflect.DeepEqual(got, []string{"mcp__new__value"}) {
		t.Fatalf("conflict overwrote the new binding: %v", got)
	}
}

// A block explicitly declared by the file is skipped and the preview shows it.
func TestRestoreDefaultsSkipsFileConfiguredBlock(t *testing.T) {
	cfg := restoreConfig(defaultCodeReview, nil)
	cfg.Blocks = map[string]config.BlockBinding{"review": {}}
	service, _ := newRestoreFixture(t, cfg)
	result := restoreBlocks(t, service, RestoreDefaultsRequest{DryRun: true})
	change := restoreChange(result, "review")
	if change.Status != "skipped" || change.Reason != "file_binding" {
		t.Fatalf("file block change = %+v", change)
	}
}

// Without a registered, applied built-in default there is nothing to restore.
func TestRestoreDefaultsSkipsWithoutRegisteredDefaults(t *testing.T) {
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	if _, err := st.UpsertBlockBinding(context.Background(), store.BlockBinding{
		Scope: "platform", Block: "review", MCPTools: []string{"mcp__team__lookup"},
	}); err != nil {
		t.Fatal(err)
	}
	result := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(result, "review")
	if change.Status != "skipped" || change.Reason != "no_registered_defaults" {
		t.Fatalf("unregistered change = %+v", change)
	}
	if got := platformBindings(t, st)["review"].MCPTools; !reflect.DeepEqual(got, []string{"mcp__team__lookup"}) {
		t.Fatalf("restore changed a block without registered defaults: %v", got)
	}
}

// A platform resource an administrator created under the same name is not the
// shipped default and must not become restorable.
func TestRestoreDefaultsKeepsSameNameCustomResource(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	if _, err := st.CreateSkill(ctx, store.Skill{
		Scope: "platform", Name: "code-review", Description: "team copy", Dir: t.TempDir(), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key: "skill:code-review", Outcome: store.ExtensionDefaultApplied,
	}); err != nil {
		t.Fatal(err)
	}
	result := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(result, "review")
	if change.Status != "skipped" || change.Reason != "no_registered_defaults" {
		t.Fatalf("custom same-name change = %+v", change)
	}
}

// A disabled built-in keeps its enabled state but still supplies the default
// grant. Disabling it after the preview invalidates the token.
func TestRestoreDefaultsIncludesDisabledBuiltin(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	rows, _ := st.ListSkills(ctx, "platform", nil)
	if _, err := service.SetSkillEnabled(ctx, rows[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review",
	}); err != nil {
		t.Fatal(err)
	}
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	if change := restoreChange(preview, "review"); change.Status != "change" {
		t.Fatalf("disabled builtin change = %+v", change)
	}
	if _, err := service.SetSkillEnabled(ctx, rows[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: preview.PreviewToken,
	}); err == nil {
		t.Fatal("restore accepted a token from before the built-in was enabled")
	}
}

// After a restore the block follows later shipped grant changes again.
func TestRestoreDefaultsFollowsLaterUpgrades(t *testing.T) {
	ctx := context.Background()
	servers := graphDefault(analysisArgs, "search_graph")
	servers[0].Tools["review"] = []string{"search_graph"}
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, servers))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review",
	}); err != nil {
		t.Fatal(err)
	}
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})

	upgraded := graphDefault(analysisArgs, "search_graph", "trace_path")
	upgraded[0].Tools["review"] = []string{"search_graph", "trace_path"}
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, upgraded)
	want := []string{"mcp__codebase-memory__search_graph", "mcp__codebase-memory__trace_path"}
	if got := platformBindings(t, st)["review"].MCPTools; !reflect.DeepEqual(got, want) {
		t.Fatalf("post-restore upgrade tools = %v, want %v", got, want)
	}
}

// Project-scoped bindings are not part of a platform restore.
func TestRestoreDefaultsLeavesProjectBindings(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review",
	}); err != nil {
		t.Fatal(err)
	}
	projectID := int64(42)
	if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope: "project", ProjectID: &projectID, Block: "review", Skills: []string{"project-rules"},
	}); err != nil {
		t.Fatal(err)
	}
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})
	rows, err := st.ListBlockBindings(ctx, "project", &projectID)
	if err != nil || len(rows) != 1 || !slices.Equal(rows[0].Skills, []string{"project-rules"}) {
		t.Fatalf("project binding = %+v, %v", rows, err)
	}
}

// A ledger-only change still reports change and writes the ledger without
// rewriting the unchanged row.
func TestRestoreDefaultsLedgerOnlyChange(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review", Skills: []string{"code-review"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key: "skill:code-review@review", Outcome: store.ExtensionDefaultSkipped, Detail: reasonCustomBinding,
	}); err != nil {
		t.Fatal(err)
	}

	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(preview, "review")
	if change.Status != "change" || change.OwnershipChanged {
		t.Fatalf("ledger-only change = %+v", change)
	}
	if len(change.Add.MCPTools)+len(change.Add.Skills)+len(change.Remove.MCPTools)+len(change.Remove.Skills) != 0 {
		t.Fatalf("ledger-only change reported entries: %+v / %+v", change.Add, change.Remove)
	}

	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})
	if got := ledgerRows(t, st)["skill:code-review@review"]; got.Outcome != store.ExtensionDefaultApplied {
		t.Fatalf("ledger after restore = %+v", got)
	}
	if got := platformBindings(t, st)["review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("row changed on a ledger-only restore: %v", got)
	}
}

// The default block set is all six bindable blocks, sorted, with every block
// visible in the preview.
func TestRestoreDefaultsAllBlocksSorted(t *testing.T) {
	service, _ := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	result := restoreBlocks(t, service, RestoreDefaultsRequest{DryRun: true})
	if len(result.Changes) != len(config.BindableBlocks) {
		t.Fatalf("changes = %+v, want every bindable block", result.Changes)
	}
	for i := 1; i < len(result.Changes); i++ {
		if result.Changes[i-1].Block >= result.Changes[i].Block {
			t.Fatalf("changes are not sorted: %+v", result.Changes)
		}
	}
}

// A restored row that carries an administrator's grant survives a later file
// declaration of the same block.
func TestRestoredBindingSurvivesFileBlock(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review", MCPTools: []string{"mcp__team__lookup"},
	}); err != nil {
		t.Fatal(err)
	}
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})

	fileBlocks := map[string]config.BlockBinding{"review": {Skills: []string{"coding-standards"}}}
	applySkillDefaults(t, service, st, fileBlocks, defaultCodeReview)

	got, ok := platformBindings(t, st)["review"]
	if !ok || !slices.Equal(got.MCPTools, []string{"mcp__team__lookup"}) {
		t.Fatalf("file release deleted the restored custom grant: %+v (present %v)", got, ok)
	}
}

// A same-name MCP server created by an administrator is not the shipped
// built-in; restore keeps its grants and writes nothing.
func TestRestoreDefaultsKeepsSameNameCustomMCP(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(nil, graphDefault(analysisArgs)))
	if _, err := st.CreateMCPServer(ctx, store.MCPServer{
		Scope: "platform", Name: "codebase-memory", Config: json.RawMessage(`{"command":"/opt/team"}`), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key: "mcp:codebase-memory", Outcome: store.ExtensionDefaultApplied,
	}); err != nil {
		t.Fatal(err)
	}
	custom := []string{"mcp__codebase-memory__team_tool"}
	if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope: "platform", Block: "review", MCPTools: custom,
	}); err != nil {
		t.Fatal(err)
	}
	result := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	change := restoreChange(result, "review")
	if change.Status != "skipped" || change.Reason != "no_registered_defaults" {
		t.Fatalf("custom same-name MCP change = %+v", change)
	}
	if got := platformBindings(t, st)["review"].MCPTools; !reflect.DeepEqual(got, custom) {
		t.Fatalf("restore changed the custom MCP grant: %v", got)
	}
}

// Restore works while a built-in is disabled and never enables it.
func TestRestoreDefaultsKeepsDisabledBuiltinDisabled(t *testing.T) {
	ctx := context.Background()
	service, st := newRestoreFixture(t, restoreConfig(defaultCodeReview, nil))
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	rows, _ := st.ListSkills(ctx, "platform", nil)
	if _, err := service.SetSkillEnabled(ctx, rows[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review",
	}); err != nil {
		t.Fatal(err)
	}
	preview := restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, DryRun: true})
	restoreBlocks(t, service, RestoreDefaultsRequest{Blocks: []string{"review"}, PreviewToken: preview.PreviewToken})

	if got := platformBindings(t, st)["review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("review skills after restore = %v", got)
	}
	skills, _ := st.ListSkills(ctx, "platform", nil)
	if len(skills) != 1 || skills[0].Enabled {
		t.Fatalf("restore changed the built-in enabled state: %+v", skills)
	}
}
