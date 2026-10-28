package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// ProjectStore is the persistence surface needed by ProjectService.
type ProjectStore interface {
	RegisterProject(ctx context.Context, project store.Project) (string, error)
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	ListProjects(ctx context.Context) ([]store.Project, error)
	ListProjectsFiltered(ctx context.Context, filter store.ProjectFilter) ([]store.Project, error)
	UpdateProject(ctx context.Context, id int64, update store.ProjectUpdate) (*store.Project, error)
	UpdateProjectToken(ctx context.Context, id int64, update store.ProjectTokenUpdate) error
	UpdateProjectHook(ctx context.Context, id int64, hookID *int64, secretEnc []byte) error
	UpdateProjectWebhook(ctx context.Context, id int64, update store.ProjectWebhookUpdate) error
	DeleteProject(ctx context.Context, id int64) error
}

// RegisterInput is an authenticated registration request.
type RegisterInput struct {
	Project     string
	AccessToken string
	Profile     string
	AutoWebhook bool
	Settings    json.RawMessage
}

// ValidateProjectInput is a read-only repository connection check.
type ValidateProjectInput struct {
	Project     string
	AccessToken string
}

// ProjectValidation is the metadata returned by a successful read-only check.
type ProjectValidation struct {
	ProjectID         int64
	PathWithNamespace string
	WebURL            string
	DefaultBranch     string
	Username          string
	AccessLevel       int
	TokenScopes       []string
	TokenExpiresAt    *time.Time
	GitHead           string
}

// Registration is the successful registration response. WebhookSecret is set
// only when the webhook was not created automatically and must be shown once.
type Registration struct {
	Project       *store.Project
	JobID         string
	WebhookSecret string
}

// ProjectListFilter selects projects for the admin list endpoint. A nil
// AccessUserID keeps the trusted management-plane view; a non-nil value is
// forwarded to the store so business users only see granted projects.
type ProjectListFilter struct {
	Query        string
	Enabled      *bool
	AccessUserID *int64
}

// UpdateProjectInput changes only the provided project fields.
type UpdateProjectInput struct {
	Profile  *string
	Settings *json.RawMessage
	Enabled  *bool
}

// WebhookRegistration is the outcome of (re)creating a project webhook. The
// webhook secret itself is never returned again: it stays in project storage.
type WebhookRegistration struct {
	Project *store.Project
	HookID  int64
}

// ProjectService registers projects and validates their GitLab credentials.
type ProjectService struct {
	store             ProjectStore
	gitlab            gitlab.API
	cipher            *secret.Cipher
	profiles          map[string]config.ProfileConfig
	publicURL         string
	hookURL           string
	workspace         string
	workspaces        *workspace.Manager
	projectExtensions interface{ CloseProject(int64) error }
	gitLabOrigin      *gitLabOrigin
	probeRemote       func(ctx context.Context, remoteURL, token string) (string, error)
	validationTimeout time.Duration
	logger            *slog.Logger
	now               func() time.Time

	mu sync.Mutex
}

// ErrProjectValidationUnavailable reports that read-only validation was not
// wired (no trusted GitLab root or no repository probe).
var ErrProjectValidationUnavailable = errors.New("app: project validation is not configured")

const (
	defaultValidationTimeout = 30 * time.Second
	reporterAccessLevel      = 20
)

// managedWebhookEvents is the configured event set for service-created hooks.
var managedWebhookEvents = []string{"push_events", "merge_requests_events", "note_events"}

var projectPathSegmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// gitLabOrigin is the trusted GitLab root. User input and API-returned URLs
// must share its scheme, host and path prefix before any network access.
type gitLabOrigin struct {
	scheme   string
	host     string
	basePath string
}

// ProjectServiceConfig configures ProjectService.
type ProjectServiceConfig struct {
	Store     ProjectStore
	GitLab    gitlab.API
	Cipher    *secret.Cipher
	Profiles  map[string]config.ProfileConfig
	PublicURL string
	// Workspace owns the shared project locks and workspace root.
	Workspace *workspace.Manager
	// ProjectExtensions closes cached project sessions before deletion.
	ProjectExtensions interface{ CloseProject(int64) error }
	// GitLabURL is the trusted GitLab root used to accept full repository
	// URLs. Requests still go through the configured GitLab API only.
	GitLabURL string
	// ProbeRemote performs the read-only HEAD check. A nil probe makes
	// validation unavailable instead of pretending success.
	ProbeRemote func(ctx context.Context, remoteURL, token string) (string, error)
	// ValidationTimeout bounds the whole check; zero uses 30 seconds.
	ValidationTimeout time.Duration
	Logger            *slog.Logger
	Now               func() time.Time
}

