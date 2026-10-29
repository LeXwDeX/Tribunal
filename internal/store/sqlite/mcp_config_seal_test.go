package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/secret"
)

func sealTestCipher(t *testing.T, b byte) *secret.Cipher {
	t.Helper()
	c, err := secret.New(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sealTestOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sealTestExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func sealTestProject(t *testing.T, s *Store, id int64, token []byte) {
	t.Helper()
	sealTestExec(t, s, `INSERT INTO projects (id,path_with_namespace,web_url,default_branch,token_enc,webhook_secret_enc,created_at,updated_at) VALUES (?, 'p', 'url', 'main', ?, X'', 'now', 'now')`, id, token)
}

func sealTestServer(t *testing.T, s *Store, id, scope string, project any, config string, enabled int, builtin string) {
	t.Helper()
	sealTestExec(t, s, `INSERT INTO mcp_servers (id,scope,project_id,name,config,enabled,builtin,created_at,updated_at) VALUES (?,?,?,?,?,?,?,'now','now')`, id, scope, project, id, config, enabled, builtin)
}

func TestMCPConfigSealEmptyAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	c := sealTestCipher(t, 1)
	s := sealTestOpen(t, path)
	if _, err := s.ConfigureMCPSecrets(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = sealTestOpen(t, path)
	if _, err := s.ConfigureMCPSecrets(ctx, c); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := s.db.QueryRow(`SELECT verifier FROM mcp_config_seal`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 2)); err == nil {
		t.Fatal("wrong key accepted")
	}
	var after []byte
	if err := s.db.QueryRow(`SELECT verifier FROM mcp_config_seal`).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("verifier changed after wrong key", err)
	}
}

