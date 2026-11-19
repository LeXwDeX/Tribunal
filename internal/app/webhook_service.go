package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

const (
	commandRun    = "run"
	commandCancel = "cancel"
	commandHelp   = "help"
)

var shaRE = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// webhookRejectionNamespace derives a separate delivery UUID for rejected
// requests. A wrong secret must not consume the UUID of the genuine delivery,
// which would suppress it as a duplicate.
var webhookRejectionNamespace = uuid.MustParse("5b6c0a0e-9f6e-4c33-8a7e-0d4e1f2a3b6c")

const (
	// deliveryEventOther replaces an unrecognized X-Gitlab-Event header on a
	// rejected delivery. The header is unauthenticated and may be almost as
	// large as the server header limit (#522).
	deliveryEventOther = "other"
	// rejectedDeliveryBucket bounds persisted rejections to one row per
	// project per bucket. The caller controls the event UUID, so keying the row
	// by it would let a wrong-secret flood fill the single SQLite connection
	// (#522).
	rejectedDeliveryBucket = time.Minute
)

// WebhookStore is the persistence surface needed by WebhookService.
type WebhookStore interface {
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error)
	RecordDelivery(ctx context.Context, delivery store.Delivery, job *store.Job) (string, bool, error)
	RecordReviewCandidateDelivery(ctx context.Context, delivery store.Delivery, candidate *store.ReviewCandidate) (store.ReviewEnqueueResult, error)
}

// WebhookRequest is the authenticated HTTP request metadata plus body.
type WebhookRequest struct {
	Event string
	Token string
	UUID  string
	Body  []byte
}

// WebhookResult describes the response decision.
type WebhookResult struct {
	Unauthorized bool
	Accepted     bool
	JobID        string
	Ignored      string
}

// WebhookService validates and persists GitLab webhook deliveries. It never
// calls GitLab or any other remote service.
type WebhookService struct {
	store          WebhookStore
	workspace      *workspace.Manager
	cipher         *secret.Cipher
	profiles       map[string]config.ProfileConfig
	reviewDefaults config.ProjectSettings
	botUsernames   map[string]bool
	// mentionTargets are the lowercased bot_usernames entries in configured
	// order. An empty list disables @-mention wake-ups (fail-safe).
	mentionTargets []string
	logger         *slog.Logger
	now            func() time.Time
}

// WebhookServiceConfig configures WebhookService.
type WebhookServiceConfig struct {
	Store          WebhookStore
	Workspace      *workspace.Manager
	Cipher         *secret.Cipher
	Profiles       map[string]config.ProfileConfig
	ReviewDefaults config.ProjectSettings
	BotUsernames   []string
	Logger         *slog.Logger
	Now            func() time.Time
}

