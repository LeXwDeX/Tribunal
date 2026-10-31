package app

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type repositorySkillTargetGit interface {
	GitOps
	ResolveCommitRef(context.Context, string, string) (string, error)
}

type repositorySkillSourceGit interface {
	repositorySkillTargetGit
	ReadRepositorySkills(context.Context, string, string, func(string) bool) (map[string][]byte, error)
	ReadRepositorySkillConfig(context.Context, string, string) ([]byte, bool, error)
}

func (r repoGit) ResolveCommitRef(ctx context.Context, dir, ref string) (string, error) {
	return r.git.ResolveCommitRef(ctx, dir, ref)
}

func (r repoGit) ReadRepositorySkills(ctx context.Context, dir, sha string, deny func(string) bool) (map[string][]byte, error) {
	return r.git.ReadRepositorySkills(ctx, dir, sha, deny)
}

func (r repoGit) ReadRepositorySkillConfig(ctx context.Context, dir, sha string) ([]byte, bool, error) {
	return r.git.ReadRepositorySkillConfig(ctx, dir, sha)
}

type RepositorySkillMetadata struct {
	Source      string `json:"source"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type RepositorySkillDiscovery struct {
	ProjectID      int64                     `json:"project_id"`
	TargetBranch   string                    `json:"target_branch"`
	TargetSHA      string                    `json:"target_sha"`
	SnapshotSHA256 string                    `json:"snapshot_sha256"`
	Enabled        bool                      `json:"enabled"`
	Skills         []RepositorySkillMetadata `json:"skills"`
}

// repositorySkillDenyPaths carries both sides of ignored renames into the
// source filter and returns the exact propagated deny set. Dossier.Ignored
// alone may only contain the destination path.
func repositorySkillDenyPaths(dossier *review.Dossier, diffs []gitlab.FileDiff) (map[string]bool, []string, error) {
	if dossier == nil || dossier.IgnorePolicy == nil || dossier.IgnorePolicy.Version != review.IgnorePolicyVersion || dossier.IgnorePolicy.Patterns == nil || len(diffs) > 100000 || len(dossier.Ignored) > 100000 {
		return nil, nil, errors.New("app: repository skill ignore policy unavailable")
	}
	patterns := append([]string{}, dossier.IgnorePolicy.Patterns...)
	if err := review.ValidateIgnorePatterns(patterns); err != nil {
		return nil, nil, errors.New("app: repository skill ignore policy unavailable")
	}
	matcher := review.NewIgnoreMatcher(patterns)
	denied := make(map[string]bool)
	links := make(map[string][]string)
	children := make(map[string][]string)
	var pathBytes, edgeCount int
	var pending []string
	mark := func(name string) {
		if name != "" && !denied[name] {
			denied[name] = true
			pending = append(pending, name)
		}
	}
	for _, name := range dossier.Ignored {
		if !validRepoPath(name) {
			return nil, nil, errors.New("app: repository skill ignore policy unavailable")
		}
		pathBytes += len(name)
		if pathBytes > 16<<20 {
			return nil, nil, errors.New("app: repository skill ignore policy unavailable")
		}
		mark(name)
	}
	for _, diff := range diffs {
		var paths []string
		seen := make(map[string]bool, 4)
		for _, candidate := range []string{diff.OldPath, diff.NewPath, strings.TrimSpace(diff.OldPath), strings.TrimSpace(diff.NewPath)} {
			if candidate == "" || seen[candidate] {
				continue
			}
			seen[candidate] = true
			pathBytes += len(candidate)
			if pathBytes > 16<<20 {
				return nil, nil, errors.New("app: repository skill ignore policy unavailable")
			}
			paths = append(paths, candidate)
			if !validRepoPath(candidate) || matcher.MatchPath(candidate) {
				mark(candidate)
			} else {
				for parent := path.Dir(candidate); parent != "."; parent = path.Dir(parent) {
					children[parent] = append(children[parent], candidate)
					edgeCount++
					if edgeCount > 1<<20 {
						return nil, nil, errors.New("app: repository skill ignore policy unavailable")
					}
				}
			}
		}
		for i := 1; i < len(paths); i++ {
			links[paths[0]] = append(links[paths[0]], paths[i])
			links[paths[i]] = append(links[paths[i]], paths[0])
		}
	}
	for i := 0; i < len(pending); i++ {
		for _, linked := range links[pending[i]] {
			mark(linked)
		}
		for _, child := range children[pending[i]] {
			mark(child)
		}
	}
	return denied, patterns, nil
}

func repositorySkillDenyFunc(denied map[string]bool, patterns []string) func(string) bool {
	matcher := review.NewIgnoreMatcher(patterns)
	return func(name string) bool {
		if !validRepoPath(name) || matcher.MatchPath(name) {
			return true
		}
		for current := name; current != "."; current = path.Dir(current) {
			if denied[current] {
				return true
			}
		}
		return false
	}
}

func repositorySkillDeny(dossier *review.Dossier, diffs []gitlab.FileDiff) (func(string) bool, error) {
	denied, patterns, err := repositorySkillDenyPaths(dossier, diffs)
	if err != nil {
		return nil, err
	}
	return repositorySkillDenyFunc(denied, patterns), nil
}

// repositorySkillDenyFromEvidence rebuilds the same filter from the stored
// deny set. It needs no original diff, so recovery and publication cannot
// silently drop ignored rename paths.
func repositorySkillDenyFromEvidence(patterns, excluded []string) (func(string) bool, error) {
	patterns = append([]string{}, patterns...)
	if err := review.ValidateIgnorePatterns(patterns); err != nil {
		return nil, errors.New("app: repository skill ignore policy unavailable")
	}
	if len(excluded) > 100000 {
		return nil, errors.New("app: repository skill ignore policy unavailable")
	}
	denied := make(map[string]bool, len(excluded))
	var pathBytes int
	for _, name := range excluded {
		if !validRepoPath(name) {
			return nil, errors.New("app: repository skill ignore policy unavailable")
		}
		pathBytes += len(name)
		if pathBytes > 16<<20 {
			return nil, errors.New("app: repository skill ignore policy unavailable")
		}
		denied[name] = true
	}
	return repositorySkillDenyFunc(denied, patterns), nil
}

// ignoredReviewPaths mirrors buildReviewFiles' ignore selection so the deny set
// is computed from the same MR files before the dossier exists.
func ignoredReviewPaths(diffs []gitlab.FileDiff, ignores []string) []string {
	matcher := review.NewIgnoreMatcher(ignores)
	out := make([]string, 0)
	for _, diff := range diffs {
		filePath := strings.TrimSpace(diff.NewPath)
		if filePath == "" {
			filePath = strings.TrimSpace(diff.OldPath)
		}
		if !validRepoPath(filePath) {
			continue
		}
		if matcher.MatchPath(filePath) || matcher.MatchPath(strings.TrimSpace(diff.OldPath)) {
			out = append(out, filePath)
		}
	}
	return out
}

// DiscoverRepositorySkills checks the dispatch authorization before and after
// reading. HTTP callers must also require platform administrator access.
func (d *Dispatcher) DiscoverRepositorySkills(ctx context.Context, projectID int64, branch string) (*RepositorySkillDiscovery, error) {
	if d == nil || d.workspace == nil || d.store == nil || d.onboarding == nil || d.cipher == nil || projectID <= 0 {
		return nil, errors.New("app: repository skill discovery unavailable")
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
	policyToken, err := ws.OutboundToken()
	if err != nil {
		return nil, err
	}
	project, err := d.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil || project.ID != projectID || !project.Enabled {
		return nil, store.ErrNotFound
	}
	settings, err := ResolveSettings(project.Settings, d.onboarding.reviewDefaults)
	if err != nil {
		return nil, err
	}
	token := ""
	if settings.AllowRepoSkills {
		plaintext, err := d.cipher.Open(project.TokenEnc, secret.TokenAAD(projectID))
		if err != nil {
			return nil, errors.New("app: project token is unavailable")
		}
		token = string(plaintext)
	}
	result, err := discoverRepositorySkills(ctx, ws, d.onboarding.git, d.onboarding.gitlab, project, token, branch, d.onboarding.reviewDefaults, d.onboarding.profiles)
	if err != nil {
		return nil, err
	}
	if err := ws.SendOutbound(ctx, policyToken, func(ctx context.Context) error {
		return authorizeDispatch(ctx, projectID)
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// discoverRepositorySkills is a source and policy primitive, not an access
// check. The caller holds the project lock and must authorize the user both
// before and after discovery before returning any metadata.
func discoverRepositorySkills(ctx context.Context, ws *workspace.Workspace, git GitOps, client GitLabClient, project *store.Project, token, branch string, defaults config.ProjectSettings, profiles map[string]config.ProfileConfig) (*RepositorySkillDiscovery, error) {
	if ws == nil || project == nil || project.ID <= 0 || ws.ProjectID != project.ID || branch == "" || strings.TrimSpace(branch) != branch {
		return nil, errors.New("app: repository skill target identity unavailable")
	}
	if _, err := repo.NormalizeRef("refs/heads/" + branch); err != nil {
		return nil, errors.New("app: invalid repository skill target branch")
	}
	settings, err := ResolveSettings(project.Settings, defaults)
	if err != nil {
		return nil, errors.New("app: project settings cannot be read safely")
	}
	out := &RepositorySkillDiscovery{ProjectID: project.ID, TargetBranch: branch, Enabled: settings.AllowRepoSkills, Skills: []RepositorySkillMetadata{}}
	if !settings.AllowRepoSkills {
		return out, nil
	}
	source, ok := git.(repositorySkillSourceGit)
	if !ok || source == nil || client == nil {
		return nil, errors.New("app: repository skill source unavailable")
	}
	sha, files, snapshot, err := loadRepositorySkillSnapshot(ctx, ws, source, client, project, token, branch, nil, defaults, profiles)
	if err != nil {
		return nil, err
	}
	items, err := skill.RepositoryCatalogue(files)
	if err != nil {
		return nil, err
	}
	out.TargetSHA = sha
	out.SnapshotSHA256 = snapshot
	for _, item := range items {
		out.Skills = append(out.Skills, RepositorySkillMetadata{Source: "repository", Name: item.Name, Description: item.Description})
	}
	return out, nil
}

// pinRepositorySkillTarget authenticates the project's remote and pins the
// exact target branch before any skill tree or repo policy is read. The caller
// holds the project workspace lock throughout the operation and subsequent use.
func pinRepositorySkillTarget(ctx context.Context, ws *workspace.Workspace, git repositorySkillTargetGit, client GitLabClient, project *store.Project, token, branch string) (string, error) {
	if ws == nil || git == nil || client == nil || project == nil || project.ID <= 0 || ws.ProjectID != project.ID || branch == "" || strings.TrimSpace(branch) != branch {
		return "", errors.New("app: repository skill target identity unavailable")
	}
	ref, err := repo.NormalizeRef("refs/heads/" + branch)
	if err != nil {
		return "", errors.New("app: invalid repository skill target branch")
	}
	remote, err := currentProjectRemote(ctx, client, project, token)
	if err != nil {
		return "", err
	}
	if err := ensureReviewRoot(ws.Root); err != nil {
		return "", err
	}
	unlock, err := ws.LockMirror(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	auth := repo.Auth{Token: token}
	if err := git.EnsureMirror(ctx, ws.MirrorDir(), remote, auth); err != nil {
		return "", err
	}
	const targetRef = "refs/tribunal/repository-skills-target"
	if err := git.Fetch(ctx, ws.MirrorDir(), auth, "+"+ref+":"+targetRef); err != nil {
		return "", err
	}
	sha, err := git.ResolveCommitRef(ctx, ws.MirrorDir(), targetRef)
	if err != nil {
		return "", err
	}
	if repo.ValidateSHA(sha) != nil {
		return "", errors.New("app: repository skill target identity unavailable")
	}
	return strings.ToLower(sha), nil
}
