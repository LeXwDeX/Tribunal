import { LOGOUT_EVENT, request, segment, streamUrl, withQuery } from './client'
import type {
  AdminProject,
  AuthStatus,
  AdminProjectPage,
  AdminUser,
  AdminUserPage,
  ApiToken,
  ApiTokenPage,
  BlockBinding,
  BindingPage,
  ContextDoc,
  ContextDocPage,
  CreatedApiToken,
  Finding,
  FindingAnnotation,
  FindingPage,
  FindingsSummary,
  LlmCall,
  McpServer,
  McpServerPage,
  McpTool,
  Me,
  CallPage,
  Page,
  ProjectAccess,
  ProjectGrant,
  ProjectGrantPage,
  ProjectPage,
  ProviderPage,
  ReviewPage,
  PlatformSettings,
  Profile,
  ProfileConfig,
  ProfilesPage,
  Project,
  ProjectRegistration,
  ProjectValidation,
  ProjectWebhookState,
  Provider,
  ProviderTestResult,
  ReviewDetail,
  ReviewGraph,
  ReviewNodeDetail,
  ReviewSummary,
  Skill,
  SkillPage,
  Stats,
  TriggerReviewResult,
  Workspace,
} from './types'

export async function fetchMe(): Promise<Me> {
  return request<Me>('/api/v1/me')
}

export async function fetchAuthStatus(): Promise<AuthStatus> {
  return request<AuthStatus>('/auth/status')
}

export async function logout(): Promise<void> {
  await request<void>('/auth/logout', { method: 'POST' })
  window.dispatchEvent(new Event(LOGOUT_EVENT))
}

export const authLoginUrl = '/auth/login'

// Projects -----------------------------------------------------------------

export interface ProjectFilters {
  q?: string
  enabled?: boolean
  limit?: number
}

export async function listProjects(filters: ProjectFilters = {}, cursor?: string): Promise<ProjectPage> {
  return request<ProjectPage>(withQuery('/api/v1/projects', { ...filters, cursor }))
}

export async function getProject(id: number | string): Promise<Project> {
  return request<Project>(`/api/v1/projects/${segment(id)}`)
}

export interface RegisterProjectInput {
  project: string
  token: string
  profile: string
  settings: Record<string, unknown>
}

export async function registerProject(input: RegisterProjectInput): Promise<ProjectRegistration> {
  return request<ProjectRegistration>('/api/v1/projects', { method: 'POST', body: input })
}

export async function validateProjectConnection(input: { project: string; token: string }): Promise<ProjectValidation> {
  return request<ProjectValidation>('/api/v1/projects/validate', { method: 'POST', body: input })
}

export interface UpdateProjectInput {
  profile?: string
  settings?: Record<string, unknown>
  enabled?: boolean
}

export async function updateProject(id: number | string, input: UpdateProjectInput): Promise<Project> {
  return request<Project>(`/api/v1/projects/${segment(id)}`, { method: 'PATCH', body: input })
}

export async function rotateProjectToken(id: number | string, token: string): Promise<void> {
  await request<void>(`/api/v1/projects/${segment(id)}/token`, { method: 'PUT', body: { token } })
}

// Explicit webhook lifecycle. The legacy recreateWebhook name stays as a
// rebuild alias for existing callers.
export async function enableWebhook(id: number | string): Promise<ProjectWebhookState> {
  return request<ProjectWebhookState>(`/api/v1/projects/${segment(id)}/webhook`, { method: 'POST' })
}

export async function rebuildWebhook(id: number | string): Promise<ProjectWebhookState> {
  return request<ProjectWebhookState>(`/api/v1/projects/${segment(id)}/webhook/rebuild`, { method: 'POST' })
}

export async function recreateWebhook(id: number | string): Promise<ProjectWebhookState> {
  return rebuildWebhook(id)
}

