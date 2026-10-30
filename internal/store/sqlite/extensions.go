package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	listCursorProjects = "projects"
	listCursorTokens   = "tokens"
	listCursorMCP      = "mcp"
	listCursorSkills   = "skills"
	listCursorBindings = "bindings"
)

// listCursor is the shared opaque keyset cursor. Each list method fills only
// the fields it needs and rejects a cursor bound to another filter.
type listCursor struct {
	Kind         string `json:"kind"`
	Query        string `json:"query,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
	UserID       int64  `json:"user_id,omitempty"`
	Scope        string `json:"scope,omitempty"`
	ProjectID    *int64 `json:"project_id,omitempty"`
	At           string `json:"at,omitempty"`
	ID           string `json:"id,omitempty"`
	RowScope     string `json:"row_scope,omitempty"`
	RowProjectID int64  `json:"row_project_id,omitempty"`
	Block        string `json:"block,omitempty"`
}

func encodeListCursor(cursor listCursor) string {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeListCursor(raw, kind string) (listCursor, error) {
	var cursor listCursor
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) == 0 {
		return cursor, fmt.Errorf("%w: %s cursor", store.ErrInvalidCursor, kind)
	}
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Kind != kind {
		return cursor, fmt.Errorf("%w: %s cursor", store.ErrInvalidCursor, kind)
	}
	if cursor.At != "" {
		if _, err := parseTime(cursor.At); err != nil {
			return cursor, fmt.Errorf("%w: %s cursor", store.ErrInvalidCursor, kind)
		}
	}
	return cursor, nil
}

func sameProjectID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// extensionScopeClauses builds the shared scope/project selector. The
// platformProject predicate differs because block bindings store platform
// ownership as project_id 0 while MCP servers and skills use NULL.
func extensionScopeClauses(scope string, projectID *int64, platformProject string) ([]string, []any, error) {
	scope = strings.TrimSpace(scope)
	if err := store.ValidateExtensionScope(scope, projectID, false); err != nil {
		return nil, nil, err
	}
	where := []string{"1 = 1"}
	args := make([]any, 0, 2)
	switch scope {
	case store.ScopePlatform:
		where = append(where, "scope = ?", platformProject)
		args = append(args, store.ScopePlatform)
	case store.ScopeProject:
		where = append(where, "scope = ?", "project_id = ?")
		args = append(args, store.ScopeProject, *projectID)
	default:
		if projectID != nil {
			where = append(where, "project_id = ?")
			args = append(args, *projectID)
		}
	}
	return where, args, nil
}

// ---------------------------------------------------------------------------
// MCP servers

const mcpServerColumns = `id, scope, project_id, name, config, config_enc, enabled, builtin, created_at, updated_at`

func (s *Store) scanMCPServer(row rowScanner) (*store.MCPServer, error) {
	var (
		server    store.MCPServer
		projectID sql.NullInt64
		createdAt string
		updatedAt string
		plain     string
		enc       []byte
	)
	if err := row.Scan(&server.ID, &server.Scope, &projectID, &server.Name,
		&plain, &enc, &server.Enabled, &server.Builtin, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, errors.New("sqlite: scan MCP server failed")
	}
	if projectID.Valid {
		value := projectID.Int64
		server.ProjectID = &value
	}
	cipher := s.mcpCipher.Load()
	if cipher == nil || plain != "{}" || len(enc) == 0 {
		return nil, errors.New("sqlite: MCP configuration unavailable")
	}
	aad, err := secret.MCPConfigAAD(server.Scope, server.ProjectID, server.ID)
	if err != nil {
		return nil, errors.New("sqlite: invalid MCP configuration identity")
	}
	config, err := cipher.Open(enc, aad)
	if err != nil || !validJSONObject(config) {
		return nil, errors.New("sqlite: invalid MCP sealed configuration")
	}
	server.Config = config
	if server.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, errors.New("sqlite: invalid MCP creation time")
	}
	if server.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, errors.New("sqlite: invalid MCP update time")
	}
	return &server, nil
}

func (s *Store) sealMCPConfig(server *store.MCPServer) ([]byte, error) {
	cipher := s.mcpCipher.Load()
	if cipher == nil {
		return nil, errors.New("sqlite: MCP cipher is required")
	}
	aad, err := secret.MCPConfigAAD(server.Scope, server.ProjectID, server.ID)
	if err != nil {
		return nil, errors.New("sqlite: invalid MCP configuration identity")
	}
	sealed, err := cipher.Seal(server.Config, aad)
	if err != nil {
		return nil, errors.New("sqlite: seal MCP configuration failed")
	}
	return sealed, nil
}

// ListMCPServers returns servers matching the scope selector. An empty scope
// lists every scope; a nil project ID with project scope is rejected.
func (s *Store) ListMCPServers(ctx context.Context, scope string, projectID *int64) ([]store.MCPServer, error) {
	where, args, err := extensionScopeClauses(scope, projectID, "project_id IS NULL")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+mcpServerColumns+` FROM mcp_servers
		WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, errors.New("sqlite: list mcp servers failed")
	}
	defer rows.Close()
	servers := make([]store.MCPServer, 0)
	for rows.Next() {
		server, err := s.scanMCPServer(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan mcp server: %w", err)
		}
		servers = append(servers, *server)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("sqlite: iterate mcp servers failed")
	}
	return servers, nil
}

