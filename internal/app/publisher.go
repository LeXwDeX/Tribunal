package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

const (
	summaryPublicationKey = "summary"
	summaryPublishKind    = "summary"
	discussionPublishKind = "discussion"
	statusPublishKind     = "commit_status"
	defaultPublishRetries = 3

	// publicationRetryReason is the fixed reason for a retryable publication
	// failure. Raw upstream text never reaches storage.
	publicationRetryReason = "retry pending"
	// publicationFailedReason is the generic terminal reason.
	publicationFailedReason = "publication is not possible"
)

// Publisher implements F7: fingerprint-checked, idempotent publication.
type Publisher struct {
	store          ReviewStore
	gitlab         GitLabClient
	cipher         *secret.Cipher
	engine         *dag.Engine
	reviewDefaults config.ProjectSettings
	profiles       map[string]config.ProfileConfig
	promptsHash    string
	publicURL      string
	logger         *slog.Logger
	now            func() time.Time
	retries        int
}

// PublisherConfig configures Publisher.
type PublisherConfig struct {
	Store          ReviewStore
	GitLab         GitLabClient
	Cipher         *secret.Cipher
	Engine         *dag.Engine
	ReviewDefaults config.ProjectSettings
	Profiles       map[string]config.ProfileConfig
	PromptsHash    string
	PublicURL      string
	Logger         *slog.Logger
	Now            func() time.Time
	Retries        int
}

// NewPublisher validates dependencies and builds the publication service.
func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil || cfg.Engine == nil {
		return nil, errors.New("app: publisher dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	retries := cfg.Retries
	if retries <= 0 {
		retries = defaultPublishRetries
	}
	return &Publisher{
		store:          cfg.Store,
		gitlab:         cfg.GitLab,
		cipher:         cfg.Cipher,
		engine:         cfg.Engine,
		reviewDefaults: cfg.ReviewDefaults,
		profiles:       cfg.Profiles,
		promptsHash:    cfg.PromptsHash,
		publicURL:      strings.TrimRight(cfg.PublicURL, "/"),
		logger:         logger,
		now:            now,
		retries:        retries,
	}, nil
}

type loadedReview struct {
	workflow *store.Workflow
	dossier  *review.Dossier
	record   *store.WorkflowDossier
	project  *store.Project
	token    string
	mrIID    int64
}

// Publish publishes one completed workflow idempotently. A stale fingerprint
// marks the workflow skipped and returns ErrStalePublication without retrying.
// Every other failure keeps publication state independent from the completed
// workflow: retryable failures stay pending, permanent failures become failed.
func (p *Publisher) Publish(ctx context.Context, workflowID string) error {
	err := p.publish(ctx, workflowID)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrStalePublication) {
		// stale() already stored the skipped state and reason.
		return err
	}
	if recordErr := p.recordPublicationFailure(ctx, workflowID, err); recordErr != nil {
		return errors.Join(err, recordErr)
	}
	return err
}

// recordPublicationFailure persists a bounded sanitized reason. The original
// error stays in logs only.
func (p *Publisher) recordPublicationFailure(ctx context.Context, workflowID string, cause error) error {
	state, reason := store.WorkflowPublicationPending, publicationRetryReason
	if IsPermanentJob(cause) {
		state = store.WorkflowPublicationFailed
		reason = publicationFailedReason
		if errors.Is(cause, ErrMRClosed) {
			reason = "merge request is not available"
		}
	}
	if err := p.store.SetWorkflowPublicationState(ctx, workflowID, state, reason); err != nil {
		p.logger.Warn("publication state update failed",
			"workflow_id", workflowID, "state", state, "error", err.Error())
		return err
	}
	p.logger.Warn("publication failed",
		"workflow_id", workflowID, "state", state, "reason", reason)
	return nil
}