// NewWebhookService builds the webhook ingress service.
func NewWebhookService(cfg WebhookServiceConfig) (*WebhookService, error) {
	if cfg.Store == nil || cfg.Cipher == nil || cfg.Workspace == nil {
		return nil, errors.New("app: webhook service dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	bots := make(map[string]bool, len(cfg.BotUsernames))
	var targets []string
	seenTargets := make(map[string]bool, len(cfg.BotUsernames))
	for _, raw := range cfg.BotUsernames {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		bots[name] = true
		lower := strings.ToLower(name)
		if !seenTargets[lower] {
			seenTargets[lower] = true
			targets = append(targets, lower)
		}
	}
	return &WebhookService{
		store:          cfg.Store,
		workspace:      cfg.Workspace,
		cipher:         cfg.Cipher,
		profiles:       cfg.Profiles,
		reviewDefaults: cfg.ReviewDefaults,
		botUsernames:   bots,
		mentionTargets: targets,
		logger:         logger,
		now:            now,
	}, nil
}

// Handle authenticates and records one delivery. A storage error is returned
// with an ignored result and no delivery record, so the HTTP layer answers 503
// and a resend with the same event UUID is accepted.
func (s *WebhookService) Handle(ctx context.Context, req WebhookRequest) (WebhookResult, error) {
	payload, err := gitlab.ParseWebhook(req.Event, req.UUID, req.Token, req.Body)
	if err != nil {
		return WebhookResult{Ignored: "bad_request"}, nil
	}
	projectID, reason := webhookProjectID(payload)
	if reason != "" {
		return WebhookResult{Ignored: reason}, nil
	}
	// Check that the project is registered before creating any per-project
	// state: the project ID comes from an unauthenticated body, so an unknown ID
	// must not leave a workspace entry behind for the life of the process
	// (#511). Only the rejection reason is used here; the project is re-read
	// inside the lifecycle read lock below.
	_, reason, err = s.lookupProject(ctx, projectID)
	if reason != "" {
		return WebhookResult{Ignored: reason}, err
	}

	unlock, err := s.workspace.Get(projectID).LifecycleRLock(ctx)
	if err != nil {
		return WebhookResult{Ignored: "storage_error"}, err
	}
	defer unlock()

	// Project deletion takes the lifecycle write lock, so a project that was
	// registered before the lock may be gone now. Re-read it inside the lock.
	project, reason, err := s.lookupProject(ctx, projectID)
	if reason != "" {
		return WebhookResult{Ignored: reason}, err
	}

	eventUUID := strings.TrimSpace(req.UUID)
	var parsedUUID uuid.UUID
	if eventUUID != "" {
		parsedUUID, err = uuid.Parse(eventUUID)
		if err != nil {
			parsedUUID = uuid.Nil
		}
	}
	mrIID := webhookMRIID(req.Event, payload)
	if !s.validToken(project, req.Token) {
		event := deliveryEventName(req.Event)
		s.logger.Warn("webhook authentication failed", "project_id", project.ID, "event", event)
		// Record the rejection when the envelope is identifiable. The delivery
		// table then shows that GitLab called with a mismatched secret instead
		// of reporting a silent green webhook (#457).
		if parsedUUID != uuid.Nil {
			s.recordRejected(ctx, project.ID, event, mrIID, "invalid_secret")
		}
		return WebhookResult{Unauthorized: true}, nil
	}
	if eventUUID == "" {
		return WebhookResult{Ignored: "missing_event_uuid"}, nil
	}
	if parsedUUID == uuid.Nil {
		return WebhookResult{Ignored: "invalid_event_uuid"}, nil
	}
	if reason := checkEventKind(req.Event, payload.ObjectKind); reason != "" {
		return s.recordIgnored(ctx, project.ID, req.Event, parsedUUID.String(), mrIID, reason)
	}
	// The stored default branch decides below which events are reviewed and the
	// ingress cannot ask GitLab, so a payload that reports another default
	// branch queues the refresh that verifies and adopts it (#604). It is
	// recorded first: a queued push refresh from this same event replaces it
	// and still synchronizes the default when it runs.
	s.requestDefaultBranchRefresh(ctx, project, payload)
	// Ingress only records the delivery and the generation bump; it performs no
	// remote call and must finish inside the 900ms HTTP processing budget, so it
	// deliberately does not take the per-MR lock. The remote note order is kept
	// by every note writer holding that lock and rechecking the generation
	// inside it (#475).

	settings, err := ResolveSettings(project.Settings, s.reviewDefaults)
	if err != nil {
		s.logger.Warn("stored project settings are invalid", "project_id", project.ID)
		return s.recordIgnored(ctx, project.ID, req.Event, parsedUUID.String(), mrIID, "invalid_settings")
	}

	job, reason := s.decide(ctx, project, settings, payload)
	if reason != "" {
		return s.recordIgnored(ctx, project.ID, req.Event, parsedUUID.String(), mrIID, reason)
	}
	return s.recordJob(ctx, project.ID, req.Event, parsedUUID.String(), mrIID, job, nil)
}

func (s *WebhookService) validToken(project *store.Project, presented string) bool {
	if presented == "" || len(project.WebhookSecretEnc) == 0 {
		return false
	}
	expected, err := s.cipher.Open(project.WebhookSecretEnc, secret.WebhookSecretAAD(project.ID))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expected, []byte(presented)) == 1
}

// lookupProject reads a registered, enabled project. A non-empty reason is a
// fixed ignored reason for a clean rejection; a non-nil error is a storage
// failure. It never creates workspace state.
func (s *WebhookService) lookupProject(ctx context.Context, projectID int64) (*store.Project, string, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "project_not_registered", nil
		}
		s.logger.Warn("webhook project lookup failed", "project_id", projectID)
		return nil, "storage_error", err
	}
	if project == nil || !project.Enabled {
		return nil, "project_not_registered", nil
	}
	return project, "", nil
}

// deliveryEventName bounds the event name stored for a rejected delivery. Only
// events the ingress understands are kept; any other unauthenticated header
// value collapses to a fixed marker (#522).
func deliveryEventName(event string) string {
	switch event {
	case "Merge Request Hook", "Note Hook", "Push Hook":
		return event
	}
	return deliveryEventOther
}

// recordRejected persists a delivery that failed authentication. It never
// returns an error: the HTTP answer stays the fixed unauthorized response so a
// caller cannot distinguish storage from authentication failure.
//
// The row key is derived from the project and the current minute bucket rather
// than the caller-controlled event UUID, so a wrong-secret flood costs at most
// one row per project per minute; later rejections collapse into the first one
// through the delivery primary key (#522).
func (s *WebhookService) recordRejected(ctx context.Context, projectID int64, event string, mrIID int64, reason string) {
	now := s.now().UTC()
	bucket := now.Truncate(rejectedDeliveryBucket).Format(time.RFC3339)
	rejectedUUID := uuid.NewSHA1(webhookRejectionNamespace, []byte(itoa(projectID)+":"+bucket)).String()
	if _, _, err := s.store.RecordDelivery(ctx, store.Delivery{
		UUID: rejectedUUID, ProjectID: projectID, MRIID: mrIID, Event: event,
		Outcome: store.DeliveryRejected, Reason: reason, ReceivedAt: now,
	}, nil); err != nil {
		s.logger.Warn("rejected delivery persistence failed", "project_id", projectID, "event", event, "error_class", errclass.Of(err))
	}
}

