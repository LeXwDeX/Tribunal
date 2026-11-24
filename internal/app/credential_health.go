package app

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// Credential health states observed at runtime. They extend the probe-driven
// vocabulary in project_credentials.go with what real GitLab calls saw, so a
// rejected credential is visible without opening the settings page. A record
// only ever holds sanitized state: no token value and no upstream body.
const (
	CredentialStateUnknown  = "unknown"
	CredentialStateOK       = "ok"
	CredentialStateRejected = "rejected"
)

// Credential sources name which credential a record describes.
const (
	CredentialSourceProject = "project"
	CredentialSourceShared  = "shared"
)

// credentialExpiryWarningWindow is how far ahead a token expiry is reported.
const credentialExpiryWarningWindow = 14 * 24 * time.Hour

// ProjectCredentialHealth is the sanitized health of one project's credential.
type ProjectCredentialHealth struct {
	ProjectID int64  `json:"project_id"`
	Source    string `json:"source"`
	State     string `json:"state"`
	// Failures counts rejected outcomes since the process started.
	Failures      int64     `json:"failures,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastFailureAt time.Time `json:"last_failure_at,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
	// TokenExpiresAt is the credential's GitLab expiry, not the check time.
	TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`
	// ExpiringSoon reports a token inside credentialExpiryWarningWindow.
	ExpiringSoon bool `json:"expiring_soon,omitempty"`
}

// SharedCredentialHealth is the sanitized health of the instance-wide bot
// credential as observed by runtime calls.
type SharedCredentialHealth struct {
	State         string    `json:"state"`
	Failures      int64     `json:"failures,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastFailureAt time.Time `json:"last_failure_at,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

// CredentialHealthSnapshot is the process-wide credential health view.
type CredentialHealthSnapshot struct {
	Shared   SharedCredentialHealth    `json:"shared"`
	Projects []ProjectCredentialHealth `json:"projects,omitempty"`
}

// CredentialHealthRegistry records credential outcomes observed by review side
// effects and by the periodic token check. It is process-local and sanitized;
// the probe cache in project_credentials.go stays the only GitLab-backed state.
type CredentialHealthRegistry struct {
	mu       sync.Mutex
	now      func() time.Time
	shared   credentialHealthRecord
	projects map[int64]*credentialHealthRecord
}

type credentialHealthRecord struct {
	Source         string
	State          string
	Failures       int64
	LastError      string
	LastFailureAt  time.Time
	CheckedAt      time.Time
	TokenExpiresAt *time.Time
	ExpiringSoon   bool
}

// NewCredentialHealthRegistry returns an empty registry. A nil now uses the
// wall clock. The registry is process-local: a restart starts from unknown.
func NewCredentialHealthRegistry(now func() time.Time) *CredentialHealthRegistry {
	if now == nil {
		now = time.Now
	}
	return &CredentialHealthRegistry{now: now, projects: make(map[int64]*credentialHealthRecord)}
}

// RecordRejection stores a rejected credential outcome. A shared rejection is
// also recorded on the instance-wide bot, because every shared project is
// affected by the same unusable credential.
func (r *CredentialHealthRegistry) RecordRejection(projectID int64, shared bool, reason string) {
	if r == nil {
		return
	}
	r.RecordOutcome(projectID, shared, CredentialStateRejected, reason, nil)
	if shared && projectID > 0 {
		r.RecordOutcome(0, true, CredentialStateRejected, reason, nil)
	}
}

// RecordOutcome stores a probe outcome and returns the state recorded before
// this call, so a caller can warn on a transition without polling. A shared
// outcome (shared true) with a positive projectID records that project's view
// of the instance-wide credential; projectID zero records the shared record.
func (r *CredentialHealthRegistry) RecordOutcome(projectID int64, shared bool, state, reason string, expiresAt *time.Time) string {
	if r == nil {
		return ""
	}
	if state != CredentialStateOK && state != CredentialStateRejected {
		state = CredentialStateUnknown
	}
	if state == CredentialStateOK {
		reason = ""
	}
	now := r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.recordLocked(projectID)
	previous := record.State
	record.Source = credentialRecordSource(projectID, shared)
	record.State = state
	record.CheckedAt = now
	record.TokenExpiresAt = expiresAt
	record.ExpiringSoon = expiresAt != nil && now.Add(credentialExpiryWarningWindow).After(expiresAt.UTC())
	if state == CredentialStateOK {
		return previous
	}
	record.Failures++
	record.LastError = reason
	record.LastFailureAt = now
	return previous
}

// Snapshot returns a copy of every record. Records are copied by value so a
// caller can never mutate the registry through the result.
func (r *CredentialHealthRegistry) Snapshot() CredentialHealthSnapshot {
	if r == nil {
		return CredentialHealthSnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := CredentialHealthSnapshot{
		Shared: SharedCredentialHealth{
			State:         r.shared.State,
			Failures:      r.shared.Failures,
			LastError:     r.shared.LastError,
			LastFailureAt: r.shared.LastFailureAt,
			CheckedAt:     r.shared.CheckedAt,
		},
	}
	if snapshot.Shared.State == "" {
		snapshot.Shared.State = CredentialStateUnknown
	}
	ids := make([]int64, 0, len(r.projects))
	for id := range r.projects {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		record := r.projects[id]
		snapshot.Projects = append(snapshot.Projects, ProjectCredentialHealth{
			ProjectID:      id,
			Source:         record.Source,
			State:          record.State,
			Failures:       record.Failures,
			LastError:      record.LastError,
			LastFailureAt:  record.LastFailureAt,
			CheckedAt:      record.CheckedAt,
			TokenExpiresAt: record.TokenExpiresAt,
			ExpiringSoon:   record.ExpiringSoon,
		})
	}
	return snapshot
}

// recordLocked picks the record for a project, or the instance-wide record for
// projectID zero. The caller holds mu.
func (r *CredentialHealthRegistry) recordLocked(projectID int64) *credentialHealthRecord {
	if projectID <= 0 {
		return &r.shared
	}
	record := r.projects[projectID]
	if record == nil {
		record = &credentialHealthRecord{}
		r.projects[projectID] = record
	}
	return record
}

func credentialRecordSource(projectID int64, shared bool) string {
	if projectID <= 0 || shared {
		return CredentialSourceShared
	}
	return CredentialSourceProject
}

// CredentialHealth returns the sanitized credential health snapshot. It never
// contacts GitLab: the periodic pass and observed rejections feed it.
func (s *ProjectService) CredentialHealth() CredentialHealthSnapshot {
	if s == nil || s.credentials == nil {
		return CredentialHealthSnapshot{}
	}
	return s.credentials.Snapshot()
}

// credentialStateFromProbe folds the probe vocabulary into the observed one.
// A transport failure is not a rejection: the credential was not judged.
func credentialStateFromProbe(state string) string {
	switch state {
	case SharedBotStateOK:
		return CredentialStateOK
	case SharedBotStateExpired, SharedBotStateInsufficient:
		return CredentialStateRejected
	default:
		return CredentialStateUnknown
	}
}

// credentialHealthCheckInterval bounds how long a rejected or never-checked
// credential stays invisible after startup. The pass only reads token metadata.
const credentialHealthCheckInterval = 30 * time.Minute

// ReconcileCredentialHealth verifies every enabled project's credential and
// records the sanitized outcome. Shared projects reuse the cached shared bot
// probe, so one pass costs at most one GitLab call per shared group plus one
// per project token. A project whose credential verifies again gets its
// credential-blocked status rows reopened. The pass never blocks startup.
func (s *ProjectService) ReconcileCredentialHealth(ctx context.Context) {
	if s == nil || s.store == nil || s.credentials == nil {
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.logger.Warn("credential health check failed", "error_class", errclass.Of(err))
		return
	}
	var shared []store.Project
	for _, project := range projects {
		if !project.Enabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		if s.tokens.UsesShared(&project) {
			shared = append(shared, project)
			continue
		}
		s.checkProjectToken(ctx, project)
	}
	if len(shared) == 0 {
		return
	}
	s.checkSharedCredential(ctx, shared)
}

// checkProjectToken probes one project token and records its outcome.
func (s *ProjectService) checkProjectToken(ctx context.Context, project store.Project) {
	token, err := s.tokens.Token(&project)
	if err != nil {
		s.recordCredentialOutcome(ctx, project.ID, false, CredentialStateUnknown, err.Error(), nil)
		return
	}
	state, reason, expiresAt := s.probeProjectToken(ctx, token)
	s.recordCredentialOutcome(ctx, project.ID, false, state, reason, expiresAt)
}

func (s *ProjectService) probeProjectToken(ctx context.Context, token string) (state, reason string, expiresAt *time.Time) {
	info, err := s.gitlab.GetTokenInfo(ctx, token)
	if err != nil {
		status := gitlab.StatusOf(err)
		if status == 401 || status == 403 {
			return CredentialStateRejected, tokenRejectedHealthMessage(status, false), nil
		}
		return CredentialStateUnknown, "token info check failed: " + errclass.Of(err), nil
	}
	if info == nil {
		return CredentialStateUnknown, "token info is unavailable", nil
	}
	if !info.Active || info.Revoked || tokenInfoExpired(info, s.now()) {
		return CredentialStateRejected, tokenRejectedHealthMessage(401, false), info.ExpiresAt
	}
	if missingTokenScope(info) != "" {
		return CredentialStateRejected, tokenRejectedHealthMessage(403, false), info.ExpiresAt
	}
	return CredentialStateOK, "", info.ExpiresAt
}

// checkSharedCredential records the instance-wide outcome once and mirrors it
// onto every shared project, so each project reports the credential it
// actually uses without repeating the transition warning per project.
func (s *ProjectService) checkSharedCredential(ctx context.Context, projects []store.Project) {
	health := s.SharedBotCredentialHealth(ctx)
	state := credentialStateFromProbe(health.State)
	reason := credentialProbeReason(health)
	previous := s.credentials.RecordOutcome(0, true, state, reason, health.ExpiresAt)
	s.warnCredentialTransition(0, CredentialSourceShared, previous, state, reason)
	for _, project := range projects {
		s.recordCredentialOutcome(ctx, project.ID, true, state, reason, health.ExpiresAt)
	}
}

// credentialProbeReason keeps a rejected shared probe inside the sanitized
// message vocabulary instead of exposing its internal state name.
func credentialProbeReason(health SharedBotCredentialHealth) string {
	if health.State == SharedBotStateExpired {
		return tokenRejectedHealthMessage(401, true)
	}
	if health.State == SharedBotStateInsufficient {
		return tokenRejectedHealthMessage(403, true)
	}
	return health.State
}

func (s *ProjectService) recordCredentialOutcome(ctx context.Context, projectID int64, shared bool, state, reason string, expiresAt *time.Time) {
	previous := s.credentials.RecordOutcome(projectID, shared, state, reason, expiresAt)
	if state == CredentialStateOK {
		s.reopenCredentialBlockedStatuses(ctx, projectID)
		return
	}
	s.warnCredentialTransition(projectID, credentialRecordSource(projectID, shared), previous, state, reason)
}

func (s *ProjectService) warnCredentialTransition(projectID int64, source, previous, state, reason string) {
	if state != CredentialStateRejected || previous == CredentialStateRejected {
		return
	}
	s.logger.Warn("gitlab credential rejected",
		"project_id", projectID,
		"credential_source", source,
		"state", state, "reason", reason)
}

// reopenCredentialBlockedStatuses rewinds the applied version of status rows
// that a rejected credential had settled, so the regular verification scan
// writes their note once the credential works again.
func (s *ProjectService) reopenCredentialBlockedStatuses(ctx context.Context, projectID int64) {
	reopener, ok := s.store.(interface {
		ReopenCredentialBlockedReviewStatuses(context.Context, int64) (int64, error)
	})
	if !ok {
		return
	}
	reopened, err := reopener.ReopenCredentialBlockedReviewStatuses(ctx, projectID)
	if err != nil {
		s.logger.Warn("reopen credential-blocked review statuses failed",
			"project_id", projectID, "error_class", errclass.Of(err))
		return
	}
	if reopened > 0 {
		s.logger.Info("reopened credential-blocked review statuses",
			"project_id", projectID, "count", reopened)
	}
}