// NewProjectService builds the registration service.
func NewProjectService(cfg ProjectServiceConfig) (*ProjectService, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil || cfg.Workspace == nil {
		return nil, errors.New("app: project service dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	publicURL := strings.TrimRight(cfg.PublicURL, "/")
	hookURL := ""
	if parsed, err := url.Parse(publicURL); err == nil && parsed.Host != "" {
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		hookURL = parsed.JoinPath("webhooks", "gitlab").String()
	}
	origin, err := parseGitLabOrigin(cfg.GitLabURL)
	if err != nil {
		return nil, fmt.Errorf("app: invalid gitlab URL: %w", err)
	}
	timeout := cfg.ValidationTimeout
	if timeout <= 0 {
		timeout = defaultValidationTimeout
	}
	return &ProjectService{
		store:             cfg.Store,
		gitlab:            cfg.GitLab,
		cipher:            cfg.Cipher,
		profiles:          cfg.Profiles,
		publicURL:         publicURL,
		hookURL:           hookURL,
		workspace:         cfg.Workspace.Root(),
		workspaces:        cfg.Workspace,
		projectExtensions: cfg.ProjectExtensions,
		gitLabOrigin:      origin,
		probeRemote:       cfg.ProbeRemote,
		validationTimeout: timeout,
		logger:            logger,
		now:               now,
	}, nil
}

// Register validates the project token and scopes, optionally creates a
// webhook, then atomically stores the project and onboarding job.
func (s *ProjectService) Register(ctx context.Context, in RegisterInput) (*Registration, error) {
	ref, err := s.parseProjectRef(in.Project)
	if err != nil {
		return nil, validationError("invalid project", Detail{Field: "project", Message: "must be a positive numeric ID, namespace/path, or URL under the configured GitLab root"})
	}
	if strings.TrimSpace(in.AccessToken) == "" {
		return nil, validationError("access token is required", Detail{Field: "token", Message: "must not be empty"})
	}
	profile := in.Profile
	if profile == "" {
		profile = "default"
	}
	if _, ok := s.profiles[profile]; !ok {
		return nil, validationError("unknown profile", Detail{Field: "profile", Message: "profile is not defined"})
	}
	canonicalSettings, err := ParseSettings(in.Settings, s.profiles)
	if err != nil {
		return nil, err
	}
	if in.AutoWebhook {
		if err := s.validatePublicURL(); err != nil {
			return nil, err
		}
	}

	project, err := s.gitlab.GetProject(ctx, in.AccessToken, projectRefString(ref))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab project lookup failed", "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	if err := s.verifyProject(project, ref, false); err != nil {
		return nil, err
	}

	info, err := s.gitlab.GetTokenInfo(ctx, in.AccessToken)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab token lookup failed", "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	if err := validateTokenInfo(info, s.now()); err != nil {
		return nil, err
	}

	tokenEnc, err := s.cipher.Seal([]byte(in.AccessToken), secret.TokenAAD(project.ID))
	if err != nil {
		return nil, internalError()
	}
	webhookSecret, err := newWebhookSecret()
	if err != nil {
		return nil, internalError()
	}
	secretEnc, err := s.cipher.Seal([]byte(webhookSecret), secret.WebhookSecretAAD(project.ID))
	if err != nil {
		return nil, internalError()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.workspaces.Get(project.ID).Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if _, err := s.store.GetProject(ctx, project.ID); err == nil {
		return nil, conflictError("project is already registered")
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, internalError()
	}
	if exists, err := s.projectDataExists(project.ID); err != nil {
		s.logger.Warn("project data inspection failed", "project_id", project.ID)
		return nil, internalError()
	} else if exists {
		return nil, conflictError("local project data remains; manual cleanup is required before registration")
	}

	webhookState := store.WebhookStateDisabled
	webhookEvents := []string{}
	webhookManagedBy := ""
	var hookID *int64
	if in.AutoWebhook {
		created, err := s.gitlab.CreateProjectHook(ctx, in.AccessToken, project.ID, gitlab.HookSpec{
			URL:                   s.hookURL,
			Secret:                webhookSecret,
			PushEvents:            true,
			MergeRequestsEvents:   true,
			NoteEvents:            true,
			EnableSSLVerification: true,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			s.logger.Warn("GitLab webhook creation failed", "project_id", project.ID, "status", gitlab.StatusOf(err))
			return nil, upstreamError(gitlab.StatusOf(err))
		}
		hookID = &created
		webhookState = store.WebhookStateEnabled
		webhookEvents = append([]string(nil), managedWebhookEvents...)
		webhookManagedBy = store.WebhookManagedByService
	}

	stored := store.Project{
		ID:                project.ID,
		PathWithNamespace: project.PathWithNamespace,
		WebURL:            project.WebURL,
		DefaultBranch:     project.DefaultBranch,
		TokenEnc:          tokenEnc,
		TokenExpiresAt:    info.ExpiresAt,
		TokenScopes:       append([]string(nil), info.Scopes...),
		WebhookSecretEnc:  secretEnc,
		HookID:            hookID,
		WebhookState:      webhookState,
		WebhookEvents:     webhookEvents,
		WebhookManagedBy:  webhookManagedBy,
		Profile:           profile,
		Settings:          canonicalSettings,
		Enabled:           true,
	}
	jobID, err := s.store.RegisterProject(ctx, stored)
	if err != nil {
		if hookID != nil {
			if delErr := s.gitlab.DeleteProjectHook(context.WithoutCancel(ctx), in.AccessToken, project.ID, *hookID); delErr != nil {
				s.logger.Warn("webhook compensation failed", "project_id", project.ID)
				return nil, &Error{Code: "internal", Message: "registration failed and webhook cleanup failed", Compensate: true}
			}
		}
		if errors.Is(err, store.ErrConflict) {
			return nil, conflictError("project is already registered")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("project registration persistence failed", "project_id", project.ID)
		return nil, internalError()
	}

	saved, err := s.store.GetProject(ctx, project.ID)
	if err != nil {
		// The registration committed. Report it; a follow-up read is diagnostic.
		s.logger.Warn("registered project read-back failed", "project_id", project.ID)
		saved = &stored
	}
	s.logger.Info("project registered", "project_id", project.ID, "job_id", jobID)

	result := &Registration{Project: saved, JobID: jobID}
	if !in.AutoWebhook {
		result.WebhookSecret = webhookSecret
	}
	return result, nil
}

// Validate performs a read-only connection check. It does not persist the
// project, encrypt the token, create a webhook, enqueue a job, or call a model.
func (s *ProjectService) Validate(ctx context.Context, in ValidateProjectInput) (*ProjectValidation, error) {
	if s.gitLabOrigin == nil || s.probeRemote == nil {
		return nil, ErrProjectValidationUnavailable
	}
	ref, err := s.parseProjectRef(in.Project)
	if err != nil {
		return nil, validationError("invalid project", Detail{Field: "project", Message: "must be a positive numeric ID, namespace/path, or URL under the configured GitLab root"})
	}
	if strings.TrimSpace(in.AccessToken) == "" {
		return nil, validationError("access token is required", Detail{Field: "token", Message: "must not be empty"})
	}

	ctx, cancel := context.WithTimeout(ctx, s.validationTimeout)
	defer cancel()

	project, err := s.gitlab.GetProject(ctx, in.AccessToken, projectRefString(ref))
	if err != nil {
		return nil, s.validationUpstreamError(ctx, "get_project", err)
	}
	if err := s.verifyProject(project, ref, true); err != nil {
		return nil, err
	}

	user, err := s.gitlab.CurrentUser(ctx, in.AccessToken)
	if err != nil {
		return nil, s.validationUpstreamError(ctx, "current_user", err)
	}
	if user == nil || user.ID <= 0 || strings.TrimSpace(user.Username) == "" {
		return nil, upstreamError(0)
	}
	access, err := s.gitlab.GetMemberAccess(ctx, in.AccessToken, project.ID, user.ID)
	if err != nil {
		return nil, s.validationUpstreamError(ctx, "get_member", err)
	}
	if access < reporterAccessLevel {
		return nil, validationError("access level is insufficient",
			Detail{Field: "access_level", Message: "Reporter or higher is required"})
	}

	info, err := s.gitlab.GetTokenInfo(ctx, in.AccessToken)
	if err != nil {
		return nil, s.validationUpstreamError(ctx, "get_token", err)
	}
	if err := validateTokenInfo(info, s.now()); err != nil {
		return nil, err
	}

	head, err := s.probeRemote(ctx, project.HTTPURLToRepo, in.AccessToken)
	if err != nil {
		if errors.Is(err, context.Canceled) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("repository HEAD probe failed")
		return nil, upstreamError(0)
	}
	if repo.ValidateSHA(head) != nil {
		s.logger.Warn("repository HEAD probe returned an invalid object ID")
		return nil, upstreamError(0)
	}

	return &ProjectValidation{
		ProjectID:         project.ID,
		PathWithNamespace: project.PathWithNamespace,
		WebURL:            project.WebURL,
		DefaultBranch:     project.DefaultBranch,
		Username:          user.Username,
		AccessLevel:       access,
		TokenScopes:       append([]string(nil), info.Scopes...),
		TokenExpiresAt:    info.ExpiresAt,
		GitHead:           strings.ToLower(head),
	}, nil
}

// validationUpstreamError keeps GitLab and timeout failures sanitized. A
// caller cancellation propagates unchanged.
func (s *ProjectService) validationUpstreamError(ctx context.Context, op string, err error) error {
	if errors.Is(err, context.Canceled) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	status := gitlab.StatusOf(err)
	s.logger.Warn("GitLab validation request failed", "operation", op, "status", status)
	return upstreamError(status)
}

// parseProjectRef accepts the original ID/namespace/path forms plus a full
// URL under the trusted GitLab root. The URL is reduced to a project path; it
// is never used as an API base or Git remote directly.
func (s *ProjectService) parseProjectRef(raw string) (gitlab.ProjectRef, error) {
	if !strings.Contains(raw, "://") {
		return gitlab.ParseProjectRef(raw)
	}
	if s.gitLabOrigin == nil {
		return gitlab.ProjectRef{}, errors.New("app: full project URLs are not configured")
	}
	path, err := s.gitLabOrigin.projectPath(raw, true)
	if err != nil {
		return gitlab.ProjectRef{}, err
	}
	return gitlab.ParseProjectRef(path)
}

// verifyProject rejects mismatched project metadata. Web and clone URLs must
// stay under the trusted GitLab root before any consumer turns the stored web
// URL into a Git remote. requireClone also demands a clone URL for probing.
func (s *ProjectService) verifyProject(project *gitlab.Project, ref gitlab.ProjectRef, requireClone bool) error {
	if project == nil || project.ID <= 0 || project.PathWithNamespace == "" ||
		project.WebURL == "" || project.DefaultBranch == "" {
		return upstreamError(0)
	}
	if requireClone && project.HTTPURLToRepo == "" {
		return upstreamError(0)
	}
	if ref.ID > 0 && project.ID != ref.ID {
		return upstreamError(0)
	}
	if ref.ID == 0 && project.PathWithNamespace != ref.Path {
		return upstreamError(0)
	}
	targetPath, err := gitlab.ParseProjectRef(project.PathWithNamespace)
	if err != nil || targetPath.ID != 0 || targetPath.Path != project.PathWithNamespace {
		return upstreamError(0)
	}
	if s.gitLabOrigin == nil {
		// Legacy configurations without a trusted root cannot check URLs.
		// Validation, which performs a Git read, still refuses to run.
		if requireClone {
			return upstreamError(0)
		}
		return nil
	}
	webPath, err := s.gitLabOrigin.projectPath(project.WebURL, false)
	if err != nil || webPath != project.PathWithNamespace {
		return upstreamError(0)
	}
	if project.HTTPURLToRepo != "" {
		clonePath, err := s.gitLabOrigin.projectPath(project.HTTPURLToRepo, true)
		if err != nil || clonePath != project.PathWithNamespace {
			return upstreamError(0)
		}
	}
	return nil
}

// parseGitLabOrigin validates the operator-configured GitLab root. A missing
// root disables URL input but leaves ID/path registration working.
func parseGitLabOrigin(raw string) (*gitLabOrigin, error) {
	if raw == "" {
		return nil, nil
	}
	if strings.TrimSpace(raw) != raw || len(raw) > 2048 ||
		strings.ContainsAny(raw, "\\%#") || hasControlChar(raw) {
		return nil, errors.New("gitlab root is malformed")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.Hostname() == "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("gitlab root is malformed")
	}
	base := strings.TrimRight(u.Path, "/")
	if strings.Contains(u.Path, "//") {
		return nil, errors.New("gitlab root is malformed")
	}
	if base != "" {
		if !strings.HasPrefix(base, "/") || strings.Contains(base, "//") {
			return nil, errors.New("gitlab root is malformed")
		}
		for _, segment := range strings.Split(strings.TrimPrefix(base, "/"), "/") {
			if segment == "." || segment == ".." || !projectPathSegmentRE.MatchString(segment) {
				return nil, errors.New("gitlab root is malformed")
			}
		}
	}
	return &gitLabOrigin{scheme: u.Scheme, host: strings.ToLower(u.Host), basePath: base}, nil
}

// hasControlChar rejects ASCII controls, including ones URL parsing accepts.
func hasControlChar(raw string) bool {
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// projectPath extracts a validated repository path from a URL under the
// trusted root. trimGitSuffix is used for clone URLs.
func (o *gitLabOrigin) projectPath(raw string, trimGitSuffix bool) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || len(raw) > 2048 ||
		strings.ContainsAny(raw, " \\%#") || hasControlChar(raw) ||
		!strings.Contains(raw, "://") {
		return "", errors.New("project URL is malformed")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil ||
		!strings.EqualFold(u.Scheme, o.scheme) || !strings.EqualFold(u.Host, o.host) ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("project URL is outside the configured GitLab root")
	}
	rawPath := strings.TrimRight(u.Path, "/")
	prefix := o.basePath + "/"
	if !strings.HasPrefix(rawPath, prefix) {
		return "", errors.New("project URL is outside the configured GitLab root")
	}
	rel := strings.TrimPrefix(rawPath, prefix)
	if trimGitSuffix && strings.HasSuffix(rel, ".git") {
		rel = strings.TrimSuffix(rel, ".git")
		if rel == "" || strings.HasSuffix(rel, "/") {
			return "", errors.New("project URL has no project path")
		}
	}
	rel = strings.TrimRight(rel, "/")
	if rel == "" || strings.HasPrefix(rel, "/") {
		return "", errors.New("project URL has no project path")
	}
	return rel, nil
}

// List returns registered projects. Encrypted fields are never serialized.
func (s *ProjectService) List(ctx context.Context) ([]store.Project, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, internalError()
	}
	return projects, nil
}

// ListFiltered returns projects matching the admin list selector.
func (s *ProjectService) ListFiltered(ctx context.Context, filter ProjectListFilter) ([]store.Project, error) {
	query := strings.TrimSpace(filter.Query)
	if len(query) > 200 {
		return nil, validationError("query is too long", Detail{Field: "q", Message: "must be at most 200 bytes"})
	}
	projects, err := s.store.ListProjectsFiltered(ctx, store.ProjectFilter{
		Query: query, Enabled: filter.Enabled, AccessUserID: filter.AccessUserID,
	})
	if err != nil {
		return nil, internalError()
	}
	return projects, nil
}

// projectPageStore is the optional bounded-read capability. The full
// ProjectStore interface stays unchanged for existing fakes.
type projectPageStore interface {
	ListProjectsFilteredPage(ctx context.Context, filter store.ProjectFilter, cursor string, limit int) ([]store.Project, string, error)
}

// ListFilteredPage returns one project page. Production SQLite implements
// the bounded method; a store without it fails instead of silently returning
// a truncated-looking full list.
func (s *ProjectService) ListFilteredPage(ctx context.Context, filter ProjectListFilter, cursor string, limit int) ([]store.Project, string, error) {
	query := strings.TrimSpace(filter.Query)
	if len(query) > 200 {
		return nil, "", validationError("query is too long", Detail{Field: "q", Message: "must be at most 200 bytes"})
	}
	paged, ok := s.store.(projectPageStore)
	if !ok {
		return nil, "", internalError()
	}
	projects, next, err := paged.ListProjectsFilteredPage(ctx,
		store.ProjectFilter{Query: query, Enabled: filter.Enabled, AccessUserID: filter.AccessUserID},
		cursor, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			return nil, "", err
		}
		return nil, "", internalError()
	}
	return projects, next, nil
}

// Update changes the profile, settings or enabled state of a project.
func (s *ProjectService) Update(ctx context.Context, id int64, in UpdateProjectInput) (*store.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	if in.Profile == nil && in.Settings == nil && in.Enabled == nil {
		return nil, validationError("at least one project field is required",
			Detail{Field: "body", Message: "provide profile, settings or enabled"})
	}
	update := store.ProjectUpdate{Enabled: in.Enabled}
	if in.Profile != nil {
		profile := strings.TrimSpace(*in.Profile)
		if _, ok := s.profiles[profile]; !ok {
			return nil, validationError("unknown profile", Detail{Field: "profile", Message: "profile is not defined"})
		}
		update.Profile = &profile
	}
	if in.Settings != nil {
		canonical, err := ParseSettings(*in.Settings, s.profiles)
		if err != nil {
			return nil, err
		}
		update.Settings = canonical
	}
	project, err := s.store.UpdateProject(ctx, id, update)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("project not registered")
		}
		s.logger.Warn("project update failed", "project_id", id)
		return nil, internalError()
	}
	return project, nil
}

// RotateToken revalidates access and scopes, then re-encrypts the new token.
func (s *ProjectService) RotateToken(ctx context.Context, id int64, token string) (*store.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, validationError("access token is required", Detail{Field: "token", Message: "must not be empty"})
	}
	project, err := s.gitlab.GetProject(ctx, token, fmt.Sprintf("%d", id))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab project lookup failed during token rotation", "project_id", id, "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	if project.ID != id {
		return nil, upstreamError(0)
	}
	info, err := s.gitlab.GetTokenInfo(ctx, token)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab token lookup failed during rotation", "project_id", id, "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	if err := validateTokenInfo(info, s.now()); err != nil {
		return nil, err
	}
	tokenEnc, err := s.cipher.Seal([]byte(token), secret.TokenAAD(id))
	if err != nil {
		return nil, internalError()
	}
	if err := s.store.UpdateProjectToken(ctx, id, store.ProjectTokenUpdate{
		TokenEnc:  tokenEnc,
		ExpiresAt: info.ExpiresAt,
		Scopes:    append([]string(nil), info.Scopes...),
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("project not registered")
		}
		s.logger.Warn("project token persistence failed", "project_id", id)
		return nil, internalError()
	}
	return s.Get(ctx, id)
}

