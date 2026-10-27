package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	commandRun    = "run"
	commandCancel = "cancel"
	commandHelp   = "help"
)

var shaRE = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// WebhookStore is the persistence surface needed by WebhookService.
type WebhookStore interface {
	GetProject(ctx context.Context, id int64) (*store.Project, error)
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
	cipher         *secret.Cipher
	profiles       map[string]config.ProfileConfig
	reviewDefaults config.ProjectSettings
	botUsernames   map[string]bool
	logger         *slog.Logger
	now            func() time.Time
}

// WebhookServiceConfig configures WebhookService.
type WebhookServiceConfig struct {
	Store          WebhookStore
	Cipher         *secret.Cipher
	Profiles       map[string]config.ProfileConfig
	ReviewDefaults config.ProjectSettings
	BotUsernames   []string
	Logger         *slog.Logger
	Now            func() time.Time
}

// NewWebhookService builds the webhook ingress service.
func NewWebhookService(cfg WebhookServiceConfig) (*WebhookService, error) {
	if cfg.Store == nil || cfg.Cipher == nil {
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
	for _, name := range cfg.BotUsernames {
		if name != "" {
			bots[name] = true
		}
	}
	return &WebhookService{
		store:          cfg.Store,
		cipher:         cfg.Cipher,
		profiles:       cfg.Profiles,
		reviewDefaults: cfg.ReviewDefaults,
		botUsernames:   bots,
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

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return WebhookResult{Ignored: "project_not_registered"}, nil
		}
		s.logger.Warn("webhook project lookup failed", "project_id", projectID)
		return WebhookResult{Ignored: "storage_error"}, err
	}
	if project == nil || !project.Enabled {
		return WebhookResult{Ignored: "project_not_registered"}, nil
	}

	if !s.validToken(project, req.Token) {
		s.logger.Warn("webhook authentication failed", "project_id", project.ID, "event", req.Event)
		return WebhookResult{Unauthorized: true}, nil
	}

	eventUUID := strings.TrimSpace(req.UUID)
	if eventUUID == "" {
		return WebhookResult{Ignored: "missing_event_uuid"}, nil
	}
	parsedUUID, err := uuid.Parse(eventUUID)
	if err != nil {
		return WebhookResult{Ignored: "invalid_event_uuid"}, nil
	}
	if reason := checkEventKind(req.Event, payload.ObjectKind); reason != "" {
		return s.recordIgnored(ctx, project.ID, req.Event, parsedUUID.String(), reason)
	}

	settings, err := ResolveSettings(project.Settings, s.reviewDefaults)
	if err != nil {
		s.logger.Warn("stored project settings are invalid", "project_id", project.ID)
		return s.recordIgnored(ctx, project.ID, req.Event, parsedUUID.String(), "invalid_settings")
	}

	job, reason := s.decide(project, settings, payload)
	if reason != "" {
		return s.recordIgnored(ctx, project.ID, req.Event, parsedUUID.String(), reason)
	}
	return s.recordJob(ctx, project.ID, req.Event, parsedUUID.String(), job)
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

func (s *WebhookService) recordIgnored(ctx context.Context, projectID int64, event, eventUUID, reason string) (WebhookResult, error) {
	_, duplicate, err := s.store.RecordDelivery(ctx, store.Delivery{
		UUID:       eventUUID,
		ProjectID:  projectID,
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

func (s *WebhookService) recordJob(ctx context.Context, projectID int64, event, eventUUID string, job *store.Job) (WebhookResult, error) {
	delivery := store.Delivery{
		UUID:       eventUUID,
		ProjectID:  projectID,
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
		var result store.ReviewEnqueueResult
		result, err = s.store.RecordReviewCandidateDelivery(ctx, delivery, candidate)
		jobID, duplicate = result.JobID, result.Duplicate
	} else if command, ok := reviewCommandFromJob(job); ok {
		if recorder, supported := s.store.(interface {
			RecordReviewDelivery(ctx context.Context, d store.Delivery, command *store.ReviewCommand) (store.ReviewEnqueueResult, error)
		}); supported {
			var result store.ReviewEnqueueResult
			result, err = recorder.RecordReviewDelivery(ctx, delivery, command)
			jobID, duplicate = result.JobID, result.Duplicate
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

func (s *WebhookService) decide(project *store.Project, settings config.ProjectSettings, payload *gitlab.Webhook) (*store.Job, string) {
	switch payload.Event {
	case "Merge Request Hook":
		return s.decideMergeRequest(project, settings, payload)
	case "Note Hook":
		return s.decideNote(project, settings, payload)
	case "Push Hook":
		return s.decidePush(project, settings, payload)
	default:
		return nil, "unsupported_event"
	}
}

func (s *WebhookService) decideMergeRequest(project *store.Project, settings config.ProjectSettings, payload *gitlab.Webhook) (*store.Job, string) {
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
		if strings.TrimSpace(attrs.OldRev) == "" {
			return nil, "missing_oldrev"
		}
		return s.newReviewJob(project, settings, payload, iid, "mr_update", false)
	default:
		return nil, "unsupported_action"
	}
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
	if _, ok := s.profiles[profile]; !ok {
		profile = "default"
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
	if user.Bot || s.botUsernames[user.Username] {
		return nil, "bot_note"
	}
	if isCommandReplyBody(attrs.Note) {
		return nil, "command_reply"
	}
	command, profile, full, explicit, reason := parseCommand(attrs.Note, project.Profile, s.profiles)
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
			if _, ok := profiles[fields[1]]; ok {
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
