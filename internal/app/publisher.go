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
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

const (
	summaryPublicationKey = "summary"
	summaryPublishKind    = "summary"
	discussionPublishKind = "discussion"
	statusPublishKind     = "commit_status"
	defaultPublishRetries = 3

	// publicationRetryReason is the fixed reason for a retryable publication
	// failure. Raw upstream text never reaches storage.
	publicationRetryReason   = "retry pending"
	publicationUnknownReason = store.WorkflowPublicationUnknownReason
	// publicationFailedReason is the generic terminal reason.
	publicationFailedReason = "publication is not possible"
)

var (
	errDiscussionUnknown = errors.New("app: discussion outcome unknown")
	// errDiscussionAbsent is proof that a successful marker scan found no
	// matching discussion. Unlike an unknown outcome it is safe to clear the
	// intent fence (#457).
	errDiscussionAbsent = errors.New("app: discussion marker is absent")
)

// permanentDiscussionStatus lists GitLab responses that definitively rejected
// the create request. The intent fence can be cleared for these; every other
// outcome stays unknown because the note may exist remotely.
func permanentDiscussionStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// Publisher implements F7: fingerprint-checked, idempotent publication.
type Publisher struct {
	store          ReviewStore
	workspace      *workspace.Manager
	gitlab         GitLabClient
	git            GitOps
	cipher         *secret.Cipher
	tokens         *projectTokenResolver
	engine         *dag.Engine
	reviewDefaults config.ProjectSettings
	profiles       map[string]config.ProfileConfig
	catalog        *llm.Catalog
	promptsHash    string
	publicURL      string
	logger         *slog.Logger
	now            func() time.Time
	retries        int
	// locks serializes publication of one workflow within this process so
	// startup recovery and the worker cannot both create the same discussion.
	locks workflowLocks
	// mrLocks serializes publication and status writes for one merge request
	// across workflow generations. The runtime injects one registry shared with
	// the status writer and every generation-bump path so a bump cannot
	// interleave with an in-flight note write (#475). Lock order is workspace
	// lock (content), MR lock, workflow lock. Webhook ingress deliberately does
	// not take this lock: it only records the delivery and generation bump, and
	// its 900ms processing budget must not wait for remote note writes.
	mrLocks *workflowLocks
}

// workflowLocks is a keyed mutex whose entries live only while held or awaited.
// It guards per-workflow publication locks and the shared per-merge-request
// registry used by the publisher, the review service, the dispatcher, the
// command worker and catch-up. Webhook ingress does not take this lock: it only
// records the delivery and enqueue in one DB transaction to keep its 900ms
// processing budget (#475).
type workflowLocks struct {
	mu      sync.Mutex
	entries map[string]*workflowLock
}

type workflowLock struct {
	ch   chan struct{}
	refs int
}

