package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// A release that drops a block from a shipped default removes the grant it
// added while the block row is still default-owned.
func TestSkillDefaultsRemoveGrantDroppedFromConfig(t *testing.T) {
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	applySkillDefaults(t, service, st, nil, []config.DefaultSkill{{
		Name: "code-review", Blocks: []string{"code-review"},
	}})

	bindings := platformBindings(t, st)
	if got := bindings["review"].Skills; len(got) != 0 {
		t.Fatalf("review skills after the grant was dropped = %v, want none", got)
	}
	if got := ledgerRows(t, st)["skill:code-review@review"]; got.Outcome != store.ExtensionDefaultSkipped || got.Detail != reasonGrantRemoved {
		t.Fatalf("dropped grant ledger = %+v, want skipped %q", got, reasonGrantRemoved)
	}
	if !sameDefaultBinding(bindings["review"], ledgerRows(t, st)["block:review"].Detail) {
		t.Fatalf("block snapshot does not match the rewritten row: %+v", bindings["review"])
	}
	if got := bindings["code-review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("code-review skills = %v, want the remaining grant", got)
	}
}

// Putting the block back into the shipped default restores the grant.
func TestSkillDefaultsReAddGrantThatReturnsToConfig(t *testing.T) {
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	applySkillDefaults(t, service, st, nil, []config.DefaultSkill{{
		Name: "code-review", Blocks: []string{"code-review"},
	}})
	if got := platformBindings(t, st)["review"].Skills; len(got) != 0 {
		t.Fatalf("review skills after the drop = %v, want none", got)
	}

	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	if got := platformBindings(t, st)["review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("review skills after the grant returned = %v, want [code-review]", got)
	}
	if got := ledgerRows(t, st)["skill:code-review@review"]; got.Outcome != store.ExtensionDefaultApplied {
		t.Fatalf("re-added grant ledger = %+v, want applied", got)
	}
}

// A block an administrator changed is never rewritten when the shipped
// default drops its grant.
func TestSkillDefaultsKeepAdministratorChangeWhenGrantIsDropped(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	custom := []string{"mcp__team__lookup"}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: store.ScopePlatform, Block: "review", MCPTools: custom,
		Skills: []string{"code-review", "team-rules"},
	}); err != nil {
		t.Fatal(err)
	}

	applySkillDefaults(t, service, st, nil, []config.DefaultSkill{{
		Name: "code-review", Blocks: []string{"code-review"},
	}})

	got := platformBindings(t, st)["review"]
	if !reflect.DeepEqual(got.MCPTools, custom) || !reflect.DeepEqual(got.Skills, []string{"code-review", "team-rules"}) {
		t.Fatalf("administrator binding changed: %+v", got)
	}
}

// Removing the whole default declaration keeps the existing registrations and
// grants. Only a default that is still shipped can drop one of its grants.
func TestEmptyDefaultExtensionsKeepBindingsAndLedger(t *testing.T) {
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	before := ledgerRows(t, st)

	applySkillDefaults(t, service, st, nil, nil)

	if got := platformBindings(t, st)["review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("review skills after default_extensions.skills was emptied = %v", got)
	}
	if after := ledgerRows(t, st); !reflect.DeepEqual(after, before) {
		t.Fatalf("ledger changed when the declaration was removed:\nbefore %+v\nafter  %+v", before, after)
	}
}

// A grant ledger row from an older release has no detail. The seeder cannot
// prove which entries it added, so it must not guess and delete entries.
func TestDroppedGrantWithoutDetailIsNotGuessed(t *testing.T) {
	skills := defaultCodeReview
	servers := graphDefault(analysisArgs, "search_graph")
	servers[0].Tools["review"] = []string{"search_graph"}
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, skills)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)

	binding := store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review",
		MCPTools: []string{"mcp__codebase-memory__search_graph", "mcp__team__lookup"},
		Skills:   []string{"code-review"},
	}
	setBlockSnapshot(t, st, binding,
		store.ExtensionDefault{Key: "mcp:codebase-memory@review", Outcome: store.ExtensionDefaultApplied},
	)
	servers[0].Tools = map[string][]string{"code-review": {"search_graph"}}

	// An MCP grant without a detail cannot be reconstructed from the server
	// name. It stays applied so every start retries it instead of claiming a
	// removal.
	for i := 0; i < 2; i++ {
		applySkillDefaults(t, service, st, nil, skills)
		applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)
		if got := sortedCopy(platformBindings(t, st)["review"].MCPTools); !reflect.DeepEqual(got, []string{"mcp__codebase-memory__search_graph", "mcp__team__lookup"}) {
			t.Fatalf("start %d tools = %v, want entries kept until proven", i, got)
		}
		if got := ledgerRows(t, st)["mcp:codebase-memory@review"]; got.Outcome != store.ExtensionDefaultApplied {
			t.Fatalf("start %d ledger = %+v, want applied for retry", i, got)
		}
	}
}

// A restored row can combine default-owned entries with grants an
// administrator added. Declaring the block in the file must then keep the row,
// because the defaults did not write those extra entries.
func TestFileBlockKeepsRestoredBindingWithCustomGrants(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	merged := store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review",
		MCPTools: []string{"mcp__team__lookup"},
		Skills:   []string{"code-review"},
	}
	snapshot, err := encodeDefaultBinding(merged)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultBlockBinding(ctx, "review", &merged,
		store.ExtensionDefault{Key: "block:review", Outcome: store.ExtensionDefaultApplied, Detail: snapshot},
		store.ExtensionDefault{Key: "skill:code-review@review", Outcome: store.ExtensionDefaultApplied, Detail: `{"mcp_tools":[],"skills":["code-review"]}`},
	); err != nil {
		t.Fatal(err)
	}

	fileBlocks := map[string]config.BlockBinding{"review": {Skills: []string{"coding-standards"}}}
	if err := ApplyExtensionDefaults(ctx, st, fileBlocks, nil, nil); err != nil {
		t.Fatalf("ApplyExtensionDefaults: %v", err)
	}

	got, ok := platformBindings(t, st)["review"]
	if !ok {
		t.Fatal("release deleted the restored row and its custom grants")
	}
	if !reflect.DeepEqual(got.MCPTools, merged.MCPTools) || !reflect.DeepEqual(got.Skills, merged.Skills) {
		t.Fatalf("release changed the restored row: %+v", got)
	}
}

// Without grant details the release cannot prove the row is purely
// default-owned, so it must keep the row.
func TestFileBlockKeepsBindingWithoutGrantDetail(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	if err := st.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key: "skill:code-review@review", Outcome: store.ExtensionDefaultApplied,
	}); err != nil {
		t.Fatal(err)
	}

	fileBlocks := map[string]config.BlockBinding{"review": {Skills: []string{"coding-standards"}}}
	if err := ApplyExtensionDefaults(ctx, st, fileBlocks, nil, nil); err != nil {
		t.Fatalf("ApplyExtensionDefaults: %v", err)
	}

	if _, ok := platformBindings(t, st)["review"]; !ok {
		t.Fatal("release deleted a row it could not prove was purely default-owned")
	}
}