// ListMCPServersPage returns one bounded page of servers. The cursor binds to
// the scope selector so it cannot resume another scope or project.
func (s *Store) ListMCPServersPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]store.MCPServer, string, error) {
	scope = strings.TrimSpace(scope)
	limit = normalizeLimit(limit)
	where, args, err := extensionScopeClauses(scope, projectID, "project_id IS NULL")
	if err != nil {
		return nil, "", err
	}
	if cursor != "" {
		bound, err := decodeListCursor(cursor, listCursorMCP)
		if err != nil {
			return nil, "", err
		}
		if bound.Scope != scope || !sameProjectID(bound.ProjectID, projectID) || bound.ID == "" {
			return nil, "", fmt.Errorf("%w: mcp cursor scope", store.ErrInvalidCursor)
		}
		at, err := parseTime(bound.At)
		if err != nil {
			return nil, "", fmt.Errorf("%w: mcp cursor", store.ErrInvalidCursor)
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, formatTime(at), formatTime(at), bound.ID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+mcpServerColumns+` FROM mcp_servers
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", errors.New("sqlite: list mcp servers failed")
	}
	defer rows.Close()
	servers := make([]store.MCPServer, 0, limit)
	for rows.Next() {
		server, err := s.scanMCPServer(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan mcp server: %w", err)
		}
		servers = append(servers, *server)
	}
	if err := rows.Err(); err != nil {
		return nil, "", errors.New("sqlite: iterate mcp servers failed")
	}
	next := ""
	if len(servers) > limit {
		servers = servers[:limit]
		last := servers[len(servers)-1]
		next = encodeListCursor(listCursor{
			Kind: listCursorMCP, Scope: scope, ProjectID: projectID,
			At: formatTime(last.CreatedAt), ID: last.ID,
		})
	}
	return servers, next, nil
}

// GetMCPServer returns one server by ID.
func (s *Store) GetMCPServer(ctx context.Context, id string) (*store.MCPServer, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("sqlite: mcp server id is required")
	}
	server, err := s.scanMCPServer(s.db.QueryRowContext(ctx,
		`SELECT `+mcpServerColumns+` FROM mcp_servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: mcp server %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get mcp server: %w", err)
	}
	return server, nil
}

// CreateMCPServer stores one server. The unique name is enforced per
// (scope, project); platform rows have no project row so uniqueness is
// checked inside the transaction instead of relying on the SQL unique index.
func (s *Store) CreateMCPServer(ctx context.Context, server store.MCPServer) (*store.MCPServer, error) {
	server.Scope = strings.TrimSpace(server.Scope)
	server.Name = strings.TrimSpace(server.Name)
	if err := store.ValidateExtensionScope(server.Scope, server.ProjectID, true); err != nil {
		return nil, err
	}
	if server.Name == "" {
		return nil, errors.New("sqlite: mcp server name is required")
	}
	if !validJSONObject(server.Config) {
		return nil, errors.New("sqlite: mcp server config must be a JSON object")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	server.ID = id
	sealed, err := s.sealMCPConfig(&server)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	server.CreatedAt = now
	server.UpdatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("sqlite: begin create mcp server failed")
	}
	defer tx.Rollback()
	if err := requireMCPNameFree(ctx, tx, server.Scope, server.ProjectID, server.Name, ""); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_servers
		(id, scope, project_id, name, config, config_enc, enabled, builtin, created_at, updated_at)
		VALUES (?, ?, ?, ?, '{}', ?, ?, ?, ?, ?)`,
		server.ID, server.Scope, nullableProjectID(server.ProjectID), server.Name,
		sealed, server.Enabled, server.Builtin, formatTime(server.CreatedAt), formatTime(server.UpdatedAt)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: mcp server %s", store.ErrConflict, server.Name)
		}
		return nil, errors.New("sqlite: insert mcp server failed")
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("sqlite: commit mcp server failed")
	}
	return &server, nil
}

// UpdateMCPServer replaces only the provided fields and returns the stored
// row.
func (s *Store) UpdateMCPServer(ctx context.Context, id string, update store.MCPServerUpdate) (*store.MCPServer, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("sqlite: mcp server id is required")
	}
	if update.Config != nil && !validJSONObject(update.Config) {
		return nil, errors.New("sqlite: mcp server config must be a JSON object")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("sqlite: begin update mcp server failed")
	}
	defer tx.Rollback()
	current, err := s.scanMCPServer(tx.QueryRowContext(ctx,
		`SELECT `+mcpServerColumns+` FROM mcp_servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: mcp server %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read mcp server: %w", err)
	}
	if update.Name != nil {
		name := strings.TrimSpace(*update.Name)
		if name == "" {
			return nil, errors.New("sqlite: mcp server name is required")
		}
		if name != current.Name {
			if err := requireMCPNameFree(ctx, tx, current.Scope, current.ProjectID, name, id); err != nil {
				return nil, err
			}
		}
		current.Name = name
	}
	if update.Config != nil {
		next := update.Config
		if update.MergeConfig != nil {
			next = update.MergeConfig(update.Config, current.Config)
		}
		if !validJSONObject(next) {
			return nil, errors.New("sqlite: mcp server config must be a JSON object")
		}
		current.Config = append(json.RawMessage(nil), next...)
	}
	if update.Enabled != nil {
		current.Enabled = *update.Enabled
	}
	current.UpdatedAt = s.now().UTC()
	sealed, err := s.sealMCPConfig(current)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_servers
		SET name = ?, config = '{}', config_enc = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		current.Name, sealed, current.Enabled,
		formatTime(current.UpdatedAt), id); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: mcp server %s", store.ErrConflict, current.Name)
		}
		return nil, errors.New("sqlite: update mcp server failed")
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("sqlite: commit mcp server update failed")
	}
	return current, nil
}