// lock waits for the workflow's lock or ctx. The returned func releases it.
func (l *workflowLocks) lock(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*workflowLock)
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &workflowLock{ch: make(chan struct{}, 1)}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()
	drop := func() {
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
	select {
	case entry.ch <- struct{}{}:
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
	return func() {
		<-entry.ch
		drop()
	}, nil
}

// reviewMRLockKey identifies the process-wide per-merge-request lock. Every
// remote note writer (publisher, status worker, command replies and fallback
// notes) and every generation-bump path outside webhook ingress uses the same
// key, so a stale write cannot slip between a recheck and its remote call.
func reviewMRLockKey(projectID, mrIID int64) string {
	return itoa(projectID) + ":" + itoa(mrIID)
}

// defaultMRLocks backs a service built without the runtime registry (tests and
// standalone helpers). Production injects one registry through the
// constructors so publication, status writing and every generation bump share
// it.
var defaultMRLocks workflowLocks

// mrLockRegistry returns the injected per-merge-request registry, or the
// package default so bare service constructions still serialize with each
// other. This differs from NewPublisher, which creates a private registry for a
// nil config: production always injects the runtime registry through every
// constructor, so the two nil branches only exist for tests and standalone
// helpers. Do not unify them — fixture publishers rely on a private registry
// for isolation. A bare construction that mixes the branches silently stops
// sharing the lock.
func mrLockRegistry(locks *workflowLocks) *workflowLocks {
	if locks == nil {
		return &defaultMRLocks
	}
	return locks
}

// PublisherConfig configures Publisher.
type PublisherConfig struct {
	Store     ReviewStore
	Workspace *workspace.Manager
	GitLab    GitLabClient
	Git       GitOps
	Cipher    *secret.Cipher
	// SharedBotToken enables shared-credential fallback for projects without
	// their own token.
	SharedBotToken string
	Engine         *dag.Engine
	ReviewDefaults config.ProjectSettings
	Profiles       map[string]config.ProfileConfig
	PromptsHash    string
	PublicURL      string
	Logger         *slog.Logger
	Now            func() time.Time
	Retries        int
	// MRLocks is the shared per-merge-request lock registry. A nil value
	// gives this publisher a private registry (tests and standalone builds).
	MRLocks *workflowLocks
}

// NewPublisher validates dependencies and builds the publication service.
func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if cfg.Store == nil || cfg.Workspace == nil || cfg.GitLab == nil || cfg.Cipher == nil || cfg.Engine == nil {
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
	mrLocks := cfg.MRLocks
	if mrLocks == nil {
		// A standalone publisher gets a private registry so test fixtures stay
		// isolated; other services fall back to defaultMRLocks instead. The
		// runtime always injects its shared registry, which every other service
		// receives too, so a bare construction that omits it can silently stop
		// sharing the lock with those services.
		mrLocks = &workflowLocks{}
	}
	return &Publisher{
		store:          cfg.Store,
		workspace:      cfg.Workspace,
		gitlab:         cfg.GitLab,
		git:            cfg.Git,
		cipher:         cfg.Cipher,
		tokens:         newProjectTokenResolver(cfg.Cipher, cfg.SharedBotToken),
		engine:         cfg.Engine,
		reviewDefaults: cfg.ReviewDefaults,
		profiles:       cfg.Profiles,
		promptsHash:    cfg.PromptsHash,
		publicURL:      strings.TrimRight(cfg.PublicURL, "/"),
		logger:         logger,
		now:            now,
		retries:        retries,
		mrLocks:        mrLocks,
	}, nil
}

func (p *Publisher) providerLabels() map[string]string {
	if p == nil || p.catalog == nil {
		return nil
	}
	return p.catalog.Labels()
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
	ctx, unlock, err := p.lockWorkflow(ctx, workflowID)
	if err != nil {
		return err
	}
	defer unlock()
	return p.publishLocked(ctx, workflowID)
}

// publishLocked requires the project workspace read lock.
func (p *Publisher) publishLocked(ctx context.Context, workflowID string) error {
	mrUnlock, err := p.lockReviewMR(ctx, workflowID)
	if err != nil {
		return err
	}
	defer mrUnlock()
	unlock, err := p.locks.lock(ctx, workflowID)
	if err != nil {
		return err
	}
	defer unlock()
	if rejected, unknown, err := p.rejectRepositorySkillPublicationLocked(ctx, p.store, workflowID); rejected || err != nil {
		return repositorySkillPublicationError(unknown, err)
	}
	err = p.publish(ctx, workflowID)
	if err == nil {
		return nil
	}
	// A revoked outbound lease stays the visible cause of a cancelled send.
	// Do not replace it with a later provenance-policy rejection.
	if errors.Is(err, workspace.ErrOutboundInvalidated) {
		return err
	}
	if errors.Is(err, ErrStalePublication) || errors.Is(err, errDiscussionUnknown) {
		if rejected, unknown, settleErr := p.rejectRepositorySkillPublicationLocked(ctx, p.store, workflowID); rejected {
			return repositorySkillPublicationError(unknown, settleErr)
		} else if settleErr != nil {
			return errors.Join(err, settleErr)
		}
	}
	err = p.preserveUnknown(ctx, workflowID, err)
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
	if errors.Is(cause, errDiscussionUnknown) {
		reason = publicationUnknownReason
	} else if IsPermanentJob(cause) {
		state = store.WorkflowPublicationFailed
		reason = publicationFailedReason
		if errors.Is(cause, ErrMRClosed) {
			reason = "merge request is not available"
		}
	}
	if err := p.store.SetWorkflowPublicationState(ctx, workflowID, state, reason); err != nil {
		p.logger.Warn("publication state update failed",
			"workflow_id", workflowID, "state", state, "error_class", errclass.Of(err))
		return err
	}
	p.logger.Warn("publication failed",
		"workflow_id", workflowID, "state", state, "reason", reason)
	return nil
}

func (p *Publisher) publish(ctx context.Context, workflowID string) error {
	if err := p.checkPublicationPolicy(ctx, workflowID); err != nil {
		return err
	}
	loaded, err := p.load(ctx, workflowID)
	if err != nil {
		return err
	}
	if loaded.workflow.Status != string(dag.WorkflowCompleted) {
		return nil
	}
	if !validPublicationPolicy(loaded.dossier) {
		return p.stale(ctx, workflowID, publicationPolicyReason(loaded.dossier))
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
	recheck := func() error {
		_, err := p.controlMatches(ctx, loaded)
		return err
	}
	cfg, err := p.freshConfig(ctx, loaded)
	if err != nil {
		return err
	}
	if !slices.Equal(loaded.dossier.IgnorePolicy.Patterns, cfg.Settings.Ignore) {
		return p.stale(ctx, workflowID, "ignore policy changed")
	}
	if hasConfig && configHash != "" && configHash != reviewConfigHash(cfg) {
		return p.stale(ctx, workflowID, "configuration changed")
	}
	if !hasPrompt || promptHash == "" {
		// Old workflows predate the prompt identity. Publishing them could
		// apply a review produced by an obsolete task layout.
		return p.stale(ctx, workflowID, "prompt bundle identity unavailable")
	}
	if p.promptsHash == "" || promptHash != p.promptsHash {
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
	index := dossierDiffIndex(*version, loaded.dossier)

	snapshot, err := p.engine.Snapshot(ctx, workflowID)
	if err != nil {
		return err
	}
	result, err := synthesisResult(snapshot)
	if err != nil {
		return PermanentJob(err)
	}
	for _, group := range result.Groups {
		if loaded.dossier.IgnoresPath(group.Path) {
			return p.stale(ctx, workflowID, "ignored finding rejected")
		}
		for _, source := range group.Sources {
			if loaded.dossier.IgnoresPath(source.Path) {
				return p.stale(ctx, workflowID, "ignored finding rejected")
			}
		}
		if group.Challenge != nil {
			for _, ref := range group.Challenge.Evidence {
				if loaded.dossier.IgnoresPath(ref.Path) {
					return p.stale(ctx, workflowID, "ignored finding rejected")
				}
			}
		}
	}
	for _, finding := range result.Output.NewFindings {
		if loaded.dossier.IgnoresPath(finding.Path) {
			return p.stale(ctx, workflowID, "ignored finding rejected")
		}
	}
	original := markPositions(result.Groups, index)
	warnings := append(append([]string(nil), result.Warnings...), cfg.Warnings...)
	// Dossier warnings disclose material degradation recorded at collection
	// time (missing context, withheld code graph, truncated background) (#457).
	warnings = append(warnings, loaded.dossier.Warnings...)
	if warning := review.BackgroundBudgetWarning(loaded.dossier); warning != "" {
		warnings = append(warnings, warning)
	}
	for i := range original {
		original[i].PublishKey = review.GroupPublishKey(loaded.dossier, original[i])
	}
	// groups is the presentation view used for posting, the summary and the
	// commit status: near-duplicate groups that are already decided to publish
	// are folded into one discussion. The stored findings (see storedGroups)
	// always keep the original adjudicated groups, so decisions, severities,
	// votes and statistics are unaffected by folding.
	groups := review.MergePresentation(original, loaded.dossier)
	folded := func(i int) bool {
		return groups[i].Decision != nil && groups[i].Decision.Source == "presentation" &&
			(original[i].Decision == nil || original[i].Decision.Source != "presentation")
	}
	// Inline discussions converge on the diff anchor (file + line + side)
	// instead of the finding wording. The primary group of each position
	// records the position key; every finding keeps its own fingerprint key so
	// feedback attribution and historical publication rows stay readable.
	positions := collectInlinePositions(groups, index)
	positionPrimary := make(map[int]string, len(positions))
	positionKeys := make(map[string]struct{}, len(positions))
	for _, position := range positions {
		positionPrimary[position.indexes[0]] = position.key
		positionKeys[position.key] = struct{}{}
	}
	// storedGroups returns the original groups with the position outcome of
	// the view. A folded group is not posted, so it keeps no publish key: the
	// display group alone owns the discussion and its feedback. A primary
	// group with a diff anchor keeps the position key even when this run falls
	// back to the summary, so one finding keeps one stable key across
	// generations; findings without a position keep their fingerprint key.
	storedGroups := func() []review.Group {
		out := make([]review.Group, len(original))
		copy(out, original)
		for i := range out {
			if folded(i) {
				out[i].PublishKey = ""
				continue
			}
			out[i].PositionOK = groups[i].PositionOK
			if key, ok := positionPrimary[i]; ok {
				out[i].PublishKey = key
			}
		}
		return out
	}
	contentMismatch := false
	for _, group := range groups {
		if group.Published() && index.AmbiguousPath(group.Path) {
			warnings = append(warnings, "部分问题的路径可能对应多个变更、被忽略或省略的变更，文件归属存在歧义，已保留在摘要中。")
			break
		}
	}
	if err := p.store.ReplaceFindings(ctx, workflowID, findingsForStore(workflowID, storedGroups())); err != nil {
		return err
	}
	if err := p.store.SetWorkflowVerdict(ctx, workflowID, result.Verdict); err != nil {
		return err
	}

	render := func() (string, error) {
		return review.RenderSummary(review.SummaryInput{
			Verdict:              result.Verdict,
			Summary:              result.Summary,
			RuleSummary:          result.RuleSummary,
			Groups:               groups,
			Fingerprint:          loaded.record.Hash,
			HeadSHA:              loaded.dossier.DiffVersion.HeadSHA,
			Profile:              loaded.workflow.Profile,
			Duration:             workflowDuration(loaded.workflow),
			DetailURL:            p.detailURL(workflowID),
			ShowModelAttribution: cfg.Settings.ShowModelAttribution,
			Warnings:             warnings,
			Scope:                result.Scope,
			Generated:            loaded.dossier.DiffSummary.Generated,
			GeneratedPaths:       loaded.dossier.DiffSummary.GeneratedPaths,
		})
	}
	body, err := render()
	if err != nil {
		return PermanentJob(fmt.Errorf("render summary: %w", err))
	}
	_, err = p.upsertSummary(ctx, loaded.project.ID, loaded.mrIID, loaded.token, loaded.record.Hash, body, workflowID, loaded.record.Generation, loaded.dossier.DiffVersion.HeadSHA, recheck)
	if err != nil {
		return err
	}

	fallback := false
	var lookup *discussionLookup // per-run cache for adoption reads
	for _, position := range positions {
		fellBack, unproven, err := p.publishInlinePosition(ctx, loaded, groups, position, positionKeys, index, workflowID, cfg, &lookup, recheck)
		if err != nil {
			return err
		}
		if fellBack {
			fallback = true
		}
		if unproven && !contentMismatch {
			warnings = append(warnings, "部分问题的历史讨论内容与当前内容不同或无法确认，当前问题已保留在摘要中，未修改历史讨论。")
			contentMismatch = true
		}
	}
	if fallback {
		if err := p.store.ReplaceFindings(ctx, workflowID, findingsForStore(workflowID, storedGroups())); err != nil {
			return err
		}
		updated, err := render()
		if err != nil {
			return PermanentJob(fmt.Errorf("render summary: %w", err))
		}
		if updated != body {
			if _, err := p.upsertSummary(ctx, loaded.project.ID, loaded.mrIID, loaded.token, loaded.record.Hash, updated, workflowID, loaded.record.Generation, loaded.dossier.DiffVersion.HeadSHA, recheck); err != nil {
				return err
			}
		}
	}
	if err := p.reconcileDiscussions(ctx, loaded, groups, index, result.Verdict, lookup, recheck); err != nil {
		return err
	}

	if cfg.Settings.CommitStatus {
		// Only an explicit PASS may report success. BLOCKING and any future
		// non-PASS verdict must never green-light the commit status.
		state := "failed"
		if result.Verdict == review.VerdictPass {
			state = "success"
		}
		if err := p.publishCommitStatus(ctx, loaded, state, statusDescription(result.Verdict, review.SummaryFoldedGroups(groups)), recheck); err != nil {
			return err
		}
	}
	return sendOutbound(ctx, p.workspace.Get(loaded.project.ID), func(callCtx context.Context) error {
		if _, err := p.controlMatches(callCtx, loaded); err != nil {
			return err
		}
		return p.store.SetWorkflowPublicationState(callCtx, workflowID, store.WorkflowPublicationPublished, "")
	})
}

// PublishFailure publishes the enabled failure status and incomplete summary.
// Publication state follows the same retry rules as a normal publication.
func (p *Publisher) PublishFailure(ctx context.Context, workflowID, reason string) error {
	ctx, unlock, err := p.lockWorkflow(ctx, workflowID)
	if err != nil {
		return err
	}
	defer unlock()
	return p.publishFailureLocked(ctx, workflowID, reason)
}

// publishFailureLocked requires the project workspace read lock.
func (p *Publisher) publishFailureLocked(ctx context.Context, workflowID, reason string) error {
	mrUnlock, err := p.lockReviewMR(ctx, workflowID)
	if err != nil {
		return err
	}
	defer mrUnlock()
	unlock, err := p.locks.lock(ctx, workflowID)
	if err != nil {
		return err
	}
	defer unlock()
	if rejected, unknown, err := p.rejectRepositorySkillPublicationLocked(ctx, p.store, workflowID); rejected || err != nil {
		return repositorySkillPublicationError(unknown, err)
	}
	err = p.publishFailure(ctx, workflowID, reason)
	if err == nil {
		return nil
	}
	if errors.Is(err, workspace.ErrOutboundInvalidated) {
		return err
	}
	if errors.Is(err, ErrStalePublication) || errors.Is(err, errDiscussionUnknown) {
		if rejected, unknown, settleErr := p.rejectRepositorySkillPublicationLocked(ctx, p.store, workflowID); rejected {
			return repositorySkillPublicationError(unknown, settleErr)
		} else if settleErr != nil {
			return errors.Join(err, settleErr)
		}
	}
	err = p.preserveUnknown(ctx, workflowID, err)
	if errors.Is(err, ErrStalePublication) {
		return err
	}
	if recordErr := p.recordPublicationFailure(ctx, workflowID, err); recordErr != nil {
		return errors.Join(err, recordErr)
	}
	return err
}

func (p *Publisher) publishFailure(ctx context.Context, workflowID, reason string) error {
	if err := p.checkPublicationPolicy(ctx, workflowID); err != nil {
		return err
	}
	loaded, err := p.load(ctx, workflowID)
	if err != nil {
		return err
	}
	if loaded.workflow.Status != string(dag.WorkflowFailed) {
		return PermanentJob(fmt.Errorf("workflow %s did not fail", workflowID))
	}
	if !validPublicationPolicy(loaded.dossier) {
		return p.stale(ctx, workflowID, publicationPolicyReason(loaded.dossier))
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
	recheck := func() error {
		_, err := p.controlMatches(ctx, loaded)
		return err
	}
	cfg, err := p.freshConfig(ctx, loaded)
	if err != nil {
		return err
	}
	if !slices.Equal(loaded.dossier.IgnorePolicy.Patterns, cfg.Settings.Ignore) {
		return p.stale(ctx, workflowID, "ignore policy changed")
	}
	if hasConfig && configHash != "" && configHash != reviewConfigHash(cfg) {
		return p.stale(ctx, workflowID, "configuration changed")
	}
	if !hasPrompt || promptHash == "" {
		// Old workflows predate the prompt identity. Publishing them could
		// apply a review produced by an obsolete task layout.
		return p.stale(ctx, workflowID, "prompt bundle identity unavailable")
	}
	if p.promptsHash == "" || promptHash != p.promptsHash {
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
	if cfg.Settings.CommitStatus {
		if err := p.publishCommitStatus(ctx, loaded, "failed", statusDescription(review.VerdictIncomplete, nil), recheck); err != nil {
			return err
		}
	}
	if !cfg.Settings.PostFailureNote {
		if !cfg.Settings.CommitStatus {
			return p.store.SetWorkflowPublicationStateIfPending(ctx, workflowID, failureNoteDisabledReason)
		}
		return p.store.SetWorkflowPublicationState(ctx, workflowID, store.WorkflowPublicationPublished, "")
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
	if _, err := p.upsertSummary(ctx, loaded.project.ID, loaded.mrIID, loaded.token, loaded.record.Hash, body, workflowID, loaded.record.Generation, loaded.dossier.DiffVersion.HeadSHA, recheck); err != nil {
		return err
	}
	return p.store.SetWorkflowPublicationState(ctx, workflowID, store.WorkflowPublicationPublished, "")
}

// Republish re-runs publication for a terminal workflow, including a manual
// request after a previous failure. Failed executions publish the failure
// summary, never a normal review result. The stale and generation fences stay
// inside Publish and PublishFailure.
func (p *Publisher) Republish(ctx context.Context, workflowID string) error {
	ctx, unlock, err := p.lockWorkflow(ctx, workflowID)
	if err != nil {
		return err
	}
	defer unlock()
	return p.republishLocked(ctx, workflowID)
}

// republishLocked requires the project workspace read lock.
func (p *Publisher) republishLocked(ctx context.Context, workflowID string) error {
	if rejected, unknown, err := p.rejectRepositorySkillPublication(ctx, p.store, workflowID); rejected || err != nil {
		return repositorySkillPublicationError(unknown, err)
	}
	if err := p.checkPublicationPolicy(ctx, workflowID); err != nil {
		if errors.Is(err, workspace.ErrOutboundInvalidated) {
			return err
		}
		if rejected, unknown, settleErr := p.rejectRepositorySkillPublication(ctx, p.store, workflowID); rejected {
			return repositorySkillPublicationError(unknown, settleErr)
		} else if settleErr != nil {
			return errors.Join(err, settleErr)
		}
		return err
	}
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
		return p.publishFailureLocked(ctx, workflowID, reviewExecutionFailedReason)
	}
	return p.publishLocked(ctx, workflowID)
}

// lockWorkflow uses the durable workflow ID to fence a deleted registration.
func (p *Publisher) lockWorkflow(ctx context.Context, id string) (context.Context, func(), error) {
	wf, err := p.store.GetWorkflow(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if wf == nil || wf.ProjectID <= 0 {
		return nil, nil, PermanentJob(fmt.Errorf("workflow is missing"))
	}
	ws := p.workspace.Get(wf.ProjectID)
	if ws == nil {
		return nil, nil, PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	unlock, err := ws.RLock(ctx)
	if err != nil {
		return nil, nil, err
	}
	ctx, err = bindOutbound(ctx, ws)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	current, err := p.store.GetWorkflow(ctx, id)
	if err != nil || current == nil || current.ID != id || current.ProjectID != wf.ProjectID {
		unlock()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, PermanentJob(fmt.Errorf("workflow is missing"))
	}
	return ctx, unlock, nil
}

// lockMR serializes summary and commit-status writes for one merge request.
// Callers hold the project workspace lock before taking it. The same registry
// is shared with the status worker, dispatcher, command worker and catch-up;
// webhook ingress does not hold it (DB-only enqueue within the 900ms budget,
// #475).
func (p *Publisher) lockMR(ctx context.Context, projectID, mrIID int64) (func(), error) {
	return mrLockRegistry(p.mrLocks).lock(ctx, reviewMRLockKey(projectID, mrIID))
}

// lockReviewMR resolves the workflow's merge request and takes its lock. It
// keeps the lock order workspace RLock, MR lock, workflow lock.
func (p *Publisher) lockReviewMR(ctx context.Context, workflowID string) (func(), error) {
	wf, err := p.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, PermanentJob(fmt.Errorf("workflow is missing"))
		}
		return nil, err
	}
	if wf == nil || wf.ProjectID <= 0 || wf.MRIID == nil || *wf.MRIID <= 0 {
		return nil, PermanentJob(fmt.Errorf("workflow is missing"))
	}
	return p.lockMR(ctx, wf.ProjectID, *wf.MRIID)
}

// publishStandaloneSummaryLocked requires the project workspace read lock.
func (p *Publisher) publishStandaloneSummaryLocked(ctx context.Context, projectID, mrIID, generation int64, head, token, fingerprint, body string) error {
	return p.publishStandaloneReviewLocked(ctx, projectID, mrIID, generation, head, token, fingerprint, body, nil)
}

// publishStandaloneReviewLocked keeps the summary and optional status under
// the same MR lock and generation/head checks as normal publication.
func (p *Publisher) publishStandaloneReviewLocked(ctx context.Context, projectID, mrIID, generation int64, head, token, fingerprint, body string, status *gitlab.CommitStatus) error {
	ctx, err := bindOutbound(ctx, p.workspace.Get(projectID))
	if err != nil {
		return err
	}
	unlock, err := p.lockMR(ctx, projectID, mrIID)
	if err != nil {
		return err
	}
	defer unlock()
	recheck := func() error {
		control, err := p.store.GetReviewControl(ctx, projectID, mrIID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return PermanentJob(fmt.Errorf("%w: review control is missing", ErrStalePublication))
			}
			return err
		}
		if control.Cancelled || control.Generation != generation || !strings.EqualFold(control.HeadSHA, head) {
			return PermanentJob(fmt.Errorf("%w: review generation changed", ErrStalePublication))
		}
		return nil
	}
	if _, err = p.upsertSummary(ctx, projectID, mrIID, token, fingerprint, body, "", generation, head, recheck); err != nil {
		return err
	}
	if status != nil {
		return p.setCommitStatus(ctx, token, projectID, head, *status, recheck)
	}
	return nil
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
	token, err := resolveProjectToken(p.tokens, p.cipher, project)
	if err != nil {
		return nil, PermanentJob(fmt.Errorf("project credential is unavailable"))
	}
	return &loadedReview{
		workflow: workflow,
		dossier:  &dossier,
		record:   record,
		project:  project,
		token:    token,
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
		if errors.Is(err, ErrInvalidRepoConfig) {
			return effectiveReviewConfig{}, PermanentJob(err)
		}
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
	cfg, err := resolveSecureReviewConfig(loaded.project, docs, p.reviewDefaults, p.profiles)
	if err != nil {
		if errors.Is(err, ErrInvalidRepoConfig) {
			return effectiveReviewConfig{}, PermanentJob(err)
		}
		return effectiveReviewConfig{}, err
	}
	return cfg, nil
}

func validPublicationPolicy(d *review.Dossier) bool {
	return publicationPolicyReason(d) == ""
}

func publicationPolicyReason(d *review.Dossier) string {
	if d == nil || d.IgnorePolicy == nil || d.IgnorePolicy.Version != review.IgnorePolicyVersion || d.IgnorePolicy.Patterns == nil {
		return "ignore policy unavailable; rerun review"
	}
	if !d.WorkspaceProvenanceValid() {
		return workspaceEvidenceReason
	}
	if !d.RepositorySkillCapabilitySupported() {
		return repositorySkillEvidenceReason
	}
	return ""
}

func (p *Publisher) checkPublicationPolicy(ctx context.Context, workflowID string) error {
	record, err := p.store.GetWorkflowDossier(ctx, workflowID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && record != nil && !review.DossierHasSupportedRepositorySkillCapability(record.Data) {
		return p.stale(ctx, workflowID, repositorySkillEvidenceReason)
	}
	var dossier review.Dossier
	if err == nil && record != nil && json.Unmarshal(record.Data, &dossier) == nil && validPublicationPolicy(&dossier) {
		return nil
	}
	return p.stale(ctx, workflowID, publicationPolicyReason(&dossier))
}

func (p *Publisher) rawDoc(ctx context.Context, token string, projectID int64, path, ref string) (string, error) {
	return loadRepoDoc(ctx, p.gitlab, p.logger, token, projectID, path, ref)
}

// preserveUnknown prevents later policy or GitLab errors from turning an
// unresolved remote effect into a false terminal publication outcome.
func (p *Publisher) preserveUnknown(ctx context.Context, workflowID string, cause error) error {
	wf, err := p.store.GetWorkflow(ctx, workflowID)
	if err != nil || wf == nil || wf.MRIID == nil {
		return errors.Join(cause, err)
	}
	unknown, err := p.store.HasUnknownDiscussionIntent(ctx, wf.ProjectID, *wf.MRIID)
	if err != nil {
		return errors.Join(cause, errDiscussionUnknown, err)
	}
	if unknown {
		if errors.Is(cause, workspace.ErrOutboundInvalidated) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
			return errors.Join(errDiscussionUnknown, cause)
		}
		return errDiscussionUnknown
	}
	return cause
}

func (p *Publisher) stale(ctx context.Context, workflowID, reason string) error {
	if err := p.preserveUnknown(ctx, workflowID, nil); err != nil {
		return err
	}
	if err := p.store.SetWorkflowPublicationStateIfPending(ctx, workflowID, "stale: "+reason); err != nil {
		return err
	}
	p.logger.Info("publication skipped as stale", "workflow_id", workflowID, "reason", reason)
	return PermanentJob(fmt.Errorf("%w: %s", ErrStalePublication, reason))
}

// summarySuperseded settles a publication whose review control advanced
// between the caller's recheck and the final-body write. A workflow records
// the skip; a standalone summary has no workflow row and only fails its job.
func (p *Publisher) summarySuperseded(ctx context.Context, workflowID string) error {
	if workflowID == "" {
		return PermanentJob(fmt.Errorf("%w: review generation changed", ErrStalePublication))
	}
	return p.stale(ctx, workflowID, "review generation changed")
}

// upsertSummary finds or creates the summary note. The marker scan recovers a
// note after a lost publication row or a deleted bound note. recheck runs before
// every remote summary attempt; a control change can still race an in-flight request.
func (p *Publisher) upsertSummary(ctx context.Context, projectID, mrIID int64, token, fingerprint, body, workflowID string, generation int64, headSHA string, recheck func() error) (id int64, err error) {
	if err := recheck(); err != nil {
		return 0, err
	}
	// #451: the status worker and this publisher share one review note. Store
	// the rendered summary in the status row first so a later status edit
	// keeps the result; then update the same note through the normal path.
	// The publication row is still written by that path.
	// The control is read only to keep the legacy no-control path.
	_, controlErr := p.store.GetReviewControl(ctx, projectID, mrIID)
	if controlErr != nil && !errors.Is(controlErr, store.ErrNotFound) {
		return 0, controlErr
	}
	if controlErr == nil {
		// Write with this publication's own generation/head, never with the
		// current control values. A review activated between recheck and this
		// store call must reject the old summary instead of adopting it (#453).
		if _, err := p.store.SetReviewStatusFinalBody(ctx, projectID, mrIID, generation, headSHA, body); err != nil {
			if errors.Is(err, store.ErrSuperseded) {
				return 0, p.summarySuperseded(ctx, workflowID)
			}
			return 0, err
		}
		// Render the same body the status worker writes: lifecycle lines
		// followed by the summary. Both writers therefore converge.
		if row, err := p.store.GetReviewStatus(ctx, projectID, mrIID); err == nil && row != nil {
			body = reviewStatusBody(reviewStatusUpdate{
				State: row.State, Detail: row.Detail, HeadSHA: row.HeadSHA,
				Generation: row.Generation, FinalBody: body,
			})
		}
	}
	var missingID int64
	var missingErr error
	// A review control means the status worker and this publisher share one
	// note, which is found by marker and current identity below. The persisted
	// binding is not trustworthy there: after a credential switch it still
	// points at the previous bot's note and GitLab answers 403 to edits by
	// another author (#514). Only the legacy no-control path updates it.
	if controlErr != nil && workflowID != "" {
		existing, err := p.store.GetPublication(ctx, projectID, mrIID, summaryPublishKind, summaryPublicationKey)
		switch {
		case err == nil:
			if id, parseErr := strconv.ParseInt(existing.GitlabID, 10, 64); parseErr == nil && id > 0 {
				notFound, err := p.updateSummaryNote(ctx, token, projectID, mrIID, id, body, recheck)
				if err != nil && (!notFound || IsPermanentJob(err)) {
					return 0, err
				}
				if err == nil {
					if _, err := p.store.UpsertPublication(ctx, store.Publication{
						ProjectID: projectID, MRIID: mrIID, Kind: summaryPublishKind,
						PublishKey: summaryPublicationKey, GitlabID: existing.GitlabID,
						WorkflowID: workflowID, Fingerprint: fingerprint, BodyHash: bodyHash(body),
					}); err != nil {
						return 0, err
					}
					return id, nil
				}
				missingID, missingErr = id, err
				if err := recheck(); err != nil {
					return 0, err
				}
			}
		case !errors.Is(err, store.ErrNotFound):
			return 0, err
		}
	}
	user, err := p.gitlab.CurrentUser(ctx, token)
	if err != nil {
		return 0, fmt.Errorf("app: summary identity lookup: %w", err)
	}
	if user == nil || user.ID <= 0 {
		return 0, errors.New("app: summary identity is unavailable")
	}
	var last error
	for attempt := 0; attempt < p.retries; attempt++ {
		notes, err := p.gitlab.ListMRNotes(ctx, token, projectID, mrIID)
		if err != nil {
			return 0, err
		}
		if err := recheck(); err != nil {
			return 0, err
		}
		// A visible note may be inaccessible to update rather than deleted.
		for _, note := range notes {
			if missingID > 0 && note.ID == missingID {
				return 0, missingErr
			}
		}
		for _, note := range notes {
			if note.AuthorID != user.ID || !strings.HasPrefix(note.Body, review.SummaryMarkerPrefix) {
				continue
			}
			if note.ID <= 0 {
				return 0, errors.New("app: summary note has invalid ID")
			}
			if note.Body != body {
				if _, err := p.updateSummaryNote(ctx, token, projectID, mrIID, note.ID, body, recheck); err != nil {
					return 0, err
				}
			}
			id = note.ID
			break
		}
		if id > 0 {
			break
		}
		if err := recheck(); err != nil {
			return 0, err
		}
		var created *gitlab.Note
		err = sendOutbound(ctx, p.workspace.Get(projectID), func(ctx context.Context) error {
			var err error
			created, err = p.gitlab.CreateMRNote(ctx, token, projectID, mrIID, body)
			return err
		})
		if err == nil {
			if created == nil || created.ID <= 0 {
				return 0, errors.New("app: summary creation returned no valid note")
			}
			id = created.ID
			break
		}
		last = err
		if gitlab.StatusOf(err) == 400 || gitlab.StatusOf(err) == 404 || errors.Is(err, workspace.ErrOutboundInvalidated) ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, err
		}
		delay := time.Duration(attempt+1) * 250 * time.Millisecond
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}
	if id == 0 {
		return 0, last
	}
	if workflowID != "" {
		if _, err := p.store.UpsertPublication(ctx, store.Publication{
			ProjectID: projectID, MRIID: mrIID, Kind: summaryPublishKind,
			PublishKey: summaryPublicationKey, GitlabID: strconv.FormatInt(id, 10),
			WorkflowID: workflowID, Fingerprint: fingerprint, BodyHash: bodyHash(body),
		}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// The bool identifies a 404 from the note update, not from the recheck.
func (p *Publisher) updateSummaryNote(ctx context.Context, token string, projectID, mrIID, noteID int64, body string, recheck func() error) (bool, error) {
	var notFound bool
	err := p.retry(ctx, func() error {
		if err := recheck(); err != nil {
			return err
		}
		return sendOutbound(ctx, p.workspace.Get(projectID), func(ctx context.Context) error {
			err := p.gitlab.UpdateMRNote(ctx, token, projectID, mrIID, noteID, body)
			notFound = gitlab.IsNotFound(err)
			return err
		})
	})
	return notFound && !errors.Is(err, workspace.ErrOutboundInvalidated) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded), err
}

// createDiscussion is called only while the intent's outbound lease is held.
func (p *Publisher) createDiscussion(ctx context.Context, token string, projectID, mrIID int64, key, body string, position gitlab.Position) (string, error) {
	created, err := p.gitlab.CreateDiscussion(ctx, token, projectID, mrIID, gitlab.NewDiscussion{Body: body, Position: &position})
	if err != nil {
		return "", err
	}
	if created == nil || strings.TrimSpace(created.ID) == "" {
		return "", errDiscussionUnknown
	}
	// Record the remote fact while the outbound lease and workspace lock
	// still prevent policy changes and project deletion from passing it.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
	defer cancel()
	if err := p.store.ConfirmDiscussionIntent(writeCtx, projectID, mrIID, key, created.ID); err != nil {
		return "", errors.Join(errDiscussionUnknown, err)
	}
	return created.ID, nil
}

// inlinePosition groups the presentation groups that share one diff anchor.
// Every group in one position updates the same discussion.
type inlinePosition struct {
	key     string
	indexes []int
}

// collectInlinePositions returns one entry per diff anchor, in first-seen
// order. Findings without a usable position stay in the summary. The key is
// the canonical anchor identity, so findings reported on either side of one
// unchanged line share a single thread.
func collectInlinePositions(groups []review.Group, index *gitlab.DiffIndex) []inlinePosition {
	byKey := make(map[string]int)
	var out []inlinePosition
	for i, group := range groups {
		if !group.Published() || !group.PositionOK {
			continue
		}
		anchor, ok := index.LocateUnambiguous(group.Path, group.Line, diffSide(group.Side))
		if !ok {
			continue
		}
		key := review.PositionAnchorKey(group.Path, group.Line, group.Side, anchor)
		if key == "" {
			continue
		}
		if at, ok := byKey[key]; ok {
			out[at].indexes = append(out[at].indexes, i)
			continue
		}
		byKey[key] = len(out)
		out = append(out, inlinePosition{key: key, indexes: []int{i}})
	}
	return out
}

// publishInlinePosition creates, adopts, or updates the single discussion for
// one diff position. fellBack reports that the position must stay in the
// summary this run; unproven additionally means a historical thread exists but
// could not be shown to belong to this bot.
func (p *Publisher) publishInlinePosition(ctx context.Context, loaded *loadedReview, groups []review.Group, plan inlinePosition, positionKeys map[string]struct{}, index *gitlab.DiffIndex, workflowID string, cfg effectiveReviewConfig, lookup **discussionLookup, recheck func() error) (fellBack, unproven bool, retErr error) {
	fallback := func(unproven bool) (bool, bool, error) {
		for _, i := range plan.indexes {
			groups[i].PositionOK = false
		}
		return true, unproven, nil
	}
	first := groups[plan.indexes[0]]
	anchor, ok := index.LocateUnambiguous(first.Path, first.Line, diffSide(first.Side))
	if !ok {
		return fallback(false)
	}
	selected := make([]review.Group, 0, len(plan.indexes))
	for _, i := range plan.indexes {
		selected = append(selected, groups[i])
	}
	baseBody, err := review.RenderPositionThread(selected, anchor, review.FindingRenderOptions{
		ShowModelAttribution: cfg.Settings.ShowModelAttribution,
		ProviderLabels:       p.providerLabels(),
		Generation:           loaded.record.Generation,
	})
	if err != nil {
		return fallback(false)
	}
	body := withWorkflowMarker(baseBody, workflowID)

	if *lookup == nil {
		*lookup = &discussionLookup{gl: p.gitlab, token: loaded.token, projectID: loaded.project.ID, mrIID: loaded.mrIID}
	}
	lookupFor := *lookup
	// Discussion reads race policy changes like any other remote call, so they
	// run under the outbound lease and an invalidation cancels them.
	prepare := func() error {
		return sendOutbound(ctx, p.workspace.Get(loaded.project.ID), func(callCtx context.Context) error {
			return lookupFor.prepare(callCtx)
		})
	}

	publication, err := p.store.GetPublication(ctx, loaded.project.ID, loaded.mrIID, discussionPublishKind, plan.key)
	switch {
	case err == nil && publication.GitlabID != "":
		if err := prepare(); err != nil {
			return false, false, err
		}
		discussion, note, ok := lookupFor.findingDiscussionByID(publication.GitlabID)
		if !ok {
			// The recorded thread is gone, foreign, or unreadable. Keep the
			// finding in the summary instead of stacking a second thread.
			return fallback(true)
		}
		if !lookupFor.claim(discussion.ID) {
			// Another position in this run already owns the thread. Updating
			// it here would overwrite that position's body.
			return fallback(true)
		}
		if normalizeDiscussionBody(body) != normalizeDiscussionBody(note.Body) {
			if err := p.updateDiscussionNote(ctx, loaded, note.ID, body, recheck); err != nil {
				if permanentDiscussionStatus(gitlab.StatusOf(err)) {
					return fallback(true)
				}
				return false, false, err
			}
			lookupFor.rememberNoteBody(discussion.ID, note.ID, body)
		}
		if err := p.saveInlinePublication(ctx, loaded, plan, discussion.ID, workflowID, baseBody); err != nil {
			return false, false, err
		}
		return false, false, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return false, false, err
	}

	if err := prepare(); err != nil {
		return false, false, err
	}
	adopted, unprovenAdoption, err := p.adoptInlineDiscussion(ctx, loaded, lookupFor, groups, plan, positionKeys, anchor, body, baseBody, workflowID, recheck)
	if err != nil {
		return false, false, err
	}
	if adopted {
		return false, false, nil
	}
	if unprovenAdoption {
		return fallback(true)
	}

	// Create with the existing uncertainty fence: the intent proves a remote ID
	// only, and adoption still requires the marker scan to match this content.
	// The generation is rechecked inside the lease so a bump that landed after
	// the last pre-send check cannot reserve and create a superseded thread.
	var created, proven bool
	var discussionID string
	err = sendOutbound(ctx, p.workspace.Get(loaded.project.ID), func(callCtx context.Context) error {
		var err error
		if err := recheck(); err != nil {
			return err
		}
		created, discussionID, err = p.store.ReserveDiscussionIntent(callCtx, loaded.project.ID, loaded.mrIID, plan.key)
		if err != nil {
			return err
		}
		if created {
			discussionID, err = p.createDiscussion(callCtx, loaded.token, loaded.project.ID, loaded.mrIID, plan.key, body, anchor)
			return err
		}
		if discussionID == "" {
			var remoteBody string
			discussionID, remoteBody, err = p.recoverDiscussionBody(callCtx, loaded.token, loaded.project.ID, loaded.mrIID, plan.key)
			if errors.Is(err, errDiscussionAbsent) {
				if p.discussionAbsentProven(loaded) {
					return errDiscussionAbsent
				}
				return errors.Join(errDiscussionUnknown, err)
			}
			if err != nil {
				return errors.Join(errDiscussionUnknown, err)
			}
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(callCtx), 4*time.Second)
			defer cancel()
			if err := p.store.ConfirmDiscussionIntent(writeCtx, loaded.project.ID, loaded.mrIID, plan.key, discussionID); err != nil {
				return errors.Join(errDiscussionUnknown, err)
			}
			// The marker scan already matched author, key and ID.
			proven = normalizeDiscussionBody(remoteBody) == normalizeDiscussionBody(body)
			return nil
		}
		// An intent proves a remote ID only; adopt it when the remote body
		// proves this workflow sent exactly the current content. The remote ID is
		// already confirmed, so a failed read is an ordinary retryable error.
		proven, err = lookupFor.provenOurs(callCtx, discussionID, body)
		return err
	})
	if errors.Is(err, errDiscussionAbsent) && !errors.Is(err, errDiscussionUnknown) {
		// A successful marker scan of an old publication proved no remote
		// discussion exists. Clearing the fence lets a later review post the
		// finding again; this run keeps it in the summary and still terminates.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
		rejectErr := p.store.RejectDiscussionIntent(writeCtx, loaded.project.ID, loaded.mrIID, plan.key)
		cancel()
		if rejectErr != nil && !errors.Is(rejectErr, store.ErrConflict) {
			return false, false, errors.Join(errDiscussionUnknown, rejectErr)
		}
		return fallback(false)
	}
	if err != nil {
		if created {
			if permanentDiscussionStatus(gitlab.StatusOf(err)) && !errors.Is(err, errDiscussionUnknown) &&
				!errors.Is(err, workspace.ErrOutboundInvalidated) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				if rejectErr := p.store.RejectDiscussionIntent(ctx, loaded.project.ID, loaded.mrIID, plan.key); rejectErr != nil {
					return false, false, errors.Join(errDiscussionUnknown, rejectErr)
				}
				return fallback(false)
			}
			return false, false, errors.Join(errDiscussionUnknown, err)
		}
		return false, false, err
	}
	if !created && !proven {
		// Unproven intents keep the conservative path: no workflow/content proof.
		return fallback(true)
	}
	if !lookupFor.claim(discussionID) {
		// A recovered intent can point at a thread another position already
		// bound this run; recording it again would move that position's body.
		return fallback(true)
	}
	if err := p.saveInlinePublication(ctx, loaded, plan, discussionID, workflowID, baseBody); err != nil {
		return false, false, err
	}
	return false, false, nil
}

// adoptInlineDiscussion reuses a historical finding thread at the same diff
// anchor. Publication rows win over position scans because they identify a
// thread even when GitLab omits note positions. Only our own note is updated;
// the discussion and every reply stay in place. The caller's discussion set
// guarantees one thread per run: a thread another position already bound is
// never updated here.
func (p *Publisher) adoptInlineDiscussion(ctx context.Context, loaded *loadedReview, lookup *discussionLookup, groups []review.Group, plan inlinePosition, positionKeys map[string]struct{}, anchor gitlab.Position, body, baseBody, workflowID string, recheck func() error) (adopted, unproven bool, retErr error) {
	first := groups[plan.indexes[0]]
	// Note positions on a context line carry both line numbers and, for a
	// rename, both paths. Match the canonical anchor so either reported side
	// reaches the same thread.
	canonicalPath, line, side := review.NormalizeAnchorPosition(first.Path, first.Line, first.Side, anchor)
	candidates := make(map[string]gitlab.Discussion)
	missing := false
	lookedUp := make(map[string]bool)
	for _, i := range plan.indexes {
		key := groups[i].PublishKey
		if key == "" {
			continue
		}
		lookedUp[key] = true
		publication, err := p.store.GetPublication(ctx, loaded.project.ID, loaded.mrIID, discussionPublishKind, key)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, false, err
		}
		if publication.GitlabID == "" {
			continue
		}
		found := false
		for _, discussion := range lookup.discussions {
			if discussion.ID != publication.GitlabID {
				continue
			}
			found = true
			if len(discussion.Notes) > 0 && discussion.Notes[0].Position != nil {
				if !notePositionMatches(*discussion.Notes[0].Position, canonicalPath, line, side) {
					// The recorded thread sits at another anchor; reusing it
					// would move this finding.
					missing = true
					break
				}
				candidates[discussion.ID] = discussion
				break
			}
			// GitLab may omit note positions. The historical finding must still
			// prove the same path, side, category, and a nearby line. The group
			// that holds this key is the one proven, not whichever finding
			// happened to come first at the anchor.
			source, err := p.adoptionSourceMatches(ctx, publication.WorkflowID, key, groups[i])
			if err != nil {
				return false, false, err
			}
			if !source {
				missing = true
				break
			}
			candidates[discussion.ID] = discussion
			break
		}
		if !found {
			missing = true
		}
	}
	// Old fingerprint rows are keyed by wording, so a changed title or
	// evidence no longer matches the current key. Scan this MR's discussion
	// rows and let the historical finding prove the same path, side, category
	// and a nearby line before reusing a thread that carries no note position.
	legacyRows, err := p.store.ListDiscussionPublications(ctx, loaded.project.ID, loaded.mrIID)
	if err != nil {
		return false, false, err
	}
	for _, publication := range legacyRows {
		if publication.GitlabID == "" || lookedUp[publication.PublishKey] {
			continue
		}
		if _, other := positionKeys[publication.PublishKey]; other {
			// Another position owns this thread in the current run.
			continue
		}
		discussion, note, visible := lookup.findingDiscussionByID(publication.GitlabID)
		proven := false
		if visible && note.Position != nil {
			if !notePositionMatches(*note.Position, canonicalPath, line, side) {
				continue
			}
			proven = true
		} else {
			for _, i := range plan.indexes {
				source, err := p.adoptionSourceMatches(ctx, publication.WorkflowID, publication.PublishKey, groups[i])
				if err != nil {
					return false, false, err
				}
				if source {
					proven = true
					break
				}
			}
		}
		if !proven {
			continue
		}
		if visible {
			candidates[discussion.ID] = discussion
			continue
		}
		// A proven historical thread is not visible; do not stack another.
		missing = true
	}
	for _, discussion := range lookup.findingDiscussionsAt(canonicalPath, line, side) {
		candidates[discussion.ID] = discussion
	}
	var best gitlab.Discussion
	bestNoteID := int64(-1)
	for _, discussion := range candidates {
		if len(discussion.Notes) == 0 {
			continue
		}
		if note := discussion.Notes[0]; note.ID > bestNoteID {
			best, bestNoteID = discussion, note.ID
		}
	}
	if bestNoteID < 0 {
		if missing {
			// A recorded historical thread is not visible; do not stack another.
			return false, true, nil
		}
		return false, false, nil
	}
	note := best.Notes[0]
	if note.System || note.AuthorID != lookup.userID {
		return false, true, nil
	}
	markerKey, ok := discussionPublishKey(note.Body)
	if !ok {
		return false, true, nil
	}
	if markerKey != plan.key {
		if _, other := positionKeys[markerKey]; other {
			// The thread is the current thread of another position in this
			// run; adopting it would overwrite that position's findings.
			return false, true, nil
		}
	}
	if lookup.isClaimed(best.ID) {
		return false, true, nil
	}
	if position := note.Position; position != nil && !notePositionMatches(*position, canonicalPath, line, side) {
		return false, true, nil
	}
	// Bind before writing: this run is sequential, so the claim cannot be
	// stolen between the check above and here, and a later position must not
	// update the same thread.
	lookup.claim(best.ID)
	// A crashed create attempt can leave an unconfirmed position intent. The
	// adopted thread settles it, so the MR no longer reports an unknown
	// remote discussion (#487). An intent already bound to another remote ID
	// stays unresolved and this position falls back to the summary.
	if err := sendOutbound(ctx, p.workspace.Get(loaded.project.ID), func(callCtx context.Context) error {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(callCtx), 4*time.Second)
		defer cancel()
		return p.store.ConfirmDiscussionIntent(writeCtx, loaded.project.ID, loaded.mrIID, plan.key, best.ID)
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return false, true, nil
		}
		return false, false, err
	}
	if normalizeDiscussionBody(body) != normalizeDiscussionBody(note.Body) {
		if err := p.updateDiscussionNote(ctx, loaded, note.ID, body, recheck); err != nil {
			if permanentDiscussionStatus(gitlab.StatusOf(err)) {
				return false, true, nil
			}
			return false, false, err
		}
		lookup.rememberNoteBody(best.ID, note.ID, body)
	}
	if err := p.saveInlinePublication(ctx, loaded, plan, best.ID, workflowID, baseBody); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// adoptionSourceMatches proves that a legacy publication row still describes
// the same finding at the current position. It is the historical guard for
// note rows that carry no GitLab position: exactly one finding under the key,
// same path/side/category, and the **same line**. A nearby-line window was
// rejected: it let a new finding one row below adopt (and rewrite) the
// previous finding's thread, which also stopped that stale thread from being
// resolved.
func (p *Publisher) adoptionSourceMatches(ctx context.Context, workflowID, key string, current review.Group) (bool, error) {
	if workflowID == "" {
		return false, nil
	}
	findings, err := p.store.ListFindings(ctx, store.FindingFilter{WorkflowID: workflowID})
	if err != nil {
		return false, err
	}
	matches, nearby := 0, false
	for _, finding := range findings {
		if finding.PublishKey != key {
			continue
		}
		matches++
		previousSide := review.Side(finding.Side)
		validPreviousSide := previousSide == "" || previousSide == review.SideNew || previousSide == review.SideOld
		validCurrentSide := current.Side == "" || current.Side == review.SideNew || current.Side == review.SideOld
		if finding.Path != "" && finding.Path == current.Path && validPreviousSide && validCurrentSide &&
			diffSide(previousSide) == diffSide(current.Side) && finding.Category == current.Category &&
			finding.Line != nil && *finding.Line > 0 && current.Line > 0 {
			nearby = *finding.Line == current.Line
		}
	}
	return matches == 1 && nearby, nil
}

// saveInlinePublication records the discussion row under the position key.
// Legacy rows keep their fingerprint keys and are never rewritten.
func (p *Publisher) saveInlinePublication(ctx context.Context, loaded *loadedReview, plan inlinePosition, discussionID, workflowID, baseBody string) error {
	return sendOutbound(ctx, p.workspace.Get(loaded.project.ID), func(callCtx context.Context) error {
		if _, err := p.controlMatches(callCtx, loaded); err != nil {
			return err
		}
		_, err := p.store.UpsertPublication(callCtx, store.Publication{
			ProjectID:   loaded.project.ID,
			MRIID:       loaded.mrIID,
			Kind:        discussionPublishKind,
			PublishKey:  plan.key,
			GitlabID:    discussionID,
			WorkflowID:  workflowID,
			Fingerprint: loaded.record.Hash,
			BodyHash:    bodyHash(baseBody),
		})
		return err
	})
}

// updateDiscussionNote updates only the bot-authored first note of a thread.
func (p *Publisher) updateDiscussionNote(ctx context.Context, loaded *loadedReview, noteID int64, body string, recheck func() error) error {
	return sendOutbound(ctx, p.workspace.Get(loaded.project.ID), func(callCtx context.Context) error {
		if err := recheck(); err != nil {
			return err
		}
		return p.gitlab.UpdateMRNote(callCtx, loaded.token, loaded.project.ID, loaded.mrIID, noteID, body)
	})
}

// notePositionMatches compares a remote note anchor with the position key.
func notePositionMatches(position gitlab.Position, path string, line int, side review.Side) bool {
	if diffSide(side) == gitlab.SideOld {
		return position.OldLine == line &&
			(position.OldPath == path || (position.OldPath == "" && position.NewPath == path))
	}
	return position.NewLine == line &&
		(position.NewPath == path || (position.NewPath == "" && position.OldPath == path))
}

// workflowMarkerPrefix tags a discussion body with the workflow that sent it.
// Discussions from older versions lack it and stay on the conservative path.
const workflowMarkerPrefix = "<!-- gitlab-ce-review:workflow id="

func withWorkflowMarker(body, workflowID string) string {
	return strings.TrimRight(body, "\r\n") + "\n" + workflowMarkerPrefix + workflowID + " -->\n"
}

func normalizeDiscussionBody(body string) string {
	return strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
}

// discussionLookup reads the bot identity and the MR discussions at most once
// per publication run. Failures are not cached, so the next finding retries.
type discussionLookup struct {
	gl          GitLabClient
	token       string
	projectID   int64
	mrIID       int64
	userID      int64
	discussions []gitlab.Discussion
	listed      bool
	// claimed records the discussion IDs already bound to a position in this
	// run. One remote thread carries one position's body, so a second
	// position must fall back to the summary instead of overwriting it.
	claimed map[string]bool
}

// claim binds one discussion to the current position. It reports false when
// another position in this run already owns the thread.
func (l *discussionLookup) claim(discussionID string) bool {
	if discussionID == "" {
		return false
	}
	if l.claimed == nil {
		l.claimed = make(map[string]bool)
	}
	if l.claimed[discussionID] {
		return false
	}
	l.claimed[discussionID] = true
	return true
}

func (l *discussionLookup) isClaimed(discussionID string) bool {
	return l.claimed != nil && l.claimed[discussionID]
}

// prepare loads the bot identity and the discussion list once per run.
func (l *discussionLookup) prepare(ctx context.Context) error {
	if l.userID <= 0 {
		user, err := l.gl.CurrentUser(ctx, l.token)
		if err != nil || user == nil || user.ID <= 0 {
			return errors.Join(errors.New("app: current user unavailable for inline finding publication"), err)
		}
		l.userID = user.ID
	}
	if !l.listed {
		discussions, err := l.gl.ListDiscussions(ctx, l.token, l.projectID, l.mrIID)
		if err != nil {
			return err
		}
		l.discussions, l.listed = discussions, true
	}
	return nil
}

// findingDiscussionByID returns a recorded thread only when its first note is
// still this bot's finding note. A deleted, foreign, or rewritten thread is
// unproven and must not receive another duplicate at the same position.
func (l *discussionLookup) findingDiscussionByID(id string) (gitlab.Discussion, gitlab.Note, bool) {
	for _, discussion := range l.discussions {
		if discussion.ID != id {
			continue
		}
		if len(discussion.Notes) == 0 {
			return discussion, gitlab.Note{}, false
		}
		note := discussion.Notes[0]
		if note.System || note.AuthorID != l.userID {
			return discussion, note, false
		}
		if _, ok := discussionPublishKey(note.Body); !ok {
			return discussion, note, false
		}
		return discussion, note, true
	}
	return gitlab.Discussion{}, gitlab.Note{}, false
}

// findingDiscussionsAt lists bot finding threads anchored at one position,
// regardless of author, so a foreign thread can block a duplicate.
func (l *discussionLookup) findingDiscussionsAt(path string, line int, side review.Side) []gitlab.Discussion {
	var out []gitlab.Discussion
	for _, discussion := range l.discussions {
		if len(discussion.Notes) == 0 {
			continue
		}
		note := discussion.Notes[0]
		if note.System || note.Position == nil {
			continue
		}
		if _, ok := discussionPublishKey(note.Body); !ok {
			continue
		}
		if !notePositionMatches(*note.Position, path, line, side) {
			continue
		}
		out = append(out, discussion)
	}
	return out
}

// rememberNoteBody keeps the per-run listing in sync after an in-place update
// so lifecycle reconciliation matches the new marker instead of resolving it.
func (l *discussionLookup) rememberNoteBody(discussionID string, noteID int64, body string) {
	for i := range l.discussions {
		if l.discussions[i].ID != discussionID {
			continue
		}
		for j := range l.discussions[i].Notes {
			if l.discussions[i].Notes[j].ID == noteID {
				l.discussions[i].Notes[j].Body = body
			}
		}
	}
}

// provenOurs reports whether the remote discussion was authored by the current
// user and carries exactly the intended body, including this workflow's
// marker. A read failure is returned so the caller retries instead of posting
// a duplicate; a mismatch is a clean false.
func (l *discussionLookup) provenOurs(ctx context.Context, discussionID, wantBody string) (bool, error) {
	if l.userID <= 0 {
		user, err := l.gl.CurrentUser(ctx, l.token)
		if err != nil || user == nil || user.ID <= 0 {
			return false, errors.Join(errors.New("app: current user unavailable for discussion adoption"), err)
		}
		l.userID = user.ID
	}
	if !l.listed {
		discussions, err := l.gl.ListDiscussions(ctx, l.token, l.projectID, l.mrIID)
		if err != nil {
			return false, err
		}
		l.discussions, l.listed = discussions, true
	}
	want := normalizeDiscussionBody(wantBody)
	for _, discussion := range l.discussions {
		if discussion.ID != discussionID {
			continue
		}
		if len(discussion.Notes) == 0 || discussion.Notes[0].System || discussion.Notes[0].AuthorID != l.userID {
			return false, nil
		}
		return normalizeDiscussionBody(discussion.Notes[0].Body) == want, nil
	}
	return false, nil
}

func (p *Publisher) recoverDiscussion(ctx context.Context, token string, projectID, mrIID int64, key string) (string, error) {
	id, _, err := p.recoverDiscussionBody(ctx, token, projectID, mrIID, key)
	if errors.Is(err, errDiscussionAbsent) {
		return "", errDiscussionUnknown
	}
	return id, err
}

// recoverDiscussionBody also returns the matched note body so the caller can
// prove content without a second listing.
func (p *Publisher) recoverDiscussionBody(ctx context.Context, token string, projectID, mrIID int64, key string) (string, string, error) {
	user, err := p.gitlab.CurrentUser(ctx, token)
	if err != nil || user == nil || user.ID <= 0 {
		return "", "", errors.Join(errDiscussionUnknown, err)
	}
	discussions, err := p.gitlab.ListDiscussions(ctx, token, projectID, mrIID)
	if err != nil {
		return "", "", errors.Join(errDiscussionUnknown, err)
	}
	marker := fmt.Sprintf("%s key=%s -->", review.FindingMarkerPrefix, key)
	id, body := "", ""
	for _, discussion := range discussions {
		if len(discussion.Notes) == 0 || discussion.Notes[0].System || discussion.Notes[0].AuthorID != user.ID ||
			strings.SplitN(discussion.Notes[0].Body, "\n", 2)[0] != marker {
			continue
		}
		if id != "" || strings.TrimSpace(discussion.ID) == "" || discussion.Notes[0].ID <= 0 {
			return "", "", errDiscussionUnknown
		}
		id, body = discussion.ID, discussion.Notes[0].Body
	}
	if id == "" {
		return "", "", errDiscussionAbsent
	}
	return id, body, nil
}

// discussionAbsentProven reports that an old publication's empty marker scan
// is conclusive. A fresh attempt keeps the deadline fence.
func (p *Publisher) discussionAbsentProven(loaded *loadedReview) bool {
	if loaded == nil || loaded.record == nil || loaded.record.UpdatedAt.IsZero() {
		return false
	}
	return p.now().Sub(loaded.record.UpdatedAt) >= discussionAbsentProofAge
}

// discussionResolver is the optional GitLab capability for converging old
// inline threads. A client without it keeps publication read-only.
type discussionResolver interface {
	ResolveDiscussion(ctx context.Context, token string, projectID, iid int64, discussionID string, resolved bool) error
}

// maxDiscussionLifecycleCalls bounds resolve/unresolve work per publication.
const maxDiscussionLifecycleCalls = 50

// discussionAbsentProofAge is how old a publication must be before an empty
// marker scan counts as proof that no remote discussion exists. Immediate
// retries stay conservative because GitLab reads can lag behind a write whose
// response was lost (#457).
const discussionAbsentProofAge = 24 * time.Hour

// discussionResolution is one desired remote thread state.
type discussionResolution struct {
	ID       string
	Resolved bool
}

// discussionScope bounds which absence a review may prove. A complete review
// covers every file, so it may resolve any stale thread. An incremental review
// re-reviewed only some files: its absence evidence covers those files alone,
// and a thread anchored elsewhere must stay open (#515).
type discussionScope struct {
	complete bool
	files    map[string]struct{}
}

// resolves reports whether a thread anchored at position may be resolved
// because its finding is missing from this review. A thread whose anchor is
// unknown cannot be attributed to a re-reviewed file and stays open.
func (s discussionScope) resolves(position *gitlab.Position) bool {
	if s.complete {
		return true
	}
	if position == nil {
		return false
	}
	if _, ok := s.files[position.NewPath]; ok {
		return true
	}
	_, ok := s.files[position.OldPath]
	return ok
}

// discussionTargets decides which bot discussions must converge. A finding
// that is still present but resolved is reopened; a discussion whose finding
// no longer appears in a review that proves the absence is resolved. An
// incomplete review proves nothing about missing findings, so it never
// resolves threads (#457); an incremental review proves absence only for the
// files in its scope (#515).
func discussionTargets(discussions []gitlab.Discussion, botID int64, currentKeys map[string]struct{}, scope discussionScope) []discussionResolution {
	if (!scope.complete && scope.files == nil) || botID <= 0 {
		return nil
	}
	out := make([]discussionResolution, 0, len(discussions))
	for _, discussion := range discussions {
		if strings.TrimSpace(discussion.ID) == "" || len(discussion.Notes) == 0 {
			continue
		}
		note := discussion.Notes[0]
		if note.System || note.AuthorID != botID {
			continue
		}
		key, ok := discussionPublishKey(note.Body)
		if !ok {
			continue
		}
		_, present := currentKeys[key]
		switch {
		case present && note.Resolved:
			out = append(out, discussionResolution{ID: discussion.ID, Resolved: false})
		case !present && !note.Resolved && scope.resolves(note.Position):
			out = append(out, discussionResolution{ID: discussion.ID, Resolved: true})
		}
	}
	return out
}

// discussionPublishKey parses the hidden finding key from the first note line.
func discussionPublishKey(body string) (string, bool) {
	line := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
	if !strings.HasPrefix(line, review.FindingMarkerPrefix) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, review.FindingMarkerPrefix))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "key="))
	rest = strings.TrimSpace(strings.TrimSuffix(rest, "-->"))
	if len(rest) != len("pk_")+32 || !strings.HasPrefix(rest, "pk_") {
		return "", false
	}
	return rest, true
}