func (s *WebhookService) recordIgnored(ctx context.Context, projectID int64, event, eventUUID string, mrIID int64, reason string) (WebhookResult, error) {
	_, duplicate, err := s.store.RecordDelivery(ctx, store.Delivery{
		UUID:       eventUUID,
		ProjectID:  projectID,
		MRIID:      mrIID,
		Event:      event,
		Outcome:    store.DeliveryIgnored,
		Reason:     reason,
		ReceivedAt: s.now().UTC(),
	}, nil)
	if err != nil {
		s.logger.Warn("webhook delivery persistence failed", "project_id", projectID, "event", event)
		return WebhookResult{Ignored: "storage_error"}, err
	}
	if duplicate {
		return WebhookResult{Ignored: "duplicate"}, nil
	}
	return WebhookResult{Ignored: reason}, nil
}

// recordJob persists the delivery and job. A non-nil expect makes the enqueue
// conditional on the review control (see store.ReviewControlExpect); a changed
// control yields Ignored "control_changed" and writes nothing.
func (s *WebhookService) recordJob(ctx context.Context, projectID int64, event, eventUUID string, mrIID int64, job *store.Job, expect *store.ReviewControlExpect) (WebhookResult, error) {
	delivery := store.Delivery{
		UUID:       eventUUID,
		ProjectID:  projectID,
		MRIID:      mrIID,
		Event:      event,
		Outcome:    store.DeliveryEnqueued,
		ReceivedAt: s.now().UTC(),
	}
	var (
		jobID     string
		duplicate bool
		err       error
	)
	if job.Kind == "review.candidate" {
		// A candidate owns its durable sequence; falling back to the generic
		// delivery would enqueue a payload that can never activate.
		candidate, ok := reviewCandidateFromJob(job)
		if !ok {
			s.logger.Warn("webhook candidate job is invalid", "project_id", projectID, "event", event)
			return WebhookResult{Ignored: "storage_error"}, errors.New("app: invalid review.candidate job")
		}
		candidate.Expect = expect
		var result store.ReviewEnqueueResult
		result, err = s.store.RecordReviewCandidateDelivery(ctx, delivery, candidate)
		jobID, duplicate = result.JobID, result.Duplicate
		if err == nil && result.Skipped {
			return WebhookResult{Ignored: "control_changed"}, nil
		}
	} else if command, ok := reviewCommandFromJob(job); ok {
		command.Expect = expect
		if recorder, supported := s.store.(interface {
			RecordReviewDelivery(ctx context.Context, d store.Delivery, command *store.ReviewCommand) (store.ReviewEnqueueResult, error)
		}); supported {
			var result store.ReviewEnqueueResult
			result, err = recorder.RecordReviewDelivery(ctx, delivery, command)
			jobID, duplicate = result.JobID, result.Duplicate
			if err == nil && result.Skipped {
				if command.Expect != nil {
					return WebhookResult{Ignored: "control_changed"}, nil
				}
				// A close/merge with nothing to cancel neither creates review
				// state nor a cancellation comment (#453).
				return WebhookResult{Ignored: "nothing_to_cancel"}, nil
			}
		} else {
			jobID, duplicate, err = s.store.RecordDelivery(ctx, delivery, job)
		}
	} else {
		jobID, duplicate, err = s.store.RecordDelivery(ctx, delivery, job)
	}
	if err != nil {
		s.logger.Warn("webhook enqueue failed", "project_id", projectID, "event", event, "kind", job.Kind)
		return WebhookResult{Ignored: "storage_error"}, err
	}
	if duplicate {
		return WebhookResult{Ignored: "duplicate"}, nil
	}
	s.logger.Info("webhook accepted", "project_id", projectID, "event", event, "kind", job.Kind, "job_id", jobID)
	return WebhookResult{Accepted: true, JobID: jobID}, nil
}

// reviewCandidateFromJob extracts the deferred review event from a candidate
// job. The store allocates its sequence and writes the durable payload.
func reviewCandidateFromJob(job *store.Job) (*store.ReviewCandidate, bool) {
	if job == nil || job.Kind != "review.candidate" {
		return nil, false
	}
	var fields struct {
		ProjectID int64  `json:"project_id"`
		MRIID     int64  `json:"mr_iid"`
		HeadSHA   string `json:"head_sha"`
	}
	if err := json.Unmarshal(job.Payload, &fields); err != nil {
		return nil, false
	}
	if fields.ProjectID <= 0 || fields.MRIID <= 0 {
		return nil, false
	}
	return &store.ReviewCandidate{
		ProjectID:   fields.ProjectID,
		MRIID:       fields.MRIID,
		HeadSHA:     fields.HeadSHA,
		Payload:     job.Payload,
		MaxAttempts: job.MaxAttempts,
	}, true
}