func (p *Publisher) publish(ctx context.Context, workflowID string) error {
	loaded, err := p.load(ctx, workflowID)
	if err != nil {
		return err
	}
	if loaded.workflow.Status != string(dag.WorkflowCompleted) {
		return nil
	}
	configHash, hasConfig := loaded.dossier.Metadata["config_hash"]
	promptHash, hasPrompt := loaded.dossier.Metadata["prompt_hash"]
	// New workflows bind the dossier to the configuration and prompt hashes;
	// older workflows stored only the dossier hash.
	expectedFingerprint := loaded.record.Hash
	if hasConfig && configHash != "" && hasPrompt && promptHash != "" {
		expectedFingerprint = reviewFingerprint(loaded.record.Hash, configHash, promptHash)
	}
	if loaded.workflow.Fingerprint != "" && loaded.workflow.Fingerprint != expectedFingerprint {
		return p.stale(ctx, workflowID, "workflow fingerprint changed")
	}
	if _, err := p.controlMatches(ctx, loaded); err != nil {
		return err
	}
	cfg, err := p.freshConfig(ctx, loaded)
	if err != nil {
		return err
	}
	if hasConfig && configHash != "" && configHash != reviewConfigHash(cfg) {
		return p.stale(ctx, workflowID, "configuration changed")
	}
	if hasPrompt && promptHash != "" && p.promptsHash != "" && promptHash != p.promptsHash {
		return p.stale(ctx, workflowID, "prompt bundle changed")
	}

	version, err := p.gitlab.LatestMRVersion(ctx, loaded.token, loaded.project.ID, loaded.mrIID)
	if err != nil {
		return err
	}
	if version == nil || !strings.EqualFold(version.HeadSHA, loaded.dossier.DiffVersion.HeadSHA) {
		return p.stale(ctx, workflowID, "merge request head changed")
	}
	if loaded.dossier.DiffVersion.BaseSHA != "" && !strings.EqualFold(version.BaseSHA, loaded.dossier.DiffVersion.BaseSHA) {
		return p.stale(ctx, workflowID, "merge request base changed")
	}
	diffs, err := p.gitlab.ListMRDiffs(ctx, loaded.token, loaded.project.ID, loaded.mrIID)
	if err != nil {
		return err
	}
	index := gitlab.BuildDiffIndex(*version, diffs)

	snapshot, err := p.engine.Snapshot(ctx, workflowID)
	if err != nil {
		return err
	}
	result, err := synthesisResult(snapshot)
	if err != nil {
		return PermanentJob(err)
	}
	groups := markPositions(result.Groups, index)
	if err := p.store.ReplaceFindings(ctx, workflowID, findingsForStore(workflowID, groups)); err != nil {
		return err
	}
	if err := p.store.SetWorkflowVerdict(ctx, workflowID, result.Verdict); err != nil {
		return err
	}

	render := func() (string, error) {
		return review.RenderSummary(review.SummaryInput{
			Verdict:              result.Verdict,
			Summary:              result.Summary,
			Groups:               groups,
			Fingerprint:          loaded.record.Hash,
			HeadSHA:              loaded.dossier.DiffVersion.HeadSHA,
			Profile:              loaded.workflow.Profile,
			Duration:             workflowDuration(loaded.workflow),
			DetailURL:            p.detailURL(workflowID),
			ShowModelAttribution: cfg.Settings.ShowModelAttribution,
			Warnings:             append(append([]string(nil), result.Warnings...), cfg.Warnings...),
		})
	}
	body, err := render()
	if err != nil {
		return PermanentJob(fmt.Errorf("render summary: %w", err))
	}
	summaryID, err := p.upsertSummary(ctx, loaded.project.ID, loaded.mrIID, loaded.token, loaded.record.Hash, body, workflowID)
	if err != nil {
		return err
	}

	fallback := false
	for i := range groups {
		group := groups[i]
		if !group.Published() || !group.PositionOK {
			continue
		}
		key := group.PublishKey
		if key == "" {
			key = review.PublishKeyFor(group.Path, group.Title, group.Evidence)
		}
		existing, err := p.store.GetPublication(ctx, loaded.project.ID, loaded.mrIID, discussionPublishKind, key)
		if err == nil && existing.GitlabID != "" {
			continue
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		findingBody, err := review.RenderFinding(group, review.FindingRenderOptions{ShowModelAttribution: cfg.Settings.ShowModelAttribution})
		if err != nil {
			groups[i].PositionOK = false
			fallback = true
			continue
		}
		position, ok := index.Locate(group.Path, group.Line, diffSide(group.Side))
		if !ok {
			groups[i].PositionOK = false
			fallback = true
			continue
		}
		discussion, err := p.createDiscussion(ctx, loaded.token, loaded.project.ID, loaded.mrIID, findingBody, position)
		if err != nil {
			if gitlab.StatusOf(err) == 400 {
				groups[i].PositionOK = false
				fallback = true
				continue
			}
			return err
		}
		if discussion == nil || discussion.ID == "" {
			return errors.New("app: GitLab returned an empty discussion")
		}
		if _, err := p.store.UpsertPublication(ctx, store.Publication{
			ProjectID:   loaded.project.ID,
			MRIID:       loaded.mrIID,
			Kind:        discussionPublishKind,
			PublishKey:  key,
			GitlabID:    discussion.ID,
			WorkflowID:  workflowID,
			Fingerprint: loaded.record.Hash,
			BodyHash:    bodyHash(findingBody),
		}); err != nil {
			return err
		}
	}
	if fallback {
		if err := p.store.ReplaceFindings(ctx, workflowID, findingsForStore(workflowID, groups)); err != nil {
			return err
		}
		updated, err := render()
		if err == nil && updated != body {
			if _, err := p.updateSummary(ctx, loaded.token, loaded.project.ID, loaded.mrIID, summaryID, updated, workflowID, loaded.record.Hash); err != nil {
				return err
			}
		}
	}

	if cfg.Settings.CommitStatus {
		// Only an explicit PASS may report success. BLOCKING and any future
		// non-PASS verdict must never green-light the commit status.
		state := "failed"
		if result.Verdict == review.VerdictPass {
			state = "success"
		}
		status := gitlab.CommitStatus{
			State:       state,
			Name:        "ai-review",
			TargetURL:   p.detailURL(workflowID),
			Description: statusDescription(result.Verdict, groups),
		}
		if err := p.setCommitStatus(ctx, loaded.token, loaded.project.ID, loaded.dossier.DiffVersion.HeadSHA, status); err != nil {
			return err
		}
		if _, err := p.store.UpsertPublication(ctx, store.Publication{
			ProjectID:   loaded.project.ID,
			MRIID:       loaded.mrIID,
			Kind:        statusPublishKind,
			PublishKey:  commitStatusPublishKey(loaded.dossier.DiffVersion.HeadSHA, state),
			GitlabID:    status.Name,
			WorkflowID:  workflowID,
			Fingerprint: loaded.record.Hash,
		}); err != nil {
			return err
		}
	}
	return p.store.SetWorkflowPublicationState(ctx, workflowID, store.WorkflowPublicationPublished, "")
}

// PublishFailure publishes the incomplete-review summary. It never writes a
// commit status or findings. Publication state follows the same retry rules as
// a normal publication.
func (p *Publisher) PublishFailure(ctx context.Context, workflowID, reason string) error {
	err := p.publishFailure(ctx, workflowID, reason)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrStalePublication) {
		return err
	}
	if recordErr := p.recordPublicationFailure(ctx, workflowID, err); recordErr != nil {
		return errors.Join(err, recordErr)
	}
	return err
}