// ImportMCPServers upserts servers by name in one transaction, so a failure
// on any entry leaves no partial import. An existing row keeps its ID and
// enabled flag; a new row is enabled. A built-in row is never overwritten: the
// check runs inside the transaction and returns store.ErrBuiltinManaged.
func (s *Store) ImportMCPServers(ctx context.Context, scope string, projectID *int64, servers []store.MCPServerImport) ([]store.MCPServer, error) {
	scope = strings.TrimSpace(scope)
	if err := store.ValidateExtensionScope(scope, projectID, true); err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, errors.New("sqlite: mcp import is empty")
	}
	for _, server := range servers {
		if strings.TrimSpace(server.Name) == "" {
			return nil, errors.New("sqlite: mcp server name is required")
		}
		if !validJSONObject(server.Config) {
			return nil, errors.New("sqlite: mcp server config must be a JSON object")
		}
	}
	if s.mcpCipher.Load() == nil {
		return nil, errors.New("sqlite: MCP cipher is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("sqlite: begin mcp import failed")
	}
	defer tx.Rollback()

	now := s.now().UTC()
	out := make([]store.MCPServer, 0, len(servers))
	for _, in := range servers {
		name := strings.TrimSpace(in.Name)
		current, err := s.scanMCPServer(tx.QueryRowContext(ctx, `SELECT `+mcpServerColumns+` FROM mcp_servers
			WHERE scope = ? AND project_id IS ? AND name = ?`, scope, nullableProjectID(projectID), name))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id, err := newID()
			if err != nil {
				return nil, err
			}
			row := store.MCPServer{
				ID: id, Scope: scope, ProjectID: projectID, Name: name,
				Config: append(json.RawMessage(nil), in.Config...), Enabled: true,
				CreatedAt: now, UpdatedAt: now,
			}
			sealed, err := s.sealMCPConfig(&row)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_servers
				(id, scope, project_id, name, config, config_enc, enabled, builtin, created_at, updated_at)
				VALUES (?, ?, ?, ?, '{}', ?, ?, '', ?, ?)`,
				row.ID, row.Scope, nullableProjectID(row.ProjectID), row.Name, sealed,
				row.Enabled, formatTime(now), formatTime(now)); err != nil {
				if isConstraintErr(err) {
					return nil, fmt.Errorf("%w: mcp server %s", store.ErrConflict, name)
				}
				return nil, errors.New("sqlite: insert mcp server failed")
			}
			out = append(out, row)
		case err != nil:
			return nil, fmt.Errorf("sqlite: read mcp server: %w", err)
		case current.Builtin != "":
			return nil, fmt.Errorf("%w: mcp server %s", store.ErrBuiltinManaged, name)
		default:
			current.Config = append(json.RawMessage(nil), in.Config...)
			current.UpdatedAt = now
			sealed, err := s.sealMCPConfig(current)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE mcp_servers SET config = '{}', config_enc = ?, updated_at = ? WHERE id = ?`,
				sealed, formatTime(now), current.ID); err != nil {
				return nil, errors.New("sqlite: update mcp server failed")
			}
			out = append(out, *current)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("sqlite: commit mcp import failed")
	}
	return out, nil
}