export async function disableWebhook(id: number | string): Promise<ProjectWebhookState> {
  return request<ProjectWebhookState>(`/api/v1/projects/${segment(id)}/webhook`, { method: 'DELETE' })
}

export async function unregisterProject(id: number | string): Promise<void> {
  await request<void>(`/api/v1/projects/${segment(id)}`, { method: 'DELETE' })
}

export async function getWorkspace(projectId: number | string): Promise<Workspace> {
  return request<Workspace>(`/api/v1/projects/${segment(projectId)}/workspace`)
}

export interface RefreshWorkspaceInput {
  regenerate_context?: boolean
  reindex?: boolean
}

export async function refreshWorkspace(projectId: number | string, input: RefreshWorkspaceInput = {}): Promise<TriggerReviewResult> {
  return request<TriggerReviewResult>(`/api/v1/projects/${segment(projectId)}/workspace/refresh`, { method: 'POST', body: input })
}

export async function listContextDocs(projectId: number | string, kind?: string, cursor?: string): Promise<ContextDocPage> {
  return request<ContextDocPage>(withQuery(`/api/v1/projects/${segment(projectId)}/context-docs`, { kind, cursor }))
}

export async function getCurrentContextDoc(projectId: number | string, kind: string): Promise<ContextDoc> {
  return request<ContextDoc>(withQuery(`/api/v1/projects/${segment(projectId)}/context-docs/current`, { kind }))
}

export async function saveCurrentContextDoc(projectId: number | string, kind: string, content: string, locked?: boolean): Promise<ContextDoc> {
  return request<ContextDoc>(withQuery(`/api/v1/projects/${segment(projectId)}/context-docs/current`, { kind }), {
    method: 'PUT',
    body: { kind, content, locked },
  })
}

export async function setContextDocLock(projectId: number | string, kind: string, locked: boolean): Promise<void> {
  // The contract names only {locked}; the document kind is carried as the
  // query selector so agents/claude can be locked independently.
  await request<void>(withQuery(`/api/v1/projects/${segment(projectId)}/context-docs/current/lock`, { kind }), {
    method: 'POST',
    body: { locked },
  })
}

// Reviews ------------------------------------------------------------------

export interface ReviewFilters {
  project_id?: number | string
  mr_iid?: number | string
  status?: string
  verdict?: string
  publication_state?: string
  profile?: string
  trigger?: string
  since?: string
  limit?: number
}

export async function listReviews(filters: ReviewFilters = {}, cursor?: string): Promise<ReviewPage> {
  return request<ReviewPage>(withQuery('/api/v1/reviews', { ...filters, cursor }))
}

export async function getReview(id: string): Promise<ReviewDetail> {
  return request<ReviewDetail>(`/api/v1/reviews/${segment(id)}`)
}

export async function getReviewGraph(id: string): Promise<ReviewGraph> {
  const value = await request<ReviewGraph>(`/api/v1/reviews/${segment(id)}/graph`)
  return { nodes: value?.nodes || [], edges: value?.edges || [] }
}

export interface FindingFilters {
  decision?: string
  severity?: string
}

export async function listFindings(reviewId: string, filters: FindingFilters = {}, cursor?: string): Promise<FindingPage> {
  return request<FindingPage>(withQuery(`/api/v1/reviews/${segment(reviewId)}/findings`, { ...filters, cursor }))
}

export async function setFindingAnnotation(
  reviewId: string,
  findingId: string,
  annotation: FindingAnnotation,
): Promise<Finding> {
  return request<Finding>(`/api/v1/reviews/${segment(reviewId)}/findings/${segment(findingId)}/annotation`, {
    method: 'PUT',
    body: { annotation },
  })
}

export async function getReviewNode(reviewId: string, nodeId: string): Promise<ReviewNodeDetail> {
  return request<ReviewNodeDetail>(`/api/v1/reviews/${segment(reviewId)}/nodes/${segment(nodeId)}`)
}