func webhookProjectID(payload *gitlab.Webhook) (int64, string) {
	fromProject := payload.Project.ID
	fromRoot := payload.ProjectID
	if fromProject > 0 && fromRoot > 0 && fromProject != fromRoot {
		return 0, "project_mismatch"
	}
	id := fromProject
	if id <= 0 {
		id = fromRoot
	}
	if id <= 0 {
		return 0, "bad_request"
	}
	return id, ""
}

// reviewCommandFromJob extracts the generation-aware review command fields
// from a review job. The store assigns the generation inside the enqueue
// transaction.
func reviewCommandFromJob(job *store.Job) (*store.ReviewCommand, bool) {
	if job == nil || (job.Kind != "review.run" && job.Kind != "review.cancel") {
		return nil, false
	}
	var fields struct {
		ProjectID int64  `json:"project_id"`
		MRIID     int64  `json:"mr_iid"`
		HeadSHA   string `json:"head_sha"`
	}
	if err := json.Unmarshal(job.Payload, &fields); err != nil {
		return nil, false
	}
	if fields.ProjectID <= 0 || fields.MRIID <= 0 {
		return nil, false
	}
	command := &store.ReviewCommand{
		ProjectID:   fields.ProjectID,
		MRIID:       fields.MRIID,
		HeadSHA:     fields.HeadSHA,
		Cancel:      job.Kind == "review.cancel",
		Payload:     job.Payload,
		MaxAttempts: job.MaxAttempts,
	}
	return command, true
}

func checkEventKind(event, objectKind string) string {
	var expected string
	switch event {
	case "Merge Request Hook":
		expected = "merge_request"
	case "Note Hook":
		expected = "note"
	case "Push Hook":
		expected = "push"
	default:
		return "unsupported_event"
	}
	if objectKind != expected {
		return "event_mismatch"
	}
	return ""
}

// enqueueCatchUpRefresh records one workspace refresh for a catch-up scan that
// enqueued a review. It is best effort: a failed enqueue is logged by class and
// never fails the scan.
func (s *WebhookService) enqueueCatchUpRefresh(ctx context.Context, project *store.Project, head string) {
	if project == nil || project.ID <= 0 || !shaRE.MatchString(head) {
		return
	}
	branch := strings.TrimSpace(project.DefaultBranch)
	if branch == "" {
		return
	}
	eventUUID := uuid.NewSHA1(catchUpNamespace, []byte("refresh:"+itoa(project.ID)+":"+head)).String()
	job := newJob("workspace.refresh", "ws:"+itoa(project.ID), map[string]any{
		"project_id": project.ID,
		"ref":        "refs/heads/" + branch,
		"branch":     branch,
		"after_sha":  head,
		"trigger":    catchUpTrigger,
	})
	if _, _, err := s.store.RecordDelivery(ctx, store.Delivery{
		UUID: eventUUID, ProjectID: project.ID, Event: catchUpRefreshEvent,
		Outcome: store.DeliveryEnqueued, ReceivedAt: s.now().UTC(),
	}, job); err != nil {
		s.logger.Warn("catch-up workspace refresh enqueue failed", "project_id", project.ID, "error_class", errclass.Of(err))
	}
}

func (s *WebhookService) decide(ctx context.Context, project *store.Project, settings config.ProjectSettings, payload *gitlab.Webhook) (*store.Job, string) {
	switch payload.Event {
	case "Merge Request Hook":
		return s.decideMergeRequest(ctx, project, settings, payload)
	case "Note Hook":
		return s.decideNote(project, settings, payload)
	case "Push Hook":
		return s.decidePush(project, settings, payload)
	default:
		return nil, "unsupported_event"
	}
}

func (s *WebhookService) decideMergeRequest(ctx context.Context, project *store.Project, settings config.ProjectSettings, payload *gitlab.Webhook) (*store.Job, string) {
	attrs := payload.ObjectAttributes
	iid := attrs.IID
	if iid <= 0 {
		iid = payload.MergeRequest.IID
	}
	if iid <= 0 {
		return nil, "bad_request"
	}
	targetProjectID := attrs.TargetProjectID
	if targetProjectID <= 0 {
		targetProjectID = payload.MergeRequest.TargetProjectID
	}
	if targetProjectID != project.ID {
		return nil, "project_mismatch"
	}
	action := strings.ToLower(strings.TrimSpace(attrs.Action))
	switch action {
	case "close", "merge":
		key := reviewCancelKey(project.ID, iid)
		return newJob("review.cancel", key, map[string]any{
			"project_id": project.ID,
			"mr_iid":     iid,
			"trigger":    "webhook",
		}), ""
	case "open", "reopen":
		return s.newReviewJob(project, settings, payload, iid, "mr_open", true)
	case "update":
		// GitLab omits oldrev on some update events (for example a draft to
		// ready transition). Compare the head with the durable review control
		// and re-run when the head changed or a filter transition made the MR
		// eligible (#457).
		if strings.TrimSpace(attrs.OldRev) == "" {
			if reason := s.reevaluateUpdateWithoutOldRev(ctx, project, settings, payload, iid); reason != "" {
				return nil, reason
			}
		}
		return s.newReviewJob(project, settings, payload, iid, "mr_update", false)
	default:
		return nil, "unsupported_action"
	}
}