func TestMCPConfigSealMigration(t *testing.T) {
	ctx := context.Background()
	s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
	c := sealTestCipher(t, 1)
	token, err := c.Seal([]byte("synthetic-token"), secret.TokenAAD(10))
	if err != nil {
		t.Fatal(err)
	}
	sealTestProject(t, s, 10, token)
	token2, err := c.Seal([]byte("synthetic-token-2"), secret.TokenAAD(11))
	if err != nil {
		t.Fatal(err)
	}
	sealTestProject(t, s, 11, token2)
	sealTestServer(t, s, "platform/a", "platform", nil, `{"token":"canary-one"}`, 0, "builtin")
	sealTestServer(t, s, "project/a", "project", 10, `{"token":"canary-two"}`, 1, "")
	sealTestServer(t, s, "project/b", "project", 11, `{"token":"canary-three"}`, 0, "builtin")
	sealTestExec(t, s, `INSERT INTO block_bindings (scope,project_id,block,mcp_tools) VALUES ('platform',0,'review','["platform/a"]')`)
	if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 2)); err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("wrong key error: %v", err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT config FROM mcp_servers WHERE id = 'platform/a'`).Scan(&raw); err != nil || raw != `{"token":"canary-one"}` {
		t.Fatalf("original changed: %q %v", raw, err)
	}
	if _, err := s.ConfigureMCPSecrets(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, scope, plain, builtin string
		project                   *int64
		enabled                   int
	}{{"platform/a", "platform", `{"token":"canary-one"}`, "builtin", nil, 0}, {"project/a", "project", `{"token":"canary-two"}`, "", func() *int64 { n := int64(10); return &n }(), 1}, {"project/b", "project", `{"token":"canary-three"}`, "builtin", func() *int64 { n := int64(11); return &n }(), 0}} {
		var config, scope, builtin string
		var project sql.NullInt64
		var enabled int
		var encrypted []byte
		if err := s.db.QueryRow(`SELECT config,config_enc,scope,project_id,enabled,builtin FROM mcp_servers WHERE id=?`, tc.id).Scan(&config, &encrypted, &scope, &project, &enabled, &builtin); err != nil {
			t.Fatal(err)
		}
		aad, err := secret.MCPConfigAAD(tc.scope, tc.project, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := c.Open(encrypted, aad)
		if err != nil || string(plain) != tc.plain || config != "{}" || scope != tc.scope || enabled != tc.enabled || builtin != tc.builtin || project.Valid != (tc.project != nil) || (tc.project != nil && project.Int64 != *tc.project) {
			t.Fatalf("bad migrated row: %v", err)
		}
		other := int64(11)
		for _, alt := range []struct {
			s  string
			p  *int64
			id string
		}{{tc.scope, tc.project, tc.id + "x"}, {"project", &other, tc.id}, {"platform", nil, tc.id}} {
			bad, err := secret.MCPConfigAAD(alt.s, alt.p, alt.id)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(aad, bad) {
				if _, err := c.Open(encrypted, bad); err == nil {
					t.Fatal("identity substitution accepted")
				}
			}
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM block_bindings WHERE mcp_tools='["platform/a"]'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("binding changed", err)
	}
	if _, err := s.ConfigureMCPSecrets(ctx, c); err != nil {
		t.Fatal(err)
	}
	sealTestServer(t, s, "late", "platform", nil, `{"token":"canary-late"}`, 1, "")
	if _, err := s.ConfigureMCPSecrets(ctx, c); err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("late plaintext accepted: %v", err)
	}
}

func TestMCPConfigSealRollback(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"write", "invalid", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			s := sealTestOpen(t, path)
			c := sealTestCipher(t, 1)
			sealTestServer(t, s, "a", "platform", nil, `{"secret":"canary-a"}`, 1, "")
			sealTestServer(t, s, "b", "platform", nil, `{"secret":"canary-b"}`, 1, "")
			switch mode {
			case "write":
				sealTestExec(t, s, `CREATE TRIGGER reject_second BEFORE UPDATE OF config ON mcp_servers WHEN OLD.id='b' BEGIN SELECT RAISE(ABORT,'canary-trigger'); END`)
			case "invalid":
				sealTestExec(t, s, `UPDATE mcp_servers SET config='canary-invalid' WHERE id='b'`)
			case "corrupt":
				sealTestExec(t, s, `UPDATE mcp_servers SET config='{}', config_enc=X'001122' WHERE id='b'`)
			}
			_, err := s.ConfigureMCPSecrets(ctx, c)
			if err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatalf("failure not sanitized: %v", err)
			}
			var raw string
			var enc []byte
			if err := s.db.QueryRow(`SELECT config,config_enc FROM mcp_servers WHERE id='a'`).Scan(&raw, &enc); err != nil || raw != `{"secret":"canary-a"}` || enc != nil {
				t.Fatalf("rollback failed: %q %v", raw, err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM mcp_config_seal`).Scan(&count); err != nil || count != 0 {
				t.Fatal("marker persisted", err)
			}
			switch mode {
			case "write":
				sealTestExec(t, s, `DROP TRIGGER reject_second`)
			default:
				sealTestExec(t, s, `UPDATE mcp_servers SET config='{}', config_enc=NULL WHERE id='b'`)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = sealTestOpen(t, path)
			if _, err := s.ConfigureMCPSecrets(ctx, c); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestMCPConfigSealPre17UpgradeFailureAndRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	s := sealTestOpen(t, path)
	sealTestExec(t, s, `DROP TABLE mcp_config_seal`)
	sealTestExec(t, s, `ALTER TABLE mcp_servers DROP COLUMN config_enc`)
	sealTestExec(t, s, `DELETE FROM schema_migrations WHERE version=17`)
	sealTestServer(t, s, "old", "platform", nil, `{"secret":"canary-old"}`, 1, "")
	sealTestExec(t, s, `CREATE TRIGGER reject_old BEFORE UPDATE OF config ON mcp_servers BEGIN SELECT RAISE(ABORT,'canary-trigger'); END`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = sealTestOpen(t, path)
	c := sealTestCipher(t, 1)
	if _, err := s.ConfigureMCPSecrets(ctx, c); err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("upgrade failure: %v", err)
	}
	var raw string
	var enc []byte
	if err := s.db.QueryRow(`SELECT config, config_enc FROM mcp_servers WHERE id='old'`).Scan(&raw, &enc); err != nil || raw != `{"secret":"canary-old"}` || enc != nil {
		t.Fatalf("upgrade rollback: %v", err)
	}
	sealTestExec(t, s, `DROP TRIGGER reject_old`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = sealTestOpen(t, path)
	if _, err := s.ConfigureMCPSecrets(ctx, c); err != nil {
		t.Fatalf("upgrade retry: %v", err)
	}
	if err := s.db.QueryRow(`SELECT config, config_enc FROM mcp_servers WHERE id='old'`).Scan(&raw, &enc); err != nil || raw != "{}" || len(enc) == 0 || strings.Contains(string(enc), "canary") {
		t.Fatalf("upgrade result: %v", err)
	}
}

func TestMCPConfigSealProjectKeyEvidence(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"legacy", "completed-legacy", "corrupt", "webhook"} {
		t.Run(mode, func(t *testing.T) {
			s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
			c := sealTestCipher(t, 1)
			var token, hook []byte
			var err error
			if mode == "legacy" || mode == "completed-legacy" {
				token, err = c.Encrypt([]byte("synthetic-token"))
			} else {
				token, err = c.Seal([]byte("synthetic-token"), secret.TokenAAD(10))
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				token = []byte("corrupt-canary")
			}
			if mode == "webhook" {
				hook, err = c.Seal([]byte("synthetic-hook"), secret.WebhookSecretAAD(10))
				if err != nil {
					t.Fatal(err)
				}
			}
			sealTestProject(t, s, 10, token)
			if hook != nil {
				sealTestExec(t, s, `UPDATE projects SET webhook_secret_enc=? WHERE id=10`, hook)
			}
			if mode == "completed-legacy" {
				sealTestExec(t, s, `INSERT INTO secret_migration (id, completed_at) VALUES (1, 'now')`)
			}
			sealTestServer(t, s, "a", "platform", nil, `{"secret":"canary"}`, 1, "")
			if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 2)); err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatalf("wrong key accepted or leaked: %v", err)
			}
			var raw string
			if err := s.db.QueryRow(`SELECT config FROM mcp_servers WHERE id='a'`).Scan(&raw); err != nil || raw != `{"secret":"canary"}` {
				t.Fatalf("plaintext changed: %v", err)
			}
			_, err = s.ConfigureMCPSecrets(ctx, c)
			if mode == "legacy" || mode == "webhook" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid project secret accepted")
			}
		})
	}
}