// CreateWebhook explicitly enables the managed GitLab webhook. Repeated calls
// reuse only the recorded hook ID; an unrecorded same-URL hook belongs to the
// user and is never adopted, updated or removed. It marks the project pending
// before the API call, records enabled or failed afterwards, and never returns
// the secret or an upstream response body.
func (s *ProjectService) CreateWebhook(ctx context.Context, id int64) (*WebhookRegistration, error) {
	return s.provisionWebhook(ctx, id, false)
}

// RebuildWebhook re-provisions the managed hook with a fresh secret. It still
// reuses the recorded hook when that hook exists, so rebuild is not an
// unconditional create.
func (s *ProjectService) RebuildWebhook(ctx context.Context, id int64) (*WebhookRegistration, error) {
	return s.provisionWebhook(ctx, id, true)
}

func (s *ProjectService) provisionWebhook(ctx context.Context, id int64, rotateSecret bool) (*WebhookRegistration, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	if err := s.validatePublicURL(); err != nil {
		return nil, err
	}

	// Serialize provisioning per service, matching registration: concurrent
	// enable or rebuild requests must not create duplicate hooks.
	s.mu.Lock()
	defer s.mu.Unlock()

	project, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	token, err := s.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
	if err != nil {
		return nil, internalError()
	}

	secret, secretEnc, err := s.webhookSecret(project, rotateSecret)
	if err != nil {
		return nil, err
	}
	if err := s.store.UpdateProjectWebhook(ctx, id, store.ProjectWebhookUpdate{
		State:  store.WebhookStatePending,
		Events: project.WebhookEvents,
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("project not registered")
		}
		s.logger.Warn("webhook pending state persistence failed", "project_id", id)
		return nil, internalError()
	}

	hooks, err := s.gitlab.ListProjectHooks(ctx, string(token), id)
	if err != nil {
		return nil, s.failWebhook(ctx, project, "list", err)
	}
	hookID, found := recordedHookID(hooks, project.HookID)
	reused := found
	if !found {
		created, err := s.gitlab.CreateProjectHook(ctx, string(token), id, s.hookSpec(secret))
		if err != nil {
			return nil, s.failWebhook(ctx, project, "create", err)
		}
		hookID = created
	} else if err := s.gitlab.UpdateProjectHook(ctx, string(token), id, hookID, s.hookSpec(secret)); err != nil {
		return nil, s.failWebhook(ctx, project, "update", err)
	}

	if err := s.store.UpdateProjectWebhook(ctx, id, store.ProjectWebhookUpdate{
		HookID:    &hookID,
		SecretEnc: secretEnc,
		State:     store.WebhookStateEnabled,
		Events:    append([]string(nil), managedWebhookEvents...),
		ManagedBy: store.WebhookManagedByService,
	}); err != nil {
		s.markWebhookFailed(ctx, project, "webhook state persistence failed")
		if !reused {
			if delErr := s.gitlab.DeleteProjectHook(context.WithoutCancel(ctx), string(token), id, hookID); delErr != nil {
				s.logger.Warn("webhook compensation failed", "project_id", id)
				return nil, &Error{Code: "internal", Message: "webhook update failed and cleanup failed", Compensate: true}
			}
		}
		s.logger.Warn("webhook state persistence failed", "project_id", id)
		return nil, internalError()
	}
	return s.webhookResult(ctx, project, hookID, secretEnc), nil
}

