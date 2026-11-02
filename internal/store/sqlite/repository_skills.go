package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
)

var (
	repositorySkillNameRE  = regexp.MustCompile(`^repo:[a-z0-9][a-z0-9_-]{0,63}$`)
	repositorySkillBlockRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

const repositorySkillManagerQuery = `SELECT EXISTS(SELECT 1 FROM project_grants pg
	JOIN users u ON u.id = pg.user_id WHERE pg.project_id = ? AND pg.user_id = ?
	AND pg.role = 'manage' AND u.role = 'admin' AND u.disabled = 0)`

func validRepositorySkillBranch(project int64, branch string) bool {
	return project > 0 && branch != "" && len(branch) <= 1024 && strings.TrimSpace(branch) == branch &&
		!strings.ContainsAny(branch, "\x00\r\n")
}

func validRepositorySkillScope(project int64, branch, block string) bool {
	return validRepositorySkillBranch(project, branch) && repositorySkillBlockRE.MatchString(block)
}

func canonicalRepositorySkillNames(names []string) ([]string, error) {
	if len(names) > 256 {
		return nil, errors.New("sqlite: too many repository skill authorizations")
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !repositorySkillNameRE.MatchString(name) || strings.Contains(name, "__") {
			return nil, errors.New("sqlite: invalid repository skill authorization")
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func parseRepositorySkillAuthorization(project int64, branch, block, encoded, targetSHA, snapshotSHA256, revision string) (*store.RepositorySkillAuthorization, error) {
	out := &store.RepositorySkillAuthorization{ProjectID: project, TargetBranch: branch, Block: block, TargetSHA: targetSHA, SnapshotSHA256: snapshotSHA256, Revision: revision, Skills: []string{}}
	if err := json.Unmarshal([]byte(encoded), &out.Skills); err != nil || out.Skills == nil || !store.ValidBlockBindingRevision(out.Revision) || out.Revision == store.BlockBindingAbsentRevision {
		return nil, errors.New("sqlite: invalid repository skill authorization state")
	}
	if len(out.Skills) > 0 {
		if repo.ValidateSHA(out.TargetSHA) != nil || out.TargetSHA != strings.ToLower(out.TargetSHA) ||
			(out.SnapshotSHA256 != "" && !store.ValidRepositorySkillSnapshotSHA256(out.SnapshotSHA256)) {
			return nil, errors.New("sqlite: invalid repository skill authorization state")
		}
	} else if out.TargetSHA != "" || out.SnapshotSHA256 != "" {
		return nil, errors.New("sqlite: invalid repository skill authorization state")
	}
	canonical, err := canonicalRepositorySkillNames(out.Skills)
	if err != nil || len(canonical) != len(out.Skills) {
		return nil, errors.New("sqlite: invalid repository skill authorization state")
	}
	for i := range canonical {
		if canonical[i] != out.Skills[i] {
			return nil, errors.New("sqlite: invalid repository skill authorization state")
		}
	}
	return out, nil
}

func scanRepositorySkillAuthorization(row rowScanner, project int64, branch, block string) (*store.RepositorySkillAuthorization, error) {
	out := &store.RepositorySkillAuthorization{ProjectID: project, TargetBranch: branch, Block: block, Skills: []string{}, Revision: store.BlockBindingAbsentRevision}
	var encoded string
	err := row.Scan(&encoded, &out.TargetSHA, &out.SnapshotSHA256, &out.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return parseRepositorySkillAuthorization(project, branch, block, encoded, out.TargetSHA, out.SnapshotSHA256, out.Revision)
}

// GetRepositorySkillAuthorization reads one exact project/branch/block grant.
// A missing grant has an empty skill list and the absent revision. The caller
// must enforce project access before exposing the result.
func (s *Store) GetRepositorySkillAuthorization(ctx context.Context, project int64, branch, block string) (*store.RepositorySkillAuthorization, error) {
	if !validRepositorySkillScope(project, branch, block) {
		return nil, errors.New("sqlite: invalid repository skill scope")
	}
	row := s.db.QueryRowContext(ctx, `SELECT skills, target_sha, snapshot_sha256, revision FROM repository_skill_authorizations
		WHERE project_id = ? AND target_branch = ? AND block = ?`, project, branch, block)
	grant, err := scanRepositorySkillAuthorization(row, project, branch, block)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read repository skill authorization: %w", err)
	}
	return grant, nil
}

// ListRepositorySkillAuthorizations reads every exact grant for one project
// and target branch. A branch with no grants returns an empty slice.
func (s *Store) ListRepositorySkillAuthorizations(ctx context.Context, project int64, branch string) ([]store.RepositorySkillAuthorization, error) {
	if !validRepositorySkillBranch(project, branch) {
		return nil, errors.New("sqlite: invalid repository skill scope")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT block, skills, target_sha, snapshot_sha256, revision FROM repository_skill_authorizations
		WHERE project_id = ? AND target_branch = ? ORDER BY block`, project, branch)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list repository skill authorizations: %w", err)
	}
	defer rows.Close()
	out := []store.RepositorySkillAuthorization{}
	for rows.Next() {
		var block, encoded, targetSHA, snapshotSHA256, revision string
		if err := rows.Scan(&block, &encoded, &targetSHA, &snapshotSHA256, &revision); err != nil {
			return nil, fmt.Errorf("sqlite: list repository skill authorizations: %w", err)
		}
		if !repositorySkillBlockRE.MatchString(block) {
			return nil, errors.New("sqlite: invalid repository skill authorization state")
		}
		grant, err := parseRepositorySkillAuthorization(project, branch, block, encoded, targetSHA, snapshotSHA256, revision)
		if err != nil {
			return nil, err
		}
		out = append(out, *grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list repository skill authorizations: %w", err)
	}
	return out, nil
}

// AuthorizeRepositorySkillManagement checks both current roles in one read.
func (s *Store) AuthorizeRepositorySkillManagement(ctx context.Context, userID, projectID int64) error {
	if userID <= 0 || projectID <= 0 {
		return store.ErrNotFound
	}
	var allowed int
	err := s.db.QueryRowContext(ctx, repositorySkillManagerQuery, projectID, userID).Scan(&allowed)
	if err != nil {
		return fmt.Errorf("sqlite: verify repository skill access: %w", err)
	}
	if allowed != 1 {
		return store.ErrNotFound
	}
	return nil
}

// SaveRepositorySkillAuthorization atomically checks the actor's current
// platform administrator role, project manage grant and expected revision.
func (s *Store) SaveRepositorySkillAuthorization(ctx context.Context, grant store.RepositorySkillAuthorization, expected string, actorID int64) (*store.RepositorySkillAuthorization, error) {
	if !validRepositorySkillScope(grant.ProjectID, grant.TargetBranch, grant.Block) || actorID <= 0 || !store.ValidBlockBindingRevision(expected) {
		return nil, errors.New("sqlite: invalid repository skill authorization")
	}
	names, err := canonicalRepositorySkillNames(grant.Skills)
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		if repo.ValidateSHA(grant.TargetSHA) != nil {
			return nil, errors.New("sqlite: invalid repository skill target SHA")
		}
		if !store.ValidRepositorySkillSnapshotSHA256(grant.SnapshotSHA256) {
			return nil, errors.New("sqlite: invalid repository skill snapshot SHA-256")
		}
		grant.TargetSHA = strings.ToLower(grant.TargetSHA)
	} else {
		grant.TargetSHA = ""
		grant.SnapshotSHA256 = ""
	}
	encoded, err := json.Marshal(names)
	if err != nil {
		return nil, errors.New("sqlite: encode repository skill authorization")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.New("sqlite: repository skill authorization revision unavailable")
	}
	newRevision := hex.EncodeToString(random[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin repository skill authorization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var allowed int
	err = tx.QueryRowContext(ctx, repositorySkillManagerQuery, grant.ProjectID, actorID).Scan(&allowed)
	if err != nil {
		return nil, fmt.Errorf("sqlite: verify repository skill authorization: %w", err)
	}
	if allowed != 1 {
		return nil, fmt.Errorf("%w: project manage grant unavailable", store.ErrNotFound)
	}
	current, err := scanRepositorySkillAuthorization(tx.QueryRowContext(ctx, `SELECT skills, target_sha, snapshot_sha256, revision FROM repository_skill_authorizations
		WHERE project_id = ? AND target_branch = ? AND block = ?`, grant.ProjectID, grant.TargetBranch, grant.Block), grant.ProjectID, grant.TargetBranch, grant.Block)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read repository skill authorization: %w", err)
	}
	if current.Revision != expected {
		return nil, store.ErrStaleRevision
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO repository_skill_authorizations (project_id, target_branch, block, skills, target_sha, snapshot_sha256, revision)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (project_id, target_branch, block)
		DO UPDATE SET skills = excluded.skills, target_sha = excluded.target_sha, snapshot_sha256 = excluded.snapshot_sha256, revision = excluded.revision`, grant.ProjectID, grant.TargetBranch, grant.Block, string(encoded), grant.TargetSHA, grant.SnapshotSHA256, newRevision)
	if err != nil {
		return nil, fmt.Errorf("sqlite: save repository skill authorization: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit repository skill authorization: %w", err)
	}
	grant.Skills = names
	grant.Revision = newRevision
	return &grant, nil
}