// reevaluateUpdateWithoutOldRev decides an update whose oldrev GitLab omitted.
// It returns "" when the event must be reviewed and an ignored reason
// otherwise. An unprovable decision keeps the previous drop behavior.
func (s *WebhookService) reevaluateUpdateWithoutOldRev(ctx context.Context, project *store.Project, settings config.ProjectSettings, payload *gitlab.Webhook, iid int64) string {
	attrs := payload.ObjectAttributes
	head := strings.ToLower(strings.TrimSpace(attrs.LastCommit.ID))
	if head == "" {
		head = strings.ToLower(strings.TrimSpace(attrs.SHA))
	}
	if !shaRE.MatchString(head) {
		return "missing_head_sha"
	}
	control, err := s.store.GetReviewControl(ctx, project.ID, iid)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Warn("review control lookup failed for update", "project_id", project.ID, "mr_iid", iid)
		return "missing_oldrev"
	}
	if err != nil || control == nil || control.HeadSHA == "" || !strings.EqualFold(control.HeadSHA, head) {
		// The head moved (or was never reviewed): this update is review-relevant.
		return ""
	}
	if updateFilterTransition(payload.Changes, project, settings) {
		return ""
	}
	return "missing_oldrev"
}

// updateFilterTransition reports a change that can turn a previously filtered
// merge request eligible. Only an explicit previous/current pair counts.
func updateFilterTransition(changes gitlab.WebhookChanges, project *store.Project, settings config.ProjectSettings) bool {
	if changes.Draft != nil && changes.Draft.Previous && !changes.Draft.Current {
		return true
	}
	if changes.Labels != nil &&
		!LabelsAllowed(changes.Labels.Previous, settings.LabelsInclude, settings.LabelsExclude) &&
		LabelsAllowed(changes.Labels.Current, settings.LabelsInclude, settings.LabelsExclude) {
		return true
	}
	if changes.TargetBranch != nil &&
		!BranchAllowed(changes.TargetBranch.Previous, project.DefaultBranch, settings.TargetBranches) &&
		BranchAllowed(changes.TargetBranch.Current, project.DefaultBranch, settings.TargetBranches) {
		return true
	}
	return false
}

func (s *WebhookService) newReviewJob(project *store.Project, settings config.ProjectSettings, payload *gitlab.Webhook, iid int64, trigger string, full bool) (*store.Job, string) {
	attrs := payload.ObjectAttributes
	if attrs.State != "" && !isOpenState(attrs.State) {
		return nil, "mr_closed"
	}
	if payload.MergeRequest.State != "" && !isOpenState(payload.MergeRequest.State) {
		return nil, "mr_closed"
	}
	sha := strings.TrimSpace(attrs.LastCommit.ID)
	if sha == "" {
		sha = strings.TrimSpace(attrs.SHA)
	}
	if sha == "" {
		sha = strings.TrimSpace(payload.MergeRequest.LastCommit.ID)
	}
	if !shaRE.MatchString(sha) {
		return nil, "missing_head_sha"
	}
	if !settings.AllowRepoConfig && settings.SkipDraft && isDraft(attrs, payload.MergeRequest) {
		// Repository config may disable skip_draft. With repo config enabled
		// the worker re-checks after reading the target branch.
		return nil, "draft"
	}
	labels := mergeLabels(attrs.Labels, payload.MergeRequest.Labels, payload.Labels)
	if !LabelsAllowed(labels, settings.LabelsInclude, settings.LabelsExclude) {
		return nil, "labels_filtered"
	}
	branch := attrs.TargetBranch
	if branch == "" {
		branch = payload.MergeRequest.TargetBranch
	}
	if !BranchAllowed(branch, project.DefaultBranch, settings.TargetBranches) {
		return nil, "branch_filtered"
	}
	profile := ProfileForLabels(labels, project.Profile, settings)
	if !isReviewProfile(s.profiles, profile) {
		profile = config.ReviewProfile
	}
	if settings.AllowRepoConfig {
		// The effective target-branch configuration can reject the event. A
		// deferred candidate avoids fencing the active review until the worker
		// accepts it.
		return newJob("review.candidate", reviewCandidateKey(project.ID, iid), map[string]any{
			"project_id": project.ID,
			"mr_iid":     iid,
			"head_sha":   strings.ToLower(sha),
			"profile":    profile,
			"trigger":    trigger,
			"full":       full,
		}), ""
	}
	key := reviewRunKey(project.ID, iid)
	return newJob("review.run", key, map[string]any{
		"project_id": project.ID,
		"mr_iid":     iid,
		"head_sha":   strings.ToLower(sha),
		"profile":    profile,
		"trigger":    trigger,
		"full":       full,
	}), ""
}