// DeleteMCPServer removes one server.
func (s *Store) DeleteMCPServer(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sqlite: mcp server id is required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id = ?`, id)
	if err != nil {
		return errors.New("sqlite: delete mcp server failed")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return errors.New("sqlite: delete mcp server failed")
	}
	if n != 1 {
		return fmt.Errorf("%w: mcp server %s", store.ErrNotFound, id)
	}
	return nil
}

func requireMCPNameFree(ctx context.Context, tx *sql.Tx, scope string, projectID *int64, name, exceptID string) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM mcp_servers
		WHERE scope = ? AND project_id IS ? AND name = ? AND id <> ?`,
		scope, nullableProjectID(projectID), name, exceptID).Scan(&found)
	if err == nil {
		return fmt.Errorf("%w: mcp server %s", store.ErrConflict, name)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return errors.New("sqlite: check mcp server name failed")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Skills

const skillColumns = `id, scope, project_id, name, description, dir, enabled, builtin, created_at`

func scanSkill(row rowScanner) (*store.Skill, error) {
	var (
		skill     store.Skill
		projectID sql.NullInt64
		createdAt string
	)
	if err := row.Scan(&skill.ID, &skill.Scope, &projectID, &skill.Name,
		&skill.Description, &skill.Dir, &skill.Enabled, &skill.Builtin, &createdAt); err != nil {
		return nil, err
	}
	if projectID.Valid {
		value := projectID.Int64
		skill.ProjectID = &value
	}
	var err error
	if skill.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	return &skill, nil
}

// ListSkills returns skills matching the scope selector.
func (s *Store) ListSkills(ctx context.Context, scope string, projectID *int64) ([]store.Skill, error) {
	where, args, err := extensionScopeClauses(scope, projectID, "project_id IS NULL")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+skillColumns+` FROM skills
		WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list skills: %w", err)
	}
	defer rows.Close()
	skills := make([]store.Skill, 0)
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan skill: %w", err)
		}
		skills = append(skills, *skill)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate skills: %w", err)
	}
	return skills, nil
}

// ListSkillsPage returns one bounded page of skills for the scope selector.
func (s *Store) ListSkillsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]store.Skill, string, error) {
	scope = strings.TrimSpace(scope)
	limit = normalizeLimit(limit)
	where, args, err := extensionScopeClauses(scope, projectID, "project_id IS NULL")
	if err != nil {
		return nil, "", err
	}
	if cursor != "" {
		bound, err := decodeListCursor(cursor, listCursorSkills)
		if err != nil {
			return nil, "", err
		}
		if bound.Scope != scope || !sameProjectID(bound.ProjectID, projectID) || bound.ID == "" {
			return nil, "", fmt.Errorf("%w: skills cursor scope", store.ErrInvalidCursor)
		}
		at, err := parseTime(bound.At)
		if err != nil {
			return nil, "", fmt.Errorf("%w: skills cursor", store.ErrInvalidCursor)
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, formatTime(at), formatTime(at), bound.ID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+skillColumns+` FROM skills
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list skills: %w", err)
	}
	defer rows.Close()
	skills := make([]store.Skill, 0, limit)
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan skill: %w", err)
		}
		skills = append(skills, *skill)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate skills: %w", err)
	}
	next := ""
	if len(skills) > limit {
		skills = skills[:limit]
		last := skills[len(skills)-1]
		next = encodeListCursor(listCursor{
			Kind: listCursorSkills, Scope: scope, ProjectID: projectID,
			At: formatTime(last.CreatedAt), ID: last.ID,
		})
	}
	return skills, next, nil
}

