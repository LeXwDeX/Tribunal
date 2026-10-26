package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestImportMCPServersIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	existing, err := s.CreateMCPServer(ctx, store.MCPServer{
		Scope: store.ScopePlatform, Name: "aa-existing", Config: json.RawMessage(`{"command":"old"}`), Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A database-level failure on the last row must roll back every row.
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER boom BEFORE INSERT ON mcp_servers
		WHEN NEW.name = 'zz-boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = s.ImportMCPServers(ctx, store.ScopePlatform, nil, []store.MCPServerImport{
		{Name: "aa-existing", Config: json.RawMessage(`{"command":"new"}`)},
		{Name: "bb-new", Config: json.RawMessage(`{"command":"x"}`)},
		{Name: "zz-boom", Config: json.RawMessage(`{"command":"y"}`)},
	})
	if err == nil {
		t.Fatal("import with a failing row succeeded")
	}
	rows, err := s.ListMCPServers(ctx, store.ScopePlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || string(rows[0].Config) != `{"command":"old"}` {
		t.Fatalf("rows after failed import = %+v", rows)
	}

	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER boom`); err != nil {
		t.Fatal(err)
	}
	out, err := s.ImportMCPServers(ctx, store.ScopePlatform, nil, []store.MCPServerImport{
		{Name: "aa-existing", Config: json.RawMessage(`{"command":"new"}`)},
		{Name: "bb-new", Config: json.RawMessage(`{"command":"x"}`)},
	})
	if err != nil || len(out) != 2 {
		t.Fatalf("import = %+v, %v", out, err)
	}
	if out[0].ID != existing.ID || string(out[0].Config) != `{"command":"new"}` || out[0].Enabled {
		t.Fatalf("updated row = %+v; want same id, new config, enabled kept false", out[0])
	}
	if out[1].Name != "bb-new" || !out[1].Enabled {
		t.Fatalf("created row = %+v", out[1])
	}
}

func TestImportMCPServersRejectsBuiltinInsideTransaction(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if _, err := s.CreateMCPServer(ctx, store.MCPServer{
		Scope: store.ScopePlatform, Name: "builtin", Config: json.RawMessage(`{}`), Enabled: true, Builtin: "codebase-memory",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ImportMCPServers(ctx, store.ScopePlatform, nil, []store.MCPServerImport{
		{Name: "aa-new", Config: json.RawMessage(`{}`)},
		{Name: "builtin", Config: json.RawMessage(`{"command":"evil"}`)},
	})
	if !errors.Is(err, store.ErrBuiltinManaged) {
		t.Fatalf("err = %v, want ErrBuiltinManaged", err)
	}
	rows, err := s.ListMCPServers(ctx, store.ScopePlatform, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v; want only the built-in", rows, err)
	}
	if _, err := s.ImportMCPServers(ctx, store.ScopePlatform, nil, nil); err == nil {
		t.Fatal("empty import succeeded")
	}
	if _, err := s.ImportMCPServers(ctx, store.ScopePlatform, nil, []store.MCPServerImport{{Name: "x", Config: json.RawMessage(`[]`)}}); err == nil {
		t.Fatal("non-object config succeeded")
	}
}