func (s *WebhookService) decideNote(project *store.Project, settings config.ProjectSettings, payload *gitlab.Webhook) (*store.Job, string) {
	attrs := payload.ObjectAttributes
	if attrs.NoteableType != "MergeRequest" {
		return nil, "unsupported_noteable"
	}
	if attrs.Action != "" && strings.ToLower(attrs.Action) != "create" {
		return nil, "unsupported_action"
	}
	if attrs.State != "" && !isOpenState(attrs.State) {
		return nil, "mr_closed"
	}
	if payload.MergeRequest.State != "" && !isOpenState(payload.MergeRequest.State) {
		return nil, "mr_closed"
	}
	iid := payload.MergeRequest.IID
	if iid <= 0 {
		iid = attrs.NoteableIID
	}
	if iid <= 0 || attrs.ID <= 0 {
		return nil, "bad_request"
	}
	targetProjectID := payload.MergeRequest.TargetProjectID
	if targetProjectID > 0 && targetProjectID != project.ID {
		return nil, "project_mismatch"
	}
	if payload.Project.ID > 0 && payload.Project.ID != project.ID {
		return nil, "project_mismatch"
	}
	user := payload.User
	if user.ID <= 0 {
		user.ID = attrs.AuthorID
	}
	if user.ID <= 0 {
		return nil, "bad_request"
	}
	command, profile, full, explicit, reason := parseCommand(attrs.Note, project.Profile, s.profiles)
	if reason == "not_review_command" && s.mentionsBot(attrs.Note) {
		// Mention wake-up. A third-party agent commonly authors notes through a
		// bot token, so user.Bot alone must not drop it. Notes from this service
		// are excluded by configured username and by the markers it writes; the
		// worker's CurrentUser comparison is the final backstop.
		if s.botUsernames[user.Username] {
			return nil, "bot_note"
		}
		if isReviewServiceNoteBody(attrs.Note) {
			return nil, "service_note"
		}
		return newJob("review.command", commandKey(project.ID, attrs.ID), map[string]any{
			"project_id":       project.ID,
			"mr_iid":           iid,
			"note_id":          attrs.ID,
			"author_id":        user.ID,
			"author_username":  user.Username,
			"author_bot":       user.Bot,
			"command":          commandRun,
			"profile":          project.Profile,
			"profile_explicit": false,
			"full":             false,
			"trigger":          "mention",
			"discussion_id":    attrs.DiscussionID,
		}), ""
	}
	if user.Bot || s.botUsernames[user.Username] {
		return nil, "bot_note"
	}
	if isCommandReplyBody(attrs.Note) {
		return nil, "command_reply"
	}
	if reason != "" {
		return nil, reason
	}
	return newJob("review.command", commandKey(project.ID, attrs.ID), map[string]any{
		"project_id":       project.ID,
		"mr_iid":           iid,
		"note_id":          attrs.ID,
		"author_id":        user.ID,
		"author_username":  user.Username,
		"author_bot":       user.Bot,
		"command":          command,
		"profile":          profile,
		"profile_explicit": explicit,
		"full":             full,
		"trigger":          "command",
		"discussion_id":    attrs.DiscussionID,
	}), ""
}

func (s *WebhookService) decidePush(project *store.Project, settings config.ProjectSettings, payload *gitlab.Webhook) (*store.Job, string) {
	ref := strings.TrimSpace(payload.Ref)
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || branch == "" {
		return nil, "unsupported_ref"
	}
	after := strings.TrimSpace(payload.After)
	if !shaRE.MatchString(after) {
		return nil, "bad_request"
	}
	if isZeroSHA(after) {
		return nil, "branch_deleted"
	}
	if !BranchAllowed(branch, project.DefaultBranch, settings.TargetBranches) {
		return nil, "branch_filtered"
	}
	return newJob("workspace.refresh", "ws:"+itoa(project.ID), map[string]any{
		"project_id": project.ID,
		"ref":        ref,
		"branch":     branch,
		"after_sha":  strings.ToLower(after),
		"trigger":    "push",
	}), ""
}

func parseCommand(note, defaultProfile string, profiles map[string]config.ProfileConfig) (command, profile string, full, explicit bool, reason string) {
	line := note
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "/review" {
		return "", "", false, false, "not_review_command"
	}
	switch len(fields) {
	case 1:
		return commandRun, defaultProfile, false, false, ""
	case 2:
		switch fields[1] {
		case "full":
			return commandRun, defaultProfile, true, false, ""
		case "cancel":
			return commandCancel, "", false, false, ""
		case "help":
			return commandHelp, "", false, false, ""
		default:
			if isReviewProfile(profiles, fields[1]) {
				return commandRun, fields[1], false, true, ""
			}
			return "", "", false, false, "unknown_profile"
		}
	default:
		return "", "", false, false, "unknown_command"
	}
}

