package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"go.yaml.in/yaml/v3"
)

// reviewInputs is the sanitized GitLab input for one review.
type reviewInputs struct {
	MR       *gitlab.MergeRequest
	Version  *gitlab.Version
	Diffs    []gitlab.FileDiff
	Pipeline *review.PipelineSummary
	Issues   []review.IssueRef
	RepoDocs review.RepoDocs
}

func (s *ReviewService) collectInputs(ctx context.Context, project *store.Project, token string, mrIID int64, head string) (*reviewInputs, error) {
	mr, err := s.gitlab.GetMergeRequest(ctx, token, project.ID, mrIID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return nil, PermanentJob(fmt.Errorf("%w: merge request not found", ErrMRClosed))
		}
		return nil, err
	}
	if mr == nil || !isOpenState(mr.State) {
		return nil, PermanentJob(fmt.Errorf("%w: state %s", ErrMRClosed, mrState(mr)))
	}
	if mr.TargetProjectID > 0 && mr.TargetProjectID != project.ID {
		return nil, PermanentJob(fmt.Errorf("%w: target project mismatch", ErrReviewSuperseded))
	}
	if !strings.EqualFold(strings.TrimSpace(mr.SHA), head) {
		return nil, PermanentJob(fmt.Errorf("%w: merge request head changed", ErrReviewSuperseded))
	}
	if strings.TrimSpace(mr.TargetBranch) == "" {
		return nil, PermanentJob(fmt.Errorf("merge request target branch is empty"))
	}

	version, err := s.gitlab.LatestMRVersion(ctx, token, project.ID, mrIID)
	if err != nil {
		return nil, err
	}
	if version == nil || !strings.EqualFold(strings.TrimSpace(version.HeadSHA), head) {
		return nil, PermanentJob(fmt.Errorf("%w: diff version head changed", ErrReviewSuperseded))
	}
	diffs, err := s.gitlab.ListMRDiffs(ctx, token, project.ID, mrIID)
	if err != nil {
		return nil, err
	}
	inputs := &reviewInputs{MR: mr, Version: version, Diffs: diffs}
	inputs.Pipeline = s.collectPipeline(ctx, token, project.ID, mrIID)
	inputs.Issues = s.collectIssues(ctx, token, project.ID, mrIID)
	docs, err := s.repoDocs(ctx, token, project.ID, mr.TargetBranch)
	if err != nil {
		return nil, err
	}
	inputs.RepoDocs = docs
	return inputs, nil
}

// repoDocs reads the target-branch documents used by review and publication.
// A missing file is an empty document; every other error is retryable so a
// transient upstream failure never changes the configuration fingerprint.
func (s *ReviewService) repoDocs(ctx context.Context, token string, projectID int64, ref string) (review.RepoDocs, error) {
	configDoc, err := s.rawDoc(ctx, token, projectID, ".gitlab-review.yml", ref)
	if err != nil {
		return review.RepoDocs{}, err
	}
	agents, err := s.rawDoc(ctx, token, projectID, "AGENTS.md", ref)
	if err != nil {
		return review.RepoDocs{}, err
	}
	claude, err := s.rawDoc(ctx, token, projectID, "CLAUDE.md", ref)
	if err != nil {
		return review.RepoDocs{}, err
	}
	return review.RepoDocs{GitLabReviewConfig: configDoc, AGENTS: agents, CLAUDE: claude}, nil
}

func mrState(mr *gitlab.MergeRequest) string {
	if mr == nil {
		return "missing"
	}
	return mr.State
}

func (s *ReviewService) collectPipeline(ctx context.Context, token string, projectID, mrIID int64) *review.PipelineSummary {
	pipelines, err := s.gitlab.ListMRPipelines(ctx, token, projectID, mrIID)
	if err != nil {
		s.logger.Warn("pipeline lookup failed", "project_id", projectID, "mr_iid", mrIID)
		return nil
	}
	var latest *gitlab.Pipeline
	for i := range pipelines {
		if latest == nil || pipelines[i].ID > latest.ID {
			latest = &pipelines[i]
		}
	}
	if latest == nil {
		return nil
	}
	summary := &review.PipelineSummary{
		ID:     latest.ID,
		Status: latest.Status,
		Ref:    latest.Ref,
		SHA:    latest.SHA,
		WebURL: latest.WebURL,
	}
	if latest.Status != "failed" {
		return summary
	}
	jobs, err := s.gitlab.ListPipelineJobs(ctx, token, projectID, latest.ID)
	if err != nil {
		s.logger.Warn("pipeline jobs lookup failed", "project_id", projectID, "pipeline_id", latest.ID)
		return summary
	}
	for _, job := range jobs {
		if job.Status != "failed" || job.AllowFailure {
			continue
		}
		summary.FailedJobs = append(summary.FailedJobs, review.JobSummary{
			Name:          job.Name,
			Stage:         job.Stage,
			Status:        job.Status,
			FailureReason: job.FailureReason,
			WebURL:        job.WebURL,
		})
	}
	return summary
}

