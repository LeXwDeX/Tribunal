package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

const oldMCPConfig = `{"command":"/bin/false"}`

// createPreflightLegacyDB follows the audit reproduction's pre-0016 fixture.
func createPreflightLegacyDB(t *testing.T, path, defaultConfig string, projectMigrationComplete bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, err := strconv.Atoi(name[:4])
		if err != nil {
			t.Fatal(err)
		}
		if version > 15 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations VALUES (?, ?, ?)`, version, name, "2026-01-01T00:00:00.000000000Z"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	cipher, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	var token []byte
	if projectMigrationComplete {
		token, err = cipher.Seal([]byte("synthetic-project-token"), secret.TokenAAD(42))
	} else {
		token, err = cipher.Encrypt([]byte("synthetic-project-token"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id,path_with_namespace,web_url,default_branch,token_enc,webhook_secret_enc,created_at,updated_at) VALUES (42,'synthetic/project','http://invalid.local/project','main',?,X'','2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z')`, token); err != nil {
		t.Fatal(err)
	}
	if projectMigrationComplete {
		if _, err := db.Exec(`INSERT INTO secret_migration (id,completed_at) VALUES (1,'2026-01-01T00:00:00.000000000Z')`); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ id, name, builtin, config string }{
		{"synthetic-old", "legacy-server", "", oldMCPConfig},
		{"synthetic-default", "codebase-memory", "codebase-memory", defaultConfig},
	} {
		if _, err := db.Exec(`INSERT INTO mcp_servers (id,scope,project_id,name,config,enabled,builtin,created_at,updated_at) VALUES (?,'platform',NULL,?,?,0,?,'2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z')`, row.id, row.name, row.config, row.builtin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO extension_defaults (key,outcome,detail,applied_at) VALUES ('mcp:codebase-memory','applied','','2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
}

type preflightDBState struct {
	versions                    string
	configColumn, projectMark   int
	defaultSkill, ledger        int
	legacyConfig, defaultConfig string
	projectToken                []byte
}

func readPreflightDBState(t *testing.T, path string) preflightDBState {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state preflightDBState
	for _, query := range []struct {
		sql  string
		dest any
	}{
		{`SELECT group_concat(version, ',') FROM (SELECT version FROM schema_migrations ORDER BY version)`, &state.versions},
		{`SELECT COUNT(*) FROM pragma_table_info('mcp_servers') WHERE name='config_enc'`, &state.configColumn},
		{`SELECT COUNT(*) FROM secret_migration`, &state.projectMark},
		{`SELECT COUNT(*) FROM skills WHERE name='code-review' AND builtin='code-review'`, &state.defaultSkill},
		{`SELECT COUNT(*) FROM extension_defaults`, &state.ledger},
		{`SELECT config FROM mcp_servers WHERE id='synthetic-old'`, &state.legacyConfig},
		{`SELECT config FROM mcp_servers WHERE id='synthetic-default'`, &state.defaultConfig},
		{`SELECT token_enc FROM projects WHERE id=42`, &state.projectToken},
	} {
		if err := db.QueryRow(query.sql).Scan(query.dest); err != nil {
			t.Fatalf("read legacy fixture: %v", err)
		}
	}
	return state
}

func TestInvalidSessionKeyDoesNotChangeExistingDatabase(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	tests := []struct {
		name, current, legacy, errorText       string
		projectMigrationComplete, staleDefault bool
	}{
		{"invalid current", "not-base64!!!", "", "auth: TRIBUNAL_SESSION_KEY must be base64 of at least 32 bytes", true, true},
		{"invalid current does not fall back", "not-base64!!!", valid, "auth: TRIBUNAL_SESSION_KEY must be base64 of at least 32 bytes", true, true},
		{"short current", base64.StdEncoding.EncodeToString([]byte("short")), "", "auth: TRIBUNAL_SESSION_KEY must be base64 of at least 32 bytes", true, true},
		{"whitespace current", "  ", valid, "auth: TRIBUNAL_SESSION_KEY must not be whitespace only", true, true},
		{"whitespace current and empty legacy", "  ", "", "auth: TRIBUNAL_SESSION_KEY must not be whitespace only", true, true},
		{"invalid legacy", "", "not-base64!!!", "auth: CRC_SESSION_KEY must be base64 of at least 32 bytes", true, true},
		{"short legacy", "", base64.StdEncoding.EncodeToString([]byte("short")), "auth: CRC_SESSION_KEY must be base64 of at least 32 bytes", true, true},
		{"pending project migration", "not-base64!!!", "", "auth: TRIBUNAL_SESSION_KEY must be base64 of at least 32 bytes", false, true},
		{"matching default", "not-base64!!!", "", "auth: TRIBUNAL_SESSION_KEY must be base64 of at least 32 bytes", true, false},
	}
	for _, tc := range tests {
		for _, check := range []bool{true, false} {
			mode := "startup"
			if check {
				mode = "check-config"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				configPath := writeRunConfig(t, dir, 1)
				t.Setenv("TRIBUNAL_SESSION_KEY", tc.current)
				t.Setenv("CRC_SESSION_KEY", tc.legacy)
				defaultConfig := oldMCPConfig
				if !tc.staleDefault {
					cfg, err := config.Load(configPath)
					if err != nil || len(cfg.DefaultExtensions.MCPServers) != 1 {
						t.Fatal("could not load synthetic default config")
					}
					raw, err := cfg.DefaultExtensions.MCPServers[0].ConfigJSON()
					if err != nil {
						t.Fatal(err)
					}
					defaultConfig = string(raw)
				}
				dbPath := filepath.Join(dir, "review.db")
				createPreflightLegacyDB(t, dbPath, defaultConfig, tc.projectMigrationComplete)
				before := readPreflightDBState(t, dbPath)
				if before.versions != "1,3,4,5,6,7,8,9,10,11,12,13,14,15" || before.configColumn != 0 || before.legacyConfig != oldMCPConfig || before.defaultConfig != defaultConfig || secret.IsSealed(before.projectToken) != tc.projectMigrationComplete || (before.projectMark == 1) != tc.projectMigrationComplete {
					t.Fatal("invalid legacy fixture")
				}
				fileBefore, err := os.ReadFile(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"--config", configPath}
				if check {
					args = append(args, "--check-config")
				}
				var stdout, stderr bytes.Buffer
				err = run(context.Background(), args, &stdout, &stderr)
				if err == nil || err.Error() != tc.errorText || stdout.Len() != 0 {
					t.Errorf("%s: stdout=%q error=%v, want no stdout and %q", mode, stdout.String(), err, tc.errorText)
				}
				for _, value := range []string{tc.current, tc.legacy} {
					if strings.TrimSpace(value) != "" && (strings.Contains(stderr.String(), value) || err != nil && strings.Contains(err.Error(), value)) {
						t.Error("session key leaked in startup output")
					}
				}
				if strings.Contains(stderr.String(), "generated an ephemeral session key") {
					t.Error("invalid session key was reported as ephemeral")
				}
				fileAfter, err := os.ReadFile(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(fileBefore, fileAfter) {
					t.Error("database file changed before session key was rejected")
				}
				for _, artifact := range []string{sqlite.LockFileName, "review.db-wal", "review.db-shm", "workspaces"} {
					if _, err := os.Stat(filepath.Join(dir, artifact)); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("unexpected storage artifact %s: %v", artifact, err)
					}
				}
				after := readPreflightDBState(t, dbPath)
				if !reflect.DeepEqual(before, after) {
					t.Errorf("database state changed: migrations %s -> %s, MCP config column %d -> %d, project sealed %v -> %v, default skills %d -> %d, ledger %d -> %d", before.versions, after.versions, before.configColumn, after.configColumn, secret.IsSealed(before.projectToken), secret.IsSealed(after.projectToken), before.defaultSkill, after.defaultSkill, before.ledger, after.ledger)
				}
			})
		}
	}
}

func TestInvalidSessionKeyDoesNotCreateStorageDirectory(t *testing.T) {
	dir := t.TempDir()
	path := writeRunConfig(t, dir, 1)
	configBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	storageDir := filepath.Join(dir, "new-storage")
	originalPath := []byte(filepath.Join(dir, "review.db"))
	if !bytes.Contains(configBytes, originalPath) {
		t.Fatal("config does not contain the expected database path")
	}
	configBytes = bytes.Replace(configBytes, originalPath, []byte(filepath.Join(storageDir, "review.db")), 1)
	if err := os.WriteFile(path, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRIBUNAL_SESSION_KEY", "")
	t.Setenv("CRC_SESSION_KEY", "invalid!!!")
	err = run(context.Background(), []string{"--config", path}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "auth: CRC_SESSION_KEY must be base64 of at least 32 bytes" {
		t.Errorf("startup error = %v", err)
	}
	if _, err := os.Stat(storageDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("storage directory created before session key rejection: %v", err)
	}
}

func TestCheckConfigSessionKeyCompatibility(t *testing.T) {
	dir := t.TempDir()
	path := writeRunConfig(t, dir, 1)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	tests := []struct {
		name, current, legacy string
		unsetCurrent          bool
	}{
		{"empty", "", "", false},
		{"unset current and empty legacy", "", "", true},
		{"legacy whitespace", "", "\t", false},
		{"current", key, "", false},
		{"trimmed current", "  " + key + "  ", "", false},
		{"raw current", strings.TrimRight(key, "="), "", false},
		{"legacy fallback", "", "  " + key + "  ", false},
		{"unset current legacy fallback", "", key, true},
		{"current overrides invalid legacy", key, "invalid!!!", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRIBUNAL_SESSION_KEY", tc.current)
			t.Setenv("CRC_SESSION_KEY", tc.legacy)
			if tc.unsetCurrent {
				if err := os.Unsetenv("TRIBUNAL_SESSION_KEY"); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			if err := run(context.Background(), []string{"--config", path, "--check-config"}, &stdout, &stderr); err != nil || stdout.String() != "configuration OK\n" {
				t.Fatalf("check-config: stdout=%q error=%v", stdout.String(), err)
			}
			if strings.Contains(stderr.String(), "generated an ephemeral session key") {
				t.Errorf("check-config warned about ephemeral key: %s", stderr.String())
			}
			for _, artifact := range []string{"review.db", sqlite.LockFileName, "workspaces"} {
				if _, err := os.Stat(filepath.Join(dir, artifact)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("check-config created %s: %v", artifact, err)
				}
			}
		})
	}
}

func TestStartupEphemeralSessionKeyWarning(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	for _, tc := range []struct {
		name, current, legacy string
		warn                  bool
	}{
		{"empty", "", "", true},
		{"legacy whitespace", "", "\t", true},
		{"current", key, "", false},
		{"legacy", "", key, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRIBUNAL_SESSION_KEY", tc.current)
			t.Setenv("CRC_SESSION_KEY", tc.legacy)
			logs := runUntilReady(t, t.TempDir(), nil)
			count := strings.Count(logs, "generated an ephemeral session key")
			if (count == 1) != tc.warn || count > 1 {
				t.Errorf("ephemeral key warning count = %d, want warning=%v", count, tc.warn)
			}
		})
	}
}