func (p *Publisher) publishFailure(ctx context.Context, workflowID, reason string) error {
	loaded, err := p.load(ctx, workflowID)
	if err != nil {
		return err
	}
	if loaded.workflow.Status != string(dag.WorkflowFailed) {
		return PermanentJob(fmt.Errorf("workflow %s did not fail", workflowID))
	}
	configHash, hasConfig := loaded.dossier.Metadata["config_hash"]
	promptHash, hasPrompt := loaded.dossier.Metadata["prompt_hash"]
	expectedFingerprint := loaded.record.Hash
	if hasConfig && configHash != "" && hasPrompt && promptHash != "" {
		expectedFingerprint = reviewFingerprint(loaded.record.Hash, configHash, promptHash)
	}
	if loaded.workflow.Fingerprint != "" && loaded.workflow.Fingerprint != expectedFingerprint {
		return p.stale(ctx, workflowID, "workflow fingerprint changed")
	}
	if _, err := p.controlMatches(ctx, loaded); err != nil {
		return err
	}
	cfg, err := p.freshConfig(ctx, loaded)
	if err != nil {
		return err
	}
	if hasConfig && configHash != "" && configHash != reviewConfigHash(cfg) {
		return p.stale(ctx, workflowID, "configuration changed")
	}
	if hasPrompt && promptHash != "" && p.promptsHash != "" && promptHash != p.promptsHash {
		return p.stale(ctx, workflowID, "prompt bundle changed")
	}
	version, err := p.gitlab.LatestMRVersion(ctx, loaded.token, loaded.project.ID, loaded.mrIID)
	if err != nil {
		return err
	}
	if version == nil || !strings.EqualFold(version.HeadSHA, loaded.dossier.DiffVersion.HeadSHA) {
		return p.stale(ctx, workflowID, "merge request head changed")
	}
	if loaded.dossier.DiffVersion.BaseSHA != "" && !strings.EqualFold(version.BaseSHA, loaded.dossier.DiffVersion.BaseSHA) {
		return p.stale(ctx, workflowID, "merge request base changed")
	}
	body, err := review.RenderSummary(review.SummaryInput{
		Verdict:          review.VerdictIncomplete,
		Fingerprint:      loaded.record.Hash,
		HeadSHA:          loaded.dossier.DiffVersion.HeadSHA,
		Profile:          loaded.workflow.Profile,
		Duration:         workflowDuration(loaded.workflow),
		DetailURL:        p.detailURL(workflowID),
		IncompleteReason: reason,
	})
	if err != nil {
		return PermanentJob(fmt.Errorf("render failure summary: %w", err))
	}
	if _, err := p.upsertSummary(ctx, loaded.project.ID, loaded.mrIID, loaded.token, loaded.record.Hash, body, workflowID); err != nil {
		return err
	}
	return p.store.SetWorkflowPublicationState(ctx, workflowID, store.WorkflowPublicationPublished, "")
}