export async function getNodeCalls(reviewId: string, nodeId: string, cursor?: string): Promise<CallPage> {
  return request<CallPage>(withQuery(`/api/v1/reviews/${segment(reviewId)}/nodes/${segment(nodeId)}/calls`, { cursor }))
}

export async function cancelReview(id: string): Promise<void> {
  await request<void>(`/api/v1/reviews/${segment(id)}/cancel`, { method: 'POST' })
}

export async function republishReview(id: string): Promise<Record<string, unknown>> {
  return request<Record<string, unknown>>(`/api/v1/reviews/${segment(id)}/republish`, { method: 'POST' })
}

export async function triggerReview(projectId: number | string, iid: number | string, input: { profile?: string; full?: boolean }): Promise<TriggerReviewResult> {
  return request<TriggerReviewResult>(
    `/api/v1/projects/${segment(projectId)}/merge_requests/${segment(iid)}/reviews`,
    { method: 'POST', body: input },
  )
}

export { streamUrl }

// Extensions ---------------------------------------------------------------

export async function listMcpServers(scope: string, projectId?: number | string, cursor?: string): Promise<McpServerPage> {
  return request<McpServerPage>(withQuery('/api/v1/mcp-servers', { scope, project_id: projectId, cursor }))
}

export interface McpInput {
  scope: 'platform' | 'project'
  project_id?: number
  name: string
  config: Record<string, unknown>
}

export async function createMcpServer(input: McpInput): Promise<McpServer> {
  return request<McpServer>('/api/v1/mcp-servers', { method: 'POST', body: input })
}

export async function updateMcpServer(id: string, input: { name?: string; config?: Record<string, unknown>; enabled?: boolean }): Promise<McpServer> {
  return request<McpServer>(`/api/v1/mcp-servers/${segment(id)}`, { method: 'PUT', body: input })
}

export async function deleteMcpServer(id: string): Promise<void> {
  await request<void>(`/api/v1/mcp-servers/${segment(id)}`, { method: 'DELETE' })
}

export async function testMcpServer(id: string): Promise<{ tools?: McpTool[]; error?: string }> {
  return request<{ tools?: McpTool[]; error?: string }>(`/api/v1/mcp-servers/${segment(id)}/test`, { method: 'POST' })
}

export async function importMcpServers(scope: string, projectId: number | undefined, config: Record<string, unknown>): Promise<unknown> {
  // The document specifies the pasted body as the import payload; scope is
  // selected via query parameters.
  return request<unknown>(withQuery('/api/v1/mcp-servers/import', { scope, project_id: projectId }), {
    method: 'POST',
    body: config,
  })
}

export async function listSkills(scope: string, projectId?: number | string, cursor?: string): Promise<SkillPage> {
  return request<SkillPage>(withQuery('/api/v1/skills', { scope, project_id: projectId, cursor }))
}

export async function registerSkillDirectory(input: { scope: 'platform' | 'project'; project_id?: number; dir: string }): Promise<Skill> {
  return request<Skill>('/api/v1/skills', { method: 'POST', body: input })
}

export async function uploadSkill(input: { scope: 'platform' | 'project'; project_id?: number; file: File }): Promise<Skill> {
  const form = new FormData()
  form.set('scope', input.scope)
  if (input.project_id) form.set('project_id', String(input.project_id))
  form.set('file', input.file)
  return request<Skill>('/api/v1/skills', { method: 'POST', body: form })
}

export async function setSkillEnabled(id: string, enabled: boolean): Promise<Skill> {
  return request<Skill>(`/api/v1/skills/${segment(id)}`, { method: 'PATCH', body: { enabled } })
}

export async function deleteSkill(id: string): Promise<void> {
  await request<void>(`/api/v1/skills/${segment(id)}`, { method: 'DELETE' })
}

export async function listBindings(scope: string, projectId?: number | string, cursor?: string): Promise<BindingPage> {
  return request<BindingPage>(withQuery('/api/v1/block-bindings', { scope, project_id: projectId, cursor }))
}

