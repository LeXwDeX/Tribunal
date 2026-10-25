// Package gitlab wraps the official GitLab API client for the operations used
// by review scheduling and publication. Every method takes the project token
// explicitly so callers cannot accidentally reuse another project's
// credentials.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "gitlab.com/gitlab-org/api/client-go"
)

const (
	defaultRequestTimeout = 30 * time.Second
	defaultConcurrency    = 8
)

// Project is the small projection used by registration.
type Project struct {
	ID                int64
	PathWithNamespace string
	WebURL            string
	HTTPURLToRepo     string
	DefaultBranch     string
}

// TokenInfo describes the token returned by GET /personal_access_tokens/self.
type TokenInfo struct {
	Scopes    []string
	Active    bool
	Revoked   bool
	ExpiresAt *time.Time
}

// User is the current authenticated GitLab user.
type User struct {
	ID       int64
	Username string
	Name     string
	Bot      bool
}

// MergeRequest is the subset of merge request data needed for command checks
// and review scheduling.
type MergeRequest struct {
	IID             int64
	State           string
	TargetProjectID int64
	SHA             string
	MergedAt        *time.Time
	Title           string
	Description     string
	SourceBranch    string
	TargetBranch    string
	Labels          []string
	Draft           bool
	Author          *User
	WebURL          string
}

// HookSpec describes the project webhook M0 creates.
type HookSpec struct {
	URL                   string
	Secret                string
	PushEvents            bool
	MergeRequestsEvents   bool
	NoteEvents            bool
	EnableSSLVerification bool
}

// Hook is the safe projection of a project webhook used for provisioning
// decisions. GitLab does not return the hook token, so it is not included.
type Hook struct {
	ID                    int64
	URL                   string
	PushEvents            bool
	MergeRequestsEvents   bool
	NoteEvents            bool
	EnableSSLVerification bool
	AlertStatus           string
}

// API is the GitLab surface consumed by the application layer. All methods
// accept the project token explicitly and never log or include it in errors.
type API interface {
	GetProject(ctx context.Context, token, projectRef string) (*Project, error)
	GetTokenInfo(ctx context.Context, token string) (*TokenInfo, error)
	CurrentUser(ctx context.Context, token string) (*User, error)
	GetMergeRequest(ctx context.Context, token string, projectID, iid int64) (*MergeRequest, error)
	GetMemberAccess(ctx context.Context, token string, projectID, userID int64) (int, error)
	ListProjectHooks(ctx context.Context, token string, projectID int64) ([]Hook, error)
	CreateProjectHook(ctx context.Context, token string, projectID int64, spec HookSpec) (int64, error)
	UpdateProjectHook(ctx context.Context, token string, projectID, hookID int64, spec HookSpec) error
	DeleteProjectHook(ctx context.Context, token string, projectID, hookID int64) error
}

// Config configures the client wrapper.
type Config struct {
	BaseURL        string
	RequestTimeout time.Duration
	MaxConcurrency int
}

// Error is a sanitized GitLab failure. It deliberately excludes upstream
// response bodies, request URLs, and credentials.
type Error struct {
	Op     string
	Status int
}

func (e *Error) Error() string {
	if e == nil {
		return "gitlab: error"
	}
	if e.Status > 0 {
		return fmt.Sprintf("gitlab: %s failed (status %d)", e.Op, e.Status)
	}
	return fmt.Sprintf("gitlab: %s failed", e.Op)
}

// StatusOf returns the upstream HTTP status, or zero for transport failures.
func StatusOf(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Status
	}
	return 0
}

// IsNotFound reports whether the upstream returned 404.
func IsNotFound(err error) bool {
	return StatusOf(err) == http.StatusNotFound
}

// ProjectRef is a validated numeric project ID or namespace/path.
type ProjectRef struct {
	ID   int64
	Path string
}