// webhookSecret returns the plain secret and its ciphertext. Rebuild always
// rotates. Enable keeps the stored secret so reusing a hook does not interrupt
// delivery; a stored secret that cannot be decrypted is an internal error
// instead of a silent rotation that would leave the old hook broken.
func (s *ProjectService) webhookSecret(project *store.Project, rotate bool) (string, []byte, error) {
	if !rotate && len(project.WebhookSecretEnc) > 0 {
		plain, err := s.cipher.Open(project.WebhookSecretEnc, secret.WebhookSecretAAD(project.ID))
		if err != nil {
			return "", nil, internalError()
		}
		return string(plain), nil, nil
	}
	value, err := newWebhookSecret()
	if err != nil {
		return "", nil, internalError()
	}
	enc, err := s.cipher.Seal([]byte(value), secret.WebhookSecretAAD(project.ID))
	if err != nil {
		return "", nil, internalError()
	}
	return value, enc, nil
}

// recordedHookID returns the recorded hook only when it still exists. Hooks at
// the same URL without a recorded ID are owned by another party.
func recordedHookID(hooks []gitlab.Hook, recorded *int64) (int64, bool) {
	if recorded == nil {
		return 0, false
	}
	for _, hook := range hooks {
		if hook.ID == *recorded {
			return hook.ID, true
		}
	}
	return 0, false
}