func (s *ReviewService) collectIssues(ctx context.Context, token string, projectID, mrIID int64) []review.IssueRef {
	issues, err := s.gitlab.ListClosingIssues(ctx, token, projectID, mrIID)
	if err != nil {
		s.logger.Warn("closing issues lookup failed", "project_id", projectID, "mr_iid", mrIID)
		return nil
	}
	out := make([]review.IssueRef, 0, len(issues))
	for _, issue := range issues {
		out = append(out, review.IssueRef{
			IID:    issue.IID,
			Title:  issue.Title,
			State:  issue.State,
			WebURL: issue.WebURL,
		})
	}
	return out
}

func (s *ReviewService) rawDoc(ctx context.Context, token string, projectID int64, path, ref string) (string, error) {
	data, err := s.gitlab.GetRawFile(ctx, token, projectID, path, ref)
	if err != nil {
		if !gitlab.IsNotFound(err) {
			s.logger.Warn("repository document lookup failed", "project_id", projectID, "path", path)
			return "", err
		}
		return "", nil
	}
	return string(data), nil
}

func mrMeta(mr *gitlab.MergeRequest) review.MRMeta {
	out := review.MRMeta{
		IID:          mr.IID,
		Title:        mr.Title,
		Description:  mr.Description,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		Draft:        mr.Draft,
		Labels:       append([]string(nil), mr.Labels...),
		WebURL:       mr.WebURL,
	}
	if mr.Author != nil {
		out.Author = mr.Author.Username
	}
	return out
}

// effectiveReviewConfig is the merged platform, project, and repo view.
type effectiveReviewConfig struct {
	Settings config.ProjectSettings
	Profile  string
	RepoDocs review.RepoDocs
	Warnings []string
}

type repoReviewConfig struct {
	Version  int      `yaml:"version"`
	Language string   `yaml:"language"`
	Profile  string   `yaml:"profile"`
	Ignore   []string `yaml:"ignore"`
	Rules    []string `yaml:"rules"`
	Review   struct {
		SkipDraft     *bool    `yaml:"skip_draft"`
		LabelsExclude []string `yaml:"labels_exclude"`
	} `yaml:"review"`
}

func (s *ReviewService) resolveConfig(project *store.Project, docs review.RepoDocs) effectiveReviewConfig {
	return resolveReviewConfig(project, docs, s.reviewDefaults, s.profiles)
}

func resolveReviewConfig(project *store.Project, docs review.RepoDocs, defaults config.ProjectSettings, profiles map[string]config.ProfileConfig) effectiveReviewConfig {
	settings, err := ResolveSettings(project.Settings, defaults)
	out := effectiveReviewConfig{Settings: settings, RepoDocs: docs}
	if err != nil {
		out.Settings = cloneSettings(defaults)
		out.Warnings = append(out.Warnings, "stored project settings are invalid; platform defaults are used")
	}
	// Normalize once so the review service and publisher hash the same value.
	if out.Settings.MaxFiles <= 0 {
		out.Settings.MaxFiles = review.DefaultMaxFiles
	}
	out.Settings.Language = strings.Join(strings.Fields(out.Settings.Language), " ")
	if !out.Settings.AllowRepoConfig || strings.TrimSpace(docs.GitLabReviewConfig) == "" {
		return out
	}
	var repo repoReviewConfig
	if err := yaml.Unmarshal([]byte(docs.GitLabReviewConfig), &repo); err != nil {
		out.Warnings = append(out.Warnings, ".gitlab-review.yml is invalid and was ignored")
		return out
	}
	out.Warnings = append(out.Warnings, repoReviewConfigWarnings(docs.GitLabReviewConfig)...)
	if language := strings.Join(strings.Fields(repo.Language), " "); language != "" {
		if len([]rune(language)) > 128 {
			out.Warnings = append(out.Warnings, ".gitlab-review.yml: language is too long and was ignored")
		} else {
			out.Settings.Language = language
		}
	}
	if strings.TrimSpace(repo.Profile) != "" {
		if _, ok := profiles[repo.Profile]; ok {
			out.Profile = repo.Profile
		} else {
			out.Warnings = append(out.Warnings, "repository profile is not configured and was ignored")
		}
	}
	out.Settings.Ignore = appendUnique(out.Settings.Ignore, repo.Ignore)
	out.Settings.CustomRules = appendUnique(out.Settings.CustomRules, repo.Rules)
	if repo.Review.SkipDraft != nil {
		out.Settings.SkipDraft = *repo.Review.SkipDraft
	}
	labels := make([]string, 0, len(repo.Review.LabelsExclude))
	for _, label := range repo.Review.LabelsExclude {
		if strings.TrimSpace(label) == "" {
			out.Warnings = append(out.Warnings, ".gitlab-review.yml: empty review.labels_exclude entry ignored")
			continue
		}
		labels = append(labels, label)
	}
	out.Settings.LabelsExclude = appendUnique(out.Settings.LabelsExclude, labels)
	return out
}