// CreateSkill stores one skill row. Uploaded archives keep Dir empty.
func (s *Store) CreateSkill(ctx context.Context, skill store.Skill) (*store.Skill, error) {
	skill.Scope = strings.TrimSpace(skill.Scope)
	skill.Name = strings.TrimSpace(skill.Name)
	skill.Description = strings.TrimSpace(skill.Description)
	if err := store.ValidateExtensionScope(skill.Scope, skill.ProjectID, true); err != nil {
		return nil, err
	}
	if skill.Name == "" {
		return nil, errors.New("sqlite: skill name is required")
	}
	if skill.Description == "" {
		return nil, errors.New("sqlite: skill description is required")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	skill.ID = id
	skill.CreatedAt = s.now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin create skill: %w", err)
	}
	defer tx.Rollback()
	var found int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM skills
		WHERE scope = ? AND project_id IS ? AND name = ?`,
		skill.Scope, nullableProjectID(skill.ProjectID), skill.Name).Scan(&found)
	if err == nil {
		return nil, fmt.Errorf("%w: skill %s", store.ErrConflict, skill.Name)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlite: check skill name: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO skills
		(id, scope, project_id, name, description, dir, enabled, builtin, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		skill.ID, skill.Scope, nullableProjectID(skill.ProjectID), skill.Name,
		skill.Description, skill.Dir, skill.Enabled, skill.Builtin, formatTime(skill.CreatedAt)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: skill %s", store.ErrConflict, skill.Name)
		}
		return nil, fmt.Errorf("sqlite: insert skill: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit skill: %w", err)
	}
	return &skill, nil
}

// DeleteSkill removes one skill row.
func (s *Store) DeleteSkill(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sqlite: skill id is required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete skill: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete skill: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: skill %s", store.ErrNotFound, id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Block bindings

const blockBindingColumns = `scope, project_id, block, mcp_tools, skills`

func scanBlockBinding(row rowScanner) (*store.BlockBinding, error) {
	var (
		binding   store.BlockBinding
		projectID int64
		tools     string
		skills    string
	)
	if err := row.Scan(&binding.Scope, &projectID, &binding.Block, &tools, &skills); err != nil {
		return nil, err
	}
	if projectID != 0 {
		value := projectID
		binding.ProjectID = &value
	}
	if err := json.Unmarshal([]byte(tools), &binding.MCPTools); err != nil {
		return nil, fmt.Errorf("sqlite: decode mcp tools: %w", err)
	}
	if err := json.Unmarshal([]byte(skills), &binding.Skills); err != nil {
		return nil, fmt.Errorf("sqlite: decode skills: %w", err)
	}
	if binding.MCPTools == nil {
		binding.MCPTools = []string{}
	}
	if binding.Skills == nil {
		binding.Skills = []string{}
	}
	binding.Revision = store.BlockBindingRevision(binding.Scope, binding.ProjectID, binding.Block, binding.MCPTools, binding.Skills)
	return &binding, nil
}

// ListBlockBindings returns bindings matching the scope selector.
func (s *Store) ListBlockBindings(ctx context.Context, scope string, projectID *int64) ([]store.BlockBinding, error) {
	where, args, err := extensionScopeClauses(scope, projectID, "project_id = 0")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+blockBindingColumns+` FROM block_bindings
		WHERE `+strings.Join(where, " AND ")+` ORDER BY scope, project_id, block`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list block bindings: %w", err)
	}
	defer rows.Close()
	bindings := make([]store.BlockBinding, 0)
	for rows.Next() {
		binding, err := scanBlockBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan block binding: %w", err)
		}
		bindings = append(bindings, *binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate block bindings: %w", err)
	}
	return bindings, nil
}

// ListBlockBindingsPage returns one bounded page ordered by
// (scope, project_id, block). The cursor binds to the request scope and to
// the last row key, so deleted rows do not invalidate it.
func (s *Store) ListBlockBindingsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]store.BlockBinding, string, error) {
	scope = strings.TrimSpace(scope)
	limit = normalizeLimit(limit)
	where, args, err := extensionScopeClauses(scope, projectID, "project_id = 0")
	if err != nil {
		return nil, "", err
	}
	if cursor != "" {
		bound, err := decodeListCursor(cursor, listCursorBindings)
		if err != nil {
			return nil, "", err
		}
		if bound.Scope != scope || !sameProjectID(bound.ProjectID, projectID) ||
			bound.RowScope == "" || bound.Block == "" {
			return nil, "", fmt.Errorf("%w: bindings cursor scope", store.ErrInvalidCursor)
		}
		where = append(where,
			"(scope > ? OR (scope = ? AND project_id > ?) OR (scope = ? AND project_id = ? AND block > ?))")
		args = append(args, bound.RowScope, bound.RowScope, bound.RowProjectID,
			bound.RowScope, bound.RowProjectID, bound.Block)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+blockBindingColumns+` FROM block_bindings
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY scope, project_id, block LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list block bindings: %w", err)
	}
	defer rows.Close()
	bindings := make([]store.BlockBinding, 0, limit)
	for rows.Next() {
		binding, err := scanBlockBinding(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan block binding: %w", err)
		}
		bindings = append(bindings, *binding)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate block bindings: %w", err)
	}
	next := ""
	if len(bindings) > limit {
		bindings = bindings[:limit]
		last := bindings[len(bindings)-1]
		rowProject := int64(0)
		if last.ProjectID != nil {
			rowProject = *last.ProjectID
		}
		next = encodeListCursor(listCursor{
			Kind: listCursorBindings, Scope: scope, ProjectID: projectID,
			RowScope: last.Scope, RowProjectID: rowProject, Block: last.Block,
		})
	}
	return bindings, next, nil
}

