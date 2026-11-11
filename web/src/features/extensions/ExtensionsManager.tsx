import { Fragment, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import {
  createMcpServer, deleteMcpServer, deleteSkill, importMcpServers, listBindings, listMcpServers, listSkills,
  registerSkillDir, restoreDefaultBindings, saveBinding, setSkillEnabled, testMcpDraft, testMcpServer,
  updateMcpServer, uploadSkill,
} from '../../api/api'
import { ApiError, errorMessage } from '../../api/client'
import type {
  BlockBinding, McpDraftTestInput, McpServer, McpServerWrite, McpTestResult, McpTool, Project, RestoreDefaultsChange,
  RestoreDefaultsResult, Skill,
} from '../../api/types'
import { useCanWrite } from '../../components/RoleContext'
import {
  ActionHint, Badge, Button, Checkbox, ConfirmDialog, Dialog, Disclosure, EmptyState, ErrorState, Field, FieldHint,
  LoadingState, Note, Panel, Select, Tabs, Textarea, useToast,
} from '../../components/ui'
import { usePaginated } from '../../lib/hooks'
import { formatJson } from '../../lib/format'
import ui from '../../styles/ui.module.css'
import styles from '../../pages/pages.module.css'
import ext from './extensions.module.css'

const blocks = ['explore', 'code-review', 'review', 'reasoner', 'validate', 'verify', 'synthesize', 'onboarding']
const tabs = [
  { key: 'mcp', label: 'MCP 服务器', panelId: 'tabpanel-mcp' },
  { key: 'skills', label: 'Skill', panelId: 'tabpanel-skills' },
  { key: 'bindings', label: '块绑定', panelId: 'tabpanel-bindings' },
]

const mcpNameHint = '首字符须为字母或数字，仅含字母、数字、点、下划线和连字符，最多 64 字符；不得包含连续下划线（__）。'
const mcpNamePattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/

const mcpTestFailureLabels: Record<string, string> = {
  invalid_config: '配置无效',
  connect_timeout: '连接超时',
  connect_failed: '连接失败',
  handshake_failed: '握手失败',
  list_tools_timeout: '读取工具超时',
  list_tools_failed: '读取工具失败',
  too_many_tools: '工具数量超出上限',
  cleanup_failed: '测试清理失败',
  test_busy: '测试并发已满，请稍后重试',
  internal: '测试服务不可用',
  forbidden: '没有权限执行此操作',
  network_error: '无法连接服务器',
}

type McpTestState = {
  pending?: boolean
  result?: McpTestResult
  error?: string
  errorCode?: string
  serverName?: string
}
type McpTestResults = Record<string, McpTestState>

function normalizeTestResult(result: McpTestResult | null | undefined): McpTestResult {
  return {
    ok: result?.ok,
    latency_ms: typeof result?.latency_ms === 'number' ? result.latency_ms : undefined,
    tool_count: typeof result?.tool_count === 'number' ? result.tool_count : undefined,
    truncated: result?.truncated === true,
    tools: Array.isArray(result?.tools) ? result.tools : [],
  }
}

function testFailureText(state?: McpTestState): string {
  if (!state) return '测试失败'
  if (state.errorCode && mcpTestFailureLabels[state.errorCode]) return mcpTestFailureLabels[state.errorCode]
  return state.error || '测试失败'
}

function failedTestState(error: unknown, serverName?: string): McpTestState {
  return {
    error: errorMessage(error),
    errorCode: error instanceof ApiError ? error.code : undefined,
    serverName,
  }
}

function succeededTestState(result: McpTestResult | null | undefined, serverName?: string): McpTestState {
  if (result?.error || result?.ok === false) {
    return { error: result?.error || '测试未成功', serverName }
  }
  return { result: normalizeTestResult(result), serverName }
}

// Validation details identify the request field; rendering them beside the
// control avoids hiding the actionable part in a generic banner.
function errorFieldMessages(error: unknown): Record<string, string> {
  if (!(error instanceof ApiError)) return {}
  const fields: Record<string, string> = {}
  for (const detail of error.details || []) {
    if (!detail.field) continue
    const message = detail.message || '输入无效'
    fields[detail.field] = fields[detail.field] ? `${fields[detail.field]}；${message}` : message
  }
  return fields
}

function hasFieldDetails(error: unknown): boolean {
  return error instanceof ApiError && Boolean(error.details?.some((detail) => detail.field))
}

const mcpConfigFieldNames = ['config', 'command', 'args', 'env', 'url', 'headers'] as const

// Map the request's validation details onto the form. Unknown fields stay in
// `unmapped` so the caller shows a banner instead of silently dropping them.
function mappedMcpFieldErrors(fields: Record<string, string>): { name: string; config: string; unmapped: string[] } {
  const known = new Set<string>(['name', ...mcpConfigFieldNames])
  return {
    name: fields.name || '',
    config: mcpConfigFieldNames.map((field) => fields[field]).filter(Boolean).join('；'),
    unmapped: Object.keys(fields).filter((field) => !known.has(field)),
  }
}

// A lost, timed-out or 5xx response does not prove the write was rejected.
function uncertainWriteError(error: unknown): unknown {
  const uncertain = !(error instanceof ApiError) || error.status === 0 || error.status === 408 || error.status >= 500
  if (!uncertain) return error
  return new Error(`${errorMessage(error)} 请求结果可能不确定，请先刷新列表确认后再重试。`)
}

function formatFileSize(bytes: number): string {
  if (!Number.isFinite(bytes)) return '—'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`
}

type ImportEntry = { name: string; config: Record<string, unknown> }

// The pasted document is the Claude Code mcpServers object. Parsing never
// stores or renders the configuration values themselves.
function parseImportDocument(text: string): { entries: ImportEntry[]; error?: string } {
  if (!text.trim()) return { entries: [] }
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    return { entries: [], error: 'JSON 格式无效。' }
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { entries: [], error: '配置必须是 JSON 对象。' }
  }
  const servers = (parsed as Record<string, unknown>).mcpServers
  if (!servers || typeof servers !== 'object' || Array.isArray(servers)) {
    return { entries: [], error: '缺少 mcpServers 对象。' }
  }
  const entries: ImportEntry[] = []
  for (const [name, config] of Object.entries(servers as Record<string, unknown>)) {
    if (!config || typeof config !== 'object' || Array.isArray(config)) {
      return { entries: [], error: `mcpServers.${name} 必须是 JSON 对象。` }
    }
    entries.push({ name, config: config as Record<string, unknown> })
  }
  entries.sort((left, right) => left.name.localeCompare(right.name))
  if (!entries.length) return { entries: [], error: 'mcpServers 至少需要一个服务器。' }
  return { entries }
}

const mcpCommand = (server: McpServer): string => {
  const config = server.config || {}
  if (typeof config.command === 'string') {
    const args = Array.isArray(config.args) ? config.args.map(String) : []
    return [config.command, ...args].join(' ')
  }
  if (typeof config.url === 'string') return config.url
  return '配置已隐藏'
}

function parseJsonObject(text: string, label: string): Record<string, unknown> {
  const parsed = JSON.parse(text) as unknown
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error(`${label}必须是 JSON 对象。`)
  }
  return parsed as Record<string, unknown>
}

function useScopeQuery(enabled: boolean, scope: string, projectId?: number) {
  const queryKey = ['mcp-servers', scope, projectId]
  return usePaginated<McpServer>(
    queryKey,
    (cursor) => listMcpServers(scope, projectId, cursor),
    'servers',
    { enabled },
  )
}

type ExtensionsManagerProps = {
  scope: 'platform' | 'project'
  projectId?: number
  tab: string
  onTabChange: (tab: string) => void
  onBindingsDirtyChange?: (dirty: boolean) => void
  projects?: Project[]
  canManage?: boolean
  // Shown in the tab panel while project scope has no project selected.
  placeholder?: ReactNode
  // Shown at the top of the tab panel in every state, e.g. a failed project refresh.
  notice?: ReactNode
}

export function ExtensionsManager(props: ExtensionsManagerProps) {
  return <ScopedExtensionsManager key={`${props.scope}-${props.projectId ?? ''}`} {...props} />
}

function ScopedExtensionsManager({
  scope, projectId, tab, onTabChange, onBindingsDirtyChange, projects = [], canManage = false, placeholder, notice,
}: ExtensionsManagerProps) {
  const platformAdmin = useCanWrite()
  // Every extension write changes host execution security, so the server
  // requires the platform admin role. Project scope additionally requires the
  // project manage grant and fails closed when it is missing.
  const canWrite = platformAdmin && (scope === 'platform' || (Boolean(projectId) && canManage))
  const toast = useToast()
  const queryClient = useQueryClient()
  // Matrix-eligible results come from enabled servers; results for disabled
  // servers are display-only and must never grant tools to a block.
  const [testResults, setTestResults] = useState<McpTestResults>({})
  const [readonlyTestResults, setReadonlyTestResults] = useState<McpTestResults>({})
  const displayTestResults = useMemo(
    () => ({ ...readonlyTestResults, ...testResults }),
    [readonlyTestResults, testResults],
  )
  const [addServerOpen, setAddServerOpen] = useState(false)
  const [importServersOpen, setImportServersOpen] = useState(false)
  const [skillUploadFile, setSkillUploadFile] = useState<File | null>(null)
  const [registerSkillOpen, setRegisterSkillOpen] = useState(false)
  const [editServer, setEditServer] = useState<McpServer | null>(null)
  const [deleteServer, setDeleteServer] = useState<McpServer | null>(null)
  const [deletePending, setDeletePending] = useState(false)
  const [deleteError, setDeleteError] = useState<unknown>(null)
  const [deleteSkillTarget, setDeleteSkillTarget] = useState<Skill | null>(null)
  const [skillDeletePending, setSkillDeletePending] = useState(false)
  const [skillDeleteError, setSkillDeleteError] = useState<unknown>(null)
  const [bindingsDirty, setBindingsDirty] = useState(false)
  const [bindingsBusy, setBindingsBusy] = useState(false)
  // The rendered tab can temporarily differ from the routed tab while a
  // guarded change is pending: keeping the bindings panel mounted preserves
  // its drafts even when the URL (browser back/forward, a link) already moved.
  const [shownTab, setShownTab] = useState(tab)
  const [pendingTab, setPendingTab] = useState<string | null>(null)

  useEffect(() => { onBindingsDirtyChange?.(bindingsDirty) }, [bindingsDirty, onBindingsDirtyChange])

  useEffect(() => {
    if (tab === shownTab) return
    if (shownTab === 'bindings' && bindingsDirty) {
      // Push the routed tab back so the URL describes the panel on screen while
      // keeping the requested entry in history, then ask before discarding.
      setPendingTab(tab)
      onTabChange(shownTab)
      return
    }
    setShownTab(tab)
  }, [tab, shownTab, bindingsDirty, onTabChange])

  const changeTab = (next: string) => {
    if (next === shownTab) return
    if (bindingsBusy) {
      toast.push('info', '正在保存绑定，请稍后切换。')
      return
    }
    if (shownTab === 'bindings' && bindingsDirty) {
      setPendingTab(next)
      return
    }
    setShownTab(next)
    onTabChange(next)
  }
  const discardBindingsAndChangeTab = () => {
    const next = pendingTab
    setPendingTab(null)
    setBindingsDirty(false)
    if (!next) return
    setShownTab(next)
    onTabChange(next)
  }

  const projectEnabled = scope === 'platform' || Boolean(projectId)
  const servers = useScopeQuery(projectEnabled, scope, projectId)
  const skills = usePaginated<Skill>(
    ['skills', scope, projectId],
    (cursor) => listSkills(scope, projectId, cursor),
    'skills',
    { enabled: projectEnabled },
  )
  const bindings = usePaginated<BlockBinding>(
    ['block-bindings', scope, projectId],
    (cursor) => listBindings(scope, projectId, cursor),
    'bindings',
    { enabled: projectEnabled },
  )

  const projectName = useMemo(() => {
    const found = projects.find((project) => project.id === projectId)
    return found?.path_with_namespace || (projectId ? `项目 #${projectId}` : '')
  }, [projects, projectId])

  useEffect(() => {
    setTestResults({})
    setReadonlyTestResults({})
  }, [scope, projectId])

  const invalidateServers = () => queryClient.invalidateQueries({ queryKey: ['mcp-servers', scope, projectId] })
  const invalidateSkills = () => queryClient.invalidateQueries({ queryKey: ['skills', scope, projectId] })
  // Restore must not treat an invalidation promise as a completed read: a
  // paused offline query or an inactive query resolves without fetching. Cancel
  // the old in-flight read, mark the cache stale, then fetch every loaded page
  // explicitly even without observers.
  const refreshBindings = async (): Promise<{ items: BlockBinding[]; complete: boolean }> => {
    const queryKey = ['block-bindings', scope, projectId] as const
    await queryClient.cancelQueries({ queryKey })
    await queryClient.invalidateQueries({ queryKey, refetchType: 'none' })
    // Read from the first page until the server stops returning a cursor. A
    // fixed page count can leave new pages unread and let a save overwrite
    // grants that were never displayed. The cap only stops a malformed,
    // looping cursor; reaching it fails closed below.
    const data = await queryClient.fetchInfiniteQuery({
      queryKey,
      queryFn: ({ pageParam }) => listBindings(scope, projectId, pageParam),
      initialPageParam: undefined as string | undefined,
      getNextPageParam: (lastPage) => lastPage?.next_cursor || undefined,
      pages: 1000,
      staleTime: 0,
      retry: false,
    })
    const items: BlockBinding[] = []
    for (const page of data.pages) {
      if (!Array.isArray(page?.bindings)) throw new Error('绑定分页响应格式无效。')
      items.push(...page.bindings)
    }
    const lastPage = data.pages[data.pages.length - 1]
    const complete = Boolean(lastPage) && !lastPage.next_cursor
    if (!complete) throw new Error('绑定列表未全部加载。')
    return { items, complete }
  }
  const invalidateBindings = () => queryClient.invalidateQueries({ queryKey: ['block-bindings', scope, projectId] })

  const runTest = async (server: McpServer) => {
    // Disabled servers can still be probed, but their tools are display-only;
    // keeping them out of testResults prevents the binding matrix from
    // offering tools the runtime will not expose.
    const updateResults = server.enabled === false ? setReadonlyTestResults : setTestResults
    updateResults((current) => ({ ...current, [server.id]: { pending: true, serverName: server.name } }))
    try {
      const result = await testMcpServer(server.id)
      updateResults((current) => ({ ...current, [server.id]: succeededTestState(result, server.name) }))
      if (server.enabled !== false && !result?.error && result?.ok !== false) {
        void invalidateServers()
        void invalidateBindings()
      }
    } catch (error) {
      updateResults((current) => ({ ...current, [server.id]: failedTestState(error, server.name) }))
    }
  }

  // Draft tests run before any row exists, so the discovered tools are kept
  // under the created/deployed server ID and name for the binding matrix.
  const handleServerCreated = (server: McpServer, draft?: McpTestState) => {
    if (draft?.result && server.enabled !== false) {
      setTestResults((current) => ({ ...current, [server.id]: { ...draft, serverName: server.name } }))
    }
    setAddServerOpen(false)
    void invalidateServers()
    void invalidateBindings()
    toast.push('success', `MCP 服务器 ${server.name} 已创建。`)
  }

  const handleServersImported = (created: McpServer[], tests: Record<string, McpTestState>) => {
    setTestResults((current) => {
      const next = { ...current }
      for (const server of created) {
        // The upsert replaced the stored configuration, so a previous tool
        // list for this ID is stale. Only a successful test of the imported
        // draft may repopulate it, and only for an enabled server.
        delete next[server.id]
        const state = tests[server.name]
        if (state?.result && server.enabled !== false) next[server.id] = { ...state, serverName: server.name }
      }
      return next
    })
    setReadonlyTestResults((current) => {
      const next = { ...current }
      for (const server of created) delete next[server.id]
      return next
    })
    setImportServersOpen(false)
    void invalidateServers()
    void invalidateBindings()
    toast.push('success', `已导入 ${created.length} 个 MCP 服务器。`)
  }

  const handleSkillCreated = (created: Skill, action: 'uploaded' | 'registered') => {
    if (action === 'uploaded') setSkillUploadFile(null)
    else setRegisterSkillOpen(false)
    void invalidateSkills()
    toast.push('success', action === 'uploaded' ? `Skill ${created.name} 已上传。` : `Skill ${created.name} 已登记。`)
  }

  const removeServer = async () => {
    if (!deleteServer) return
    setDeletePending(true)
    setDeleteError(null)
    try {
      await deleteMcpServer(deleteServer.id)
      setTestResults((current) => {
        const next = { ...current }
        delete next[deleteServer.id]
        return next
      })
      setReadonlyTestResults((current) => {
        const next = { ...current }
        delete next[deleteServer.id]
        return next
      })
      setDeleteServer(null)
      toast.push('success', 'MCP 服务器已删除。')
      await invalidateServers()
      await invalidateBindings()
    } catch (error) {
      setDeleteError(error)
    } finally {
      setDeletePending(false)
    }
  }

  const removeSkill = async () => {
    if (!deleteSkillTarget) return
    setSkillDeletePending(true)
    setSkillDeleteError(null)
    try {
      await deleteSkill(deleteSkillTarget.id)
      setDeleteSkillTarget(null)
      toast.push('success', 'Skill 已删除。')
      await invalidateSkills()
      await invalidateBindings()
    } catch (error) {
      setSkillDeleteError(error)
    } finally {
      setSkillDeletePending(false)
    }
  }

  const toggleSkill = async (target: Skill) => {
    try {
      await setSkillEnabled(target.id, !target.enabled)
      toast.push('success', target.enabled ? 'Skill 已禁用，重启服务后生效。' : 'Skill 已启用，重启服务后生效。')
      await invalidateSkills()
    } catch (error) {
      toast.push('error', errorMessage(error))
    }
  }

  // Counts are shown only when a list is fully loaded; a paged list has no total.
  const loadedCount = (list: { isSuccess: boolean; hasNextPage?: boolean; items: unknown[] }) => (
    projectEnabled && list.isSuccess && !list.hasNextPage ? list.items.length : undefined
  )
  const tabItems = tabs.map((item) => ({
    ...item,
    count: item.key === 'mcp' ? loadedCount(servers) : item.key === 'skills' ? loadedCount(skills) : undefined,
  }))

  return (
    <div className={ui.pageStack}>
      <Tabs items={tabItems} value={shownTab} onChange={changeTab} label="扩展类型" idPrefix="extension-tab" />
      <div role="tabpanel" id={`tabpanel-${shownTab}`} aria-labelledby={`extension-tab-${shownTab}`} className={ui.pageStack}>
      {notice}
      {!projectEnabled ? (
        placeholder ?? <EmptyState title="请选择项目" description="项目级扩展需要指定项目后才能读取。" />
      ) : null}

      {projectEnabled && shownTab === 'mcp' ? (
        <McpTab
          canWrite={canWrite}
          scope={scope}
          projectName={projectName}
          servers={servers}
          testResults={displayTestResults}
          onTest={runTest}
          onEdit={setEditServer}
          onDelete={setDeleteServer}
          onAdd={() => setAddServerOpen(true)}
          onImport={() => setImportServersOpen(true)}
        />
      ) : null}

      {projectEnabled && shownTab === 'skills' ? (
        <SkillsTab
          canWrite={canWrite}
          skills={skills}
          bindings={bindings.items}
          onToggle={toggleSkill}
          onDelete={setDeleteSkillTarget}
          onUpload={setSkillUploadFile}
          onRegisterDir={() => setRegisterSkillOpen(true)}
        />
      ) : null}

      {projectEnabled && shownTab === 'bindings' ? (
        // Drafts belong to one scope: a cached project switch must remount
        // instead of merging project A's draft into project B's baseline.
        <BindingsTab
          key={`${scope}-${projectId ?? ''}`}
          canWrite={canWrite}
          scope={scope}
          projectId={projectId}
          bindingState={bindings}
          servers={servers.items.filter((server) => server.enabled !== false)}
          skills={skills.items}
          testResults={testResults}
          refreshBindings={refreshBindings}
          onDirtyChange={setBindingsDirty}
          onBusyChange={setBindingsBusy}
        />
      ) : null}
      </div>

      {canWrite && editServer ? (
        <EditMcpDialog
          server={editServer}
          onClose={() => setEditServer(null)}
          onSaved={() => {
            // A rename or config replacement invalidates the session's tool
            // list for this server; the next test repopulates it.
            setTestResults((current) => {
              const next = { ...current }
              delete next[editServer.id]
              return next
            })
            setReadonlyTestResults((current) => {
              const next = { ...current }
              delete next[editServer.id]
              return next
            })
            void invalidateServers()
          }}
        />
      ) : null}
      {canWrite && addServerOpen ? (
        <AddMcpDialog
          scope={scope}
          projectId={projectId}
          projectName={projectName}
          onClose={() => setAddServerOpen(false)}
          onCreated={handleServerCreated}
        />
      ) : null}
      {canWrite && importServersOpen ? (
        <ImportMcpDialog
          scope={scope}
          projectId={projectId}
          projectName={projectName}
          onClose={() => setImportServersOpen(false)}
          onImported={handleServersImported}
        />
      ) : null}
      {canWrite && skillUploadFile ? (
        <UploadSkillDialog
          file={skillUploadFile}
          scope={scope}
          projectId={projectId}
          projectName={projectName}
          onClose={() => setSkillUploadFile(null)}
          onUploaded={(created) => handleSkillCreated(created, 'uploaded')}
        />
      ) : null}
      {canWrite && registerSkillOpen ? (
        <RegisterSkillDirDialog
          scope={scope}
          projectId={projectId}
          projectName={projectName}
          onClose={() => setRegisterSkillOpen(false)}
          onRegistered={(created) => handleSkillCreated(created, 'registered')}
        />
      ) : null}

      <ConfirmDialog
        open={Boolean(deleteServer)}
        onClose={() => setDeleteServer(null)}
        onConfirm={() => { void removeServer() }}
        title="删除 MCP 服务器？"
        description={`将删除 ${deleteServer?.name || '该服务器'}。现有块绑定可能仍引用其工具，需要随后调整。`}
        confirmLabel="删除"
        pending={deletePending}
        error={deleteError}
      />
      <ConfirmDialog
        open={Boolean(deleteSkillTarget)}
        onClose={() => setDeleteSkillTarget(null)}
        onConfirm={() => { void removeSkill() }}
        title="删除 Skill？"
        description={`将删除 ${deleteSkillTarget?.name || '该 Skill'}。现有块绑定可能仍引用它。`}
        confirmLabel="删除"
        pending={skillDeletePending}
        error={skillDeleteError}
      />
      <ConfirmDialog
        open={Boolean(pendingTab)}
        onClose={() => setPendingTab(null)}
        onConfirm={discardBindingsAndChangeTab}
        title="放弃未保存的绑定修改？"
        description="切换标签会丢弃块绑定矩阵中尚未保存的修改。"
        confirmLabel="放弃并切换"
      />
    </div>
  )
}