// reconcileDiscussions converges bot-authored inline threads after a review.
// An incremental review only resolves threads anchored to the files it
// re-reviewed (#515). Failures are logged and never fail the publication; only
// a policy cancellation or an expired generation stops it.
func (p *Publisher) reconcileDiscussions(ctx context.Context, loaded *loadedReview, groups []review.Group, index *gitlab.DiffIndex, verdict string, lookup *discussionLookup, recheck func() error) error {
	resolver, ok := p.gitlab.(discussionResolver)
	if !ok {
		return nil
	}
	if verdict != review.VerdictPass && verdict != review.VerdictBlocking {
		return nil
	}
	if lookup == nil {
		lookup = &discussionLookup{gl: p.gitlab, token: loaded.token, projectID: loaded.project.ID, mrIID: loaded.mrIID}
	}
	if lookup.userID <= 0 {
		user, err := p.gitlab.CurrentUser(ctx, loaded.token)
		if err != nil || user == nil || user.ID <= 0 {
			p.logger.Warn("discussion lifecycle identity unavailable", "project_id", loaded.project.ID, "error_class", errclass.Of(err))
			return nil
		}
		lookup.userID = user.ID
	}
	if !lookup.listed {
		discussions, err := p.gitlab.ListDiscussions(ctx, loaded.token, loaded.project.ID, loaded.mrIID)
		if err != nil {
			p.logger.Warn("discussion lifecycle listing failed", "project_id", loaded.project.ID, "error_class", errclass.Of(err))
			return nil
		}
		lookup.discussions, lookup.listed = discussions, true
	}
	// A position key may only stand in for the group's key when a position
	// thread actually exists. A fallback run whose remote thread still carries
	// the fingerprint marker must keep that fingerprint key, or reconciliation
	// would resolve a live discussion (#487).
	live := make(map[string]struct{}, len(lookup.discussions))
	remoteMarkers := make(map[string]struct{}, len(lookup.discussions))
	for _, discussion := range lookup.discussions {
		if discussion.ID != "" {
			live[discussion.ID] = struct{}{}
		}
		if len(discussion.Notes) == 0 {
			continue
		}
		if markerKey, ok := discussionPublishKey(discussion.Notes[0].Body); ok {
			remoteMarkers[markerKey] = struct{}{}
		}
	}
	current := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if !group.Published() {
			continue
		}
		key := group.PublishKey
		positionKey := review.PositionPublishKey(group.Path, group.Line, group.Side)
		// Locate even when this run fell back to the summary: the anchor still
		// names the thread that must stay current.
		if index != nil {
			if anchor, ok := index.LocateUnambiguous(group.Path, group.Line, diffSide(group.Side)); ok {
				// Use the same canonical anchor identity as collection and
				// rendering, so a context-line thread is never resolved just
				// because the finding was reported on the other side.
				positionKey = review.PositionAnchorKey(group.Path, group.Line, group.Side, anchor)
			}
		}
		if positionKey != "" && p.positionThreadLive(ctx, loaded, positionKey, remoteMarkers, live) {
			// A position thread is current even when this run only kept the
			// finding in the summary; resolving it would hide the discussion.
			key = positionKey
		}
		if key == "" {
			key = review.PublishKeyFor(group.Path, group.Title, group.Evidence)
		}
		if key != "" {
			current[key] = struct{}{}
		}
	}
	scope := discussionScope{complete: true}
	if incremental := loaded.dossier.Incremental; incremental != nil && !incremental.Full {
		scope = discussionScope{files: make(map[string]struct{}, len(incremental.Files))}
		for _, file := range incremental.Files {
			scope.files[file] = struct{}{}
		}
	}
	for i, target := range discussionTargets(lookup.discussions, lookup.userID, current, scope) {
		if i >= maxDiscussionLifecycleCalls {
			break
		}
		if err := recheck(); err != nil {
			return err
		}
		resolved := target.Resolved
		err := sendOutbound(ctx, p.workspace.Get(loaded.project.ID), func(callCtx context.Context) error {
			return resolver.ResolveDiscussion(callCtx, loaded.token, loaded.project.ID, loaded.mrIID, target.ID, resolved)
		})
		if err != nil {
			if errors.Is(err, workspace.ErrOutboundInvalidated) || ctx.Err() != nil {
				return err
			}
			p.logger.Warn("discussion lifecycle update failed",
				"project_id", loaded.project.ID, "mr_iid", loaded.mrIID, "error_class", errclass.Of(err))
		}
	}
	return nil
}

