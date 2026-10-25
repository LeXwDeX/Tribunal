package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

// ErrContextProjection identifies a failure to mirror the database-active
// context document into the workspace. The database remains authoritative, so
// refresh must surface this error instead of reporting success.
var ErrContextProjection = errors.New("app: context projection failed")

// ContextStore is the persistence surface needed for manual context edits and
// lock changes.
type ContextStore interface {
	GetWorkspace(ctx context.Context, projectID int64) (*store.Workspace, error)
	GetCurrentContextDoc(ctx context.Context, projectID int64, kind string) (*store.ContextDoc, error)
	SaveManualContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error)
	LockContextDoc(ctx context.Context, projectID int64, baseSHA, kind string, locked bool, userID *int64) error
}

// ContextServiceConfig configures ContextService.
type ContextServiceConfig struct {
	Store     ContextStore
	Workspace *workspace.Manager
}

// ContextService owns manual context edits. The database is the source of
// truth; every successful edit is projected into the shared workspace while
// holding the same workspace lock used by refresh.
type ContextService struct {
	store     ContextStore
	workspace *workspace.Manager
}

// NewContextService validates dependencies.
func NewContextService(cfg ContextServiceConfig) (*ContextService, error) {
	if cfg.Store == nil || cfg.Workspace == nil {
		return nil, errors.New("app: context service store and workspace are required")
	}
	return &ContextService{store: cfg.Store, workspace: cfg.Workspace}, nil
}

// SaveContext appends a manual revision and projects the stored row to disk.
// The read of the current lock, the save and the projection happen under the
// workspace lock, so a concurrent refresh cannot race the lock inheritance.
func (s *ContextService) SaveContext(ctx context.Context, projectID int64, kind, content string, locked *bool, userID *int64) (*store.ContextDoc, error) {
	if projectID <= 0 {
		return nil, errors.New("app: context project id is required")
	}
	if kind != "agents" && kind != "claude" {
		return nil, fmt.Errorf("app: invalid context kind %q", kind)
	}
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("app: context content is required")
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return nil, errors.New("app: workspace is unavailable")
	}
	unlock, err := ws.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	state, stateErr := s.store.GetWorkspace(ctx, projectID)
	if stateErr != nil && !errors.Is(stateErr, store.ErrNotFound) {
		return nil, stateErr
	}
	current, currentErr := s.store.GetCurrentContextDoc(ctx, projectID, kind)
	if currentErr != nil && !errors.Is(currentErr, store.ErrNotFound) {
		return nil, currentErr
	}
	baseSHA := ""
	lockValue := false
	if currentErr == nil {
		baseSHA = current.BaseSHA
		lockValue = current.Locked
	}
	if baseSHA == "" {
		if stateErr != nil || state == nil || strings.TrimSpace(state.BaseSHA) == "" {
			return nil, fmt.Errorf("%w: context %s for project %d", store.ErrNotFound, kind, projectID)
		}
		baseSHA = state.BaseSHA
	}
	if locked != nil {
		lockValue = *locked
	}

	doc, err := s.store.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: projectID,
		BaseSHA:   baseSHA,
		Kind:      kind,
		Content:   content,
		Locked:    lockValue,
		UpdatedBy: userID,
	})
	if err != nil {
		return nil, err
	}
	stale := stateErr == nil && state != nil && state.BaseSHA != "" && state.BaseSHA != doc.BaseSHA
	if err := projectContextDoc(ws, doc, stale); err != nil {
		return nil, err
	}
	return doc, nil
}

// LockContext toggles the lock on the active revision and projects the flag
// into the workspace metadata. Content is not rewritten.
func (s *ContextService) LockContext(ctx context.Context, projectID int64, kind string, locked bool, userID *int64) error {
	if projectID <= 0 {
		return errors.New("app: context project id is required")
	}
	if kind != "agents" && kind != "claude" {
		return fmt.Errorf("app: invalid context kind %q", kind)
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return errors.New("app: workspace is unavailable")
	}
	unlock, err := ws.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	current, err := s.store.GetCurrentContextDoc(ctx, projectID, kind)
	if err != nil {
		return err
	}
	if !current.Active {
		return fmt.Errorf("%w: active context %s for project %d", store.ErrNotFound, kind, projectID)
	}
	if err := s.store.LockContextDoc(ctx, projectID, current.BaseSHA, kind, locked, userID); err != nil {
		return err
	}
	updated := *current
	updated.Locked = locked
	if userID != nil {
		value := *userID
		updated.UpdatedBy = &value
	}
	state, stateErr := s.store.GetWorkspace(ctx, projectID)
	if stateErr != nil && !errors.Is(stateErr, store.ErrNotFound) {
		return stateErr
	}
	stale := stateErr == nil && state != nil && state.BaseSHA != "" && state.BaseSHA != updated.BaseSHA
	if err := projectContextDoc(ws, &updated, stale); err != nil {
		return err
	}
	return nil
}

// projectContextDoc mirrors exactly the database-active revision. An inactive
// revision is ignored so a generated retry cannot overwrite the current file.
func projectContextDoc(ws *workspace.Workspace, doc *store.ContextDoc, stale bool) error {
	if ws == nil {
		return errors.New("app: workspace is unavailable")
	}
	if doc == nil || !doc.Active {
		return nil
	}
	name := ""
	switch doc.Kind {
	case "agents":
		name = "AGENTS.md"
	case "claude":
		name = "CLAUDE.md"
	default:
		return fmt.Errorf("app: invalid context kind %q", doc.Kind)
	}
	meta := workspace.ContextMeta{
		SourceSHA:   doc.BaseSHA,
		Model:       doc.Model,
		GeneratedAt: doc.CreatedAt,
		Locked:      doc.Locked,
		Stale:       stale,
	}
	if err := ws.WriteActiveContext(doc.BaseSHA, map[string][]byte{name: []byte(doc.Content)}, meta); err != nil {
		return fmt.Errorf("app: project context %s at %s: %w", name, doc.BaseSHA, err)
	}
	return nil
}
