package app

import (
	"context"
	"errors"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type repositorySkillGrantLister interface {
	ListRepositorySkillAuthorizations(context.Context, int64, string) ([]store.RepositorySkillAuthorization, error)
}

type repositorySkillRuntime struct {
	blocks     map[string]config.BlockBinding
	skills     review.SkillProvider
	provenance *review.RepositorySkillProvenance
	cleanup    func()
}

func noRepositorySkillProvenance() *review.RepositorySkillProvenance {
	return &review.RepositorySkillProvenance{Version: review.RepositorySkillProvenanceVersion, Mode: "none"}
}

// resolveRepositorySkillRuntime applies only stored grants whose reviewed
// target commit and filtered snapshot still match the current target branch
// tip. It runs under the caller's workspace read lock before any agent starts.
func (s *ReviewService) resolveRepositorySkillRuntime(ctx context.Context, project *store.Project, token, branch string, deny func(string) bool, baseBlocks map[string]config.BlockBinding, baseSkills review.SkillProvider) (*repositorySkillRuntime, error) {
	none := &repositorySkillRuntime{blocks: cloneBlockBindings(baseBlocks), skills: baseSkills, provenance: noRepositorySkillProvenance()}
	if none.blocks == nil {
		none.blocks = make(map[string]config.BlockBinding)
	}
	if project == nil || project.ID <= 0 {
		return nil, errors.New("app: repository skill project identity unavailable")
	}
	settings, err := ResolveSettings(project.Settings, s.reviewDefaults)
	if err != nil {
		return nil, errors.New("app: project settings cannot be read safely")
	}
	if !settings.AllowRepoSkills {
		return none, nil
	}
	if branch == "" || strings.TrimSpace(branch) != branch {
		return none, nil
	}
	lister, ok := s.store.(repositorySkillGrantLister)
	if !ok || lister == nil {
		return none, nil
	}
	grants, err := lister.ListRepositorySkillAuthorizations(ctx, project.ID, branch)
	if err != nil {
		return nil, err
	}
	granted := make([]store.RepositorySkillAuthorization, 0, len(grants))
	for _, grant := range grants {
		if len(grant.Skills) > 0 {
			granted = append(granted, grant)
		}
	}
	if len(granted) == 0 {
		return none, nil
	}
	ws := s.workspace.Get(project.ID)
	if ws == nil {
		return nil, errors.New("app: repository skill workspace is unavailable")
	}
	source, ok := s.git.(repositorySkillSourceGit)
	if !ok || source == nil || s.gitlab == nil {
		return nil, PermanentJob(errors.New("app: repository skill source is unavailable; reauthorize repository skills"))
	}
	sha, files, snapshot, err := loadRepositorySkillSnapshot(ctx, ws, source, s.gitlab, project, token, branch, deny, s.reviewDefaults, s.profiles)
	if err != nil {
		return nil, err
	}
	for _, grant := range granted {
		if repo.ValidateSHA(grant.TargetSHA) != nil || !strings.EqualFold(grant.TargetSHA, sha) {
			return nil, PermanentJob(errors.New("app: repository skill target changed; reauthorize repository skills"))
		}
		if !store.ValidRepositorySkillSnapshotSHA256(grant.SnapshotSHA256) || grant.SnapshotSHA256 != snapshot {
			return nil, PermanentJob(errors.New("app: repository skill snapshot changed; reauthorize repository skills"))
		}
	}
	blocks, identities, err := applyRepositorySkillAuthorizations(baseBlocks, project.ID, branch, sha, grants)
	if err != nil {
		return nil, PermanentJob(errors.New("app: repository skill authorization is unavailable; reauthorize repository skills"))
	}
	dir, remove, err := repo.MaterializeRepositorySkills(ctx, ws.Root, files)
	if err != nil {
		return nil, PermanentJob(errors.New("app: repository skill snapshot is unsafe; reauthorize repository skills"))
	}
	cleanup := func() { _ = remove() }
	loader, err := skill.New(skill.Options{RepoSkillsDir: dir})
	if err != nil {
		cleanup()
		return nil, PermanentJob(errors.New("app: repository skill snapshot is unreadable; reauthorize repository skills"))
	}
	return &repositorySkillRuntime{
		blocks: blocks,
		skills: &combinedRepositorySkillProvider{base: baseSkills, repository: NewSkillProvider(loader)},
		provenance: &review.RepositorySkillProvenance{
			Version:        review.RepositorySkillProvenanceVersion,
			Mode:           "repository",
			TargetBranch:   branch,
			TargetSHA:      sha,
			GrantRevisions: identities,
			SnapshotSHA256: snapshot,
		},
		cleanup: cleanup,
	}, nil
}

func (e *reviewExecution) cleanupRepositorySkills() {
	if e != nil && e.repositoryCleanup != nil {
		e.repositoryCleanup()
		e.repositoryCleanup = nil
	}
}

func (e *reviewExecution) repositorySkillsVerified() bool {
	return e != nil && e.repository != nil && e.repository.Mode == "repository"
}

// loadRepositorySkillSnapshot pins the authenticated target branch and returns
// the exact commit, filtered files and snapshot hash that authorization uses.
func loadRepositorySkillSnapshot(ctx context.Context, ws *workspace.Workspace, source repositorySkillSourceGit, client GitLabClient, project *store.Project, token, branch string, deny func(string) bool, defaults config.ProjectSettings, profiles map[string]config.ProfileConfig) (string, map[string][]byte, string, error) {
	sha, err := pinRepositorySkillTarget(ctx, ws, source, client, project, token, branch)
	if err != nil {
		return "", nil, "", err
	}
	settings, err := ResolveSettings(project.Settings, defaults)
	if err != nil {
		return "", nil, "", errors.New("app: project settings cannot be read safely")
	}
	var docs review.RepoDocs
	if settings.AllowRepoConfig {
		data, _, err := source.ReadRepositorySkillConfig(ctx, ws.MirrorDir(), sha)
		if err != nil {
			return "", nil, "", err
		}
		docs.GitLabReviewConfig = string(data)
	}
	cfg, err := resolveSecureReviewConfig(project, docs, defaults, profiles)
	if err != nil {
		return "", nil, "", err
	}
	policy := &review.Dossier{IgnorePolicy: &review.IgnorePolicy{Version: review.IgnorePolicyVersion, Patterns: append([]string{}, cfg.Settings.Ignore...)}}
	filter := policy.IgnoresPath
	if deny != nil {
		filter = func(name string) bool { return policy.IgnoresPath(name) || deny(name) }
	}
	files, err := source.ReadRepositorySkills(ctx, ws.MirrorDir(), sha, filter)
	if err != nil {
		return "", nil, "", err
	}
	snapshot, err := repo.RepositorySkillsSnapshotSHA256(files)
	if err != nil {
		return "", nil, "", err
	}
	return sha, files, snapshot, nil
}

// repositorySkillProvenanceCurrent checks stored grants against the
// provenance recorded by the original execution. It never reads repository
// content; execution validates content separately.
func repositorySkillProvenanceCurrent(ctx context.Context, lister repositorySkillGrantLister, project *store.Project, defaults config.ProjectSettings, dossier *review.Dossier, record *review.RepositorySkillProvenance) error {
	if record == nil || record.Mode == "none" {
		return nil
	}
	if record.Mode != "repository" || dossier == nil || dossier.MR.TargetBranch != record.TargetBranch {
		return errors.New("app: repository skill provenance is unavailable")
	}
	if project == nil || !project.Enabled {
		return errors.New("app: repository skill project is unavailable")
	}
	settings, err := ResolveSettings(project.Settings, defaults)
	if err != nil || !settings.AllowRepoSkills {
		return errors.New("app: repository skills are disabled")
	}
	if lister == nil {
		return errors.New("app: repository skill grants are unavailable")
	}
	grants, err := lister.ListRepositorySkillAuthorizations(ctx, project.ID, record.TargetBranch)
	if err != nil {
		return err
	}
	current := make(map[string]store.RepositorySkillAuthorization, len(grants))
	for _, grant := range grants {
		current[grant.Block] = grant
	}
	authorized := false
	for block, revision := range record.GrantRevisions {
		grant, ok := current[block]
		if !ok {
			grant = store.RepositorySkillAuthorization{ProjectID: project.ID, TargetBranch: record.TargetBranch, Block: block, Skills: []string{}, Revision: store.BlockBindingAbsentRevision}
		}
		if grant.Revision != revision {
			return errors.New("app: repository skill authorization changed")
		}
		if len(grant.Skills) > 0 {
			if !strings.EqualFold(grant.TargetSHA, record.TargetSHA) || grant.SnapshotSHA256 != record.SnapshotSHA256 {
				return errors.New("app: repository skill authorization changed")
			}
			authorized = true
		}
	}
	if !authorized {
		return errors.New("app: repository skill authorization is absent")
	}
	for block, grant := range current {
		if len(grant.Skills) == 0 {
			continue
		}
		if _, ok := record.GrantRevisions[block]; !ok {
			return errors.New("app: repository skill authorization changed")
		}
	}
	return nil
}

// repositorySkillPublicationCurrent reauthenticates the recorded repository
// capability before any pending or manually requested publication. It checks
// the stored grants first, then fetches the target branch tip again and
// recomputes the filtered snapshot, so branch movement or a filter change
// cannot publish output derived from a stale capability.
func (p *Publisher) repositorySkillPublicationCurrent(ctx context.Context, project *store.Project, dossier *review.Dossier) error {
	if p == nil || dossier == nil || dossier.RepositorySkillProvenance == nil || dossier.RepositorySkillProvenance.Mode == "none" {
		return nil
	}
	record := dossier.RepositorySkillProvenance
	if record.Mode != "repository" || dossier.IgnorePolicy == nil || dossier.IgnorePolicy.Version != review.IgnorePolicyVersion || dossier.IgnorePolicy.Patterns == nil {
		return errors.New("app: repository skill provenance is unavailable")
	}
	lister, _ := p.store.(repositorySkillGrantLister)
	if err := repositorySkillProvenanceCurrent(ctx, lister, project, p.reviewDefaults, dossier, record); err != nil {
		return err
	}
	source, ok := p.git.(repositorySkillSourceGit)
	if !ok || source == nil || p.gitlab == nil || p.cipher == nil || p.workspace == nil || project == nil {
		return errors.New("app: repository skill publication source is unavailable")
	}
	ws := p.workspace.Get(project.ID)
	if ws == nil {
		return errors.New("app: repository skill publication workspace is unavailable")
	}
	token, err := p.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
	if err != nil {
		return errors.New("app: project token is unavailable")
	}
	deny, err := repositorySkillDenyFromEvidence(dossier.IgnorePolicy.Patterns, dossier.RepositorySkillExcluded)
	if err != nil {
		return err
	}
	sha, _, snapshot, err := loadRepositorySkillSnapshot(ctx, ws, source, p.gitlab, project, string(token), record.TargetBranch, deny, p.reviewDefaults, p.profiles)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sha, record.TargetSHA) {
		return errors.New("app: repository skill target moved")
	}
	if snapshot != record.SnapshotSHA256 {
		return errors.New("app: repository skill snapshot changed")
	}
	return nil
}