// UpsertBlockBinding creates or replaces one block binding without a revision
// check. It is for trusted internal writers (default seeding, restore and
// migrations); the HTTP layer must use UpsertBlockBindingIfRevision.
func (s *Store) UpsertBlockBinding(ctx context.Context, binding store.BlockBinding) (*store.BlockBinding, error) {
	return upsertBlockBinding(ctx, s.db, binding)
}

// UpsertBlockBindingIfRevision creates or replaces one block binding only when
// the stored content still matches expectedRevision. The read and write share
// one transaction, so a concurrent writer, restore or delete cannot slip
// between the comparison and the upsert. A missing row matches
// BlockBindingAbsentRevision.
func (s *Store) UpsertBlockBindingIfRevision(ctx context.Context, binding store.BlockBinding,
	expectedRevision string) (*store.BlockBinding, error) {
	binding.Scope = strings.TrimSpace(binding.Scope)
	binding.Block = strings.TrimSpace(binding.Block)
	if err := store.ValidateExtensionScope(binding.Scope, binding.ProjectID, true); err != nil {
		return nil, err
	}
	if binding.Block == "" {
		return nil, errors.New("sqlite: block is required")
	}
	if !store.ValidBlockBindingRevision(expectedRevision) {
		return nil, errors.New("sqlite: invalid expected block binding revision")
	}
	projectValue := int64(0)
	if binding.ProjectID != nil {
		projectValue = *binding.ProjectID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin block binding write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	currentRevision := store.BlockBindingAbsentRevision
	current, err := scanBlockBinding(tx.QueryRowContext(ctx, `SELECT `+blockBindingColumns+` FROM block_bindings
		WHERE scope = ? AND project_id = ? AND block = ?`,
		binding.Scope, projectValue, binding.Block))
	switch {
	case err == nil:
		currentRevision = current.Revision
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, fmt.Errorf("sqlite: read block binding: %w", err)
	}
	if expectedRevision != currentRevision {
		return nil, fmt.Errorf("%w: block binding %s", store.ErrStaleRevision, binding.Block)
	}
	row, err := upsertBlockBinding(ctx, tx, binding)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit block binding write: %w", err)
	}
	return row, nil
}