func isOpenState(state string) bool {
	return state == "open" || state == "opened"
}

// mentionNoteLimit bounds the note body inspected for @-mentions. Any commenter
// can post an arbitrarily large note without the webhook secret, so looking for
// mentions above this size would let them spend the ingress CPU/memory budget.
// An oversized note is treated as not mentioning the bot.
const mentionNoteLimit = 128 << 10

// isReviewServiceNoteBody reports whether this service wrote the note. A
// mention must never wake the service from its own review text, which may
// quote user content containing @names.
func isReviewServiceNoteBody(body string) bool {
	return isCommandReplyBody(body) ||
		gitlab.HasSummaryMarker(body) ||
		strings.HasPrefix(body, reviewLegacyStatusMarkerPrefix) ||
		strings.HasPrefix(body, review.FindingMarkerPrefix)
}

// mentionsBot reports whether the note addresses one of the configured
// bot_usernames. An empty list disables mention detection entirely, and an
// oversized body is treated as not mentioning (see mentionNoteLimit).
func (s *WebhookService) mentionsBot(body string) bool {
	if len(s.mentionTargets) == 0 || len(body) > mentionNoteLimit {
		return false
	}
	text := strings.ToLower(stripMentionNoise(body))
	for _, target := range s.mentionTargets {
		if containsMention(text, target) {
			return true
		}
	}
	return false
}

var (
	autolinkRE = regexp.MustCompile(`<[^>\n]*>`)
	linkDestRE = regexp.MustCompile(`\]\([^)\n]*\)`)
	bareURLRE  = regexp.MustCompile(`(?i)https?://[^\s<>]+`)
)

// stripMentionNoise removes the regions where an @name is not an actual
// mention: code blocks and spans, autolinks, Markdown link targets and bare
// URLs. Code regions are replaced with a space instead of being deleted so two
// text fragments cannot splice into a mention.
func stripMentionNoise(body string) string {
	text := stripInlineCode(stripCodeBlocks(body))
	text = autolinkRE.ReplaceAllString(text, " ")
	// Remove bare URLs before Markdown destinations so a URL containing
	// parentheses or @ is consumed whole instead of exposing its tail.
	text = bareURLRE.ReplaceAllString(text, " ")
	text = linkDestRE.ReplaceAllString(text, "] ")
	return text
}

// stripCodeBlocks removes fenced code blocks and 4-space (or tab) indented
// code lines. It walks the note line by line without materialising a slice, so
// memory and time stay linear in the note size. One newline is kept per source
// line, including dropped lines, so kept fragments never splice together.
func stripCodeBlocks(body string) string {
	var out strings.Builder
	out.Grow(len(body))
	var (
		fenceChar byte
		fenceLen  int
	)
	for start := 0; start < len(body); {
		lineEnd := strings.IndexByte(body[start:], '\n')
		hasNewline := lineEnd >= 0
		if hasNewline {
			lineEnd += start
		} else {
			lineEnd = len(body)
		}
		line := body[start:lineEnd]
		keep := false
		if fenceChar != 0 {
			if isFenceClose(line, fenceChar, fenceLen) {
				fenceChar, fenceLen = 0, 0
			}
		} else if indentedCode(line) {
			// Drop the line; its newline is still emitted below.
		} else if run, ch := fenceOpen(line); run > 0 {
			fenceChar, fenceLen = ch, run
		} else {
			keep = true
		}
		if keep {
			out.WriteString(line)
		}
		if !hasNewline {
			break
		}
		out.WriteByte('\n')
		start = lineEnd + 1
	}
	return out.String()
}

// indentedCode reports a CommonMark indented code line: at least four leading
// spaces, or any leading tab (a tab advances to a tab stop at column four).
func indentedCode(line string) bool {
	spaces := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			spaces++
			if spaces >= 4 {
				return true
			}
		case '\t':
			return true
		default:
			return false
		}
	}
	return false
}

// fenceOpen reports an opening fence run. Lines with four or more leading
// spaces are handled as indented code before this check.
func fenceOpen(line string) (int, byte) {
	offset := 0
	for offset < len(line) && offset < 3 && line[offset] == ' ' {
		offset++
	}
	if offset >= len(line) || (line[offset] != '`' && line[offset] != '~') {
		return 0, 0
	}
	ch := line[offset]
	run := 0
	for offset+run < len(line) && line[offset+run] == ch {
		run++
	}
	if run < 3 {
		return 0, 0
	}
	return run, ch
}