func TestMCPConfigSealProjectKeyEvidenceWithDamagedFields(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                       string
		legacy, complete, separate bool
	}{
		{name: "legacy token and damaged webhook", legacy: true},
		{name: "sealed token and damaged webhook", complete: true},
		{name: "damaged project before valid project", complete: true, separate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
			c := sealTestCipher(t, 1)
			goodID := int64(10)
			if tc.separate {
				goodID = 11
			}
			var token []byte
			var err error
			if tc.legacy {
				token, err = c.Encrypt([]byte("synthetic-token"))
			} else {
				token, err = c.Seal([]byte("synthetic-token"), secret.TokenAAD(goodID))
			}
			if err != nil {
				t.Fatal(err)
			}
			damaged := []byte("corrupt-canary")
			if tc.separate {
				sealTestProject(t, s, 10, damaged)
				sealTestProject(t, s, goodID, token)
			} else {
				sealTestProject(t, s, goodID, token)
				sealTestExec(t, s, `UPDATE projects SET webhook_secret_enc=? WHERE id=?`, damaged, goodID)
			}
			if tc.complete {
				sealTestExec(t, s, `INSERT INTO secret_migration (id, completed_at) VALUES (1, 'now')`)
			}
			sealTestServer(t, s, "server", "platform", nil, `{"secret":"mcp-canary"}`, 1, "")
			if _, err := s.ConfigureMCPSecrets(ctx, sealTestCipher(t, 2)); err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatalf("wrong key: %v", err)
			}
			var raw string
			var encrypted []byte
			if err := s.db.QueryRow(`SELECT config,config_enc FROM mcp_servers WHERE id='server'`).Scan(&raw, &encrypted); err != nil || raw != `{"secret":"mcp-canary"}` || encrypted != nil {
				t.Fatalf("wrong key changed MCP configuration: %v", err)
			}
			if _, err := s.ConfigureMCPSecrets(ctx, c); err != nil {
				t.Fatalf("valid key with damaged field: %v", err)
			}
			if err := s.db.QueryRow(`SELECT config,config_enc FROM mcp_servers WHERE id='server'`).Scan(&raw, &encrypted); err != nil || raw != "{}" || len(encrypted) == 0 {
				t.Fatalf("MCP not sealed: %v", err)
			}
			var got []byte
			query := `SELECT webhook_secret_enc FROM projects WHERE id=?`
			damagedID := goodID
			if tc.separate {
				query = `SELECT token_enc FROM projects WHERE id=?`
				damagedID = 10
			}
			if err := s.db.QueryRow(query, damagedID).Scan(&got); err != nil || !bytes.Equal(got, damaged) {
				t.Fatalf("damaged field changed: %v", err)
			}
			if err := s.db.QueryRow(`SELECT token_enc FROM projects WHERE id=?`, goodID).Scan(&got); err != nil || !bytes.Equal(got, token) {
				t.Fatalf("valid field changed: %v", err)
			}
		})
	}
}

