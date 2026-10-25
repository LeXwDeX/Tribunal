package server

import (
	"context"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// Store is the read surface of the persistence layer used by the M6 admin
// API. The SQLite store implements all of it; handlers check for a nil Store
// and answer 503 when the operator did not wire one.
//
// The interface deliberately exposes only what the HTTP layer consumes so
// tests can use small fakes for the review, workspace and stats endpoints.
type Store interface {
	ListWorkflows(ctx context.Context, filter store.WorkflowFilter, cursor string, limit int) ([]store.Workflow, string, error)
	GetWorkflow(ctx context.Context, id string) (*store.Workflow, error)
	GetWorkflowNodes(ctx context.Context, workflowID string) ([]store.WorkflowNode, error)
	GetWorkflowDossier(ctx context.Context, workflowID string) (*store.WorkflowDossier, error)
	ListFindings(ctx context.Context, filter store.FindingFilter) ([]store.Finding, error)
	SetFindingAnnotation(ctx context.Context, workflowID, findingID, annotation, annotatedBy string) (store.Finding, error)
	ListLLMCalls(ctx context.Context, filter store.LLMCallFilter) ([]store.LLMCall, string, error)
	ListPublications(ctx context.Context, workflowID string) ([]store.Publication, error)
	Stats(ctx context.Context, filter store.StatsFilter) (store.Stats, error)

	GetWorkspace(ctx context.Context, projectID int64) (*store.Workspace, error)
	GetCurrentContextDoc(ctx context.Context, projectID int64, kind string) (*store.ContextDoc, error)
	ListContextDocs(ctx context.Context, filter store.ContextDocFilter) ([]store.ContextDoc, string, error)
	SaveManualContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error)
	LockContextDoc(ctx context.Context, projectID int64, baseSHA, kind string, locked bool, userID *int64) error
}

// ReviewController starts, cancels and republishes review workflows. It
// returns durable job IDs; the app layer implements it. The interface keeps
// internal/server free of app and review imports.
type ReviewController interface {
	StartReview(ctx context.Context, projectID, mrIID int64, profile string, full bool) (jobID string, err error)
	CancelReview(ctx context.Context, workflowID string) (jobID string, err error)
	RepublishReview(ctx context.Context, workflowID string) (jobID string, err error)
}

// WorkspaceService enqueues workspace refresh jobs.
type WorkspaceService interface {
	RefreshWorkspace(ctx context.Context, projectID int64, regenerateContext, reindex bool) (jobID string, err error)
}

// ContextService saves manual context revisions and lock changes. It owns the
// database-to-workspace projection; production handlers must not call the
// store directly because that would skip the disk projection.
type ContextService interface {
	SaveContext(ctx context.Context, projectID int64, kind, content string, locked *bool, userID *int64) (*store.ContextDoc, error)
	LockContext(ctx context.Context, projectID int64, kind string, locked bool, userID *int64) error
}

// MCPTester starts one MCP server and lists its tools. The server entry passed
// in carries the unmasked config; the server layer masks only responses.
type MCPTester interface {
	TestMCPServer(ctx context.Context, server MCPServer) ([]MCPTool, error)
}

// ProviderTestResult is the outcome of a provider connectivity probe.
type ProviderTestResult struct {
	Model     string
	LatencyMS int64
}

// ProviderTester sends one minimal request to a configured provider.
type ProviderTester interface {
	TestProvider(ctx context.Context, name string) (ProviderTestResult, error)
}

// SessionStore persists browser sessions. IDHash is the SHA-256 digest of the
// random cookie token; plaintext tokens never reach this interface.
type SessionStore interface {
	CreateSession(ctx context.Context, session store.Session, limit int) error
	GetSession(ctx context.Context, idHash []byte, now time.Time) (*store.Session, error)
	DeleteSession(ctx context.Context, idHash []byte) error
}

// TokenStore persists API tokens per browser session user. Plaintext never
// reaches this interface; lookup receives the SHA-256 digest only.
type TokenStore interface {
	CreateAPIToken(ctx context.Context, user store.TokenUser, token store.APIToken) (*store.APIToken, error)
	ListAPITokens(ctx context.Context, userID int64) ([]store.APIToken, error)
	ListAPITokensPage(ctx context.Context, userID int64, cursor string, limit int) ([]store.APIToken, string, error)
	DeleteAPIToken(ctx context.Context, userID int64, id string) error
	FindAPITokenByHash(ctx context.Context, hash []byte, now time.Time) (*store.APIToken, *store.TokenUser, error)
}

// ExtensionService persists MCP servers, skills and block bindings. The app
// layer implements it over the SQLite extension tables.
type ExtensionService interface {
	ListMCPServers(ctx context.Context, scope string, projectID *int64) ([]MCPServer, error)
	ListMCPServersPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]MCPServer, string, error)
	GetMCPServer(ctx context.Context, id string) (*MCPServer, error)
	CreateMCPServer(ctx context.Context, in MCPServerInput) (*MCPServer, error)
	UpdateMCPServer(ctx context.Context, id string, in MCPServerUpdate) (*MCPServer, error)
	DeleteMCPServer(ctx context.Context, id string) error
	ImportMCPServers(ctx context.Context, in MCPImportInput) ([]MCPServer, error)

	ListSkills(ctx context.Context, scope string, projectID *int64) ([]Skill, error)
	ListSkillsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]Skill, string, error)
	UploadSkill(ctx context.Context, in SkillUpload) (*Skill, error)
	RegisterSkillDir(ctx context.Context, in SkillDirInput) (*Skill, error)
	DeleteSkill(ctx context.Context, id string) error

	ListBlockBindings(ctx context.Context, scope string, projectID *int64) ([]BlockBinding, error)
	ListBlockBindingsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]BlockBinding, string, error)
	UpsertBlockBinding(ctx context.Context, in BlockBindingInput) (*BlockBinding, error)
}

// extensionScope is the shared scope selector for MCP servers, skills and
// block bindings.
type extensionScope struct {
	scope     string
	projectID *int64
}

// streamPollInterval is the default SSE snapshot interval.
const streamPollInterval = 2 * time.Second