// Republish re-runs publication for a terminal workflow, including a manual
// request after a previous failure. Failed executions publish the failure
// summary, never a normal review result. The stale and generation fences stay
// inside Publish and PublishFailure.
func (p *Publisher) Republish(ctx context.Context, workflowID string) error {
	loaded, err := p.load(ctx, workflowID)
	if err != nil {
		return err
	}
	switch loaded.workflow.Status {
	case string(dag.WorkflowCompleted):
	case string(dag.WorkflowFailed):
	default:
		return PermanentJob(fmt.Errorf("workflow %s is not publishable", workflowID))
	}
	if err := p.store.SetWorkflowPublicationState(ctx, workflowID, store.WorkflowPublicationPending, ""); err != nil {
		return err
	}
	if loaded.workflow.Status == string(dag.WorkflowFailed) {
		return p.PublishFailure(ctx, workflowID, reviewExecutionFailedReason)
	}
	return p.Publish(ctx, workflowID)
}

// PublishStandaloneSummary upserts a summary for a review with no workflow
// (for example, all files were ignored). The marker scan keeps it idempotent.
func (p *Publisher) PublishStandaloneSummary(ctx context.Context, projectID, mrIID int64, token, fingerprint, body string) error {
	_, err := p.upsertSummary(ctx, projectID, mrIID, token, fingerprint, body, "")
	return err
}