// positionThreadLive reports whether a position thread exists for this run: a
// remote thread already carries the position marker, or the recorded position
// publication still points at a visible discussion. Without either, the
// finding's remote thread is still the older fingerprint thread and its
// fingerprint key must stay current.
func (p *Publisher) positionThreadLive(ctx context.Context, loaded *loadedReview, positionKey string, remoteMarkers, live map[string]struct{}) bool {
	if _, ok := remoteMarkers[positionKey]; ok {
		return true
	}
	publication, err := p.store.GetPublication(ctx, loaded.project.ID, loaded.mrIID, discussionPublishKind, positionKey)
	if err != nil || publication.GitlabID == "" {
		return false
	}
	_, ok := live[publication.GitlabID]
	return ok
}

// commitStatusPublishKey records the effective state with the SHA. GitLab
// stores each status as a separate object, so the state belongs to the
// idempotency key. internal/server parses this format for the admin detail.
func commitStatusPublishKey(sha, state string) string {
	return "status:" + sha + ":" + state
}

func (p *Publisher) publishCommitStatus(ctx context.Context, loaded *loadedReview, state, description string, recheck func() error) error {
	// A Reporter-level shared bot cannot create commit statuses. The effective
	// config disables this path; this guard also prevents a publication row for
	// a status that was never written.
	if sharedCredentialProject(loaded.project) {
		return nil
	}
	status := gitlab.CommitStatus{
		State: state, Name: "ai-review", TargetURL: p.detailURL(loaded.workflow.ID), Description: description,
	}
	sha := loaded.dossier.DiffVersion.HeadSHA
	if err := p.setCommitStatus(ctx, loaded.token, loaded.project.ID, sha, status, recheck); err != nil {
		return err
	}
	_, err := p.store.UpsertPublication(ctx, store.Publication{
		ProjectID: loaded.project.ID, MRIID: loaded.mrIID, Kind: statusPublishKind,
		PublishKey: commitStatusPublishKey(sha, state), GitlabID: status.Name,
		WorkflowID: loaded.workflow.ID, Fingerprint: loaded.record.Hash,
	})
	return err
}

