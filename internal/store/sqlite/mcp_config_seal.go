package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/LeXwDeX/tribunal/internal/secret"
)

var mcpVerifierAAD = []byte("tribunal/mcp-config/verifier/v1")
var mcpVerifierValue = []byte("tribunal/mcp-config/key-verifier/v1")

// ConfigureMCPSecrets authenticates the key and atomically seals legacy MCP
// configurations. Call it during startup before using MCP store operations.
// The supplied key is trusted only when no project or MCP ciphertext exists;
// unverified reports that case so the caller can warn the operator.
func (s *Store) ConfigureMCPSecrets(ctx context.Context, cipher *secret.Cipher) (unverified bool, err error) {
	s.mcpCipher.Store(nil)
	if cipher == nil {
		return false, errors.New("sqlite: MCP cipher is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, errors.New("sqlite: begin MCP secret configuration failed")
	}
	defer tx.Rollback()

	var verifier []byte
	err = tx.QueryRowContext(ctx, `SELECT verifier FROM mcp_config_seal WHERE id = 1`).Scan(&verifier)
	complete := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, errors.New("sqlite: read MCP secret marker failed")
	}
	verified := false
	if complete {
		plain, err := cipher.Open(verifier, mcpVerifierAAD)
		if err != nil || !bytes.Equal(plain, mcpVerifierValue) {
			return false, errors.New("sqlite: MCP key verification failed")
		}
		verified = true
	} else {
		verified, err = verifyProjectSecrets(ctx, tx, cipher)
		if err != nil {
			return false, err
		}
	}

	type record struct {
		id, scope, config string
		project           sql.NullInt64
		enc               []byte
		null              bool
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, scope, project_id, config, config_enc, config_enc IS NULL FROM mcp_servers ORDER BY id`)
	if err != nil {
		return false, errors.New("sqlite: read MCP configuration failed")
	}
	var records []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.id, &r.scope, &r.project, &r.config, &r.enc, &r.null); err != nil {
			rows.Close()
			return false, errors.New("sqlite: scan MCP configuration failed")
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, errors.New("sqlite: iterate MCP configuration failed")
	}
	if err := rows.Close(); err != nil {
		return false, errors.New("sqlite: close MCP configuration failed")
	}

	for _, r := range records {
		var project *int64
		if r.project.Valid {
			project = &r.project.Int64
		}
		aad, err := secret.MCPConfigAAD(r.scope, project, r.id)
		if err != nil {
			return false, errors.New("sqlite: invalid MCP configuration identity")
		}
		if !r.null {
			if r.config != "{}" {
				return false, errors.New("sqlite: unexpected MCP plaintext configuration")
			}
			plain, err := cipher.Open(r.enc, aad)
			if err != nil || !validJSONObject(plain) {
				return false, errors.New("sqlite: invalid MCP sealed configuration")
			}
			verified = true
			continue
		}
		if complete {
			return false, errors.New("sqlite: unsealed MCP configuration after migration")
		}
		if !validJSONObject([]byte(r.config)) {
			return false, errors.New("sqlite: invalid MCP plaintext configuration")
		}
		enc, err := cipher.Seal([]byte(r.config), aad)
		if err != nil {
			return false, errors.New("sqlite: seal MCP configuration failed")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_servers SET config = '{}', config_enc = ? WHERE id = ?`, enc, r.id); err != nil {
			return false, errors.New("sqlite: update MCP configuration failed")
		}
	}
	if !complete {
		verifier, err := cipher.Seal(mcpVerifierValue, mcpVerifierAAD)
		if err != nil {
			return false, errors.New("sqlite: seal MCP key verifier failed")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_config_seal (id, verifier) VALUES (1, ?)`, verifier); err != nil {
			return false, errors.New("sqlite: write MCP secret marker failed")
		}
	}
	if err := tx.Commit(); err != nil {
		return false, errors.New("sqlite: commit MCP secret configuration failed")
	}
	s.mcpCipher.Store(cipher)
	return !complete && !verified, nil
}

// Legacy project ciphertext is valid evidence only before its own migration completes.
func verifyProjectSecrets(ctx context.Context, tx *sql.Tx, cipher *secret.Cipher) (bool, error) {
	var complete bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM secret_migration WHERE id = 1)`).Scan(&complete); err != nil {
		return false, errors.New("sqlite: read project secret marker failed")
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, token_enc, webhook_secret_enc FROM projects`)
	if err != nil {
		return false, errors.New("sqlite: read project secrets failed")
	}
	defer rows.Close()
	var present, authenticated bool
	for rows.Next() {
		var id int64
		var token, hook []byte
		if err := rows.Scan(&id, &token, &hook); err != nil {
			return false, errors.New("sqlite: scan project secrets failed")
		}
		for _, field := range []struct{ value, aad []byte }{{token, secret.TokenAAD(id)}, {hook, secret.WebhookSecretAAD(id)}} {
			if len(field.value) == 0 {
				continue
			}
			present = true
			var err error
			if complete {
				_, err = cipher.Open(field.value, field.aad)
			} else {
				_, _, err = cipher.Rewrap(field.value, field.aad)
			}
			if err == nil {
				authenticated = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, errors.New("sqlite: iterate project secrets failed")
	}
	if err := rows.Close(); err != nil {
		return false, errors.New("sqlite: close project secrets failed")
	}
	// Project migration isolates damaged fields, so one authenticated field is sufficient key evidence.
	if present && !authenticated {
		return false, errors.New("sqlite: project secret key verification failed")
	}
	return authenticated, nil
}