export async function saveBinding(block: string, input: { scope: 'platform' | 'project'; project_id?: number; mcp_tools: string[]; skills: string[] }): Promise<BlockBinding> {
  return request<BlockBinding>(`/api/v1/block-bindings/${segment(block)}`, { method: 'PUT', body: input })
}

// System -------------------------------------------------------------------

export async function listProviders(cursor?: string): Promise<ProviderPage> {
  return request<ProviderPage>(withQuery('/api/v1/providers', { cursor }))
}

export async function testProvider(name: string): Promise<ProviderTestResult> {
  return request<ProviderTestResult>(`/api/v1/providers/${segment(name)}/test`, { method: 'POST' })
}

function normalizeProfile(item: Profile): Profile {
  const raw = item as unknown as Record<string, unknown>
  return {
    ...item,
    name: item.name || '未命名档位',
    config:
      raw.config && typeof raw.config === 'object'
        ? (raw.config as ProfileConfig)
        : (raw as ProfileConfig),
  }
}

export async function listProfiles(cursor?: string): Promise<ProfilesPage> {
  const page = await request<ProfilesPage>(withQuery('/api/v1/profiles', { cursor }))
  return { ...page, profiles: (page.profiles || []).map(normalizeProfile) }
}

export interface StatsFilters {
  from?: string
  to?: string
  project_id?: number | string
}

export async function getStats(filters: StatsFilters = {}): Promise<Stats> {
  return request<Stats>(withQuery('/api/v1/stats', { ...filters }))
}

export async function getSettings(): Promise<PlatformSettings> {
  return request<PlatformSettings>('/api/v1/settings')
}

export async function listApiTokens(cursor?: string): Promise<ApiTokenPage> {
  return request<ApiTokenPage>(withQuery('/api/v1/api-tokens', { cursor }))
}

export async function createApiToken(input: { name: string; expires_in_days: number }, signal?: AbortSignal): Promise<CreatedApiToken> {
  return request<CreatedApiToken>('/api/v1/api-tokens', { method: 'POST', body: input, signal })
}

export async function revokeApiToken(id: string | number): Promise<void> {
  await request<void>(`/api/v1/api-tokens/${segment(id)}`, { method: 'DELETE' })
}

// Access administration ----------------------------------------------------

export async function listAdminUsers(cursor?: string): Promise<AdminUserPage> {
  return request<AdminUserPage>(withQuery('/api/v1/admin/users', { cursor }))
}

export async function setAdminUserDisabled(id: number | string, disabled: boolean): Promise<AdminUser> {
  return request<AdminUser>(`/api/v1/admin/users/${segment(id)}`, { method: 'PATCH', body: { disabled } })
}

export async function revokeAdminUserCredentials(id: number | string): Promise<void> {
  await request<void>(`/api/v1/admin/users/${segment(id)}/credentials`, { method: 'DELETE' })
}

export async function listAdminProjects(cursor?: string): Promise<AdminProjectPage> {
  return request<AdminProjectPage>(withQuery('/api/v1/admin/projects', { cursor }))
}

export async function listAdminGrants(projectId: number | string, cursor?: string): Promise<ProjectGrantPage> {
  return request<ProjectGrantPage>(withQuery(`/api/v1/admin/projects/${segment(projectId)}/grants`, { cursor }))
}

export async function setAdminGrant(projectId: number | string, userId: number | string, role: ProjectAccess): Promise<ProjectGrant> {
  return request<ProjectGrant>(`/api/v1/admin/projects/${segment(projectId)}/grants/${segment(userId)}`, {
    method: 'PUT',
    body: { role },
  })
}

export async function deleteAdminGrant(projectId: number | string, userId: number | string): Promise<void> {
  await request<void>(`/api/v1/admin/projects/${segment(projectId)}/grants/${segment(userId)}`, { method: 'DELETE' })
}

export function summaryFindings(summary?: FindingsSummary): FindingsSummary {
  return summary || {}
}