func (p *Publisher) load(ctx context.Context, workflowID string) (*loadedReview, error) {
	workflow, err := p.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, PermanentJob(fmt.Errorf("workflow %s is unknown", workflowID))
		}
		return nil, err
	}
	if workflow.Kind != "review" {
		return nil, PermanentJob(fmt.Errorf("workflow %s is not a review", workflowID))
	}
	if workflow.MRIID == nil || *workflow.MRIID <= 0 {
		return nil, PermanentJob(fmt.Errorf("workflow %s has no merge request", workflowID))
	}
	record, err := p.store.GetWorkflowDossier(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return nil, PermanentJob(fmt.Errorf("decode dossier: %w", err))
	}
	if err := review.ValidateDossier(&dossier); err != nil {
		return nil, PermanentJob(err)
	}
	if record.Hash == "" || dossier.MR.IID != *workflow.MRIID {
		return nil, PermanentJob(fmt.Errorf("workflow %s dossier does not match", workflowID))
	}
	project, err := p.store.GetProject(ctx, workflow.ProjectID)
	if err != nil {
		return nil, err
	}
	if project == nil || !project.Enabled {
		return nil, PermanentJob(fmt.Errorf("project %d is disabled", workflow.ProjectID))
	}
	token, err := p.cipher.Decrypt(project.TokenEnc)
	if err != nil {
		return nil, PermanentJob(fmt.Errorf("project token is unavailable"))
	}
	return &loadedReview{
		workflow: workflow,
		dossier:  &dossier,
		record:   record,
		project:  project,
		token:    string(token),
		mrIID:    *workflow.MRIID,
	}, nil
}

func (p *Publisher) controlMatches(ctx context.Context, loaded *loadedReview) (*store.ReviewControl, error) {
	control, err := p.store.GetReviewControl(ctx, loaded.project.ID, loaded.mrIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, p.stale(ctx, loaded.workflow.ID, "review control is missing")
		}
		return nil, err
	}
	if control.Cancelled || control.Generation != loaded.record.Generation {
		return nil, p.stale(ctx, loaded.workflow.ID, "review generation changed")
	}
	if control.HeadSHA != "" && !strings.EqualFold(control.HeadSHA, loaded.dossier.DiffVersion.HeadSHA) {
		return nil, p.stale(ctx, loaded.workflow.ID, "review head changed")
	}
	if loaded.workflow.HeadSHA != "" && !strings.EqualFold(loaded.workflow.HeadSHA, loaded.dossier.DiffVersion.HeadSHA) {
		return nil, p.stale(ctx, loaded.workflow.ID, "workflow head changed")
	}
	return control, nil
}

func (p *Publisher) freshConfig(ctx context.Context, loaded *loadedReview) (effectiveReviewConfig, error) {
	mr, err := p.gitlab.GetMergeRequest(ctx, loaded.token, loaded.project.ID, loaded.mrIID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return effectiveReviewConfig{}, PermanentJob(fmt.Errorf("%w: merge request not found", ErrMRClosed))
		}
		return effectiveReviewConfig{}, err
	}
	if mr == nil || !isOpenState(mr.State) {
		return effectiveReviewConfig{}, p.stale(ctx, loaded.workflow.ID, "merge request is not open")
	}
	if !strings.EqualFold(mr.SHA, loaded.dossier.DiffVersion.HeadSHA) {
		return effectiveReviewConfig{}, p.stale(ctx, loaded.workflow.ID, "merge request head changed")
	}
	if loaded.dossier.MR.TargetBranch != "" && mr.TargetBranch != loaded.dossier.MR.TargetBranch {
		return effectiveReviewConfig{}, p.stale(ctx, loaded.workflow.ID, "target branch changed")
	}
	configDoc, err := p.rawDoc(ctx, loaded.token, loaded.project.ID, ".gitlab-review.yml", mr.TargetBranch)
	if err != nil {
		return effectiveReviewConfig{}, err
	}
	agents, err := p.rawDoc(ctx, loaded.token, loaded.project.ID, "AGENTS.md", mr.TargetBranch)
	if err != nil {
		return effectiveReviewConfig{}, err
	}
	claude, err := p.rawDoc(ctx, loaded.token, loaded.project.ID, "CLAUDE.md", mr.TargetBranch)
	if err != nil {
		return effectiveReviewConfig{}, err
	}
	docs := review.RepoDocs{GitLabReviewConfig: configDoc, AGENTS: agents, CLAUDE: claude}
	return resolveReviewConfig(loaded.project, docs, p.reviewDefaults, p.profiles), nil
}

