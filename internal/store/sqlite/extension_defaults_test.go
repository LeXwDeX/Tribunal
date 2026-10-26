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