// DisableWebhook removes only the recorded service hook. A missing hook is
// already disabled; the encrypted secret is kept for a later re-enable.
func (s *ProjectService) DisableWebhook(ctx context.Context, id int64) (*store.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	project, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if project.HookID != nil {
		token, err := s.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
		if err != nil {
			return nil, internalError()
		}
		if err := s.gitlab.DeleteProjectHook(context.WithoutCancel(ctx), string(token), id, *project.HookID); err != nil {
			if gitlab.StatusOf(err) != 404 {
				return nil, s.failWebhook(ctx, project, "delete", err)
			}
		}
	}
	if err := s.store.UpdateProjectWebhook(ctx, id, store.ProjectWebhookUpdate{
		ClearHookID: true,
		State:       store.WebhookStateDisabled,
		Events:      project.WebhookEvents,
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("project not registered")
		}
		s.logger.Warn("webhook disable state persistence failed", "project_id", id)
		return nil, internalError()
	}
	return s.Get(ctx, id)
}

func (s *ProjectService) hookSpec(secret string) gitlab.HookSpec {
	return gitlab.HookSpec{
		URL:                   s.hookURL,
		Secret:                secret,
		PushEvents:            true,
		MergeRequestsEvents:   true,
		NoteEvents:            true,
		EnableSSLVerification: true,
	}
}

