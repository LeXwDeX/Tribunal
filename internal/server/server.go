// Package server implements the HTTP surface: health checks, GitLab webhook
// ingress, authenticated project administration, OAuth sessions, and the
// embedded web console.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	webhookBodyLimit = 5 << 20
	adminBodyLimit   = 1 << 20
	webhookTimeout   = 900 * time.Millisecond
	pingTimeout      = time.Second
)

// AdminService is the project registration and lifecycle surface.
type AdminService interface {
	Register(ctx context.Context, in app.RegisterInput) (*app.Registration, error)
	Validate(ctx context.Context, in app.ValidateProjectInput) (*app.ProjectValidation, error)
	List(ctx context.Context) ([]store.Project, error)
	ListFiltered(ctx context.Context, filter app.ProjectListFilter) ([]store.Project, error)
	ListFilteredPage(ctx context.Context, filter app.ProjectListFilter, cursor string, limit int) ([]store.Project, string, error)
	Get(ctx context.Context, id int64) (*store.Project, error)
	Update(ctx context.Context, id int64, in app.UpdateProjectInput) (*store.Project, error)
	RotateToken(ctx context.Context, id int64, token string) (*store.Project, error)
	CreateWebhook(ctx context.Context, id int64) (*app.WebhookRegistration, error)
	RebuildWebhook(ctx context.Context, id int64) (*app.WebhookRegistration, error)
	DisableWebhook(ctx context.Context, id int64) (*store.Project, error)
	Delete(ctx context.Context, id int64) error
}

// ProjectResidueService inspects and removes local files left by unregistered
// projects. It is optional; the routes answer 503 when unwired.
type ProjectResidueService interface {
	ListProjectResidues(ctx context.Context) ([]app.ProjectResidue, error)
	CleanupProjectResidues(ctx context.Context) (*app.ProjectResidueCleanup, error)
}

// WebhookAcceptor accepts one authenticated delivery.
type WebhookAcceptor interface {
	Handle(ctx context.Context, req app.WebhookRequest) (app.WebhookResult, error)
}

// Pinger checks database availability for readiness.
type Pinger interface {
	Ping(ctx context.Context) error
}

// WorkerState reports whether the job worker is running.
type WorkerState interface {
	Ready() bool
}

// Config configures the HTTP server. M0 fields are required; the M6 admin
// dependencies are optional and their routes answer 503 when unwired.
type Config struct {
	AdminToken string
	Projects   AdminService
	Residues   ProjectResidueService
	Webhooks   WebhookAcceptor
	Database   Pinger
	Worker     WorkerState
	Auth       AuthConfig
	Logger     *slog.Logger

	Store      Store
	Reviews    ReviewController
	Workspace  WorkspaceService
	Contexts   ContextService
	Extensions ExtensionService
	MCP        MCPTester
	LLM        ProviderTester
	Tokens     TokenStore
	Sessions   SessionStore
	Access     AccessStore
	Platform   *config.Config
	StreamPoll time.Duration
	Now        func() time.Time
}

