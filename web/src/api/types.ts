// API response types. Fields are optional where the backend design does not
// guarantee a value; the UI must show an explicit unavailable state instead of
// inventing defaults.

export type Role = 'admin' | 'viewer'
export type ProjectAccess = 'read' | 'manage'
export type PublicationState = 'pending' | 'published' | 'skipped' | 'failed'

export interface Me {
  id: number | string
  username: string
  name?: string
  role: Role
  csrf_token: string
}

export interface ApiErrorBody {
  error?: {
    code?: string
    message?: string
    request_id?: string
    details?: Array<{ field?: string; message?: string }>
  }
}

export interface Page<T> {
  next_cursor?: string
  [key: string]: unknown
}

export type ProjectPage = Page<Project> & { projects: Project[] }
export type ReviewPage = Page<ReviewSummary> & { reviews: ReviewSummary[] }
export type FindingPage = Page<Finding> & { findings: Finding[] }
export type McpServerPage = Page<McpServer> & { servers: McpServer[] }
export type SkillPage = Page<Skill> & { skills: Skill[] }
export type BindingPage = Page<BlockBinding> & { bindings: BlockBinding[] }
export type ProviderPage = Page<Provider> & { providers: Provider[] }
export type ProfilesPage = Page<Profile> & { profiles: Profile[] }
export type ApiTokenPage = Page<ApiToken> & { tokens: ApiToken[] }
export type ContextDocPage = Page<ContextDoc> & { documents: ContextDoc[] }
export type CallPage = Page<LlmCall> & { calls: LlmCall[] }

export interface AdminUser {
  id: number
  username: string
  name?: string
  disabled: boolean
  last_login_at?: string
}

export interface AdminProject {
  id: number
  path_with_namespace: string
}

export interface ProjectGrant {
  project_id: number
  user_id: number
  role: ProjectAccess
  username?: string
}

export type AdminUserPage = Page<AdminUser> & { users: AdminUser[] }
export type AdminProjectPage = Page<AdminProject> & { projects: AdminProject[] }
export type ProjectGrantPage = Page<ProjectGrant> & { grants: ProjectGrant[] }

export interface ProjectSettings {
  language?: string
  skip_draft?: boolean
  target_branches?: string[]
  labels_include?: string[]
  labels_exclude?: string[]
  ignore?: string[]
  max_files?: number
  max_diff_lines?: number
  commit_status?: boolean
  post_failure_note?: boolean
  show_model_attribution?: boolean
  allow_repo_config?: boolean
  allow_repo_skills?: boolean
  profile_by_label?: Record<string, string>
  custom_rules?: string[]
  [key: string]: unknown
}

export interface Workspace {
  project_id?: number
  state?: string
  base_sha?: string
  index_sha?: string
  context_sha?: string
  last_refresh_at?: string
  last_error?: string
  updated_at?: string
  index_status?: string
}

export interface ProjectWorkspaceSummary {
  state?: string
  base_sha?: string
  index_status?: string
  last_refresh_at?: string
  last_error?: string
}

export interface Project {
  id: number
  path_with_namespace: string
  access?: ProjectAccess
  web_url?: string
  default_branch?: string
  token_expires_at?: string
  token_scopes?: string[]
  hook_id?: number
  webhook_state?: 'disabled' | 'pending' | 'enabled' | 'failed' | 'unknown' | string
  webhook_events?: string[]
  webhook_error?: string
  webhook_managed_by?: string
  webhook_created_at?: string
  webhook_updated_at?: string
  profile: string
  settings: ProjectSettings
  enabled: boolean
  created_at?: string
  updated_at?: string
  workspace?: ProjectWorkspaceSummary
  workspace_state?: string
  last_review?: ReviewSummary
}

export interface ProjectRegistration {
  project: Project
  job_id: string
  webhook_secret?: string
}

export interface ProjectWebhookState {
  project?: Project
  hook_id: number
  state: string
  events: string[]
  last_error: string
  managed_by?: string
  created_at?: string
  updated_at?: string
}