var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// ParseProjectRef validates a project identifier. URLs, attempts at path
// traversal, whitespace, and non-positive numeric IDs are rejected.
func ParseProjectRef(raw string) (ProjectRef, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return ProjectRef{}, errors.New("gitlab: invalid project identifier")
	}
	if len(raw) > 255 {
		return ProjectRef{}, errors.New("gitlab: project identifier is too long")
	}
	if isDigits(raw) {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return ProjectRef{}, errors.New("gitlab: invalid project identifier")
		}
		return ProjectRef{ID: id}, nil
	}
	if !strings.Contains(raw, "/") || strings.HasPrefix(raw, "/") || strings.HasSuffix(raw, "/") ||
		strings.Contains(raw, "//") || strings.ContainsAny(raw, "\\?#% \t\r\n") {
		return ProjectRef{}, errors.New("gitlab: invalid project identifier")
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == "." || segment == ".." || !segmentRE.MatchString(segment) {
			return ProjectRef{}, errors.New("gitlab: invalid project identifier")
		}
	}
	return ProjectRef{Path: raw}, nil
}

func isDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

// Client is the SDK-backed implementation of API.
type Client struct {
	baseURL string
	timeout time.Duration
	http    *http.Client
	sem     chan struct{}
}

// New creates a client. It rejects a base URL that does not have an http(s)
// scheme, a host, or that contains credentials, a query, or a fragment.
func New(cfg Config) (*Client, error) {
	base, err := validateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	concurrency := cfg.MaxConcurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	httpClient := &http.Client{
		Transport: transport.Clone(),
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		baseURL: base,
		timeout: timeout,
		http:    httpClient,
		sem:     make(chan struct{}, concurrency),
	}, nil
}

func validateBaseURL(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("gitlab: invalid base URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("gitlab: invalid base URL")
	}
	return strings.TrimRight(raw, "/"), nil
}

func (c *Client) sdk(token string) (*sdk.Client, error) {
	return sdk.NewClient(token,
		sdk.WithBaseURL(c.baseURL),
		sdk.WithHTTPClient(c.http),
		sdk.WithoutRetries(),
		sdk.WithCustomRetryMax(0),
		sdk.WithURLWarningLogger(slog.New(slog.DiscardHandler)),
	)
}

func (c *Client) prepare(ctx context.Context, token string) (context.Context, *sdk.Client, func(), error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil, nil, errors.New("gitlab: token is required")
	}
	// The request timeout covers both the concurrency wait and the HTTP call.
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	select {
	case c.sem <- struct{}{}:
	case <-reqCtx.Done():
		cancel()
		return nil, nil, nil, reqCtx.Err()
	}
	release := func() { <-c.sem }
	sdkClient, err := c.sdk(token)
	if err != nil {
		cancel()
		release()
		return nil, nil, nil, fmt.Errorf("gitlab: create client: %w", err)
	}
	return reqCtx, sdkClient, func() {
		cancel()
		release()
	}, nil
}

func mapError(ctx context.Context, op string, resp *sdk.Response, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	status := 0
	if resp != nil && resp.Response != nil {
		status = resp.StatusCode
	} else {
		var errorResponse *sdk.ErrorResponse
		if errors.As(err, &errorResponse) && errorResponse.Response != nil {
			status = errorResponse.Response.StatusCode
		}
	}
	// The upstream error text can include URLs and response bodies. Never wrap
	// it; callers only need the operation and status for safe diagnostics.
	return &Error{Op: op, Status: status}
}

