package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

func setBlockSnapshot(t *testing.T, st *sqlite.Store, binding store.BlockBinding, grants ...store.ExtensionDefault) {
	t.Helper()
	detail, err := encodeDefaultBinding(binding)
	if err != nil {
		t.Fatal(err)
	}
	rows := append([]store.ExtensionDefault{{
		Key: blockSnapshotKey(binding.Block), Outcome: store.ExtensionDefaultApplied, Detail: detail,
	}}, grants...)
	if err := st.SetDefaultBlockBinding(context.Background(), binding.Block, &binding, rows...); err != nil {
		t.Fatal(err)
	}
}

func dropSkillFromReview(skills []config.DefaultSkill) []config.DefaultSkill {
	out := make([]config.DefaultSkill, 0, len(skills))
	for _, skill := range skills {
		if skill.Name == "code-review" {
			skill.Blocks = []string{"code-review"}
		}
		out = append(out, skill)
	}
	return out
}

// An applied Skill grant with an empty detail still proves the Skill name
// because SkillDefaults grants exactly the Skill's own name. The upgrade
// removes that name, keeps custom entries, and is idempotent across restarts.
func TestDroppedSkillGrantWithoutDetailIsRemovedByName(t *testing.T) {
	ctx := context.Background()
	skills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review", "review"}}}
	servers := graphDefault(analysisArgs, "search_graph")
	servers[0].Tools["review"] = []string{"search_graph"}
	service, st := newRestoreFixture(t, restoreConfig(skills, servers))
	applySkillDefaults(t, service, st, nil, skills)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, servers)
	rows, _ := st.ListSkills(ctx, "platform", nil)
	if _, err := service.SetSkillEnabled(ctx, rows[0].ID, false); err != nil {
		t.Fatal(err)
	}

	binding := store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review",
		MCPTools: []string{"mcp__codebase-memory__search_graph", "mcp__team__lookup"},
		Skills:   []string{"code-review", "team-rules"},
	}
	setBlockSnapshot(t, st, binding,
		store.ExtensionDefault{Key: "skill:code-review@review", Outcome: store.ExtensionDefaultApplied},
		store.ExtensionDefault{Key: "mcp:codebase-memory@review", Outcome: store.ExtensionDefaultApplied, Detail: `{"mcp_tools":["mcp__codebase-memory__search_graph"],"skills":[]}`},
	)

	current := newRestoreServiceOn(t, st, restoreConfig(dropSkillFromReview(skills), servers))
	applySkillDefaults(t, current, st, nil, dropSkillFromReview(skills))
	applyMCPDefaults(t, current, st, nil, mcp.Config{}, servers)

	got := platformBindings(t, st)["review"]
	if !reflect.DeepEqual(sortedCopy(got.Skills), []string{"team-rules"}) {
		t.Fatalf("review skills = %v, want only the custom skill", got.Skills)
	}
	if !reflect.DeepEqual(sortedCopy(got.MCPTools), []string{"mcp__codebase-memory__search_graph", "mcp__team__lookup"}) {
		t.Fatalf("review tools = %v", got.MCPTools)
	}
	if !sameDefaultBinding(got, ledgerRows(t, st)["block:review"].Detail) {
		t.Fatalf("snapshot does not match the row after removing the skill")
	}
	if row := ledgerRows(t, st)["skill:code-review@review"]; row.Outcome != store.ExtensionDefaultSkipped || row.Detail != reasonGrantRemoved {
		t.Fatalf("skill ledger = %+v, want removed", row)
	}

	before := platformBindings(t, st)
	applySkillDefaults(t, current, st, nil, dropSkillFromReview(skills))
	applyMCPDefaults(t, current, st, nil, mcp.Config{}, servers)
	if after := platformBindings(t, st); !reflect.DeepEqual(after, before) {
		t.Fatalf("restart changed the row:\nbefore %+v\nafter  %+v", before, after)
	}
	if row := ledgerRows(t, st)["skill:code-review@review"]; row.Outcome != store.ExtensionDefaultSkipped || row.Detail != reasonGrantRemoved {
		t.Fatalf("restart changed the removed marker: %+v", row)
	}
	skillsAfter, _ := st.ListSkills(ctx, "platform", nil)
	if len(skillsAfter) != 1 || skillsAfter[0].Enabled {
		t.Fatalf("upgrade changed the disabled skill: %+v", skillsAfter)
	}
}

// A row the administrator changed keeps its entries even when the grant has
// an empty detail.
func TestDroppedSkillGrantWithoutDetailKeepsAdminChangedBlock(t *testing.T) {
	ctx := context.Background()
	skills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review", "review"}}}
	service, st := newRestoreFixture(t, restoreConfig(skills, nil))
	applySkillDefaults(t, service, st, nil, skills)
	if err := st.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key: "skill:code-review@review", Outcome: store.ExtensionDefaultApplied,
	}); err != nil {
		t.Fatal(err)
	}
	setPlatformBinding(t, service, "review", nil, []string{"code-review", "team-rules"})

	current := newRestoreServiceOn(t, st, restoreConfig(dropSkillFromReview(skills), nil))
	applySkillDefaults(t, current, st, nil, dropSkillFromReview(skills))

	got := platformBindings(t, st)["review"]
	if !reflect.DeepEqual(got.Skills, []string{"code-review", "team-rules"}) {
		t.Fatalf("administrator row changed: %v", got.Skills)
	}
	if row := ledgerRows(t, st)["skill:code-review@review"]; row.Outcome != store.ExtensionDefaultSkipped || row.Detail != reasonCustomBinding {
		t.Fatalf("skill ledger = %+v, want custom binding", row)
	}
}

// A platform resource with the built-in name but no builtin marker is the
// administrator's own Skill. The empty-detail inference must not remove it.
func TestDroppedSkillGrantWithoutDetailKeepsSameNameCustom(t *testing.T) {
	ctx := context.Background()
	skills := []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review"}}}
	service, st := newRestoreFixture(t, restoreConfig(skills, nil))
	skill, err := st.CreateSkill(ctx, store.Skill{
		Scope: "platform", Name: "code-review", Description: "team copy", Dir: t.TempDir(), Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if skill.Builtin != "" {
		t.Fatalf("custom skill carries builtin marker %q", skill.Builtin)
	}
	if err := st.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key: "skill:code-review", Outcome: store.ExtensionDefaultApplied,
	}); err != nil {
		t.Fatal(err)
	}
	binding := store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review", Skills: []string{"code-review"},
	}
	setBlockSnapshot(t, st, binding,
		store.ExtensionDefault{Key: "skill:code-review@review", Outcome: store.ExtensionDefaultApplied},
	)

	applySkillDefaults(t, service, st, nil, skills)

	got := platformBindings(t, st)["review"]
	if !reflect.DeepEqual(got.Skills, []string{"code-review"}) {
		t.Fatalf("custom same-name skill removed: %v", got.Skills)
	}
	if row := ledgerRows(t, st)["skill:code-review@review"]; row.Outcome != store.ExtensionDefaultSkipped || row.Detail != reasonCustomBinding {
		t.Fatalf("ledger = %+v, want custom binding", row)
	}
}