export interface ProjectValidation {
  project_id: number
  path_with_namespace: string
  web_url: string
  default_branch: string
  username: string
  access_level: number
  token_scopes: string[]
  token_expires_at?: string
  git_head: string
}

export interface PatchProject {
  profile?: string
  settings?: ProjectSettings
  enabled?: boolean
}

export type Severity = 'P0' | 'P1' | 'P2' | 'info'
export type FindingDecision = 'publish' | 'drop' | 'mark' | 'downgrade'
export type ChallengeStatus = 'confirmed' | 'refuted' | 'uncertain' | 'skipped'
export type FindingAnnotation = '' | 'valid' | 'invalid' | 'uncertain'

export interface GraphNode {
  id: string
  kind: string
  status: string
  depends_on?: string[]
  provider?: string
  attempt?: number
}

export interface GraphEdge {
  from: string
  to: string
}

export interface ReviewGraph {
  nodes: GraphNode[]
  edges: GraphEdge[]
}

export interface FindingsSummary {
  published?: number
  marked?: number
  dropped?: number
  by_severity?: Partial<Record<Severity, number>>
  [key: string]: unknown
}

export interface ReviewUsage {
  tokens_in?: number
  tokens_out?: number
  cost_micros?: number
}

export interface Publications {
  summary_note_id?: number
  discussions?: number
  commit_status?: string
  [key: string]: unknown
}

export interface ReviewSummary {
  id: string
  project_id: number
  project_path?: string
  mr_iid: number
  mr_title?: string
  head_sha?: string
  base_sha?: string
  profile?: string
  trigger?: string
  status: string
  verdict?: string | null
  error?: string
  publication_state?: PublicationState
  publication_error?: string
  findings_summary?: FindingsSummary
  usage?: ReviewUsage
  duration_ms?: number
  created_at?: string
  started_at?: string
  finished_at?: string
  nodes?: GraphNode[]
}

export interface ReviewDetail extends ReviewSummary {
  access?: ProjectAccess
  publications?: Publications
  error?: string
  cancel_reason?: string
  fingerprint?: string
}

export interface Finding {
  id: string
  workflow_id: string
  origin_node: string
  origin_index?: number
  origin_provider?: string
  severity: Severity
  final_severity?: Severity
  category?: string
  path?: string
  line?: number
  side?: string
  title: string
  body?: string
  evidence?: string | null
  confidence?: string
  votes?: number
  challenge_status?: ChallengeStatus
  challenge_node?: string
  challenge_evidence?: string | null
  decision?: FindingDecision
  decision_reason?: string
  position_ok?: boolean
  annotation?: FindingAnnotation
  annotated_by?: string
  annotated_at?: string
}

export interface NodeUsage {
  tokens_in?: number
  tokens_out?: number
  cost_micros?: number
}

export interface ReviewNodeDetail {
  id: string
  kind: string
  status: string
  depends_on?: string[]
  provider?: string
  attempt?: number
  duration_ms?: number
  params?: unknown
  input?: unknown
  output?: unknown
  error?: string
  usage?: NodeUsage
  started_at?: string
  finished_at?: string
}

export interface LlmCall {
  id?: string
  workflow_id?: string
  node_id?: string
  turn?: number
  provider?: string
  model?: string
  request?: unknown
  response?: unknown
  input_tokens?: number
  output_tokens?: number
  cache_read?: number
  cache_write?: number
  latency_ms?: number
  stop_reason?: string
  error_kind?: string
  created_at?: string
}

export interface ContextDoc {
  id: string
  project_id: number
  base_sha?: string
  kind: string
  source?: string
  content: string
  locked?: boolean
  model?: string
  workflow_id?: string
  created_at?: string
  updated_at?: string
  updated_by?: number
}