func (p *Publisher) rawDoc(ctx context.Context, token string, projectID int64, path, ref string) (string, error) {
	data, err := p.gitlab.GetRawFile(ctx, token, projectID, path, ref)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

func (p *Publisher) stale(ctx context.Context, workflowID, reason string) error {
	if err := p.store.SetWorkflowPublicationStateIfPending(ctx, workflowID, "stale: "+reason); err != nil {
		return err
	}
	p.logger.Info("publication skipped as stale", "workflow_id", workflowID, "reason", reason)
	return PermanentJob(fmt.Errorf("%w: %s", ErrStalePublication, reason))
}

// upsertSummary finds or creates the summary note. The marker scan recovers a
// note whose publication row was lost in a crash.
func (p *Publisher) upsertSummary(ctx context.Context, projectID, mrIID int64, token, fingerprint, body, workflowID string) (int64, error) {
	if workflowID != "" {
		existing, err := p.store.GetPublication(ctx, projectID, mrIID, summaryPublishKind, summaryPublicationKey)
		switch {
		case err == nil:
			if id, parseErr := strconv.ParseInt(existing.GitlabID, 10, 64); parseErr == nil && id > 0 {
				if err := p.updateSummaryNote(ctx, token, projectID, mrIID, id, body); err != nil {
					return 0, err
				}
				if _, err := p.store.UpsertPublication(ctx, store.Publication{
					ProjectID: projectID, MRIID: mrIID, Kind: summaryPublishKind,
					PublishKey: summaryPublicationKey, GitlabID: existing.GitlabID,
					WorkflowID: workflowID, Fingerprint: fingerprint, BodyHash: bodyHash(body),
				}); err != nil {
					return 0, err
				}
				return id, nil
			}
		case !errors.Is(err, store.ErrNotFound):
			return 0, err
		}
	}
	notes, err := p.gitlab.ListMRNotes(ctx, token, projectID, mrIID)
	if err != nil {
		return 0, err
	}
	authorID := p.botID(ctx, token)
	for _, note := range notes {
		if !strings.HasPrefix(note.Body, review.SummaryMarkerPrefix) {
			continue
		}
		if authorID != 0 && note.AuthorID != authorID {
			continue
		}
		if err := p.updateSummaryNote(ctx, token, projectID, mrIID, note.ID, body); err != nil {
			return 0, err
		}
		if workflowID != "" {
			if _, err := p.store.UpsertPublication(ctx, store.Publication{
				ProjectID: projectID, MRIID: mrIID, Kind: summaryPublishKind,
				PublishKey: summaryPublicationKey, GitlabID: strconv.FormatInt(note.ID, 10),
				WorkflowID: workflowID, Fingerprint: fingerprint, BodyHash: bodyHash(body),
			}); err != nil {
				return 0, err
			}
		}
		return note.ID, nil
	}
	created, err := p.createNote(ctx, token, projectID, mrIID, body)
	if err != nil {
		return 0, err
	}
	if workflowID != "" {
		if _, err := p.store.UpsertPublication(ctx, store.Publication{
			ProjectID: projectID, MRIID: mrIID, Kind: summaryPublishKind,
			PublishKey: summaryPublicationKey, GitlabID: strconv.FormatInt(created.ID, 10),
			WorkflowID: workflowID, Fingerprint: fingerprint, BodyHash: bodyHash(body),
		}); err != nil {
			return 0, err
		}
	}
	return created.ID, nil
}

func (p *Publisher) updateSummary(ctx context.Context, token string, projectID, mrIID, noteID int64, body, workflowID, fingerprint string) (int64, error) {
	if err := p.updateSummaryNote(ctx, token, projectID, mrIID, noteID, body); err != nil {
		return 0, err
	}
	if _, err := p.store.UpsertPublication(ctx, store.Publication{
		ProjectID: projectID, MRIID: mrIID, Kind: summaryPublishKind,
		PublishKey: summaryPublicationKey, GitlabID: strconv.FormatInt(noteID, 10),
		WorkflowID: workflowID, Fingerprint: fingerprint, BodyHash: bodyHash(body),
	}); err != nil {
		return 0, err
	}
	return noteID, nil
}

func (p *Publisher) updateSummaryNote(ctx context.Context, token string, projectID, mrIID, noteID int64, body string) error {
	return p.retry(ctx, func() error {
		return p.gitlab.UpdateMRNote(ctx, token, projectID, mrIID, noteID, body)
	})
}

func (p *Publisher) createNote(ctx context.Context, token string, projectID, mrIID int64, body string) (*gitlab.Note, error) {
	var note *gitlab.Note
	err := p.retry(ctx, func() error {
		created, err := p.gitlab.CreateMRNote(ctx, token, projectID, mrIID, body)
		if err != nil {
			return err
		}
		note = created
		return nil
	})
	return note, err
}

func (p *Publisher) createDiscussion(ctx context.Context, token string, projectID, mrIID int64, body string, position gitlab.Position) (*gitlab.Discussion, error) {
	var discussion *gitlab.Discussion
	err := p.retry(ctx, func() error {
		created, err := p.gitlab.CreateDiscussion(ctx, token, projectID, mrIID, gitlab.NewDiscussion{Body: body, Position: &position})
		if err != nil {
			return err
		}
		discussion = created
		return nil
	})
	return discussion, err
}

// commitStatusPublishKey records the effective state with the SHA. GitLab
// stores each status as a separate object, so the state belongs to the
// idempotency key. internal/server parses this format for the admin detail.
func commitStatusPublishKey(sha, state string) string {
	return "status:" + sha + ":" + state
}

func (p *Publisher) setCommitStatus(ctx context.Context, token string, projectID int64, sha string, status gitlab.CommitStatus) error {
	return p.retry(ctx, func() error {
		return p.gitlab.SetCommitStatus(ctx, token, projectID, sha, status)
	})
}

func (p *Publisher) retry(ctx context.Context, fn func() error) error {
	var last error
	for attempt := 0; attempt < p.retries; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		last = err
		if gitlab.StatusOf(err) == 400 || gitlab.StatusOf(err) == 404 ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		delay := time.Duration(attempt+1) * 250 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return last
}

func (p *Publisher) botID(ctx context.Context, token string) int64 {
	user, err := p.gitlab.CurrentUser(ctx, token)
	if err != nil || user == nil {
		return 0
	}
	return user.ID
}

func (p *Publisher) detailURL(workflowID string) string {
	if p.publicURL == "" || workflowID == "" {
		return ""
	}
	return p.publicURL + "/reviews/" + workflowID
}

func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func diffSide(side review.Side) gitlab.Side {
	if side == review.SideOld {
		return gitlab.SideOld
	}
	return gitlab.SideNew
}

func workflowDuration(workflow *store.Workflow) time.Duration {
	if workflow == nil || workflow.StartedAt == nil || workflow.FinishedAt == nil {
		return 0
	}
	return workflow.FinishedAt.Sub(*workflow.StartedAt)
}

func statusDescription(verdict string, groups []review.Group) string {
	counts := map[review.Severity]int{}
	for _, group := range groups {
		if group.Published() {
			counts[group.EffectiveSeverity()]++
		}
	}
	value := fmt.Sprintf("AI review: %s (P0 %d, P1 %d, P2 %d)", verdict, counts[review.SeverityP0], counts[review.SeverityP1], counts[review.SeverityP2])
	return truncateRunes(value, 140)
}

// markPositions fills PositionOK and the stable publish key for each group.
func markPositions(groups []review.Group, index *gitlab.DiffIndex) []review.Group {
	out := make([]review.Group, len(groups))
	copy(out, groups)
	for i := range out {
		group := &out[i]
		group.PositionOK = false
		if group.Line > 0 && group.Path != "" && index != nil {
			_, ok := index.Locate(group.Path, group.Line, diffSide(group.Side))
			group.PositionOK = ok
		}
		if group.PublishKey == "" && group.Path != "" && group.Title != "" {
			group.PublishKey = review.PublishKeyFor(group.Path, group.Title, group.Evidence)
		}
	}
	return out
}

// synthesisResult decodes the last completed synthesize node output.
func synthesisResult(snapshot dag.WorkflowView) (review.SynthesisResult, error) {
	var selected *dag.NodeView
	for i := len(snapshot.Order) - 1; i >= 0; i-- {
		node := snapshot.Nodes[snapshot.Order[i]]
		if node.Spec.Kind != review.KindSynthesize || node.Status != dag.NodeCompleted {
			continue
		}
		copy := node
		selected = &copy
		break
	}
	if selected == nil {
		return review.SynthesisResult{}, errors.New("app: workflow has no completed synthesize node")
	}
	var result review.SynthesisResult
	if err := json.Unmarshal(selected.Output, &result); err != nil {
		return review.SynthesisResult{}, fmt.Errorf("app: decode synthesis result: %w", err)
	}
	if err := result.Validate(); err != nil {
		return review.SynthesisResult{}, fmt.Errorf("app: invalid synthesis result: %w", err)
	}
	return result, nil
}

func findingsForStore(workflowID string, groups []review.Group) []store.Finding {
	out := make([]store.Finding, 0, len(groups))
	for _, group := range groups {
		origin := group.Origin
		if len(group.Sources) > 0 {
			origin = group.Sources[0].Origin
		}
		finding := store.Finding{
			ID:             group.ID,
			WorkflowID:     workflowID,
			OriginNode:     origin.Node,
			OriginIndex:    origin.Index,
			OriginProvider: origin.Provider,
			Severity:       string(group.EffectiveSeverity()),
			Category:       group.Category,
			Path:           group.Path,
			Side:           string(group.Side),
			Title:          group.Title,
			Body:           group.Body,
			Evidence:       group.Evidence,
			Confidence:     string(group.Confidence),
			DedupeGroup:    group.ID,
			Votes:          group.Votes,
			PublishKey:     group.PublishKey,
		}
		if origin.Node == "" {
			finding.OriginNode = review.KindSynthesize
		}
		if finding.Category == "" {
			finding.Category = "logic"
		}
		if finding.Votes == 0 {
			finding.Votes = 1
		}
		if group.Line > 0 {
			line := group.Line
			finding.Line = &line
		}
		positionOK := group.PositionOK
		finding.PositionOK = &positionOK
		if group.Decision != nil {
			finding.Decision = string(group.Decision.Action)
			finding.DecisionReason = group.Decision.Reason
			if group.Decision.Severity.Valid() {
				finding.FinalSeverity = string(group.Decision.Severity)
			}
		}
		if group.Challenge != nil {
			finding.ChallengeStatus = string(group.Challenge.Status)
			finding.ChallengeNode = group.Challenge.Node
			if encoded, err := json.Marshal(group.Challenge); err == nil {
				finding.ChallengeEvidence = encoded
			}
		}
		out = append(out, finding)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
