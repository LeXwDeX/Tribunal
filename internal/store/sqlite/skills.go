package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// GetSkill returns one stored skill.
func (s *Store) GetSkill(ctx context.Context, id string) (*store.Skill, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("sqlite: skill id is required")
	}
	item, err := scanSkill(s.db.QueryRowContext(ctx,
		`SELECT `+skillColumns+` FROM skills WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: skill %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get skill: %w", err)
	}
	return item, nil
}

// SetSkillDir records the extracted or copied server-side skill directory.
func (s *Store) SetSkillDir(ctx context.Context, id, dir string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sqlite: skill id is required")
	}
	if strings.TrimSpace(dir) == "" {
		return errors.New("sqlite: skill directory is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE skills SET dir = ? WHERE id = ?`, dir, id)
	if err != nil {
		return fmt.Errorf("sqlite: set skill directory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set skill directory: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: skill %s", store.ErrNotFound, id)
	}
	return nil
}