// failWebhook records a bounded, sanitized failure and returns the usual
// upstream error. A caller cancellation leaves the pending state untouched.
func (s *ProjectService) failWebhook(ctx context.Context, project *store.Project, operation string, cause error) error {
	if errors.Is(cause, context.Canceled) {
		return cause
	}
	status := gitlab.StatusOf(cause)
	message := fmt.Sprintf("GitLab webhook %s failed", operation)
	if status > 0 {
		message = fmt.Sprintf("GitLab webhook %s failed (status %d)", operation, status)
	}
	s.markWebhookFailed(ctx, project, message)
	s.logger.Warn("GitLab webhook provisioning failed", "project_id", project.ID, "operation", operation, "status", status)
	return upstreamError(status)
}

// markWebhookFailed is best-effort: the original provisioning error is the
// useful one, so a failed state write is only logged.
func (s *ProjectService) markWebhookFailed(ctx context.Context, project *store.Project, message string) {
	if err := s.store.UpdateProjectWebhook(context.WithoutCancel(ctx), project.ID, store.ProjectWebhookUpdate{
		State:  store.WebhookStateFailed,
		Error:  message,
		Events: project.WebhookEvents,
	}); err != nil {
		s.logger.Warn("webhook failure state persistence failed", "project_id", project.ID)
	}
}