// sharedCredentialProjectState looks up the sentinel predicate for callers
// that only have a project ID, so no commit status request can be issued for a
// shared project even through a direct path.
func (p *Publisher) sharedCredentialProjectState(ctx context.Context, projectID int64) bool {
	if p == nil || p.store == nil {
		return false
	}
	project, err := p.store.GetProject(ctx, projectID)
	return err == nil && sharedCredentialProject(project)
}

func (p *Publisher) setCommitStatus(ctx context.Context, token string, projectID int64, sha string, status gitlab.CommitStatus, recheck func() error) error {
	if p.sharedCredentialProjectState(ctx, projectID) {
		return nil
	}
	return p.retry(ctx, func() error {
		if err := recheck(); err != nil {
			return err
		}
		return sendOutbound(ctx, p.workspace.Get(projectID), func(ctx context.Context) error {
			return p.gitlab.SetCommitStatus(ctx, token, projectID, sha, status)
		})
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
		if IsPermanentJob(err) || gitlab.StatusOf(err) == 400 || gitlab.StatusOf(err) == 404 || errors.Is(err, workspace.ErrOutboundInvalidated) ||
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
			_, ok := index.LocateUnambiguous(group.Path, group.Line, diffSide(group.Side))
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
	safeOutput, err := review.SafeDiagnosticOutput(selected.Spec.Kind, selected.Output, snapshot)
	if err != nil {
		return review.SynthesisResult{}, fmt.Errorf("app: sanitize synthesis result: %w", err)
	}
	if err := json.Unmarshal(safeOutput, &result); err != nil {
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