// GetProject resolves a numeric ID or namespace/path and returns its metadata.
func (c *Client) GetProject(ctx context.Context, token, projectRef string) (*Project, error) {
	ref, err := ParseProjectRef(projectRef)
	if err != nil {
		return nil, err
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	var pid any = ref.Path
	if ref.ID > 0 {
		pid = ref.ID
	}
	p, resp, err := client.Projects.GetProject(pid, nil, sdk.WithContext(reqCtx))
	if err != nil {
		return nil, mapError(reqCtx, "get_project", resp, err)
	}
	if p == nil || p.ID <= 0 {
		return nil, &Error{Op: "get_project"}
	}
	return &Project{
		ID:                p.ID,
		PathWithNamespace: p.PathWithNamespace,
		WebURL:            p.WebURL,
		HTTPURLToRepo:     p.HTTPURLToRepo,
		DefaultBranch:     p.DefaultBranch,
	}, nil
}

// GetTokenInfo reads the scopes and state of the supplied token.
func (c *Client) GetTokenInfo(ctx context.Context, token string) (*TokenInfo, error) {
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	pat, resp, err := client.PersonalAccessTokens.GetSinglePersonalAccessToken(sdk.WithContext(reqCtx))
	if err != nil {
		return nil, mapError(reqCtx, "get_token", resp, err)
	}
	if pat == nil {
		return nil, &Error{Op: "get_token"}
	}
	info := &TokenInfo{
		Scopes:  append([]string(nil), pat.Scopes...),
		Active:  pat.Active,
		Revoked: pat.Revoked,
	}
	if pat.ExpiresAt != nil {
		t := time.Time(*pat.ExpiresAt)
		info.ExpiresAt = &t
	}
	return info, nil
}

// CurrentUser returns the identity associated with the token.
func (c *Client) CurrentUser(ctx context.Context, token string) (*User, error) {
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	u, resp, err := client.Users.CurrentUser(sdk.WithContext(reqCtx))
	if err != nil {
		return nil, mapError(reqCtx, "current_user", resp, err)
	}
	if u == nil || u.ID <= 0 {
		return nil, &Error{Op: "current_user"}
	}
	return &User{ID: u.ID, Username: u.Username, Name: u.Name, Bot: u.Bot}, nil
}

// GetMergeRequest returns the current state of a merge request.
func (c *Client) GetMergeRequest(ctx context.Context, token string, projectID, iid int64) (*MergeRequest, error) {
	if projectID <= 0 || iid <= 0 {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	mr, resp, err := client.MergeRequests.GetMergeRequest(projectID, iid, nil, sdk.WithContext(reqCtx))
	if err != nil {
		return nil, mapError(reqCtx, "get_merge_request", resp, err)
	}
	if mr == nil {
		return nil, &Error{Op: "get_merge_request"}
	}
	out := &MergeRequest{
		IID:             mr.IID,
		State:           mr.State,
		TargetProjectID: mr.TargetProjectID,
		SHA:             mr.SHA,
		Title:           mr.Title,
		Description:     mr.Description,
		SourceBranch:    mr.SourceBranch,
		TargetBranch:    mr.TargetBranch,
		Labels:          append([]string(nil), mr.Labels...),
		Draft:           mr.Draft,
		WebURL:          mr.WebURL,
	}
	if mr.MergedAt != nil {
		merged := *mr.MergedAt
		out.MergedAt = &merged
	}
	if mr.Author != nil {
		out.Author = &User{ID: mr.Author.ID, Username: mr.Author.Username, Name: mr.Author.Name}
	}
	return out, nil
}

// GetMemberAccess returns the inherited project access level for a user.
func (c *Client) GetMemberAccess(ctx context.Context, token string, projectID, userID int64) (int, error) {
	if projectID <= 0 || userID <= 0 {
		return 0, errors.New("gitlab: invalid project or user ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return 0, err
	}
	defer done()

	member, resp, err := client.ProjectMembers.GetInheritedProjectMember(projectID, userID, sdk.WithContext(reqCtx))
	if err != nil {
		return 0, mapError(reqCtx, "get_member", resp, err)
	}
	if member == nil {
		return 0, &Error{Op: "get_member"}
	}
	return int(member.AccessLevel), nil
}

// ListProjectHooks returns every project webhook. The hook token is never
// returned by GitLab, so callers can only match hooks by ID or URL.
func (c *Client) ListProjectHooks(ctx context.Context, token string, projectID int64) ([]Hook, error) {
	if projectID <= 0 {
		return nil, errors.New("gitlab: invalid project ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	const perPage = int64(100)
	hooks := make([]Hook, 0)
	for page := int64(1); ; page++ {
		if page > int64(maxListPages) {
			return nil, errors.New("gitlab: list_hooks pagination limit exceeded")
		}
		batch, resp, err := client.Projects.ListProjectHooks(projectID,
			&sdk.ListProjectHooksOptions{ListOptions: sdk.ListOptions{Page: page, PerPage: perPage}},
			sdk.WithContext(reqCtx))
		if err != nil {
			return nil, mapError(reqCtx, "list_hooks", resp, err)
		}
		for _, hook := range batch {
			if hook == nil {
				continue
			}
			hooks = append(hooks, Hook{
				ID:                    hook.ID,
				URL:                   hook.URL,
				PushEvents:            hook.PushEvents,
				MergeRequestsEvents:   hook.MergeRequestsEvents,
				NoteEvents:            hook.NoteEvents,
				EnableSSLVerification: hook.EnableSSLVerification,
				AlertStatus:           hook.AlertStatus,
			})
		}
		if int64(len(batch)) < perPage {
			break
		}
	}
	return hooks, nil
}

// CreateProjectHook creates a project webhook. Retries are disabled globally,
// so a failed mutating call is never repeated with a different request ID.
func (c *Client) CreateProjectHook(ctx context.Context, token string, projectID int64, spec HookSpec) (int64, error) {
	if projectID <= 0 {
		return 0, errors.New("gitlab: invalid project ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return 0, err
	}
	defer done()

	options := &sdk.AddProjectHookOptions{
		URL:                   &spec.URL,
		Token:                 &spec.Secret,
		PushEvents:            boolPtr(spec.PushEvents),
		MergeRequestsEvents:   boolPtr(spec.MergeRequestsEvents),
		NoteEvents:            boolPtr(spec.NoteEvents),
		EnableSSLVerification: boolPtr(spec.EnableSSLVerification),
	}
	hook, resp, err := client.Projects.AddProjectHook(projectID, options, sdk.WithContext(reqCtx))
	if err != nil {
		return 0, mapError(reqCtx, "create_hook", resp, err)
	}
	if hook == nil || hook.ID <= 0 {
		return 0, &Error{Op: "create_hook"}
	}
	return hook.ID, nil
}

// UpdateProjectHook reconfigures a managed project webhook. The secret is set
// again so a re-enable restores the token the service verifies on delivery.
func (c *Client) UpdateProjectHook(ctx context.Context, token string, projectID, hookID int64, spec HookSpec) error {
	if projectID <= 0 || hookID <= 0 {
		return errors.New("gitlab: invalid project or hook ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return err
	}
	defer done()

	options := &sdk.EditProjectHookOptions{
		URL:                   &spec.URL,
		Token:                 &spec.Secret,
		PushEvents:            boolPtr(spec.PushEvents),
		MergeRequestsEvents:   boolPtr(spec.MergeRequestsEvents),
		NoteEvents:            boolPtr(spec.NoteEvents),
		EnableSSLVerification: boolPtr(spec.EnableSSLVerification),
	}
	hook, resp, err := client.Projects.EditProjectHook(projectID, hookID, options, sdk.WithContext(reqCtx))
	if err != nil {
		return mapError(reqCtx, "update_hook", resp, err)
	}
	if hook == nil || hook.ID != hookID {
		return &Error{Op: "update_hook"}
	}
	return nil
}

// DeleteProjectHook deletes a webhook. It is used only to compensate a failed
// registration after GitLab already created the hook.
func (c *Client) DeleteProjectHook(ctx context.Context, token string, projectID, hookID int64) error {
	if projectID <= 0 || hookID <= 0 {
		return errors.New("gitlab: invalid project or hook ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return err
	}
	defer done()

	resp, err := client.Projects.DeleteProjectHook(projectID, hookID, sdk.WithContext(reqCtx))
	if err != nil {
		return mapError(reqCtx, "delete_hook", resp, err)
	}
	return nil
}

func boolPtr(value bool) *bool {
	return &value
}