export interface McpServer {
  id: string
  scope: 'platform' | 'project'
  project_id?: number
  name: string
  config: Record<string, unknown>
  enabled: boolean
  status?: string
  last_error?: string
  tools?: McpTool[]
  created_at?: string
  updated_at?: string
}

export interface McpTool {
  name: string
  description?: string
}

export interface Skill {
  id: string
  scope: 'platform' | 'project'
  project_id?: number
  name: string
  description?: string
  dir?: string
  enabled: boolean
  /** Shipped with the platform: can be disabled, not deleted. */
  builtin?: boolean
  created_at?: string
}

export interface BlockBinding {
  scope: 'platform' | 'project'
  project_id?: number
  block: string
  mcp_tools: string[]
  skills: string[]
}

export interface ProviderStats24h {
  calls?: number
  errors?: number
  error_rate?: number
  p50_latency_ms?: number
  [key: string]: unknown
}

export interface Provider {
  name: string
  protocol?: string
  base_url?: string
  model?: string
  max_concurrency?: number
  rpm?: number
  max_tokens?: number
  api_key_env?: string
  stats_24h?: ProviderStats24h
  calls_24h?: number
  errors_24h?: number
  error_rate_24h?: number
  p50_latency_ms?: number
  last_error?: string
  [key: string]: unknown
}

export interface ProviderTestResult {
  ok?: boolean
  message?: string
  model?: string
  latency_ms?: number
  error?: string
  [key: string]: unknown
}

export interface ProfileConfig {
  plan?: Record<string, unknown>
  propose?: Array<Record<string, unknown>>
  reasoner?: Record<string, unknown>
  consensus?: Record<string, unknown>
  challenge?: Record<string, unknown>
  on_uncertain?: string
  synthesize?: Record<string, unknown>
  budget?: Record<string, unknown>
  agent?: Record<string, unknown>
  [key: string]: unknown
}

export interface Profile {
  name: string
  config?: ProfileConfig
  [key: string]: unknown
}

export interface DailyStats {
  day: string
  runs: number
  cost_micros: number
  duration_samples: number
  avg_duration_ms: number | null
  p95_duration_ms: number | null
}

export interface StatsBreakdown {
  runs: number
  cost_micros: number
  duration_samples: number
  avg_duration_ms: number | null
  p95_duration_ms: number | null
  trend: DailyStats[]
}

export interface ProjectStatsRow extends StatsBreakdown {
  project_id: number
  blocking: number
  published: number
}

export interface ProviderStatsRow extends StatsBreakdown {
  provider: string
}

export interface BlockStatsRow extends StatsBreakdown {
  block: string
}

export interface ProviderQualityRow {
  provider: string
  proposed: number
  reviewed: number
  valid: number
  invalid: number
  accuracy: number | null
  overturned: number
  overturned_ratio: number | null
  refuted: number
  refuted_reviewed: number
  false_refuted: number
  false_kill_rate: number | null
}

// Older servers omit some fields; render only keys that are actually present.
export interface Stats extends Record<string, unknown> {
  duration_samples?: number
  trend?: DailyStats[]
  by_project?: ProjectStatsRow[]
  by_provider?: ProviderStatsRow[]
  by_block?: BlockStatsRow[]
  quality_by_provider?: ProviderQualityRow[]
}

export interface PlatformSettings {
  [key: string]: unknown
}

export interface ApiToken {
  id: string | number
  name: string
  created_at?: string
  expires_at?: string
  last_used_at?: string
}

export interface CreatedApiToken {
  id: string | number
  token: string
  name?: string
}

export interface WorkspaceRefreshResult {
  job_id?: string
}

export interface TriggerReviewResult {
  job_id?: string
}

export type CollectionKey =
  | 'projects'
  | 'reviews'
  | 'findings'
  | 'servers'
  | 'skills'
  | 'bindings'
  | 'providers'
  | 'profiles'
  | 'calls'
  | 'documents'
  | 'tokens'

export interface FindingFilters {
  decision?: string
  severity?: string
}