func isFenceClose(line string, ch byte, min int) bool {
	offset := 0
	for offset < len(line) && offset < 3 && line[offset] == ' ' {
		offset++
	}
	if offset >= len(line) || line[offset] != ch {
		return false
	}
	run := 0
	for offset+run < len(line) && line[offset+run] == ch {
		run++
	}
	if run < min {
		return false
	}
	return strings.TrimSpace(line[offset+run:]) == ""
}

// stripInlineCode replaces inline code spans with a single space. Delimiters
// are paired in one linear pass: a run of N backticks closes the earliest
// unpaired run of the same length. Backslash-escaped backticks are literal
// text and do not delimit a span.
func stripInlineCode(text string) string {
	type span struct{ start, end int }
	pending := make(map[int]int)
	var spans []span
	for i := 0; i < len(text); {
		switch text[i] {
		case '\\':
			run := 1
			for i+run < len(text) && text[i+run] == '\\' {
				run++
			}
			if run%2 == 1 && i+run < len(text) && text[i+run] == '`' {
				i += run + 1
				continue
			}
			i += run
		case '`':
			run := 1
			for i+run < len(text) && text[i+run] == '`' {
				run++
			}
			if start, ok := pending[run]; ok {
				spans = append(spans, span{start, i + run})
				delete(pending, run)
			} else {
				pending[run] = i
			}
			i += run
		default:
			i++
		}
	}
	if len(spans) == 0 {
		return text
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var out strings.Builder
	out.Grow(len(text))
	cursor := 0
	for _, s := range spans {
		if s.start < cursor {
			if s.end > cursor {
				cursor = s.end
			}
			continue
		}
		out.WriteString(text[cursor:s.start])
		out.WriteByte(' ')
		cursor = s.end
	}
	out.WriteString(text[cursor:])
	return out.String()
}

// containsMention applies the boundary rule to an already lowercased note: the
// byte before @ must not be a word byte, and the byte after the username must
// not be a GitLab username byte (letters, digits, '_', '.' or '-').
func containsMention(text, target string) bool {
	needle := "@" + target
	for offset := 0; offset+len(needle) <= len(text); {
		idx := strings.Index(text[offset:], needle)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(needle)
		if (start == 0 || !isMentionLeadByte(text[start-1])) &&
			(end == len(text) || !isMentionUsernameByte(text[end])) {
			return true
		}
		offset = start + 1
	}
	return false
}

func isMentionLeadByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// isMentionUsernameByte follows GitLab's username character set, so a trailing
// dot or dash means a longer username rather than a sentence boundary.
func isMentionUsernameByte(b byte) bool {
	return isMentionLeadByte(b) || b == '.' || b == '-'
}

func isDraft(attrs gitlab.WebhookObjectAttributes, mr gitlab.WebhookMergeRequest) bool {
	if attrs.Draft || attrs.WorkInProgress || mr.Draft || mr.WorkInProgress {
		return true
	}
	title := attrs.Title
	if title == "" {
		title = mr.Title
	}
	lower := strings.ToLower(strings.TrimSpace(title))
	return strings.HasPrefix(lower, "draft:") || strings.HasPrefix(lower, "wip:")
}

func mergeLabels(groups ...gitlab.Labels) []string {
	seen := make(map[string]struct{})
	var labels []string
	for _, group := range groups {
		for _, label := range group {
			if _, ok := seen[label]; ok {
				continue
			}
			seen[label] = struct{}{}
			labels = append(labels, label)
		}
	}
	return labels
}

func newJob(kind, key string, payload any) *store.Job {
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(`{}`)
	}
	return &store.Job{Kind: kind, Key: key, Payload: encoded, MaxAttempts: 3}
}

func reviewCandidateKey(projectID, iid int64) string {
	return "review.candidate:" + itoa(projectID) + ":" + itoa(iid)
}

func reviewRunKey(projectID, iid int64) string {
	return "review:" + itoa(projectID) + ":" + itoa(iid)
}

func reviewCancelKey(projectID, iid int64) string {
	return "review.cancel:" + itoa(projectID) + ":" + itoa(iid)
}

func commandKey(projectID, noteID int64) string {
	return "review.command:" + itoa(projectID) + ":" + itoa(noteID)
}

func isZeroSHA(sha string) bool {
	for _, r := range sha {
		if r != '0' {
			return false
		}
	}
	return sha != ""
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

// webhookMRIID extracts the merge request identity from an event. Note hooks
// carry the note ID in object_attributes.iid, so the noteable or merge_request
// fields are authoritative there. Push and unknown events have no MR.
func webhookMRIID(event string, payload *gitlab.Webhook) int64 {
	if payload == nil {
		return 0
	}
	switch event {
	case "Note Hook":
		if payload.ObjectAttributes.NoteableIID > 0 {
			return payload.ObjectAttributes.NoteableIID
		}
	case "Merge Request Hook":
		if payload.ObjectAttributes.IID > 0 {
			return payload.ObjectAttributes.IID
		}
	}
	if payload.MergeRequest.IID > 0 {
		return payload.MergeRequest.IID
	}
	return 0
}
