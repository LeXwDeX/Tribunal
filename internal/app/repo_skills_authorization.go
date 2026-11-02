package app

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type repositorySkillGrantStore interface {
	GetRepositorySkillAuthorization(context.Context, int64, string, string) (*store.RepositorySkillAuthorization, error)
	SaveRepositorySkillAuthorization(context.Context, store.RepositorySkillAuthorization, string, int64) (*store.RepositorySkillAuthorization, error)
}

// RepositorySkillAuthorization returns one exact grant, independently of
// legacy block bindings. HTTP callers also require platform administrator.
func (d *Dispatcher) RepositorySkillAuthorization(ctx context.Context, projectID int64, branch, block string) (*store.RepositorySkillAuthorization, error) {
	if d == nil || d.store == nil || d.workspace == nil || projectID <= 0 || branch == "" || strings.TrimSpace(branch) != branch {
		return nil, errors.New("app: repository skill authorization unavailable")
	}
	if _, err := repo.NormalizeRef("refs/heads/" + branch); err != nil {
		return nil, validationError("invalid target branch", Detail{Field: "target_branch", Message: "invalid target branch"})
	}
	st, ok := d.store.(repositorySkillGrantStore)
	if !ok {
		return nil, errors.New("app: repository skill authorization unavailable")
	}
	ws := d.workspace.Get(projectID)
	lifecycleUnlock, err := ws.LifecycleRLock(ctx)
	if err != nil {
		return nil, err
	}
	defer lifecycleUnlock()
	if err := authorizeDispatch(ctx, projectID); err != nil {
		return nil, err
	}
	unlock, err := ws.RLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	project, err := d.store.GetProject(ctx, projectID)
	if err != nil || project == nil || project.ID != projectID {
		if err != nil {
			return nil, err
		}
		return nil, store.ErrNotFound
	}
	grant, err := st.GetRepositorySkillAuthorization(ctx, projectID, branch, block)
	if err != nil {
		return nil, err
	}
	if err := authorizeDispatch(ctx, projectID); err != nil {
		return nil, err
	}
	return grant, nil
}

// ReplaceRepositorySkillAuthorization checks non-empty grants against the
// target commit and filtered snapshot the administrator reviewed. Empty grants
// remain available to revoke authorization even when repository skills are
// disabled, and clear both the target and snapshot SHA.
func (d *Dispatcher) ReplaceRepositorySkillAuthorization(ctx context.Context, grant store.RepositorySkillAuthorization, expectedRevision, targetSHA, snapshotSHA256 string, actorID int64) (*store.RepositorySkillAuthorization, error) {
	if d == nil || d.store == nil || d.workspace == nil || d.onboarding == nil || d.cipher == nil || grant.ProjectID <= 0 || actorID <= 0 || !store.ValidBlockBindingRevision(expectedRevision) || grant.TargetBranch == "" || strings.TrimSpace(grant.TargetBranch) != grant.TargetBranch {
		return nil, errors.New("app: repository skill authorization unavailable")
	}
	if _, err := repo.NormalizeRef("refs/heads/" + grant.TargetBranch); err != nil {
		return nil, validationError("invalid target branch", Detail{Field: "target_branch", Message: "invalid target branch"})
	}
	st, ok := d.store.(repositorySkillGrantStore)
	if !ok {
		return nil, errors.New("app: repository skill authorization unavailable")
	}
	ws := d.workspace.Get(grant.ProjectID)
	// Workspace lock waits stay under the caller's context. Only the outbound
	// drain is bounded, by ChangeOutbound itself.
	lifecycleUnlock, err := ws.LifecycleRLock(ctx)
	if err != nil {
		return nil, err
	}
	defer lifecycleUnlock()
	if err := authorizeDispatch(ctx, grant.ProjectID); err != nil {
		return nil, err
	}
	// Drain once before the content lock and again before writing, to fence
	// sends admitted between those steps without locking inside the callback.
	if err := ws.ChangeOutbound(ctx, func(context.Context) error { return nil }); err != nil {
		return nil, err
	}
	unlock, err := ws.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	project, err := d.store.GetProject(ctx, grant.ProjectID)
	if err != nil || project == nil || project.ID != grant.ProjectID {
		if err != nil {
			return nil, err
		}
		return nil, store.ErrNotFound
	}
	grant.TargetSHA = ""
	grant.SnapshotSHA256 = ""
	if len(grant.Skills) > 0 {
		settings, err := ResolveSettings(project.Settings, d.onboarding.reviewDefaults)
		if err != nil || !settings.AllowRepoSkills || !project.Enabled {
			return nil, conflictError("repository skills are disabled")
		}
		if repo.ValidateSHA(targetSHA) != nil {
			return nil, validationError("target SHA is required", Detail{Field: "target_sha", Message: "must identify the reviewed target commit"})
		}
		if !store.ValidRepositorySkillSnapshotSHA256(snapshotSHA256) {
			return nil, validationError("snapshot SHA-256 is required", Detail{Field: "snapshot_sha256", Message: "must identify the reviewed filtered snapshot"})
		}
		token, err := d.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
		if err != nil {
			return nil, errors.New("app: project token is unavailable")
		}
		catalogue, err := discoverRepositorySkills(ctx, ws, d.onboarding.git, d.onboarding.gitlab, project, string(token), grant.TargetBranch, d.onboarding.reviewDefaults, d.onboarding.profiles)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(catalogue.TargetSHA, targetSHA) {
			return nil, conflictError("repository skill target changed; reload and retry")
		}
		if catalogue.SnapshotSHA256 != snapshotSHA256 {
			return nil, conflictError("repository skill snapshot changed; reload and retry")
		}
		grant.TargetSHA = catalogue.TargetSHA
		grant.SnapshotSHA256 = catalogue.SnapshotSHA256
		allowed := make(map[string]bool, len(catalogue.Skills))
		for _, item := range catalogue.Skills {
			allowed[item.Name] = true
		}
		for _, name := range grant.Skills {
			if !allowed[name] {
				return nil, validationError("repository skill is unavailable", Detail{Field: "skills", Message: "skill is not available in the filtered target snapshot"})
			}
		}
	}
	var result *store.RepositorySkillAuthorization
	err = ws.ChangeOutbound(ctx, func(ctx context.Context) error {
		if len(grant.Skills) > 0 {
			current, err := d.store.GetProject(ctx, grant.ProjectID)
			if err != nil {
				return err
			}
			if current == nil || !current.Enabled || !bytes.Equal(current.Settings, project.Settings) {
				return conflictError("repository skill policy changed; reload and retry")
			}
		}
		if err := authorizeDispatch(ctx, grant.ProjectID); err != nil {
			return err
		}
		var err error
		result, err = st.SaveRepositorySkillAuthorization(ctx, grant, expectedRevision, actorID)
		return err
	})
	if errors.Is(err, store.ErrStaleRevision) {
		return nil, conflictError("repository skill authorization changed; reload and retry")
	}
	return result, err
}