func upsertBlockBinding(ctx context.Context, db execer, binding store.BlockBinding) (*store.BlockBinding, error) {
	binding.Scope = strings.TrimSpace(binding.Scope)
	binding.Block = strings.TrimSpace(binding.Block)
	if err := store.ValidateExtensionScope(binding.Scope, binding.ProjectID, true); err != nil {
		return nil, err
	}
	if binding.Block == "" {
		return nil, errors.New("sqlite: block is required")
	}
	if binding.MCPTools == nil {
		binding.MCPTools = []string{}
	}
	if binding.Skills == nil {
		binding.Skills = []string{}
	}
	tools, err := json.Marshal(binding.MCPTools)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode mcp tools: %w", err)
	}
	skills, err := json.Marshal(binding.Skills)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode skills: %w", err)
	}
	projectValue := int64(0)
	if binding.ProjectID != nil {
		projectValue = *binding.ProjectID
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO block_bindings
		(scope, project_id, block, mcp_tools, skills) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (scope, project_id, block) DO UPDATE SET
			mcp_tools = excluded.mcp_tools,
			skills = excluded.skills`,
		binding.Scope, projectValue, binding.Block, string(tools), string(skills)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: block binding %s", store.ErrConflict, binding.Block)
		}
		return nil, fmt.Errorf("sqlite: upsert block binding: %w", err)
	}
	binding.Revision = store.BlockBindingRevision(binding.Scope, binding.ProjectID, binding.Block, binding.MCPTools, binding.Skills)
	return &binding, nil
}

func nullableProjectID(projectID *int64) any {
	if projectID == nil {
		return nil
	}
	return *projectID
}

func validJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal([]byte(trimmed), &object) == nil && object != nil
}