// repoReviewConfigWarnings reports fields outside the repository whitelist.
// They stay unapplied; the warning is surfaced in the published summary.
func repoReviewConfigWarnings(raw string) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var warnings []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		switch key {
		case "version", "language", "profile", "ignore", "rules":
		case "review":
			review := root.Content[i+1]
			if review.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(review.Content); j += 2 {
				switch review.Content[j].Value {
				case "skip_draft", "labels_exclude":
				default:
					warnings = append(warnings, fmt.Sprintf(".gitlab-review.yml: unknown field review.%s ignored", review.Content[j].Value))
				}
			}
		default:
			warnings = append(warnings, fmt.Sprintf(".gitlab-review.yml: unknown field %s ignored", key))
		}
	}
	return warnings
}

// profileExplicit distinguishes an operator-selected profile from the
// automatic webhook default. Legacy queued command/manual payloads carry no
// explicit flag, so their trigger remains the only available evidence.
func profileExplicit(payload reviewRunPayload) bool {
	if payload.ProfileExplicit != nil {
		return *payload.ProfileExplicit
	}
	switch payload.Trigger {
	case "command", "manual":
		return true
	default:
		return false
	}
}

// reviewFilterReason applies draft, label, and branch filters to automatic
// triggers only. Manual commands keep their existing bypass semantics.
func reviewFilterReason(trigger string, mr *gitlab.MergeRequest, project *store.Project, settings config.ProjectSettings) string {
	switch trigger {
	case "mr_open", "mr_update":
	default:
		return ""
	}
	if mr == nil {
		return ""
	}
	if settings.SkipDraft && mergeRequestDraft(mr) {
		return "draft"
	}
	if !LabelsAllowed(mr.Labels, settings.LabelsInclude, settings.LabelsExclude) {
		return "labels_filtered"
	}
	if !BranchAllowed(mr.TargetBranch, project.DefaultBranch, settings.TargetBranches) {
		return "branch_filtered"
	}
	return ""
}

func mergeRequestDraft(mr *gitlab.MergeRequest) bool {
	if mr == nil {
		return false
	}
	if mr.Draft {
		return true
	}
	title := strings.ToLower(strings.TrimSpace(mr.Title))
	return strings.HasPrefix(title, "draft:") || strings.HasPrefix(title, "wip:")
}

func reviewConfigHash(cfg effectiveReviewConfig) string {
	payload, err := json.Marshal(struct {
		Settings config.ProjectSettings `json:"settings"`
		Profile  string                 `json:"profile"`
		RepoDocs review.RepoDocs        `json:"repo_docs"`
	}{cfg.Settings, cfg.Profile, cfg.RepoDocs})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// reviewFingerprint binds a workflow to the exact dossier, configuration, and
// prompt bundle used to produce it. Format:
// sha256("<dossier hash>\n<config hash>\n<prompt hash>").
func reviewFingerprint(dossierHash, configHash, promptHash string) string {
	return sha256Hex(dossierHash + "\n" + configHash + "\n" + promptHash)
}

func (s *ReviewService) profileFor(project *store.Project, requested, repoProfile string, labels []string, explicit bool) (string, config.ProfileConfig) {
	var candidates []string
	if explicit {
		candidates = []string{requested, repoProfile}
	} else {
		candidates = []string{repoProfile, requested}
	}
	candidates = append(candidates, ProfileForLabels(labels, project.Profile, s.reviewDefaults), "default")
	seen := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if cfg, ok := s.profiles[name]; ok {
			return name, cfg
		}
	}
	return "custom", config.ProfileConfig{}
}

// allowedProviders returns the provider allowlist used by graph validation.
func (s *ReviewService) allowedProviders() []string {
	return append([]string(nil), s.providerNames...)
}

func (s *ReviewService) contextDoc(ctx context.Context, projectID int64) string {
	doc, err := s.store.GetCurrentContextDoc(ctx, projectID, "agents")
	if err != nil {
		return ""
	}
	return doc.Content
}