// Server routes all HTTP requests.
type Server struct {
	adminToken string
	projects   AdminService
	residues   ProjectResidueService
	webhooks   WebhookAcceptor
	database   Pinger
	worker     WorkerState
	auth       *authService
	sessions   SessionStore
	bindings   AdminBindingStore // from the session or token store; SQLite implements it
	roles      PlatformRoleStore // from the session or token store; SQLite implements it
	web        fs.FS
	logger     *slog.Logger
	handler    http.Handler

	store      Store
	reviews    ReviewController
	workspace  WorkspaceService
	contexts   ContextService
	extensions ExtensionService
	mcp        MCPTester
	llm        ProviderTester
	tokens     TokenStore
	access     AccessStore
	platform   *config.Config
	streamPoll time.Duration
	now        func() time.Time

	// shutdown closes when the HTTP server starts shutting down. Long-lived
	// responses watch it because Shutdown does not cancel active requests.
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

// New validates dependencies and builds the routing tree.
func New(cfg Config) (*Server, error) {
	if cfg.Projects == nil || cfg.Webhooks == nil {
		return nil, errors.New("server: project and webhook services are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	auth, err := newAuthService(cfg.Auth)
	if err != nil {
		return nil, err
	}
	web, err := newWebFS()
	if err != nil {
		return nil, err
	}
	sessions := cfg.Sessions
	if sessions == nil {
		// The SQLite store implements both interfaces; reusing the token store
		// keeps test wiring that predates Sessions working without a second
		// in-memory session backend.
		sessions, _ = cfg.Tokens.(SessionStore)
	}
	if auth.enabled() && sessions == nil {
		return nil, errors.New("server: session store is required when OAuth is configured")
	}
	bindings, _ := sessions.(AdminBindingStore)
	if bindings == nil {
		bindings, _ = cfg.Tokens.(AdminBindingStore)
	}
	roles, _ := sessions.(PlatformRoleStore)
	if roles == nil {
		roles, _ = cfg.Tokens.(PlatformRoleStore)
	}
	s := &Server{
		adminToken: cfg.AdminToken,
		projects:   cfg.Projects,
		residues:   cfg.Residues,
		webhooks:   cfg.Webhooks,
		database:   cfg.Database,
		worker:     cfg.Worker,
		auth:       auth,
		sessions:   sessions,
		bindings:   bindings,
		roles:      roles,
		web:        web,
		logger:     logger,
		store:      cfg.Store,
		reviews:    cfg.Reviews,
		workspace:  cfg.Workspace,
		contexts:   cfg.Contexts,
		extensions: cfg.Extensions,
		mcp:        cfg.MCP,
		llm:        cfg.LLM,
		tokens:     cfg.Tokens,
		access:     cfg.Access,
		platform:   cfg.Platform,
		streamPoll: cfg.StreamPoll,
		now:        cfg.Now,
		shutdown:   make(chan struct{}),
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.syncPlatformRoles()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("POST /webhooks/gitlab", s.handleWebhook)
	mux.HandleFunc("GET /auth/status", s.handleAuthStatus)
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/v1/me", s.apiAuth(s.handleMe))
	mux.HandleFunc("POST /api/v1/projects", s.apiAuth(s.guard(policyPlatformAdmin, s.handleRegisterProject)))
	mux.HandleFunc("POST /api/v1/projects/validate", s.apiAuth(s.guard(policyPlatformAdmin, s.handleValidateProject)))
	mux.HandleFunc("GET /api/v1/projects", s.apiAuth(s.handleListProjects))
	mux.HandleFunc("GET /api/v1/projects/{id}", s.apiAuth(s.guard(policyProjectRead, s.handleGetProject)))
	mux.HandleFunc("PATCH /api/v1/projects/{id}", s.apiAuth(s.guard(policyProjectManage, s.handleUpdateProject)))
	mux.HandleFunc("PUT /api/v1/projects/{id}/token", s.apiAuth(s.guard(policyProjectManage, s.handleRotateProjectToken)))
	mux.HandleFunc("POST /api/v1/projects/{id}/webhook", s.apiAuth(s.guard(policyProjectManage, s.handleCreateProjectWebhook)))
	mux.HandleFunc("POST /api/v1/projects/{id}/webhook/rebuild", s.apiAuth(s.guard(policyProjectManage, s.handleRebuildProjectWebhook)))
	mux.HandleFunc("DELETE /api/v1/projects/{id}/webhook", s.apiAuth(s.guard(policyProjectManage, s.handleDisableProjectWebhook)))
	mux.HandleFunc("DELETE /api/v1/projects/{id}", s.apiAuth(s.guard(policyProjectManage, s.handleDeleteProject)))
	// Reviews (workflows).
	mux.HandleFunc("GET /api/v1/reviews", s.apiAuth(s.handleListReviews))
	mux.HandleFunc("POST /api/v1/projects/{id}/merge_requests/{iid}/reviews", s.apiAuth(s.guard(policyProjectManage, s.handleTriggerReview)))
	mux.HandleFunc("GET /api/v1/reviews/{wf_id}", s.apiAuth(s.guard(policyReviewRead, s.handleGetReview)))
	mux.HandleFunc("GET /api/v1/reviews/{wf_id}/graph", s.apiAuth(s.guard(policyReviewRead, s.handleReviewGraph)))
	mux.HandleFunc("GET /api/v1/reviews/{wf_id}/findings", s.apiAuth(s.guard(policyReviewRead, s.handleReviewFindings)))
	mux.HandleFunc("PUT /api/v1/reviews/{wf_id}/findings/{finding_id}/annotation", s.apiAuth(s.guard(policyReviewManage, s.handleSetFindingAnnotation)))
	mux.HandleFunc("GET /api/v1/reviews/{wf_id}/nodes/{node_id}", s.apiAuth(s.guard(policyReviewRead, s.handleReviewNode)))
	mux.HandleFunc("GET /api/v1/reviews/{wf_id}/nodes/{node_id}/calls", s.apiAuth(s.guard(policyReviewRead, s.handleNodeCalls)))
	mux.HandleFunc("GET /api/v1/reviews/{wf_id}/stream", s.apiAuth(s.guard(policyReviewRead, s.handleReviewStream)))
	mux.HandleFunc("POST /api/v1/reviews/{wf_id}/cancel", s.apiAuth(s.guard(policyReviewManage, s.handleCancelReview)))
	mux.HandleFunc("POST /api/v1/reviews/{wf_id}/republish", s.apiAuth(s.guard(policyReviewManage, s.handleRepublishReview)))
	// Workspace and context documents.
	mux.HandleFunc("GET /api/v1/projects/{id}/workspace", s.apiAuth(s.guard(policyProjectRead, s.handleGetWorkspace)))
	mux.HandleFunc("POST /api/v1/projects/{id}/workspace/refresh", s.apiAuth(s.guard(policyProjectManage, s.handleRefreshWorkspace)))
	mux.HandleFunc("GET /api/v1/projects/{id}/context-docs", s.apiAuth(s.guard(policyProjectRead, s.handleListContextDocs)))
	mux.HandleFunc("GET /api/v1/projects/{id}/context-docs/current", s.apiAuth(s.guard(policyProjectRead, s.handleGetCurrentContextDoc)))
	mux.HandleFunc("PUT /api/v1/projects/{id}/context-docs/current", s.apiAuth(s.guard(policyProjectManage, s.handleSaveCurrentContextDoc)))
	mux.HandleFunc("POST /api/v1/projects/{id}/context-docs/current/lock", s.apiAuth(s.guard(policyProjectManage, s.handleLockContextDoc)))
	// Extensions: MCP servers, skills, block bindings. Scope-level grants are
	// checked inside each handler because the selector is in the query or body.
	mux.HandleFunc("GET /api/v1/mcp-servers", s.apiAuth(s.guard(policyAuthenticated, s.handleListMCPServers)))
	mux.HandleFunc("POST /api/v1/mcp-servers", s.apiAuth(s.guard(policyAuthenticated, s.handleCreateMCPServer)))
	mux.HandleFunc("POST /api/v1/mcp-servers/import", s.apiAuth(s.guard(policyAuthenticated, s.handleImportMCPServers)))
	mux.HandleFunc("PUT /api/v1/mcp-servers/{id}", s.apiAuth(s.guard(policyAuthenticated, s.handleUpdateMCPServer)))
	mux.HandleFunc("DELETE /api/v1/mcp-servers/{id}", s.apiAuth(s.guard(policyAuthenticated, s.handleDeleteMCPServer)))
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/test", s.apiAuth(s.guard(policyAuthenticated, s.handleTestMCPServer)))
	mux.HandleFunc("GET /api/v1/skills", s.apiAuth(s.guard(policyAuthenticated, s.handleListSkills)))
	mux.HandleFunc("POST /api/v1/skills", s.apiAuth(s.guard(policyAuthenticated, s.handleUploadSkillOrRegisterDirectory)))
	mux.HandleFunc("DELETE /api/v1/skills/{id}", s.apiAuth(s.guard(policyPlatformAdmin, s.handleDeleteSkill)))
	// Every skill write needs the platform admin role. Checking it before the
	// lookup keeps a missing and an existing skill indistinguishable to others.
	mux.HandleFunc("PATCH /api/v1/skills/{id}", s.apiAuth(s.guard(policyPlatformAdmin, s.handleUpdateSkill)))
	mux.HandleFunc("GET /api/v1/block-bindings", s.apiAuth(s.guard(policyAuthenticated, s.handleListBlockBindings)))
	mux.HandleFunc("PUT /api/v1/block-bindings/{block}", s.apiAuth(s.guard(policyAuthenticated, s.handleUpsertBlockBinding)))
	// System.
	mux.HandleFunc("GET /api/v1/providers", s.apiAuth(s.guard(policyPlatformAdmin, s.handleListProviders)))
	mux.HandleFunc("POST /api/v1/providers/{name}/test", s.apiAuth(s.guard(policyPlatformAdmin, s.handleTestProvider)))
	mux.HandleFunc("GET /api/v1/profiles", s.apiAuth(s.guard(policyPlatformAdmin, s.handleListProfiles)))
	mux.HandleFunc("GET /api/v1/stats", s.apiAuth(s.handleStats))
	mux.HandleFunc("GET /api/v1/settings", s.apiAuth(s.guard(policyPlatformAdmin, s.handleSettings)))
	// API tokens (browser self-service; CSRF still applies).
	mux.HandleFunc("GET /api/v1/api-tokens", s.apiAuth(s.guard(policyAuthenticated, s.handleListAPITokens)))
	mux.HandleFunc("POST /api/v1/api-tokens", s.apiAuth(s.guard(policyAuthenticated, s.handleCreateAPIToken)))
	mux.HandleFunc("DELETE /api/v1/api-tokens/{id}", s.apiAuth(s.guard(policyAuthenticated, s.handleDeleteAPIToken)))
	// Access administration. The documented /admin surface and the /api/v1
	// equivalent share handlers.
	for _, prefix := range []string{"/admin", "/api/v1/admin"} {
		mux.HandleFunc("GET "+prefix+"/users", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminListUsers)))
		mux.HandleFunc("PATCH "+prefix+"/users/{id}", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminSetUserDisabled)))
		mux.HandleFunc("DELETE "+prefix+"/users/{id}/credentials", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminRevokeCredentials)))
		mux.HandleFunc("GET "+prefix+"/projects", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminListProjects)))
		mux.HandleFunc("GET "+prefix+"/projects/residues", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminListProjectResidues)))
		mux.HandleFunc("POST "+prefix+"/projects/residues/cleanup", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminCleanupProjectResidues)))
		mux.HandleFunc("POST "+prefix+"/projects/{id}/onboarding/retry", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminRetryOnboarding)))
		mux.HandleFunc("GET "+prefix+"/projects/{id}/grants", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminListGrants)))
		mux.HandleFunc("PUT "+prefix+"/projects/{id}/grants/{user_id}", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminSetGrant)))
		mux.HandleFunc("DELETE "+prefix+"/projects/{id}/grants/{user_id}", s.apiAuth(s.guard(policyPlatformAdmin, s.handleAdminDeleteGrant)))
	}
	mux.HandleFunc("/api/", s.apiAuth(s.handleAPINotFound))
	mux.HandleFunc("/api/v1", s.apiAuth(s.handleAPINotFound))
	mux.HandleFunc("/", s.handleWeb)

	s.handler = s.requestID(s.logRequests(s.recoverPanic(mux)))
	return s, nil
}

