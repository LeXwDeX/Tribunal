package sqlite

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestExtensionDefaultLedgerRoundtrip(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)

	rows, err := s.ListExtensionDefaults(ctx)
	if err != nil {
		t.Fatalf("ListExtensionDefaults empty: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("fresh ledger = %+v, want empty", rows)
	}

	if err := s.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key:     "skill:code-review",
		Outcome: store.ExtensionDefaultApplied,
	}); err != nil {
		t.Fatalf("PutExtensionDefault: %v", err)
	}
	if err := s.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key:     "block:review",
		Outcome: store.ExtensionDefaultApplied,
		Detail:  `{"mcp_tools":[],"skills":["code-review"]}`,
	}); err != nil {
		t.Fatalf("PutExtensionDefault block: %v", err)
	}
	// A later write replaces the outcome and detail of the same key.
	if err := s.PutExtensionDefault(ctx, store.ExtensionDefault{
		Key:     "block:review",
		Outcome: store.ExtensionDefaultApplied,
		Detail:  `{"mcp_tools":["mcp__a__b"],"skills":["code-review"]}`,
	}); err != nil {
		t.Fatalf("PutExtensionDefault replace: %v", err)
	}

	rows, err = s.ListExtensionDefaults(ctx)
	if err != nil {
		t.Fatalf("ListExtensionDefaults: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if rows[0].Key != "block:review" || rows[1].Key != "skill:code-review" {
		t.Fatalf("rows are not sorted by key: %+v", rows)
	}
	if rows[0].Detail != `{"mcp_tools":["mcp__a__b"],"skills":["code-review"]}` {
		t.Fatalf("detail = %q, want replaced value", rows[0].Detail)
	}
	if rows[1].Outcome != store.ExtensionDefaultApplied || rows[1].AppliedAt.IsZero() {
		t.Fatalf("skill row = %+v", rows[1])
	}
}

func TestExtensionDefaultLedgerRejectsInvalidRows(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	for _, row := range []store.ExtensionDefault{
		{Key: " ", Outcome: store.ExtensionDefaultApplied},
		{Key: "skill:x", Outcome: "maybe"},
	} {
		if err := s.PutExtensionDefault(ctx, row); err == nil {
			t.Fatalf("PutExtensionDefault(%+v) succeeded", row)
		}
	}
}

func TestSetDefaultBlockBindingIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	binding := store.BlockBinding{Scope: store.ScopePlatform, Block: "review", Skills: []string{"code-review"}}
	snapshot := store.ExtensionDefault{Key: "block:review", Outcome: store.ExtensionDefaultApplied, Detail: `{"skills":["code-review"]}`}

	// An invalid ledger row rolls the binding back.
	if err := s.SetDefaultBlockBinding(ctx, "review", &binding, snapshot,
		store.ExtensionDefault{Key: "skill:x", Outcome: "maybe"}); err == nil {
		t.Fatal("invalid ledger row accepted")
	}
	if rows, _ := s.ListBlockBindings(ctx, store.ScopePlatform, nil); len(rows) != 0 {
		t.Fatalf("binding kept after rollback: %+v", rows)
	}
	if rows, _ := s.ListExtensionDefaults(ctx); len(rows) != 0 {
		t.Fatalf("ledger kept after rollback: %+v", rows)
	}

	if err := s.SetDefaultBlockBinding(ctx, "review", &binding, snapshot); err != nil {
		t.Fatalf("SetDefaultBlockBinding: %v", err)
	}
	rows, err := s.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil || len(rows) != 1 || rows[0].Skills[0] != "code-review" {
		t.Fatalf("bindings = %+v, %v", rows, err)
	}

	released := store.ExtensionDefault{Key: "block:review", Outcome: store.ExtensionDefaultSkipped, Detail: "file"}
	if err := s.SetDefaultBlockBinding(ctx, "review", nil, released); err != nil {
		t.Fatalf("release: %v", err)
	}
	if rows, _ := s.ListBlockBindings(ctx, store.ScopePlatform, nil); len(rows) != 0 {
		t.Fatalf("binding kept after release: %+v", rows)
	}
	ledger, err := s.ListExtensionDefaults(ctx)
	if err != nil || len(ledger) != 1 || ledger[0].Outcome != store.ExtensionDefaultSkipped {
		t.Fatalf("ledger after release = %+v, %v", ledger, err)
	}
}

func TestSetDefaultBlockBindingRejectsForeignRows(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	project := int64(7)
	for _, tc := range []struct {
		block   string
		binding *store.BlockBinding
	}{
		{block: " "},
		{block: "review", binding: &store.BlockBinding{Scope: store.ScopeProject, ProjectID: &project, Block: "review"}},
		{block: "review", binding: &store.BlockBinding{Scope: store.ScopePlatform, Block: "verify"}},
	} {
		if err := s.SetDefaultBlockBinding(ctx, tc.block, tc.binding); err == nil {
			t.Fatalf("SetDefaultBlockBinding(%q, %+v) succeeded", tc.block, tc.binding)
		}
	}
}

func TestBuiltinMarkerAndSkillUpdates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	builtin, err := s.CreateSkill(ctx, store.Skill{
		Scope: "platform", Name: "code-review", Description: "old", Dir: "/x/skill", Enabled: true, Builtin: "code-review",
	})
	if err != nil {
		t.Fatalf("CreateSkill builtin: %v", err)
	}
	custom, err := s.CreateSkill(ctx, store.Skill{Scope: "platform", Name: "team", Description: "team", Enabled: true})
	if err != nil {
		t.Fatalf("CreateSkill custom: %v", err)
	}

	if err := s.SetSkillEnabled(ctx, builtin.ID, false); err != nil {
		t.Fatalf("SetSkillEnabled: %v", err)
	}
	if err := s.SetSkillDescription(ctx, builtin.ID, "new"); err != nil {
		t.Fatalf("SetSkillDescription: %v", err)
	}
	got, err := s.GetSkill(ctx, builtin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Builtin != "code-review" || got.Enabled || got.Description != "new" {
		t.Fatalf("builtin skill = %+v", got)
	}
	rows, err := s.ListSkills(ctx, "platform", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == custom.ID && row.Builtin != "" {
			t.Fatalf("custom skill marked builtin: %+v", row)
		}
	}
	for _, err := range []error{
		s.SetSkillEnabled(ctx, "missing", true),
		s.SetSkillDescription(ctx, "missing", "x"),
		s.SetSkillDescription(ctx, builtin.ID, " "),
	} {
		if err == nil {
			t.Fatal("invalid skill update succeeded")
		}
	}

	server, err := s.CreateMCPServer(ctx, store.MCPServer{
		Scope: "platform", Name: "codebase-memory", Config: []byte(`{"command":"x"}`), Enabled: true, Builtin: "codebase-memory",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer builtin: %v", err)
	}
	read, err := s.GetMCPServer(ctx, server.ID)
	if err != nil || read.Builtin != "codebase-memory" {
		t.Fatalf("GetMCPServer = %+v, %v", read, err)
	}
}
