package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestMCPConfigRequiresEncryptionKey(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.CreateMCPServer(ctx, store.MCPServer{
		Scope: "platform", Name: "synthetic", Config: json.RawMessage(`{"headers":{"X-Auth":"synthetic-canary"}}`),
	})
	if err == nil {
		t.Fatal("create accepted a config without an encryption key")
	}
}

func TestMCPImportRequiresEncryptionKey(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "mcp-import.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.ImportMCPServers(ctx, store.ScopePlatform, nil, []store.MCPServerImport{
		{Name: "synthetic", Config: json.RawMessage(`{"headers":{"X-Auth":"import-canary"}}`)},
	})
	if err == nil {
		t.Error("import accepted a config without an encryption key")
	} else if strings.Contains(err.Error(), "import-canary") {
		t.Error("import error exposed config")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_servers`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Errorf("failed import persisted %d rows", count)
	}
}

func TestMCPConfigEncryptedCRUDAndTampering(t *testing.T) {
	ctx := context.Background()
	s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
	if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateMCPServer(ctx, store.MCPServer{Scope: store.ScopePlatform, Name: "first", Config: json.RawMessage(`{"token":"canary-first"}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateMCPServer(ctx, store.MCPServer{Scope: store.ScopePlatform, Name: "second", Config: json.RawMessage(`{"token":"canary-second"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	var enc []byte
	if err := s.db.QueryRow(`SELECT config,config_enc FROM mcp_servers WHERE id=?`, first.ID).Scan(&raw, &enc); err != nil || raw != "{}" || len(enc) == 0 || strings.Contains(string(enc), "canary") {
		t.Fatalf("config not encrypted: %q %v", raw, err)
	}
	if _, err := s.UpdateMCPServer(ctx, first.ID, store.MCPServerUpdate{Config: json.RawMessage(`{"token":"canary-updated"}`)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMCPServer(ctx, first.ID)
	if err != nil || string(got.Config) != `{"token":"canary-updated"}` {
		t.Fatalf("read: %v", err)
	}
	sealTestExec(t, s, `UPDATE mcp_servers SET config_enc=(SELECT config_enc FROM mcp_servers WHERE id=?) WHERE id=?`, second.ID, first.ID)
	if _, err := s.GetMCPServer(ctx, first.ID); err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("swapped ciphertext accepted: %v", err)
	}
	if _, err := s.ListMCPServers(ctx, "", nil); err == nil {
		t.Fatal("list accepted swapped ciphertext")
	}
	if _, _, err := s.ListMCPServersPage(ctx, "", nil, "", 10); err == nil {
		t.Fatal("page accepted swapped ciphertext")
	}
	sealTestExec(t, s, `UPDATE mcp_servers SET config_enc=X'001122' WHERE id=?`, second.ID)
	if _, err := s.GetMCPServer(ctx, second.ID); err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("corrupt ciphertext accepted: %v", err)
	}
}

func TestMCPConfigRejectsUnsealedAndUpdateWithoutKey(t *testing.T) {
	ctx := context.Background()
	for _, mutation := range []string{"config='{}', config_enc=NULL", "config='{}', config_enc=X''", `config='{"token":"canary"}'`} {
		t.Run(mutation, func(t *testing.T) {
			s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
			cipher := sealTestCipher(t, 1)
			if _, err := s.ConfigureMCPSecrets(ctx, cipher); err != nil {
				t.Fatal(err)
			}
			row, err := s.CreateMCPServer(ctx, store.MCPServer{Scope: store.ScopePlatform, Name: "test", Config: json.RawMessage(`{"secret":"canary"}`)})
			if err != nil {
				t.Fatal(err)
			}
			sealTestExec(t, s, `UPDATE mcp_servers SET `+mutation+` WHERE id=?`, row.ID)
			if _, err := s.GetMCPServer(ctx, row.ID); err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatalf("invalid data accepted: %v", err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "db")
	s := sealTestOpen(t, path)
	if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	row, err := s.CreateMCPServer(ctx, store.MCPServer{Scope: store.ScopePlatform, Name: "test", Config: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 2)); err == nil {
		t.Fatal("wrong key accepted")
	}
	enabled := true
	if _, err := s.UpdateMCPServer(ctx, row.ID, store.MCPServerUpdate{Enabled: &enabled}); err == nil {
		t.Fatal("update accepted without configured key")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = sealTestOpen(t, path)
	if _, err := s.UpdateMCPServer(ctx, row.ID, store.MCPServerUpdate{Enabled: &enabled}); err == nil {
		t.Fatal("update accepted after unconfigured reopen")
	}
}

func TestMCPImportSQLFailureRollsBackAndHidesError(t *testing.T) {
	ctx := context.Background()
	s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
	if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	sealTestExec(t, s, `CREATE TRIGGER reject_mcp BEFORE INSERT ON mcp_servers WHEN NEW.name='second' BEGIN SELECT RAISE(ABORT,'canary-trigger'); END`)
	_, err := s.ImportMCPServers(ctx, store.ScopePlatform, nil, []store.MCPServerImport{{Name: "first", Config: json.RawMessage(`{"secret":"canary"}`)}, {Name: "second", Config: json.RawMessage(`{}`)}})
	if err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("unexpected error: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mcp_servers`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("import not rolled back: %d %v", count, err)
	}
}

func TestMCPUpdateSQLFailureRollsBackAndHidesError(t *testing.T) {
	ctx := context.Background()
	s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
	if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	row, err := s.CreateMCPServer(ctx, store.MCPServer{Scope: store.ScopePlatform, Name: "one", Config: json.RawMessage(`{"secret":"original"}`)})
	if err != nil {
		t.Fatal(err)
	}
	sealTestExec(t, s, `CREATE TRIGGER reject_update BEFORE UPDATE ON mcp_servers BEGIN SELECT RAISE(ABORT,'canary-trigger'); END`)
	_, err = s.UpdateMCPServer(ctx, row.ID, store.MCPServerUpdate{Config: json.RawMessage(`{"secret":"canary-value"}`)})
	if err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := s.GetMCPServer(ctx, row.ID)
	if err != nil || string(got.Config) != `{"secret":"original"}` {
		t.Fatalf("failed update changed config: %v", err)
	}
}