// webhookResult reads the committed row back, falling back to the known values
// when the diagnostic read fails.
func (s *ProjectService) webhookResult(ctx context.Context, fallback *store.Project, hookID int64, secretEnc []byte) *WebhookRegistration {
	saved, err := s.Get(ctx, fallback.ID)
	if err != nil {
		copy := *fallback
		copy.HookID = &hookID
		if len(secretEnc) > 0 {
			copy.WebhookSecretEnc = secretEnc
		}
		copy.WebhookState = store.WebhookStateEnabled
		copy.WebhookEvents = append([]string(nil), managedWebhookEvents...)
		copy.WebhookError = ""
		copy.WebhookManagedBy = store.WebhookManagedByService
		s.logger.Warn("project read-back after webhook update failed", "project_id", fallback.ID)
		saved = &copy
	}
	return &WebhookRegistration{Project: saved, HookID: hookID}
}

// Delete commits the registration deletion before removing local project data.
func (s *ProjectService) Delete(ctx context.Context, id int64) error {
	if id <= 0 {
		return validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	// Serialize with enable/disable/register so deletion cannot race a hook
	// that is being provisioned and leave it unaccounted for at GitLab.
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.workspaces.Get(id).Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	project, err := s.store.GetProject(ctx, id)
	missing := errors.Is(err, store.ErrNotFound)
	if err != nil && !missing {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return internalError()
	}
	exists, err := s.projectDataExists(id)
	if err != nil {
		s.logger.Warn("project data inspection failed", "project_id", id)
		return internalError()
	}
	if missing {
		if exists {
			return conflictError("local project data remains; manual cleanup is required")
		}
		return notFoundError("project not registered")
	}
	if s.projectExtensions != nil {
		if err := s.projectExtensions.CloseProject(id); err != nil {
			s.logger.Warn("project extensions close failed", "project_id", id)
			return internalError()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if project.HookID != nil {
		if token, decErr := s.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID)); decErr != nil {
			s.logger.Warn("project token decrypt failed during delete", "project_id", id)
		} else {
			hookCtx, cancel := context.WithTimeout(ctx, defaultValidationTimeout)
			delErr := s.gitlab.DeleteProjectHook(hookCtx, string(token), id, *project.HookID)
			cancel()
			if delErr != nil {
				s.logger.Warn("webhook removal failed during project delete", "project_id", id, "status", gitlab.StatusOf(delErr))
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.store.DeleteProject(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFoundError("project not registered")
		}
		s.logger.Warn("project delete failed", "project_id", id)
		return internalError()
	}
	if err := s.removeProjectData(id); err != nil {
		s.logger.Warn("project workspace cleanup failed after registration deletion", "project_id", id)
		return &Error{Code: "internal", Message: "project registration deleted but local cleanup is incomplete", Compensate: true}
	}
	return nil
}

// projectDataExists checks both targets without following symlinked parents.
func (s *ProjectService) projectDataExists(id int64) (bool, error) {
	exists := false
	for _, path := range s.projectDataPaths(id) {
		found, err := inspectPathUnder(s.workspace, path)
		if err != nil {
			return false, err
		}
		exists = exists || found
	}
	return exists, nil
}

func (s *ProjectService) projectDataPaths(id int64) []string {
	return []string{
		filepath.Join(s.workspace, strconv.FormatInt(id, 10)),
		projectSkillsContainer(s.workspace, id),
	}
}

// removeProjectData removes the project workspace and its managed skill
// directory. It refuses to follow symlinked workspace roots.
func (s *ProjectService) removeProjectData(id int64) error {
	for _, path := range s.projectDataPaths(id) {
		if err := removePathUnder(s.workspace, path); err != nil {
			return err
		}
	}
	return nil
}

// removePathUnder removes target only when every existing component from root
// to its parent is a real directory. A symlinked workspace root or parent
// must never redirect cleanup outside the configured workspace.
func removePathUnder(root, target string) error {
	if _, err := inspectPathUnder(root, target); err != nil {
		return err
	}
	return removePath(target)
}

func inspectPathUnder(root, target string) (bool, error) {
	if !filepath.IsAbs(root) {
		return false, errors.New("app: workspace root is not absolute")
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, errors.New("app: cleanup path is outside the workspace")
	}
	current := root
	parts := strings.Split(rel, string(filepath.Separator))
	parents := parts[:len(parts)-1]
	for i := 0; i <= len(parents); i++ {
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, errors.New("app: cleanup parent is not a real directory")
		}
		if i < len(parents) {
			current = filepath.Join(current, parents[i])
		}
	}
	_, err = os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func removePath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}

// Get returns one registered project.
func (s *ProjectService) Get(ctx context.Context, id int64) (*store.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	project, err := s.store.GetProject(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("project not registered")
		}
		return nil, internalError()
	}
	return project, nil
}

func (s *ProjectService) validatePublicURL() error {
	if s.publicURL == "" {
		return validationError("server.public_url is required for auto_webhook", Detail{Field: "auto_webhook", Message: "public_url is not configured"})
	}
	u, err := url.Parse(s.publicURL)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		u.RawQuery != "" || u.Fragment != "" {
		return validationError("server.public_url is invalid", Detail{Field: "auto_webhook", Message: "public_url is not usable"})
	}
	return nil
}

func validateTokenInfo(info *gitlab.TokenInfo, now time.Time) error {
	if info == nil {
		return upstreamError(0)
	}
	if !info.Active || info.Revoked {
		return validationError("access token is not active", Detail{Field: "token", Message: "must be active and not revoked"})
	}
	if info.ExpiresAt != nil {
		// GitLab returns a date, not a timestamp: a token remains valid through
		// the whole UTC day named by expires_at.
		if now.UTC().Format("2006-01-02") > info.ExpiresAt.UTC().Format("2006-01-02") {
			return validationError("access token is expired", Detail{Field: "token", Message: "must not be expired"})
		}
	}
	scopes := make(map[string]bool, len(info.Scopes))
	for _, scope := range info.Scopes {
		scopes[scope] = true
	}
	var missing []Detail
	if !scopes["api"] {
		missing = append(missing, Detail{Field: "token.scopes", Message: "api scope is required"})
	}
	if !scopes["read_repository"] {
		missing = append(missing, Detail{Field: "token.scopes", Message: "read_repository scope is required"})
	}
	if len(missing) > 0 {
		return validationError("access token scopes are insufficient", missing...)
	}
	return nil
}

func projectRefString(ref gitlab.ProjectRef) string {
	if ref.ID > 0 {
		return fmt.Sprintf("%d", ref.ID)
	}
	return ref.Path
}

func newWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("app: generate webhook secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