function McpTab({
  canWrite, scope, projectName, servers, testResults, onTest, onEdit, onDelete, onAdd, onImport,
}: {
  canWrite: boolean
  scope: string
  projectName: string
  servers: ReturnType<typeof useScopeQuery>
  testResults: McpTestResults
  onTest: (server: McpServer) => void
  onEdit: (server: McpServer) => void
  onDelete: (server: McpServer) => void
  onAdd: () => void
  onImport: () => void
}) {
  return (
    <Panel
      title="MCP 服务器"
      aside={scope === 'platform' ? '平台级' : `项目级 · ${projectName}`}
    >
      {canWrite ? (
        <>
          <div className={ext.panelActions}>
            <Button variant="primary" onClick={onAdd}>新增 MCP 服务器</Button>
            <Button variant="secondary" onClick={onImport}>粘贴 Claude Code 配置导入</Button>
          </div>
          <ActionHint>新增或导入后可先测试连接；测试不写入服务器。</ActionHint>
        </>
      ) : null}
      {servers.isPending ? <LoadingState /> : null}
      {servers.isError ? <ErrorState error={servers.error} onRetry={servers.retry} /> : null}
      {servers.isSuccess && servers.items.length === 0 ? (
        <EmptyState
          title="还没有 MCP 服务器"
          description={canWrite
            ? '内置 MCP 由平台管理；可新增或导入自定义服务器。'
            : '内置 MCP 由平台管理。'}
        />
      ) : null}
      {servers.items.length ? (
        <div className="tableWrap">
          <table className={ext.mcpTable}>
            <thead>
              <tr>
                <th scope="col">名称</th>
                <th scope="col">作用域</th>
                <th scope="col">命令</th>
                <th scope="col">状态</th>
                <th scope="col" className="right">工具</th>
                {canWrite ? <th scope="col" className="right">操作</th> : null}
              </tr>
            </thead>
            <tbody>
              {servers.items.map((server) => {
                const result = testResults[server.id]
                const hasDetail = Boolean(server.last_error || result)
                return (
                  <Fragment key={server.id}>
                    <tr>
                      <td>
                        <div className={ext.nameCell}>
                          <span className="mono">{server.name}</span>
                          {server.builtin ? <Badge tone="info" plain>内置</Badge> : null}
                        </div>
                      </td>
                      <td className="muted">{server.scope === 'platform' ? '平台级' : `项目级 · ${projectName || `#${server.project_id}`}`}</td>
                      <td><code className={ext.command}>{mcpCommand(server)}</code></td>
                      <td>
                        <div className={ext.nameCell}>
                          <Badge tone={server.enabled ? 'ok' : 'neutral'}>{server.enabled ? '启用' : '停用'}</Badge>
                          <span className="small muted">{server.status || '状态未提供'}</span>
                        </div>
                      </td>
                      <td className="right num">
                        {result?.result && typeof result.result.tool_count === 'number'
                          ? result.result.tool_count
                          : server.tools?.length ? server.tools.length : <span className="muted">未提供</span>}
                      </td>
                      {canWrite ? (
                        <td className="right">
                          <div className={ext.rowActions}>
                            <Button variant="secondary" className={ui.smallButton} onClick={() => onTest(server)} loading={result?.pending}>测试并列出工具</Button>
                            {!server.builtin ? (
                              <>
                                <Button variant="secondary" className={ui.smallButton} onClick={() => onEdit(server)}>编辑</Button>
                                <Button variant="danger" className={ui.smallButton} onClick={() => onDelete(server)}>删除</Button>
                              </>
                            ) : null}
                          </div>
                        </td>
                      ) : null}
                    </tr>
                    {hasDetail ? (
                      <tr className={ext.detailRow}>
                        <td colSpan={canWrite ? 6 : 5}>
                          {server.last_error ? <span className={styles.resultError}>{server.last_error}</span> : null}
                          {result?.pending ? <span className={styles.testResult} role="status">正在测试连接…</span> : null}
                          {result?.error ? (
                            <div className={ext.testFailure}>
                              <span className={styles.resultError} role="status">测试失败：{testFailureText(result)}</span>
                              <Button variant="secondary" className={ui.smallButton} onClick={() => onTest(server)}>重试测试</Button>
                            </div>
                          ) : null}
                          {result?.result ? <TestResultPanel result={result.result} /> : null}
                        </td>
                      </tr>
                    ) : null}
                  </Fragment>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : null}
      {servers.hasNextPage ? <Button variant="secondary" onClick={servers.fetchNextPage} loading={servers.isFetchingNextPage}>加载更多 MCP 服务器</Button> : null}
    </Panel>
  )
}

function ToolTable({ tools }: { tools: McpTool[] }) {
  return (
    <div className="tableWrap">
      <table className={ext.toolTable}>
        <thead>
          <tr>
            <th scope="col">工具</th>
            <th scope="col">描述</th>
            <th scope="col">注解</th>
          </tr>
        </thead>
        <tbody>
          {tools.map((tool) => (
            <tr key={tool.name}>
              <td><code className="mono small">{tool.name}</code></td>
              <td className="small muted">{tool.description || '—'}</td>
              <td>
                <div className={ext.toolBadges}>
                  {tool.annotated === false ? (
                    <Badge tone="neutral" plain>未注解（按可绑定处理）</Badge>
                  ) : (
                    <>
                      {tool.annotated ? (
                        <>
                          <Badge tone={tool.read_only ? 'ok' : 'neutral'} plain>{tool.read_only ? '只读' : '非只读'}</Badge>
                          <Badge tone={tool.destructive ? 'danger' : 'neutral'} plain>{tool.destructive ? '破坏性' : '非破坏性'}</Badge>
                        </>
                      ) : null}
                      <Badge tone={tool.bindable === false ? 'warn' : 'info'} plain>{tool.bindable === false ? '不可绑定' : '可绑定'}</Badge>
                    </>
                  )}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function TestResultPanel({ result }: { result: McpTestResult }) {
  const tools = result.tools || []
  const count = typeof result.tool_count === 'number' ? result.tool_count : tools.length
  return (
    <div className={ext.testPanel}>
      <div className={ext.testSummary} role="status">
        <Badge tone="ok">连接成功</Badge>
        <span>{typeof result.latency_ms === 'number' ? `延迟 ${result.latency_ms} ms` : '延迟不可用'}</span>
        <span>{`工具 ${count} 个${result.truncated ? '（列表已截断）' : ''}`}</span>
      </div>
      {tools.length ? <ToolTable tools={tools} /> : <span className={ext.testEmpty}>测试成功，但服务端没有返回工具列表。</span>}
    </div>
  )
}

function SkillsTab({
  canWrite, skills, bindings, onToggle, onDelete, onUpload, onRegisterDir,
}: {
  canWrite: boolean
  skills: ReturnType<typeof usePaginated<Skill>>
  bindings: BlockBinding[]
  onToggle: (skill: Skill) => void
  onDelete: (skill: Skill) => void
  onUpload: (file: File) => void
  onRegisterDir: () => void
}) {
  const fileInput = useRef<HTMLInputElement | null>(null)
  const usage = useMemo(() => {
    const map = new Map<string, string[]>()
    for (const binding of bindings) {
      for (const skill of binding.skills || []) {
        const list = map.get(skill) || []
        list.push(binding.block)
        map.set(skill, list)
      }
    }
    return map
  }, [bindings])

  return (
    <Panel title="Skill" note="目前只支持指令类 Skill，脚本不会执行。">
      {canWrite ? (
        <>
          <div className={ext.panelActions}>
            <Button variant="primary" onClick={() => fileInput.current?.click()}>上传 Skill zip</Button>
            <input
              ref={fileInput}
              type="file"
              accept=".zip,application/zip"
              className="srOnly"
              aria-label="Skill zip 文件"
              tabIndex={-1}
              onChange={(event) => {
                const file = event.target.files?.[0]
                // Reset so selecting the same file again still fires change.
                event.target.value = ''
                if (file) onUpload(file)
              }}
            />
            <Button variant="secondary" onClick={onRegisterDir}>登记服务器目录</Button>
          </div>
          <ActionHint>zip 必须包含 SKILL.md；目录登记会复制服务端目录到受管目录，不执行脚本。</ActionHint>
        </>
      ) : null}
      {skills.isPending ? <LoadingState /> : null}
      {skills.isError ? <ErrorState error={skills.error} onRetry={skills.retry} /> : null}
      {skills.isSuccess && skills.items.length === 0 ? (
        <EmptyState
          title="还没有 Skill"
          description={canWrite
            ? '内置 Skill 由平台管理；可上传 zip 或登记服务端目录。'
            : '内置 Skill 由平台管理。'}
        />
      ) : null}
      {skills.items.length ? (
        <div className={ext.skillCards}>
          {skills.items.map((skill) => (
            <article key={skill.id} className={ext.skillCard}>
              <div className={ext.skillHeader}>
                <span className={ext.skillName}>{skill.name}</span>
                <span className={ext.nameCell}>
                  {skill.builtin ? <Badge tone="info" plain>内置</Badge> : null}
                  <Badge tone="neutral" plain>{skill.scope === 'platform' ? '平台级' : `项目级${skill.project_id ? ` #${skill.project_id}` : ''}`}</Badge>
                </span>
              </div>
              <p className={ext.skillDescription}>{skill.description || '—'}</p>
              <dl className={ext.skillMeta}>
                <dt>来源目录</dt><dd>{skill.dir || '—'}</dd>
                <dt>挂载到</dt><dd>{usage.get(skill.name)?.join(', ') || '—'}</dd>
              </dl>
              <div className={ext.skillActions}>
                <Badge tone={skill.enabled ? 'ok' : 'neutral'}>{skill.enabled ? '启用' : '已禁用'}</Badge>
                {canWrite && !skill.builtin ? (
                  <span className={ext.rowActionsEnd}>
                    <Button variant="secondary" className={ui.smallButton} onClick={() => onToggle(skill)}>{skill.enabled ? '禁用' : '启用'}</Button>
                    <Button variant="secondary" className={ui.smallButton} onClick={() => onDelete(skill)}>删除</Button>
                  </span>
                ) : null}
              </div>
            </article>
          ))}
        </div>
      ) : null}
      {skills.hasNextPage ? <Button variant="secondary" onClick={skills.fetchNextPage} loading={skills.isFetchingNextPage}>加载更多 Skill</Button> : null}
    </Panel>
  )
}

type BindingDraft = { mcp_tools: string[]; skills: string[] }
type BindingDrafts = {
  baseline: Record<string, BindingDraft>
  local: Record<string, BindingDraft>
  conflicts: Record<string, boolean>
  revisions: Record<string, string>
}
type BindingSubmission = {
  block: string
  draft: BindingDraft
  baseline: BindingDraft
  baselineRevision: string | null
}

// A missing row is written with this sentinel. A row whose revision is lost or
// malformed must never be mistaken for an absent row.
const absentRevision = 'absent'
type RestoreTarget = 'all' | string
type RestorePhase = 'posting' | 'refreshing' | 'loaded' | 'failed'
type RestoreOperation = { id: string; phase: RestorePhase; error?: unknown; uncertain?: boolean; stale?: boolean }
type RestoreRegistry = { operations: Record<string, RestoreOperation> }
type RestoreView = 'idle' | 'posting' | 'refreshing' | 'loaded' | 'failed'

// Restore survives BindingsTab remounts: the operation lives in the QueryClient
// cache, keyed by scope, until the refreshed snapshot is merged by a mounted tab.
const restoreRegistryKey = (scope: 'platform' | 'project', projectId?: number) => (
  ['block-bindings-restore', scope, projectId] as const
)

let restoreSequence = 0
function nextRestoreOperationId(): string {
  restoreSequence += 1
  return `${Date.now()}-${restoreSequence}`
}

function updateRestoreOperations(
  queryClient: QueryClient,
  key: readonly unknown[],
  update: (operations: Record<string, RestoreOperation>) => Record<string, RestoreOperation>,
) {
  queryClient.setQueryData<RestoreRegistry>(key, (current) => ({
    operations: update({ ...(current?.operations || {}) }),
  }))
}

function addRestoreOperation(queryClient: QueryClient, key: readonly unknown[], id: string) {
  updateRestoreOperations(queryClient, key, (operations) => ({
    ...operations,
    [id]: { id, phase: 'posting' },
  }))
}

function setRestorePhase(
  queryClient: QueryClient,
  key: readonly unknown[],
  ids: string[],
  phase: RestorePhase,
  options: { error?: unknown; uncertain?: boolean; stale?: boolean } = {},
) {
  updateRestoreOperations(queryClient, key, (operations) => {
    for (const id of ids) {
      const previous = operations[id]
      if (!previous) continue
      const next: RestoreOperation = { id, phase }
      if (previous.uncertain || options.uncertain) next.uncertain = true
      if (previous.stale || options.stale) next.stale = true
      if (phase === 'failed') next.error = options.error
      operations[id] = next
    }
    return operations
  })
}

// A lost, timed-out, aborted, undecodable or gateway response does not prove
// the restore was rejected: the server may have committed it before the
// response was lost.
function isUncertainRestoreError(error: unknown): boolean {
  if (error instanceof ApiError) {
    return error.status === 0 || error.status === 408 || error.status >= 500
  }
  return true
}

function removeRestoreOperations(queryClient: QueryClient, key: readonly unknown[], ids: string[]) {
  updateRestoreOperations(queryClient, key, (operations) => {
    for (const id of ids) delete operations[id]
    return operations
  })
}

function bindingsFromCache(
  queryClient: QueryClient,
  scope: 'platform' | 'project',
  projectId?: number,
): BlockBinding[] {
  const data = queryClient.getQueryData<{ pages?: Array<{ bindings?: BlockBinding[] }> }>(
    ['block-bindings', scope, projectId],
  )
  const items: BlockBinding[] = []
  for (const page of data?.pages || []) {
    if (Array.isArray(page?.bindings)) items.push(...page.bindings)
  }
  return items
}

const emptyDraft: BindingDraft = { mcp_tools: [], skills: [] }
const emptyDrafts: BindingDrafts = { baseline: {}, local: {}, conflicts: {}, revisions: {} }

function sortedKey(values: string[]): string {
  return values.slice().sort().join('\u0000')
}

function sameEntries(left: string[], right: string[]): boolean {
  return left.length === right.length && sortedKey(left) === sortedKey(right)
}

function sameDraft(left: BindingDraft, right: BindingDraft): boolean {
  return sameEntries(left.mcp_tools, right.mcp_tools) && sameEntries(left.skills, right.skills)
}

function cloneDraft(draft: BindingDraft): BindingDraft {
  return { mcp_tools: [...draft.mcp_tools], skills: [...draft.skills] }
}

function validRevision(value: unknown): value is string {
  // The API treats a revision as an opaque token. Only reject values that
  // cannot identify a stored row: missing, empty, non-string, or the sentinel
  // reserved for a row that does not exist.
  return typeof value === 'string' && value.length > 0 && value !== absentRevision
}

function bindingDrafts(items: BlockBinding[]): Record<string, BindingDraft> {
  const result: Record<string, BindingDraft> = {}
  for (const item of items) {
    if (!result[item.block]) {
      result[item.block] = { mcp_tools: [...(item.mcp_tools || [])], skills: [...(item.skills || [])] }
    }
  }
  return result
}

function bindingRevisions(items: BlockBinding[]): Record<string, string> {
  const result: Record<string, string> = {}
  for (const item of items) {
    if (validRevision(item.revision) && !result[item.block]) result[item.block] = item.revision
  }
  return result
}

// Merge a server snapshot without silently overwriting local edits. A block
// whose local draft and remote value both changed keeps its draft but is marked
// as a conflict until the admin explicitly accepts the new baseline. A complete
// snapshot (all pages loaded) lets a missing block count as a remote deletion;
// a partial snapshot must not be mistaken for one.
function mergeBindingDrafts(
  current: BindingDrafts,
  items: BlockBinding[],
  complete: boolean,
): BindingDrafts {
  const server = bindingDrafts(items)
  const serverRevisions = bindingRevisions(items)
  const baseline: Record<string, BindingDraft> = {}
  const local: Record<string, BindingDraft> = {}
  const conflicts: Record<string, boolean> = {}
  const revisions: Record<string, string> = {}
  const blocks = new Set([
    ...Object.keys(server),
    ...Object.keys(current.baseline),
    ...Object.keys(current.local),
    ...Object.keys(current.conflicts),
    ...Object.keys(current.revisions),
  ])

  for (const block of blocks) {
    const hasServer = Boolean(server[block])
    const previousBaseline = current.baseline[block] || emptyDraft
    const previousLocal = current.local[block] || emptyDraft
    const wasConflict = Boolean(current.conflicts[block])

    if (!hasServer && !complete) {
      // A partial snapshot cannot prove that a block was deleted. Keep both
      // sides of every block that was already known from the loaded pages.
      if (current.baseline[block]) baseline[block] = cloneDraft(previousBaseline)
      if (current.local[block]) local[block] = cloneDraft(previousLocal)
      if (wasConflict) conflicts[block] = true
      if (current.revisions[block]) revisions[block] = current.revisions[block]
      continue
    }

    const serverDraft = hasServer ? server[block] : emptyDraft
    const sameAsLocal = sameDraft(serverDraft, previousLocal)
    const dirty = !sameDraft(previousBaseline, previousLocal)
    // Revisions hash grant order and row existence, so a revision change can
    // differ from the visible draft. Treat it as a remote change as well.
    const revisionChanged = current.revisions[block] !== (hasServer ? serverRevisions[block] : undefined)
    const remoteChanged = !sameDraft(previousBaseline, serverDraft) || revisionChanged
    const conflict = !sameAsLocal && (wasConflict || (dirty && remoteChanged))

    baseline[block] = cloneDraft(serverDraft)
    // A complete snapshot that omits the block gives it no revision: a later
    // save must claim the absent sentinel, not a stale token.
    if (hasServer && serverRevisions[block]) revisions[block] = serverRevisions[block]
    if (conflict) {
      conflicts[block] = true
      local[block] = cloneDraft(previousLocal)
    } else if (dirty && !sameAsLocal) {
      local[block] = cloneDraft(previousLocal)
    } else {
      // Adopt the snapshot when the target already matches it, so a later
      // edit keeps the server's grant order instead of the old local order.
      local[block] = cloneDraft(serverDraft)
    }
  }

  return { baseline, local, conflicts, revisions }
}
function BindingsTab({
  canWrite, scope, projectId, bindingState, servers, skills, testResults, refreshBindings, onDirtyChange, onBusyChange,
}: {
  canWrite: boolean
  scope: 'platform' | 'project'
  projectId?: number
  bindingState: ReturnType<typeof usePaginated<BlockBinding>>
  servers: McpServer[]
  skills: Skill[]
  testResults: McpTestResults
  refreshBindings: () => Promise<{ items: BlockBinding[]; complete: boolean }>
  onDirtyChange?: (dirty: boolean) => void
  onBusyChange?: (busy: boolean) => void
}) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const bindingSnapshot = JSON.stringify(bindingState.items)
  const invalidRevision = bindingState.items.some((item) => !validRevision(item.revision))
  const bindingsComplete = bindingState.isSuccess && !bindingState.hasNextPage && !bindingState.isFetchingNextPage
  const [drafts, setDrafts] = useState<BindingDrafts>(() => (
    mergeBindingDrafts(emptyDrafts, bindingState.items, bindingsComplete)
  ))
  // Tracks the snapshot that is already merged into the editable drafts.
  const [draftSnapshot, setDraftSnapshot] = useState(bindingSnapshot)
  const mergeKey = `${bindingSnapshot}|${bindingsComplete ? 'complete' : 'partial'}`
  const appliedMerge = useRef(mergeKey)
  const { baseline, local, conflicts, revisions } = drafts
  const merged = draftSnapshot === bindingSnapshot
  const bindingsReady = bindingsComplete && !bindingState.isFetching && merged && !invalidRevision
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<unknown>(null)
  const [saveRefreshState, setSaveRefreshState] = useState<'idle' | 'pending' | 'failed'>('idle')
  const [saveRefreshError, setSaveRefreshError] = useState<unknown>(null)
  const [saveRefreshConflict, setSaveRefreshConflict] = useState(false)
  const [pendingSave, setPendingSave] = useState<{
    submitted: BindingSubmission[]
    absentBlocks: string[]
  } | null>(null)
  const [restoreTarget, setRestoreTarget] = useState<RestoreTarget | null>(null)
  const [restorePreview, setRestorePreview] = useState<RestoreDefaultsResult | null>(null)
  const [previewPending, setPreviewPending] = useState(false)
  const [previewError, setPreviewError] = useState<unknown>(null)
  const [restoreError, setRestoreError] = useState<unknown>(null)
  const [restoreConflict, setRestoreConflict] = useState(false)
  const restoreRequest = useRef(0)
  const restoreKey = restoreRegistryKey(scope, projectId)
  const restoreQuery = useQuery<RestoreRegistry>({
    queryKey: restoreKey,
    queryFn: () => Promise.resolve({ operations: {} }),
    enabled: false,
    gcTime: Infinity,
  })
  const restoreOps = Object.values(restoreQuery.data?.operations || {})
  const restorePending = restoreOps.some((operation) => operation.phase === 'posting')
  const restoring = restoreOps.length > 0
  const restoreFailed = restoreOps.find((operation) => operation.phase === 'failed')
  const restoreRefreshing = restoreOps.some((operation) => operation.phase === 'refreshing')
  const restoreLoaded = restoreOps.some((operation) => operation.phase === 'loaded')
  const restoreUncertain = restoreOps.some((operation) => operation.uncertain)
  const restoreStale = restoreOps.some((operation) => operation.stale)
  const restoreView: RestoreView = restoreRefreshing
    ? 'refreshing'
    : restoreFailed
      ? 'failed'
      : restoreLoaded
        ? 'loaded'
        : restorePending
          ? 'posting'
          : 'idle'
  const restoreReadFailed = restoreView === 'failed' || restoreView === 'loaded'
  const restoreTitle = restoreReadFailed
    ? restoreStale
      ? '预览已过期，读取绑定失败'
      : restoreUncertain
        ? '恢复结果不确定，读取绑定失败'
        : '恢复已完成，但读取绑定失败'
    : restoreView === 'posting'
      ? '正在恢复默认授权'
      : restoreStale
        ? '预览已过期，正在重新读取绑定'
        : restoreUncertain
          ? '恢复结果不确定，正在重新读取绑定'
          : '恢复已完成，正在重新读取绑定'
  const restoreHint = restoreReadFailed
    ? restoreStale
      ? '预览已过期，不会重复提交恢复。重新读取成功后才能继续编辑绑定。'
      : restoreUncertain
        ? '服务器可能已提交恢复，不会重复提交。请核对实际绑定；重新读取成功后才能继续编辑绑定。'
        : '恢复请求已成功，不会重复提交。重新读取成功后才能继续编辑绑定。'
    : restoreUncertain
      ? '服务器可能已提交恢复，不会重复提交。请核对实际绑定；重新读取成功后才能继续编辑绑定。'
      : '恢复请求不会重复提交；重新读取成功后才能继续编辑绑定。'
  const restoreStatusText = restoreView === 'failed'
    ? restoreStale
      ? '预览已过期，读取绑定失败。重新读取成功后才能继续编辑绑定。'
      : restoreUncertain
        ? '恢复结果不确定，读取绑定失败。重新读取成功后才能继续编辑绑定。'
        : '恢复已完成，但读取绑定失败。重新读取成功后才能继续编辑绑定。'
    : restoreView === 'posting'
      ? '正在恢复默认授权…'
      : restoreView === 'loaded'
        ? restoreStale
          ? '正在应用最新绑定…'
          : restoreUncertain ? '正在应用服务器绑定…' : '正在应用新的绑定…'
        : restoreStale
          ? '预览已过期，正在读取最新绑定…'
          : restoreUncertain ? '恢复结果不确定，正在读取服务器绑定…' : '默认绑定已恢复，正在读取新的绑定…'

  // Merge each distinct server snapshot into the baseline while keeping dirty
  // drafts. The content key avoids merging the same page data repeatedly when
  // usePaginated rebuilds its items array.
  useLayoutEffect(() => {
    if (appliedMerge.current === mergeKey) return
    appliedMerge.current = mergeKey
    setDrafts((current) => mergeBindingDrafts(current, bindingState.items, bindingsComplete))
    setDraftSnapshot(bindingSnapshot)
  }, [mergeKey, bindingSnapshot, bindingState.items, bindingsComplete])

  // A loaded restore may outlive this component. Only release its lock after
  // the cached list is rendered into the drafts, so no stale grant is editable.
  const loadedRestoreIds = restoreOps.filter((operation) => operation.phase === 'loaded').map((operation) => operation.id)
  const cachedBindingSnapshot = JSON.stringify(bindingsFromCache(queryClient, scope, projectId))
  useLayoutEffect(() => {
    if (!loadedRestoreIds.length) return
    if (!bindingState.isSuccess || bindingState.isFetching || bindingState.isError) return
    if (draftSnapshot !== bindingSnapshot || bindingSnapshot !== cachedBindingSnapshot) return
    removeRestoreOperations(queryClient, restoreKey, loadedRestoreIds)
  }, [loadedRestoreIds, bindingState.isSuccess, bindingState.isFetching, bindingState.isError, draftSnapshot, bindingSnapshot, cachedBindingSnapshot, queryClient, restoreKey])

  const conflictBlocks = useMemo(() => (
    Object.keys(conflicts).filter((block) => conflicts[block])
  ), [conflicts])
  const hasConflicts = conflictBlocks.length > 0

  const rowNames = useMemo(() => {
    const names = [...blocks]
    for (const block of Object.keys(local)) if (!names.includes(block)) names.push(block)
    for (const block of conflictBlocks) if (!names.includes(block)) names.push(block)
    return names
  }, [local, conflictBlocks])

  const toolMeta = useMemo(() => {
    const meta = new Map<string, boolean>()
    // A tool stays bindable unless a test result explicitly marks it unsafe.
    const add = (name: string, bindable = true) => {
      const previous = meta.get(name)
      meta.set(name, previous === undefined ? bindable : previous && bindable)
    }
    for (const binding of bindingState.items) for (const tool of binding.mcp_tools || []) add(tool)
    for (const server of servers) {
      for (const tool of server.tools || []) add(`mcp__${server.name}__${tool.name}`, tool.bindable !== false)
    }
    for (const state of Object.values(testResults)) {
      if (!state.result || !state.serverName) continue
      for (const tool of state.result.tools || []) {
        add(`mcp__${state.serverName}__${tool.name}`, tool.bindable !== false)
      }
    }
    return meta
  }, [bindingState.items, servers, testResults])
  const toolColumns = useMemo(() => Array.from(toolMeta.keys()).sort(), [toolMeta])

  const skillColumns = useMemo(() => {
    const set = new Set<string>()
    for (const binding of bindingState.items) for (const skill of binding.skills || []) set.add(skill)
    for (const skill of skills) set.add(skill.name)
    return Array.from(set).sort()
  }, [bindingState.items, skills])

  const discardConflict = (block: string) => {
    setDrafts((current) => {
      if (!current.conflicts[block]) return current
      const nextConflicts = { ...current.conflicts }
      delete nextConflicts[block]
      return {
        ...current,
        conflicts: nextConflicts,
        local: { ...current.local, [block]: cloneDraft(current.baseline[block] || emptyDraft) },
      }
    })
  }

  const editable = canWrite && bindingsReady && !restoring && !saving && !hasConflicts && saveRefreshState === 'idle'

  const toggle = (block: string, group: 'mcp_tools' | 'skills', value: string) => {
    if (!editable) return
    setDrafts((current) => {
      const existing = current.local[block] || emptyDraft
      const values = new Set(existing[group])
      if (values.has(value)) values.delete(value)
      else values.add(value)
      return {
        ...current,
        local: { ...current.local, [block]: { ...existing, [group]: Array.from(values) } },
      }
    })
  }

  const canRestore = canWrite && scope === 'platform'
  const dirty = useMemo(() => rowNames.some((block) => (
    !sameDraft(baseline[block] || emptyDraft, local[block] || emptyDraft)
  )), [rowNames, baseline, local])
  useEffect(() => { onDirtyChange?.(dirty) }, [dirty, onDirtyChange])
  useEffect(() => { onBusyChange?.(saving || saveRefreshState !== 'idle') }, [saving, saveRefreshState, onBusyChange])

  const previewRestore = async (target: RestoreTarget) => {
    const requestId = ++restoreRequest.current
    setPreviewPending(true)
    setRestorePreview(null)
    setPreviewError(null)
    setRestoreError(null)
    setRestoreConflict(false)
    try {
      const result = await restoreDefaultBindings(
        target === 'all' ? { dry_run: true } : { blocks: [target], dry_run: true },
      )
      if (restoreRequest.current === requestId) setRestorePreview(result)
    } catch (error) {
      if (restoreRequest.current === requestId) setPreviewError(error)
    } finally {
      if (restoreRequest.current === requestId) setPreviewPending(false)
    }
  }

  const openRestore = (target: RestoreTarget) => {
    if (!canRestore || dirty || saving || restoring || hasConflicts) return
    setRestoreError(null)
    setRestoreTarget(target)
    void previewRestore(target)
  }

  const closeRestore = () => {
    if (restorePending) return
    restoreRequest.current += 1
    setRestoreTarget(null)
    setRestorePreview(null)
    setPreviewPending(false)
    setPreviewError(null)
    setRestoreError(null)
    setRestoreConflict(false)
  }

  const runRestoreRefresh = async (ids: string[]) => {
    if (!ids.length) return
    // Only refetch the list: never repeat the write when its outcome is
    // uncertain or the read failed. Operations live in the shared registry, so
    // an unmounted instance cannot drop this lock.
    setRestorePhase(queryClient, restoreKey, ids, 'refreshing')
    try {
      await refreshBindings()
      setRestorePhase(queryClient, restoreKey, ids, 'loaded')
    } catch (error) {
      setRestorePhase(queryClient, restoreKey, ids, 'failed', { error })
    }
  }

  const retryRestoreReads = () => {
    const ids = restoreOps
      .filter((operation) => operation.phase === 'failed' || operation.phase === 'loaded')
      .map((operation) => operation.id)
    if (ids.length) void runRestoreRefresh(ids)
  }

  const confirmRestore = async () => {
    if (!restoreTarget || !restorePreview || restoreConflict || restoring) return
    const operationId = nextRestoreOperationId()
    addRestoreOperation(queryClient, restoreKey, operationId)
    setRestoreError(null)
    let result: RestoreDefaultsResult
    try {
      const response: unknown = await restoreDefaultBindings(restoreTarget === 'all'
        ? { dry_run: false, preview_token: restorePreview.preview_token }
        : { blocks: [restoreTarget], dry_run: false, preview_token: restorePreview.preview_token })
      // A proxy can replace JSON with HTML, a 204, or null after the server has
      // already applied the restore. Validate inside the try so this follows
      // the uncertain-read path instead of showing success or hanging.
      if (!response || typeof response !== 'object'
        || typeof (response as { restart_required?: unknown }).restart_required !== 'boolean') {
        throw new Error('恢复响应格式无效。')
      }
      result = response as RestoreDefaultsResult
    } catch (error) {
      if (isUncertainRestoreError(error)) {
        // The write may have reached the server. Keep the lock, close the
        // dialog, and read the server state instead of letting the admin edit
        // the old matrix or submit the restore again.
        setRestorePhase(queryClient, restoreKey, [operationId], 'refreshing', { uncertain: true })
        restoreRequest.current += 1
        setRestoreTarget(null)
        setRestorePreview(null)
        void runRestoreRefresh([operationId])
        return
      }
      if (error instanceof ApiError && error.status === 409) {
        // The preview no longer describes the server. The write did not
        // happen, but its old snapshot must not be editable until V2 is read.
        setRestorePhase(queryClient, restoreKey, [operationId], 'refreshing', { stale: true })
        setRestoreConflict(true)
        void runRestoreRefresh([operationId])
        return
      }
      removeRestoreOperations(queryClient, restoreKey, [operationId])
      setRestoreError(error)
      return
    }
    toast.push('success', result.restart_required
      ? '默认绑定已恢复，重启服务后生效。'
      : '默认绑定已恢复。')
    restoreRequest.current += 1
    setRestoreTarget(null)
    setRestorePreview(null)
    setRestorePhase(queryClient, restoreKey, [operationId], 'refreshing')
    void runRestoreRefresh([operationId])
  }

  const refreshAfterSave = async (reason: 'saved' | 'conflict' = 'saved') => {
    setSaveRefreshState('pending')
    setSaveRefreshConflict(reason === 'conflict')
    try {
      const refreshed = await refreshBindings()
      const refreshedSnapshot = JSON.stringify(refreshed.items)
      setDrafts((current) => mergeBindingDrafts(current, refreshed.items, refreshed.complete))
      setDraftSnapshot(refreshedSnapshot)
      appliedMerge.current = `${refreshedSnapshot}|${refreshed.complete ? 'complete' : 'partial'}`
      setSaveRefreshError(null)
      setSaveRefreshState('idle')
    } catch (error) {
      setSaveRefreshError(error)
      setSaveRefreshState('failed')
    }
  }

  const performSave = async (submitted: BindingSubmission[]) => {
    setSaving(true)
    setSaveError(null)
    setSaveRefreshError(null)
    setSaveRefreshState('idle')
    try {
      const results = await Promise.allSettled(submitted.map(({ block, draft, baselineRevision }) => saveBinding(block, {
        scope,
        project_id: projectId,
        mcp_tools: draft.mcp_tools,
        skills: draft.skills,
        expected_revision: baselineRevision ?? absentRevision,
      })))
      const failed = results.filter((result): result is PromiseRejectedResult => result.status === 'rejected')
      const nonConflictFailures = failed.filter((result) => !(result.reason instanceof ApiError && result.reason.status === 409))
      const conflicted = failed.some((result) => result.reason instanceof ApiError && result.reason.status === 409)
      if (nonConflictFailures.length) {
        setSaveError(nonConflictFailures[0].reason)
        toast.push('error', `部分绑定未保存（${failed.length}/${submitted.length} 个请求失败）。`)
      } else if (!conflicted) {
        toast.push('success', '块绑定已保存。')
      }
      // Only a fulfilled write advances that block's baseline, and only when
      // both the baseline draft and its revision have not moved underneath it.
      // A server response without a usable revision stays dirty until the
      // following full read supplies one.
      setDrafts((current) => {
        let nextBaseline = current.baseline
        let nextRevisions = current.revisions
        let changed = false
        submitted.forEach(({ block, draft, baseline: submittedBaseline, baselineRevision }, index) => {
          const result = results[index]
          if (result.status !== 'fulfilled') return
          if (!validRevision(result.value?.revision)) return
          if (current.conflicts[block]) return
          if (!sameDraft(current.baseline[block] || emptyDraft, submittedBaseline)) return
          if ((current.revisions[block] || null) !== baselineRevision) return
          if (nextBaseline === current.baseline) nextBaseline = { ...nextBaseline }
          if (nextRevisions === current.revisions) nextRevisions = { ...nextRevisions }
          nextBaseline[block] = draft
          nextRevisions[block] = result.value.revision
          changed = true
        })
        return changed ? { ...current, baseline: nextBaseline, revisions: nextRevisions } : current
      })
      // A conflict is resolved only by re-reading from the first page. Never
      // retry the rejected write automatically.
      if (conflicted || results.some((result) => result.status === 'fulfilled')) {
        await refreshAfterSave(conflicted ? 'conflict' : 'saved')
      }
    } finally {
      setSaving(false)
    }
  }

  const save = () => {
    if (!editable || hasConflicts || saving) return
    const submitted = rowNames
      .filter((block) => !sameDraft(baseline[block] || emptyDraft, local[block] || emptyDraft))
      .map((block) => ({
        block,
        draft: cloneDraft(local[block] || emptyDraft),
        baseline: cloneDraft(baseline[block] || emptyDraft),
        baselineRevision: revisions[block] || null,
      }))
    if (!submitted.length) {
      toast.push('info', '没有需要保存的绑定变更。')
      return
    }
    const absentBlocks = submitted.filter(({ baselineRevision }) => !baselineRevision).map(({ block }) => block)
    if (absentBlocks.length) {
      // A missing project row can inherit the platform grant, and a missing
      // platform row can inherit configuration `blocks`. Creating a row is a
      // full override, so require explicit acknowledgement first.
      setPendingSave({ submitted, absentBlocks })
      return
    }
    void performSave(submitted)
  }

  if (bindingState.isPending) return <Panel title="块绑定"><LoadingState /></Panel>
  if (bindingState.isFetchNextPageError && saveRefreshState === 'idle' && restoreView === 'idle') {
    return (
      <Panel title="块绑定">
        <ErrorState
          error={bindingState.error}
          title="读取更多绑定失败"
          onRetry={bindingState.retry}
        />
        <p className={ext.restoreHint} role="status">
          绑定未全部加载；重新读取成功后才能编辑和保存。
        </p>
      </Panel>
    )
  }
  if (saveRefreshState === 'failed' && !bindingState.isError) {
    const conflict = saveRefreshConflict
    return (
      <Panel title="块绑定">
        <ErrorState
          error={saveRefreshError ?? bindingState.error}
          title={conflict ? '绑定版本已过期，读取最新绑定失败' : '绑定已保存，但读取最新绑定失败'}
          onRetry={() => { void refreshAfterSave(conflict ? 'conflict' : 'saved') }}
        />
        <p className={ext.restoreHint} role="status">
          {conflict
            ? '保存请求未通过版本校验，不会自动重试。重新读取成功后才能继续编辑绑定。'
            : '保存请求已成功，不会重复提交。重新读取成功后才能继续编辑绑定。'}
        </p>
      </Panel>
    )
  }
  if (bindingState.isError) {
    if (saveRefreshState !== 'idle') {
      const pending = saveRefreshState === 'pending'
      const conflict = saveRefreshConflict
      return (
        <Panel title="块绑定">
          <ErrorState
            error={saveRefreshError ?? bindingState.error}
            title={pending
              ? '正在重新读取最新绑定'
              : conflict ? '绑定版本已过期，读取最新绑定失败' : '绑定已保存，但读取最新绑定失败'}
            onRetry={pending ? undefined : () => { void refreshAfterSave(conflict ? 'conflict' : 'saved') }}
          />
          <p className={ext.restoreHint} role="status">
            {pending
              ? conflict ? '正在读取最新绑定…' : '正在读取保存后的最新绑定…'
              : conflict
                ? '保存请求未通过版本校验，不会自动重试。重新读取成功后才能继续编辑绑定。'
                : '保存请求已成功，不会重复提交。重新读取成功后才能继续编辑绑定。'}
          </p>
        </Panel>
      )
    }
    if (restoreView !== 'idle') {
      return (
        <Panel title="块绑定">
          <ErrorState
            error={restoreFailed?.error ?? bindingState.error}
            title={restoreTitle}
            onRetry={restoreReadFailed ? retryRestoreReads : undefined}
          />
          <p className={ext.restoreHint} role="status">{restoreHint}</p>
        </Panel>
      )
    }
    return <Panel title="块绑定"><ErrorState error={bindingState.error} onRetry={bindingState.retry} /></Panel>
  }
  if (invalidRevision) {
    return (
      <Panel title="块绑定">
        <ErrorState
          error={new Error('绑定版本信息缺失或格式错误，无法安全保存。')}
          title="读取绑定版本失败"
          onRetry={bindingState.retry}
        />
        <p className={ext.restoreHint} role="status">
          重新读取成功后才能编辑和保存绑定。
        </p>
      </Panel>
    )
  }

  return (
    <>
    <Panel
      title="块绑定"
      aside={scope === 'platform' ? '平台默认绑定' : '项目覆盖绑定'}
      note={scope === 'platform' ? undefined : '未覆盖的块继承平台默认。'}
    >
      {canRestore ? (
        <div className={ext.restoreBar}>
          <div className={ext.restoreText}>
            <span className={ext.restoreTitle}>内置默认授权</span>
            {dirty ? (
              <span className={ext.restoreWarning} role="status">先保存绑定再恢复默认，未保存的更改会丢失。</span>
            ) : null}
          </div>
          <Button variant="secondary" disabled={dirty || saving || restoring || hasConflicts} onClick={() => openRestore('all')}>恢复全部默认</Button>
        </div>
      ) : null}
      {canRestore ? (
        <p className={ext.restoreHint}>恢复只处理平台级绑定。全部恢复由服务端选择全部可恢复块。</p>
      ) : null}
      {hasConflicts ? (
        <div className={styles.warning}>
          <div role="alert" tabIndex={0}>
            检测到 {conflictBlocks.length} 个块同时存在本地修改和远端变化。保存和恢复默认已停用。
            选择对应操作只放弃该块的本地更改，其他块草稿不受影响。
          </div>
          <div className={ext.conflictActions}>
            {conflictBlocks.map((block) => (
              <Button
                key={block}
                variant="secondary"
                className={ui.smallButton}
                onClick={() => discardConflict(block)}
              >
                放弃 {block} 的本地更改并使用最新绑定
              </Button>
            ))}
          </div>
        </div>
      ) : null}
      {!toolColumns.length && !skillColumns.length ? (
        <div className={styles.warning}>
          没有可绑定的工具或 Skill。先添加 MCP 服务器和 Skill，并运行 MCP 测试以发现工具；已保存的绑定名称也会显示为列。
        </div>
      ) : null}
      {!bindingsReady ? (
        <div className={styles.warning} role="status">
          {bindingState.hasNextPage
            ? '绑定尚未全部加载；加载完才能编辑或保存，否则未加载页的授权可能被覆盖。'
            : '正在读取并合并完整的绑定列表，完成前不能编辑或保存。'}
        </div>
      ) : null}
      {saveRefreshState === 'pending' ? (
        <div className={ext.restoreBar}>
          <div className={ext.restoreText}>
            <span role="status">{saveRefreshConflict ? '正在读取最新绑定…' : '正在读取保存后的最新绑定…'}</span>
          </div>
        </div>
      ) : null}
      {restoreView !== 'idle' ? (
        <div className={ext.restoreBar}>
          <div className={ext.restoreText}>
            <span role="status">{restoreStatusText}</span>
            {restoreUncertain ? (
              <span>服务器可能已提交恢复；请核对实际绑定。</span>
            ) : null}
          </div>
          {restoreView === 'failed' ? (
            <Button variant="secondary" onClick={retryRestoreReads}>重新读取</Button>
          ) : null}
        </div>
      ) : (
      <>
          <div className={styles.matrixWrap}>
            <table className={styles.matrix}>
              <thead>
                <tr>
                  <th scope="col">块</th>
                  {canRestore ? <th scope="col" className="right">默认</th> : null}
                  {toolColumns.map((tool) => (
                    <th key={tool} scope="col">
                      <span>{tool}</span>
                      {toolMeta.get(tool) === false ? <span className={ext.matrixLocked}>不可绑定</span> : null}
                    </th>
                  ))}
                  {skillColumns.map((skill) => <th key={`skill-${skill}`} scope="col">skill: {skill}</th>)}
                </tr>
              </thead>
              <tbody>
                {rowNames.map((block) => {
                  const row = local[block] || emptyDraft
                  return (
                    <tr key={block}>
                      <th scope="row">
                        <div className={ext.nameCell}>
                          <span>{block}</span>
                          {conflicts[block] ? <Badge tone="warn" plain>冲突</Badge> : null}
                        </div>
                      </th>
                      {canRestore ? (
                        <td className="right">
                          {blocks.includes(block) ? (
                            <Button
                              variant="secondary"
                              className={ui.smallButton}
                              disabled={dirty || saving || restoring || hasConflicts}
                              onClick={() => openRestore(block)}
                              aria-label={`恢复 ${block} 的默认绑定`}
                            >
                              恢复
                            </Button>
                          ) : null}
                        </td>
                      ) : null}
                      {toolColumns.map((tool) => {
                        const on = row.mcp_tools.includes(tool)
                        const bindable = toolMeta.get(tool) !== false
                        return (
                          <td key={tool}>
                            <button
                              type="button"
                              className={[styles.matrixCell, on ? styles.matrixCellOn : ''].filter(Boolean).join(' ')}
                              aria-pressed={on}
                              aria-label={`${block} 使用 ${tool}`}
                              disabled={!editable || (!bindable && !on)}
                              onClick={() => toggle(block, 'mcp_tools', tool)}
                            >
                              {on ? '✓' : ''}
                            </button>
                          </td>
                        )
                      })}
                      {skillColumns.map((skill) => {
                        const on = row.skills.includes(skill)
                        return (
                          <td key={`skill-${skill}`}>
                            <button
                              type="button"
                              className={[styles.matrixCell, on ? styles.matrixCellOn : ''].filter(Boolean).join(' ')}
                              aria-pressed={on}
                              aria-label={`${block} 使用 Skill ${skill}`}
                              disabled={!editable}
                              onClick={() => toggle(block, 'skills', skill)}
                            >
                              {on ? '✓' : ''}
                            </button>
                          </td>
                        )
                      })}
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
          <span className={styles.matrixNote}>工具名用 `mcp__server__tool` 形式；MCP 测试成功后发现的工具出现在列中。</span>
      {saveError ? <ErrorState error={saveError} title="保存绑定失败" /> : null}
      {canWrite ? (
        <div className={styles.actionsRight}>
          <Button variant="primary" onClick={save} loading={saving} disabled={!editable}>保存绑定</Button>
        </div>
      ) : <span className="small muted">只读账号不能修改块绑定。</span>}
      {bindingState.hasNextPage ? <Button variant="secondary" onClick={bindingState.fetchNextPage} loading={bindingState.isFetchingNextPage}>加载更多绑定</Button> : null}
      </>
      )}
    </Panel>
    <ConfirmDialog
      open={Boolean(pendingSave)}
      onClose={() => setPendingSave(null)}
      onConfirm={() => {
        if (!pendingSave) return
        const submitted = pendingSave.submitted
        setPendingSave(null)
        void performSave(submitted)
      }}
      title="新建绑定会覆盖继承授权？"
      description={pendingSave
        ? `将新建 ${pendingSave.absentBlocks.join('、')} 的绑定。新建为完整覆盖；未选中的继承授权或配置文件授权不会保留。`
        : ''}
      confirmLabel="新建并保存"
    />
    {canRestore ? (
      <RestoreDialog
        target={restoreTarget}
        preview={restorePreview}
        previewPending={previewPending}
        previewError={previewError}
        pending={restorePending}
        error={restoreError}
        conflict={restoreConflict}
        readPending={restoreView === 'refreshing' || restoreView === 'loaded'}
        onClose={closeRestore}
        onPreview={() => { if (restoreTarget) void previewRestore(restoreTarget) }}
        onConfirm={() => { void confirmRestore() }}
      />
    ) : null}
    </>
  )
}

const restoreStatusLabels = {
  change: { label: '将更改', tone: 'warn' as const },
  unchanged: { label: '无需更改', tone: 'ok' as const },
  skipped: { label: '已跳过', tone: 'neutral' as const },
}

function skipReason(reason: string): string {
  if (reason === 'file_binding') return '该块在配置文件中显式声明，不会恢复。'
  if (reason === 'no_registered_defaults') return '没有已登记的内置默认，保留现有绑定。'
  return reason ? `跳过原因：${reason}` : '没有可恢复的内容。'
}

function RestoreEntries({ label, values }: { label: string; values: string[] }) {
  if (!values.length) return null
  return (
    <div className={ext.restoreGroup}>
      <h4 className={ext.restoreGroupTitle}>{label}</h4>
      <ul className={ext.restoreList}>
        {values.map((value) => <li key={value}><code>{value}</code></li>)}
      </ul>
    </div>
  )
}

function RestoreChangeCard({ change: item }: { change: RestoreDefaultsChange }) {
  const id = useId()
  const status = restoreStatusLabels[item.status] || { label: item.status, tone: 'neutral' as const }
  const added = item.add.skills.length + item.add.mcp_tools.length > 0
  const removed = item.remove.skills.length + item.remove.mcp_tools.length > 0
  const changed = item.status === 'change'
  const ownershipOnly = changed && item.ownership_changed && !added && !removed
  return (
    <section className={ext.restoreBlock} aria-labelledby={id}>
      <div className={ext.restoreBlockHeader}>
        <h3 className={ext.restoreBlockTitle} id={id}>{item.block}</h3>
        <Badge tone={status.tone}>{status.label}</Badge>
      </div>
      {item.status === 'skipped' ? <p className={ext.restoreReason}>{skipReason(item.reason)}</p> : null}
      {item.status === 'unchanged' ? <p className={ext.restoreReason}>当前绑定已与内置默认一致。</p> : null}
      {ownershipOnly ? (
        <div className={ext.restoreOwnership}>
          <Badge tone="info" plain>仅所有权变化</Badge>
          <span>绑定内容不变，恢复后由默认扩展管理。</span>
        </div>
      ) : null}
      {changed && item.ownership_changed && !ownershipOnly ? (
        <p className={ext.restoreOwnershipNote}>恢复后由默认扩展管理。</p>
      ) : null}
      {changed ? (
        <div className={ext.restoreGroups}>
          <RestoreEntries label="新增 Skill" values={item.add.skills} />
          <RestoreEntries label="新增 MCP 工具" values={item.add.mcp_tools} />
          <RestoreEntries label="删除 Skill" values={item.remove.skills} />
          <RestoreEntries label="删除 MCP 工具" values={item.remove.mcp_tools} />
        </div>
      ) : null}
      {changed && !added && !removed && !item.ownership_changed ? (
        <p className={ext.restoreReason}>绑定内容不变，将更新默认授权记录。</p>
      ) : null}
    </section>
  )
}

function RestoreDialog({
  target, preview, previewPending, previewError, pending, error, conflict, readPending, onClose, onPreview, onConfirm,
}: {
  target: RestoreTarget | null
  preview: RestoreDefaultsResult | null
  previewPending: boolean
  previewError: unknown
  pending: boolean
  error: unknown
  conflict: boolean
  readPending: boolean
  onClose: () => void
  onPreview: () => void
  onConfirm: () => void
}) {
  if (target === null) return null
  const all = target === 'all'
  const title = all ? '恢复全部默认绑定' : `恢复 ${target} 的默认绑定`
  return (
    <Dialog
      open
      onClose={onClose}
      title={title}
      description={all
        ? '先预览将新增和删除的平台授权。确认前不会写入；全部恢复范围由服务端决定。'
        : '先预览将新增和删除的平台授权。确认前不会写入；仅处理平台级绑定。'}
      wide
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          {!previewPending && (conflict || !preview) ? (
            <Button variant="secondary" onClick={onPreview} loading={previewPending} disabled={readPending}>重新预览</Button>
          ) : null}
          {preview && !conflict ? (
            <Button variant="primary" onClick={onConfirm} loading={pending} disabled={readPending}>确认恢复</Button>
          ) : null}
        </>
      )}
    >
      {previewPending ? <LoadingState label="正在预览默认绑定…" /> : null}
      {readPending ? (
        <p className={ext.restoreHint} role="status">正在读取最新绑定，完成后才能重新预览或确认。</p>
      ) : null}
      {conflict ? (
        <div className={styles.warning} role="alert">预览已过期，绑定已变化。请重新预览后再确认。</div>
      ) : null}
      {previewError ? <ErrorState error={previewError} title="预览失败" /> : null}
      {previewError ? <p className={ext.restoreHint}>预览失败时不会写入任何绑定。</p> : null}
      {error ? <ErrorState error={error} title="恢复失败" /> : null}
      {preview ? (
        <>
          <p className={ext.restoreHint}>
            {all ? '服务端会检查全部可恢复块。' : '仅处理该块的平台级绑定。'}
            确认前不会写入；新增和删除内容如下。
          </p>
          <div className={ext.restoreChanges}>
            {preview.changes.map((item) => <RestoreChangeCard key={item.block} change={item} />)}
          </div>
          {preview.restart_required ? <p className={ext.restoreRestart}>恢复后需要重启服务才能生效。</p> : null}
        </>
      ) : null}
    </Dialog>
  )
}

function useDialogRequest(scope: string, projectId: number | undefined, action: string) {
  const toast = useToast()
  const mounted = useRef(false)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])
  const reportFailure = (error: unknown, setError: (value: unknown) => void) => {
    const uncertain = !(error instanceof ApiError) || error.status === 0 || error.status === 408 || error.status >= 500
    const safeError = error instanceof ApiError && error.status === 403
      ? new ApiError(403, 'forbidden', '当前账号没有执行此操作的权限。')
      : new Error(uncertain
        ? `${error instanceof ApiError && error.status === 0 ? '无法连接服务器。' : '请求未能确认。'}结果可能不确定，请先检查状态再重试。`
        : '请检查输入后重试。')
    if (mounted.current) {
      setError(safeError)
    } else {
      toast.push('error', `${scope === 'project' ? `项目 #${projectId}` : '平台级'}${action}失败：${safeError.message}`)
    }
  }
  return { mounted, reportFailure }
}

function EditMcpDialog({ server, onClose, onSaved }: { server: McpServer | null; onClose: () => void; onSaved: () => void }) {
  const { mounted, reportFailure } = useDialogRequest(server?.scope || 'platform', server?.project_id, '编辑 MCP 服务器')
  const nameId = useId()
  const [name, setName] = useState('')
  const [enabled, setEnabled] = useState(true)
  const [replaceConfig, setReplaceConfig] = useState(false)
  const [configText, setConfigText] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [fieldError, setFieldError] = useState('')

  useEffect(() => {
    if (server) {
      setName(server.name)
      setEnabled(server.enabled !== false)
      setReplaceConfig(false)
      setConfigText('')
      setError(null)
      setFieldError('')
      setPending(false)
    }
  }, [server])

  const submit = async () => {
    if (!server) return
    if (!name.trim()) { setFieldError('请输入名称。'); return }
    let config: Record<string, unknown> | undefined
    if (replaceConfig) {
      try {
        config = parseJsonObject(configText, '替换配置')
      } catch {
        setFieldError('替换配置 JSON 无效或不是对象。')
        return
      }
    }
    setFieldError('')
    setPending(true)
    setError(null)
    try {
      // When config is not explicitly replaced, omit it so redacted GET values
      // are never submitted back as if they were secrets.
      await updateMcpServer(server.id, { name: name.trim(), enabled, ...(config ? { config } : {}) })
      onSaved()
      if (mounted.current) onClose()
    } catch (caught) {
      reportFailure(caught, setError)
    } finally {
      if (mounted.current) setPending(false)
    }
  }

  return (
    <Dialog
      open={Boolean(server)}
      onClose={onClose}
      title="编辑 MCP 服务器"
      description="默认只改名称和启用状态；为保护密钥，服务端不返回配置内容。"
      wide
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>保存</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} title="编辑 MCP 服务器失败" /> : null}
      <div className="formGrid">
        <Field label="名称" htmlFor={nameId} error={fieldError}>
          <input id={nameId} className="input" value={name} onChange={(event) => setName(event.target.value)} aria-invalid={Boolean(fieldError)} disabled={pending} />
        </Field>
        <Field label="启用" htmlFor="mcp-edit-enabled">
          <Select
            id="mcp-edit-enabled"
            value={enabled ? 'true' : 'false'}
            options={[{ value: 'true', label: '启用' }, { value: 'false', label: '停用' }]}
            onChange={(value) => setEnabled(value === 'true')}
            disabled={pending}
          />
        </Field>
      </div>
      {server ? (
        <Disclosure summary="查看服务端返回的配置（已隐藏）">
          <pre className={`pre ${ui.jsonBlock}`}>{formatJson(server.config)}</pre>
        </Disclosure>
      ) : null}
      <Checkbox
        id="mcp-replace-config-enabled"
        label="替换完整配置"
        checked={replaceConfig}
        disabled={pending}
        onChange={(event) => {
          setReplaceConfig(event.target.checked)
          setConfigText('')
          setFieldError('')
        }}
      />
      {replaceConfig ? (
        <>
          <Field label="新的配置 JSON" htmlFor="mcp-replace-config" hint="从空白开始填写完整配置。">
            <Textarea id="mcp-replace-config" code rows={9} value={configText} onChange={(event) => setConfigText(event.target.value)} spellCheck={false} disabled={pending} />
          </Field>
          <div className={styles.warning}>
            替换会覆盖已有配置。脱敏的 env/header 值不会保留；如配置需要密钥，请重新填写完整值，且不要粘贴脱敏占位符。
          </div>
        </>
      ) : null}
    </Dialog>
  )
}

function useMountedRef() {
  const mounted = useRef(false)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])
  return mounted
}

type DialogScope = 'platform' | 'project'

function scopeLabel(scope: DialogScope, projectName?: string): string {
  return scope === 'platform' ? '平台级' : `项目级${projectName ? ` · ${projectName}` : ''}`
}

type McpDraftForm = McpServerWrite

function AddMcpDialog({
  scope, projectId, projectName, onClose, onCreated,
}: {
  scope: DialogScope
  projectId?: number
  projectName: string
  onClose: () => void
  onCreated: (server: McpServer, draft?: McpTestState) => void
}) {
  const mounted = useMountedRef()
  const nameId = useId()
  const transportId = useId()
  const commandId = useId()
  const argsId = useId()
  const envId = useId()
  const urlId = useId()
  const headersId = useId()
  const enabledHintId = useId()
  const [transport, setTransport] = useState<'stdio' | 'http'>('stdio')
  const [name, setName] = useState('')
  const [command, setCommand] = useState('')
  const [argsText, setArgsText] = useState('')
  const [envText, setEnvText] = useState('')
  const [url, setUrl] = useState('')
  const [headersText, setHeadersText] = useState('')
  const [enabled, setEnabled] = useState(true)
  const [pasteText, setPasteText] = useState('')
  const [pasteError, setPasteError] = useState('')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [testState, setTestState] = useState<McpTestState | null>(null)
  const [testedDraft, setTestedDraft] = useState<McpDraftForm | null>(null)
  const [testing, setTesting] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const testGeneration = useRef(0)

  // Any draft edit invalidates the previous probe: the displayed tools and
  // the status no longer describe what would be saved. Bumping the generation
  // also makes an in-flight response land harmlessly.
  const invalidateTest = () => {
    testGeneration.current += 1
    setTesting(false)
    setTestState(null)
    setTestedDraft(null)
    setFieldErrors({})
  }

  const buildDraft = (): { draft?: McpDraftForm; errors: Record<string, string> } => {
    const errors: Record<string, string> = {}
    const trimmedName = name.trim()
    if (!trimmedName) errors.name = '请输入名称。'
    else if (!mcpNamePattern.test(trimmedName)) errors.name = `名称无效：${mcpNameHint}`
    else if (trimmedName.includes('__')) errors.name = '名称不得包含连续下划线（__）。'
    const config: Record<string, unknown> = {}
    if (transport === 'stdio') {
      if (!command.trim()) errors.command = '请输入启动命令。'
      else config.command = command.trim()
      const args = argsText.split('\n').map((line) => line.trim()).filter(Boolean)
      if (args.length) config.args = args
      if (envText.trim()) {
        try {
          config.env = parseJsonObject(envText, '环境变量')
        } catch {
          errors.env = '环境变量 JSON 无效或不是对象。'
        }
      }
    } else {
      if (!url.trim()) errors.url = '请输入服务地址。'
      else config.url = url.trim()
      if (headersText.trim()) {
        try {
          config.headers = parseJsonObject(headersText, '请求头')
        } catch {
          errors.headers = '请求头 JSON 无效或不是对象。'
        }
      }
    }
    if (Object.keys(errors).length) return { errors }
    return {
      draft: {
        scope,
        ...(scope === 'project' && projectId ? { project_id: projectId } : {}),
        name: trimmedName,
        config,
        enabled,
      },
      errors,
    }
  }

  const fillFromPaste = () => {
    let value: Record<string, unknown>
    try {
      value = parseJsonObject(pasteText, '服务器配置')
    } catch {
      setPasteError('JSON 无效或不是对象。')
      return
    }
    const pastedName = typeof value.name === 'string' ? value.name : ''
    if (typeof value.command === 'string') {
      setTransport('stdio')
      setCommand(value.command)
      setArgsText(Array.isArray(value.args) ? value.args.map(String).join('\n') : '')
      setEnvText(value.env && typeof value.env === 'object' && !Array.isArray(value.env)
        ? JSON.stringify(value.env, null, 2)
        : '')
      setUrl('')
      setHeadersText('')
    } else if (typeof value.url === 'string') {
      setTransport('http')
      setUrl(value.url)
      setHeadersText(value.headers && typeof value.headers === 'object' && !Array.isArray(value.headers)
        ? JSON.stringify(value.headers, null, 2)
        : '')
      setCommand('')
      setArgsText('')
      setEnvText('')
    } else {
      setPasteError('配置必须包含 command 或 url。')
      return
    }
    if (pastedName) setName(pastedName)
    setPasteError('')
    invalidateTest()
  }

  const runTest = async () => {
    const { draft, errors } = buildDraft()
    if (!draft) {
      setFieldErrors(errors)
      return
    }
    setFieldErrors({})
    setError(null)
    setTesting(true)
    const input: McpDraftTestInput = draft
    const generation = testGeneration.current + 1
    testGeneration.current = generation
    setTestedDraft(draft)
    setTestState({ pending: true, serverName: draft.name })
    try {
      const result = await testMcpDraft(input)
      if (!mounted.current || testGeneration.current !== generation) return
      setTestState(succeededTestState(result, draft.name))
    } catch (caught) {
      if (!mounted.current || testGeneration.current !== generation) return
      const fields = errorFieldMessages(caught)
      if (Object.keys(fields).length) {
        const mapped = mappedMcpFieldErrors(fields)
        setFieldErrors({ name: mapped.name, config: mapped.config })
        setTestState({
          error: mapped.name || mapped.config ? '配置校验未通过，请检查标记的字段。' : errorMessage(caught),
          serverName: draft.name,
        })
      } else {
        setTestState(failedTestState(caught, draft.name))
      }
    } finally {
      if (mounted.current && testGeneration.current === generation) setTesting(false)
    }
  }

  const submit = async () => {
    const { draft, errors } = buildDraft()
    if (!draft) {
      setFieldErrors(errors)
      return
    }
    setFieldErrors({})
    setError(null)
    setPending(true)
    try {
      const created = await createMcpServer(draft)
      // Only a test of the exact draft being saved may contribute its tools;
      // editing the form after a test makes the old tool list inapplicable.
      const unchanged = testedDraft !== null && JSON.stringify(testedDraft) === JSON.stringify(draft)
      if (mounted.current) onCreated(created, testState?.result && unchanged ? testState : undefined)
    } catch (caught) {
      if (!mounted.current) return
      const fields = errorFieldMessages(caught)
      const mapped = mappedMcpFieldErrors(fields)
      setFieldErrors({ name: mapped.name, config: mapped.config })
      if (caught instanceof ApiError && caught.status === 409) {
        setError(new Error('同名 MCP 服务器已存在，请更换名称。'))
      } else if (mapped.unmapped.length || (hasFieldDetails(caught) && !mapped.name && !mapped.config)) {
        // A detail on an unmapped field (scope, project_id, body, ...) must
        // stay visible; dropping it would leave the form looking valid.
        setError(caught)
      } else if (hasFieldDetails(caught)) {
        setError(null)
      } else {
        setError(uncertainWriteError(caught))
      }
    } finally {
      if (mounted.current) setPending(false)
    }
  }

  const testLabel = testState?.result ? '重新测试' : testState?.error ? '重试测试' : '测试连接'

  return (
    <Dialog
      open
      onClose={pending ? () => {} : onClose}
      title="新增 MCP 服务器"
      description={`${scopeLabel(scope, projectName)}。配置兼容 Claude Code mcpServers，保存前可测试连接。`}
      wide
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="secondary" onClick={() => { void runTest() }} loading={testing} disabled={pending}>{testLabel}</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending} disabled={testing}>保存</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} title="创建 MCP 服务器失败" /> : null}
      <div className="formGrid">
        <Field label="名称" htmlFor={nameId} hint={mcpNameHint} error={fieldErrors.name}>
          <input
            id={nameId}
            className="input"
            value={name}
            onChange={(event) => { setName(event.target.value); invalidateTest() }}
            aria-invalid={Boolean(fieldErrors.name)}
            disabled={pending}
          />
        </Field>
        <Field label="传输方式" htmlFor={transportId} hint="stdio 启动本地命令；http 连接远程 MCP 端点。">
          <Select
            id={transportId}
            value={transport}
            options={[{ value: 'stdio', label: 'stdio（本地命令）' }, { value: 'http', label: 'http（远程 URL）' }]}
            onChange={(value) => {
              setTransport(value === 'http' ? 'http' : 'stdio')
              invalidateTest()
            }}
            disabled={pending}
          />
        </Field>
      </div>
      {transport === 'stdio' ? (
        <>
          <div className="formGrid">
            <Field label="启动命令" htmlFor={commandId} hint="例如 npx、uvx 或服务端二进制路径。" error={fieldErrors.command}>
              <input
                id={commandId}
                className="input"
                value={command}
                onChange={(event) => { setCommand(event.target.value); invalidateTest() }}
                aria-invalid={Boolean(fieldErrors.command)}
                disabled={pending}
              />
            </Field>
            <Field label="参数" htmlFor={argsId} hint="每行一个参数；留空表示没有参数。">
              <Textarea id={argsId} rows={4} value={argsText} onChange={(event) => { setArgsText(event.target.value); invalidateTest() }} disabled={pending} />
            </Field>
          </div>
          <Field label="环境变量" htmlFor={envId} hint="JSON 对象；值可用 ${NAME} 引用服务端环境变量。" error={fieldErrors.env}>
            <Textarea id={envId} code rows={4} value={envText} onChange={(event) => { setEnvText(event.target.value); invalidateTest() }} spellCheck={false} aria-invalid={Boolean(fieldErrors.env)} disabled={pending} />
          </Field>
        </>
      ) : (
        <>
          <div className="formGrid">
            <Field label="服务地址" htmlFor={urlId} hint="必须是服务端可访问的 MCP HTTP 端点。" error={fieldErrors.url}>
              <input
                id={urlId}
                className="input"
                value={url}
                onChange={(event) => { setUrl(event.target.value); invalidateTest() }}
                aria-invalid={Boolean(fieldErrors.url)}
                disabled={pending}
              />
            </Field>
            <Field label="请求头" htmlFor={headersId} hint="JSON 对象；值可用 ${NAME} 引用服务端环境变量。" error={fieldErrors.headers}>
              <Textarea id={headersId} code rows={4} value={headersText} onChange={(event) => { setHeadersText(event.target.value); invalidateTest() }} spellCheck={false} aria-invalid={Boolean(fieldErrors.headers)} disabled={pending} />
            </Field>
          </div>
        </>
      )}
      {fieldErrors.config ? <span className={styles.resultError} role="alert">{fieldErrors.config}</span> : null}
      <div className={ext.checkboxField}>
        <Checkbox
          id="mcp-add-enabled"
          label="创建后启用"
          checked={enabled}
          disabled={pending}
          aria-describedby={`${enabledHintId}-hint`}
          onChange={(event) => { setEnabled(event.target.checked); invalidateTest() }}
        />
        <FieldHint id={`${enabledHintId}-hint`}>停用的服务器不会参与扩展解析，之后可在列表中启用。</FieldHint>
      </div>
      <Disclosure summary="粘贴单个服务器 JSON 配置">
        <Field label="服务器 JSON" htmlFor="mcp-add-paste" hint="支持 name、command、args、env、url 和 headers；不会写入服务器，仅填入表单。">
          <Textarea id="mcp-add-paste" code rows={6} value={pasteText} onChange={(event) => setPasteText(event.target.value)} spellCheck={false} disabled={pending} />
        </Field>
        {pasteError ? <span className={styles.resultError} role="alert">{pasteError}</span> : null}
        <div className={ext.dialogActions}>
          <Button variant="secondary" onClick={fillFromPaste} disabled={pending}>填入表单</Button>
        </div>
      </Disclosure>
      {testState?.pending ? <p className={styles.testResult} role="status">正在测试连接…</p> : null}
      {testState?.error ? <p className={styles.resultError} role="status">测试失败：{testFailureText(testState)}</p> : null}
      {testState?.result ? <TestResultPanel result={testState.result} /> : null}
      <ActionHint>测试不写入服务器；保存时服务端会再次校验名称和配置。</ActionHint>
    </Dialog>
  )
}

function ImportMcpDialog({
  scope, projectId, projectName, onClose, onImported,
}: {
  scope: DialogScope
  projectId?: number
  projectName: string
  onClose: () => void
  onImported: (servers: McpServer[], tests: Record<string, McpTestState>) => void
}) {
  const mounted = useMountedRef()
  const [text, setText] = useState('')
  const parsed = useMemo(() => parseImportDocument(text), [text])
  const [results, setResults] = useState<Record<string, McpTestState>>({})
  const [testingAll, setTestingAll] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const documentGeneration = useRef(0)

  // Editing the pasted document starts a new generation: a probe that was
  // started against the old text must not write its result under the same
  // server name after the document changed.
  const resetResults = () => {
    documentGeneration.current += 1
    setResults({})
  }

  const runEntryTest = async (entry: ImportEntry) => {
    const generation = documentGeneration.current
    setResults((current) => ({ ...current, [entry.name]: { pending: true, serverName: entry.name } }))
    try {
      const result = await testMcpDraft({
        scope,
        ...(scope === 'project' && projectId ? { project_id: projectId } : {}),
        name: entry.name,
        config: entry.config,
        enabled: true,
      })
      if (mounted.current && documentGeneration.current === generation) {
        setResults((current) => ({ ...current, [entry.name]: succeededTestState(result, entry.name) }))
      }
    } catch (caught) {
      if (mounted.current && documentGeneration.current === generation) {
        setResults((current) => ({ ...current, [entry.name]: failedTestState(caught, entry.name) }))
      }
    }
  }

  const runAllTests = async () => {
    if (!parsed.entries.length) return
    setTestingAll(true)
    setError(null)
    const generation = documentGeneration.current
    for (const entry of parsed.entries) {
      if (!mounted.current || documentGeneration.current !== generation) break
      // Sequential probes stay below the server's process-wide test limit.
      await runEntryTest(entry)
    }
    if (mounted.current) setTestingAll(false)
  }

  const submit = async () => {
    if (!parsed.entries.length || parsed.error) return
    setPending(true)
    setError(null)
    try {
      const document = { mcpServers: Object.fromEntries(parsed.entries.map((entry) => [entry.name, entry.config])) }
      const response = await importMcpServers(scope, projectId, document)
      if (mounted.current) onImported(response?.servers || [], results)
    } catch (caught) {
      if (!mounted.current) return
      if (caught instanceof ApiError && caught.status === 409) {
        setError(new Error('导入失败：存在同名服务器，整批未写入。请修改名称后重试。'))
      } else if (caught instanceof ApiError && caught.status === 400) {
        setError(new Error(`导入失败：配置校验未通过，整批未写入。${errorMessage(caught)}`))
      } else {
        setError(uncertainWriteError(caught))
      }
    } finally {
      if (mounted.current) setPending(false)
    }
  }

  const anyPending = Object.values(results).some((state) => state.pending)

  return (
    <Dialog
      open
      onClose={pending ? () => {} : onClose}
      title="导入 MCP 服务器"
      description={`${scopeLabel(scope, projectName)}。粘贴 Claude Code mcpServers 文档；导入前可逐项测试连通性。`}
      wide
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button
            variant="primary"
            onClick={() => { void submit() }}
            loading={pending}
            disabled={Boolean(parsed.error) || !parsed.entries.length || testingAll || anyPending}
          >
            {parsed.entries.length ? `导入 ${parsed.entries.length} 个服务器` : '导入'}
          </Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} title="导入失败" /> : null}
      <Field label="Claude Code 配置" htmlFor="mcp-import-document" hint="粘贴包含 mcpServers 的 JSON 文档；导入和测试结果都不会回显配置值。">
        <Textarea
          id="mcp-import-document"
          code
          rows={10}
          value={text}
          onChange={(event) => { setText(event.target.value); resetResults() }}
          spellCheck={false}
          disabled={pending || testingAll || anyPending}
        />
      </Field>
      {parsed.error ? <span className={styles.resultError} role="alert">{parsed.error}</span> : null}
      {parsed.entries.length ? (
        <>
          <p className={ext.importSummary} role="status">已解析 {parsed.entries.length} 个服务器：{parsed.entries.map((entry) => entry.name).join('、')}</p>
          <div className={ext.dialogActions}>
            <Button variant="secondary" onClick={() => { void runAllTests() }} loading={testingAll} disabled={pending || anyPending}>逐项测试全部</Button>
          </div>
          <ul className={ext.importList}>
            {parsed.entries.map((entry) => {
              const state = results[entry.name]
              return (
                <li key={entry.name} className={ext.importItem}>
                  <div className={ext.importItemMain} aria-live="polite">
                    <code className="mono small">{entry.name}</code>
                    {state?.pending ? <span className="small muted" role="status">测试中…</span> : null}
                    {state?.result ? (
                      <>
                        <Badge tone="ok">连接成功</Badge>
                        <span className="small muted">
                          {typeof state.result.latency_ms === 'number' ? `延迟 ${state.result.latency_ms} ms` : '延迟不可用'}
                        </span>
                        <span className="small muted">
                          工具 {typeof state.result.tool_count === 'number' ? state.result.tool_count : (state.result.tools || []).length} 个
                        </span>
                      </>
                    ) : null}
                    {state?.error ? (
                      <span className={styles.resultError}>测试失败：{testFailureText(state)}</span>
                    ) : null}
                  </div>
                  <Button
                    variant="secondary"
                    className={ui.smallButton}
                    onClick={() => { void runEntryTest(entry) }}
                    loading={state?.pending}
                    disabled={pending || testingAll}
                  >
                    {state?.result ? '重新测试' : state?.error ? '重试' : '测试'}
                  </Button>
                </li>
              )
            })}
          </ul>
        </>
      ) : null}
      <ActionHint>导入在单个事务内按名称写入；任一项校验失败则整批不写入。测试只验证连通性，不写入服务器。</ActionHint>
    </Dialog>
  )
}

function UploadSkillDialog({
  file, scope, projectId, projectName, onClose, onUploaded,
}: {
  file: File
  scope: DialogScope
  projectId?: number
  projectName: string
  onClose: () => void
  onUploaded: (skill: Skill) => void
}) {
  const mounted = useMountedRef()
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [fileError, setFileError] = useState('')

  const submit = async () => {
    const zipName = /\.zip$/i.test(file.name)
    const zipType = ['application/zip', 'application/x-zip-compressed'].includes(file.type)
    if (!zipName && !zipType) {
      setFileError('请选择 .zip 文件。')
      return
    }
    if (file.size > 8 * 1024 * 1024) {
      setFileError('zip 文件不能超过 8 MiB。')
      return
    }
    setPending(true)
    setError(null)
    setFileError('')
    try {
      const created = await uploadSkill(scope, projectId, file)
      if (mounted.current) onUploaded(created)
    } catch (caught) {
      if (!mounted.current) return
      const fields = errorFieldMessages(caught)
      setFileError(fields.file || '')
      if (caught instanceof ApiError && caught.status === 409) {
        setError(new Error('同名 Skill 已存在，请更换名称或删除旧项后重试。'))
      } else if (!fields.file) {
        setError(uncertainWriteError(caught))
      } else {
        setError(null)
      }
    } finally {
      if (mounted.current) setPending(false)
    }
  }

  return (
    <Dialog
      open
      onClose={pending ? () => {} : onClose}
      title="上传 Skill zip"
      description={`${scopeLabel(scope, projectName)}。zip 会解压到受管 Skill 目录，脚本不会执行。`}
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>上传</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} title="上传失败" /> : null}
      <Note tone="info">
        zip 必须包含 SKILL.md；压缩包不超过 8 MiB，解压总量不超过 16 MiB，单文件不超过 4 MiB，最多 512 个文件。同名 Skill 不会被覆盖。
      </Note>
      <div className={ext.fileRow}>
        <span className={ext.fileName}>{file.name}</span>
        <span className="small muted">{formatFileSize(file.size)}</span>
      </div>
      {fileError ? <span className={styles.resultError} role="alert">{fileError}</span> : null}
      <ActionHint>上传成功后列表刷新；内置 Skill 无替换或删除入口。</ActionHint>
    </Dialog>
  )
}

function RegisterSkillDirDialog({
  scope, projectId, projectName, onClose, onRegistered,
}: {
  scope: DialogScope
  projectId?: number
  projectName: string
  onClose: () => void
  onRegistered: (skill: Skill) => void
}) {
  const mounted = useMountedRef()
  const [dir, setDir] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [fieldError, setFieldError] = useState('')

  const submit = async () => {
    const trimmed = dir.trim()
    if (!trimmed.startsWith('/')) {
      setFieldError('请输入以 / 开头的绝对路径。')
      return
    }
    if (trimmed.includes('\n') || trimmed.includes('\0')) {
      setFieldError('路径不能包含换行或空字符。')
      return
    }
    setFieldError('')
    setError(null)
    setPending(true)
    try {
      const created = await registerSkillDir(scope, projectId, trimmed)
      if (mounted.current) onRegistered(created)
    } catch (caught) {
      if (!mounted.current) return
      const fields = errorFieldMessages(caught)
      setFieldError(fields.dir || '')
      if (caught instanceof ApiError && caught.status === 409) {
        setError(new Error('同名 Skill 已存在，请更换名称或删除旧项后重试。'))
      } else if (!fields.dir) {
        setError(uncertainWriteError(caught))
      } else {
        setError(null)
      }
    } finally {
      if (mounted.current) setPending(false)
    }
  }

  return (
    <Dialog
      open
      onClose={pending ? () => {} : onClose}
      title="登记服务器目录"
      description={`${scopeLabel(scope, projectName)}。读取服务端目录并复制到受管 Skill 目录。`}
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>登记</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} title="登记失败" /> : null}
      <Note tone="warn">
        该操作读取服务端文件系统路径并复制到受管目录，源目录不会被修改或删除。只登记自己控制的目录，目录及其内部不能包含符号链接。
      </Note>
      <Field label="服务端绝对路径" htmlFor="skill-register-dir" hint="必须是以 / 开头的绝对路径，且目录中必须包含 SKILL.md。" error={fieldError}>
        <input
          id="skill-register-dir"
          className="input"
          value={dir}
          onChange={(event) => setDir(event.target.value)}
          aria-invalid={Boolean(fieldError)}
          disabled={pending}
        />
      </Field>
      <ActionHint>目录登记是高级选项，推荐用 zip 上传。</ActionHint>
    </Dialog>
  )
}