func TestMCPConfigSealProjectScanErrorAfterKeyEvidence(t *testing.T) {
	ctx := context.Background()
	s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
	c := sealTestCipher(t, 1)
	token, err := c.Seal([]byte("synthetic-token"), secret.TokenAAD(10))
	if err != nil {
		t.Fatal(err)
	}
	sealTestServer(t, s, "server", "platform", nil, `{"secret":"mcp-canary"}`, 1, "")
	sealTestExec(t, s, `CREATE TEMP TABLE projects (id, token_enc, webhook_secret_enc)`)
	sealTestExec(t, s, `INSERT INTO projects VALUES (10, ?, X''), ('invalid-id', X'', X'')`, token)
	if _, err := s.ConfigureMCPSecrets(ctx, c); err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("scan error ignored or leaked: %v", err)
	}
	var raw string
	var encrypted []byte
	if err := s.db.QueryRow(`SELECT config,config_enc FROM mcp_servers WHERE id='server'`).Scan(&raw, &encrypted); err != nil || raw != `{"secret":"mcp-canary"}` || encrypted != nil {
		t.Fatalf("scan failure changed MCP configuration: %v", err)
	}
}

func TestMCPConfigSealIdentityTampering(t *testing.T) {
	ctx := context.Background()
	for _, change := range []struct{ name, sql string }{
		{"id", `UPDATE mcp_servers SET id='other' WHERE id='server'`},
		{"project", `UPDATE mcp_servers SET project_id=11 WHERE id='server'`},
		{"scope", `UPDATE mcp_servers SET scope='platform', project_id=NULL WHERE id='server'`},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
			c := sealTestCipher(t, 1)
			for _, id := range []int64{10, 11} {
				token, err := c.Seal([]byte("synthetic-token"), secret.TokenAAD(id))
				if err != nil {
					t.Fatal(err)
				}
				sealTestProject(t, s, id, token)
			}
			sealTestServer(t, s, "server", "project", 10, `{"secret":"canary"}`, 1, "")
			if _, err := s.ConfigureMCPSecrets(ctx, c); err != nil {
				t.Fatal(err)
			}
			sealTestExec(t, s, change.sql)
			if _, err := s.ConfigureMCPSecrets(ctx, c); err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatalf("tampering accepted: %v", err)
			}
		})
	}
}

func TestMCPConfigSealReportsUnverifiedKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	s := sealTestOpen(t, path)
	c := sealTestCipher(t, 1)
	unverified, err := s.ConfigureMCPSecrets(ctx, c)
	if err != nil || !unverified {
		t.Fatalf("first seal = %v, %v; want an unverified key", unverified, err)
	}
	unverified, err = s.ConfigureMCPSecrets(ctx, c)
	if err != nil || unverified {
		t.Fatalf("restart = %v, %v; want a verified key", unverified, err)
	}
}

func TestMCPConfigSealReportsVerifiedProjectCiphertext(t *testing.T) {
	ctx := context.Background()
	s := sealTestOpen(t, filepath.Join(t.TempDir(), "db"))
	c := sealTestCipher(t, 1)
	token, err := c.Seal([]byte("synthetic-token"), secret.TokenAAD(10))
	if err != nil {
		t.Fatal(err)
	}
	sealTestProject(t, s, 10, token)
	unverified, err := s.ConfigureMCPSecrets(ctx, c)
	if err != nil || unverified {
		t.Fatalf("project ciphertext = %v, %v; want a verified key", unverified, err)
	}
}