// Handler returns the composed HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// BeginShutdown ends open review streams. Register it with
// http.Server.RegisterOnShutdown; it is safe to call more than once.
func (s *Server) BeginShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

type contextKey int

const requestIDKey contextKey = iota

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.NewV7()
		requestID := ""
		if err == nil {
			requestID = id.String()
		} else {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer (for
// example to set a webhook body read deadline).
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelWarn
		}
		s.logger.Log(r.Context(), level, "http request",
			"request_id", requestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", recorder.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() == nil {
				return
			}
			if responseStarted(w) {
				// The panic value can contain request data; do not log it.
				s.logger.Warn("panic after response started", "request_id", requestID(r.Context()), "path", r.URL.Path)
				return
			}
			// The panic value can contain request data; do not log it.
			s.logger.Warn("panic recovered", "request_id", requestID(r.Context()), "path", r.URL.Path)
			if r.URL.Path == "/webhooks/gitlab" {
				writeWebhookResponse(w, r, map[string]any{"ignored": "internal_error"})
				return
			}
			writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		}()
		next.ServeHTTP(w, r)
	})
}

func responseStarted(w http.ResponseWriter) bool {
	if recorder, ok := w.(*statusWriter); ok {
		return recorder.status != 0
	}
	return false
}

func (s *Server) authorized(r *http.Request) bool {
	if s.adminToken == "" {
		return false
	}
	value := bearerValue(r)
	if value == "" {
		return false
	}
	return constantTimeEqualStrings(s.adminToken, value)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("server: multiple JSON values")
		}
		return err
	}
	return nil
}

func parseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("server: invalid id")
	}
	return id, nil
}
