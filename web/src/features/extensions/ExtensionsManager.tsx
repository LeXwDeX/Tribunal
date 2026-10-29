import { Fragment, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  createMcpServer, deleteMcpServer, deleteSkill, importMcpServers, listBindings, listMcpServers,
  listSkills, registerSkillDirectory, restoreDefaultBindings, saveBinding, setSkillEnabled, testMcpServer,
  updateMcpServer, uploadSkill,
} from '../../api/api'
import { ApiError, errorMessage } from '../../api/client'
import type {
  BlockBinding, McpServer, McpTool, Project, RestoreDefaultsChange, RestoreDefaultsResult, Skill,
} from '../../api/types'
import { useCanWrite } from '../../components/RoleContext'
import {
  Badge, Button, ConfirmDialog, Dialog, EmptyState, ErrorState, Field, LoadingState, Panel, Tabs, useToast,
} from '../../components/ui'
import { usePaginated } from '../../lib/hooks'
import { formatJson } from '../../lib/format'
import ui from '../../styles/ui.module.css'
import styles from '../../pages/pages.module.css'
import ext from './extensions.module.css'

const blocks = ['code-review', 'review', 'reasoner', 'verify', 'synthesize', 'onboarding']
const tabs = [
  { key: 'mcp', label: 'MCP 服务器', panelId: 'tabpanel-mcp' },
  { key: 'skills', label: 'Skill', panelId: 'tabpanel-skills' },
  { key: 'bindings', label: '块绑定', panelId: 'tabpanel-bindings' },
]

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

export function ExtensionsManager({
  scope, projectId, tab, onTabChange, projects = [], canManage = false, placeholder, notice,
}: {
  scope: 'platform' | 'project'
  projectId?: number
  tab: string
  onTabChange: (tab: string) => void
  projects?: Project[]
  canManage?: boolean
  // Shown in the tab panel while project scope has no project selected.
  placeholder?: ReactNode
  // Shown at the top of the tab panel in every state, e.g. a failed project refresh.
  notice?: ReactNode
}) {
  const platformAdmin = useCanWrite()
  // Every extension write changes host execution security, so the server
  // requires the platform admin role. Project scope additionally requires the
  // project manage grant and fails closed when it is missing.
  const canWrite = platformAdmin && (scope === 'platform' || (Boolean(projectId) && canManage))
  const toast = useToast()
  const queryClient = useQueryClient()
  const [testResults, setTestResults] = useState<Record<string, { tools?: McpTool[]; error?: string; pending?: boolean }>>({})
  const [addOpen, setAddOpen] = useState(false)
  const [importOpen, setImportOpen] = useState(false)
  const [editServer, setEditServer] = useState<McpServer | null>(null)
  const [deleteServer, setDeleteServer] = useState<McpServer | null>(null)
  const [deletePending, setDeletePending] = useState(false)
  const [deleteError, setDeleteError] = useState<unknown>(null)
  const [uploadOpen, setUploadOpen] = useState(false)
  const [directoryOpen, setDirectoryOpen] = useState(false)
  const [deleteSkillTarget, setDeleteSkillTarget] = useState<Skill | null>(null)
  const [skillDeletePending, setSkillDeletePending] = useState(false)
  const [skillDeleteError, setSkillDeleteError] = useState<unknown>(null)

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
  }, [scope, projectId])

  const invalidateServers = () => queryClient.invalidateQueries({ queryKey: ['mcp-servers', scope, projectId] })
  const invalidateSkills = () => queryClient.invalidateQueries({ queryKey: ['skills', scope, projectId] })
  const invalidateBindings = () => queryClient.invalidateQueries({ queryKey: ['block-bindings', scope, projectId] })

  const runTest = async (server: McpServer) => {
    setTestResults((current) => ({ ...current, [server.id]: { pending: true } }))
    try {
      const result = await testMcpServer(server.id)
      if (result?.error) {
        setTestResults((current) => ({ ...current, [server.id]: { error: result.error } }))
      } else {
        setTestResults((current) => ({
          ...current,
          [server.id]: { tools: Array.isArray(result?.tools) ? result.tools : [] },
        }))
        void invalidateServers()
        void invalidateBindings()
      }
    } catch (error) {
      setTestResults((current) => ({ ...current, [server.id]: { error: errorMessage(error) } }))
    }
  }

  const removeServer = async () => {
    if (!deleteServer) return
    setDeletePending(true)
    setDeleteError(null)
    try {
      await deleteMcpServer(deleteServer.id)
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

  const toggleServer = async (target: McpServer) => {
    try {
      await updateMcpServer(target.id, { enabled: !target.enabled })
      toast.push('success', target.enabled ? 'MCP 服务器已停用，重启服务后生效。' : 'MCP 服务器已启用，重启服务后生效。')
      await invalidateServers()
    } catch (error) {
      toast.push('error', errorMessage(error))
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
      <Tabs items={tabItems} value={tab} onChange={onTabChange} label="扩展类型" idPrefix="extension-tab" />
      <div role="tabpanel" id={`tabpanel-${tab}`} aria-labelledby={`extension-tab-${tab}`} className={ui.pageStack}>
      {notice}
      {!projectEnabled ? (
        placeholder ?? <EmptyState title="请选择项目" description="项目级扩展需要指定项目后才能读取。" />
      ) : null}

      {projectEnabled && tab === 'mcp' ? (
        <McpTab
          canWrite={canWrite}
          scope={scope}
          projectId={projectId}
          projectName={projectName}
          servers={servers}
          testResults={testResults}
          onTest={runTest}
          onAdd={() => setAddOpen(true)}
          onImport={() => setImportOpen(true)}
          onEdit={setEditServer}
          onToggle={toggleServer}
          onDelete={setDeleteServer}
        />
      ) : null}

      {projectEnabled && tab === 'skills' ? (
        <SkillsTab
          canWrite={canWrite}
          skills={skills}
          bindings={bindings.items}
          onUpload={() => setUploadOpen(true)}
          onDirectory={() => setDirectoryOpen(true)}
          onToggle={toggleSkill}
          onDelete={setDeleteSkillTarget}
        />
      ) : null}

      {projectEnabled && tab === 'bindings' ? (
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
          onSaved={invalidateBindings}
        />
      ) : null}
      </div>

      {canWrite ? (
        <>
          <AddMcpDialog open={addOpen} scope={scope} projectId={projectId} onClose={() => setAddOpen(false)} onSaved={() => { setAddOpen(false); void invalidateServers() }} />
          <ImportMcpDialog open={importOpen} scope={scope} projectId={projectId} onClose={() => setImportOpen(false)} onSaved={() => { setImportOpen(false); void invalidateServers() }} />
          <EditMcpDialog server={editServer} onClose={() => setEditServer(null)} onSaved={() => { setEditServer(null); void invalidateServers() }} />
          <UploadSkillDialog open={uploadOpen} scope={scope} projectId={projectId} onClose={() => setUploadOpen(false)} onSaved={() => { setUploadOpen(false); void invalidateSkills() }} />
          <DirectorySkillDialog open={directoryOpen} scope={scope} projectId={projectId} onClose={() => setDirectoryOpen(false)} onSaved={() => { setDirectoryOpen(false); void invalidateSkills() }} />
        </>
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
    </div>
  )
}

function McpTab({
  canWrite, scope, projectName, servers, testResults, onTest, onAdd, onImport, onEdit, onToggle, onDelete,
}: {
  canWrite: boolean
  scope: string
  projectId?: number
  projectName: string
  servers: ReturnType<typeof useScopeQuery>
  testResults: Record<string, { tools?: McpTool[]; error?: string; pending?: boolean }>
  onTest: (server: McpServer) => void
  onAdd: () => void
  onImport: () => void
  onEdit: (server: McpServer) => void
  onToggle: (server: McpServer) => void
  onDelete: (server: McpServer) => void
}) {
  return (
    <Panel
      title="MCP 服务器"
      aside={scope === 'platform' ? '平台级' : `项目级 · ${projectName}`}
    >
      {canWrite ? (
        <div className={styles.actionsRight}>
          <Button variant="secondary" onClick={onImport}>粘贴 Claude Code 配置导入</Button>
          <Button variant="primary" onClick={onAdd}>新增 MCP 服务器</Button>
        </div>
      ) : null}
      {servers.isPending ? <LoadingState /> : null}
      {servers.isError ? <ErrorState error={servers.error} onRetry={servers.retry} /> : null}
      {servers.isSuccess && servers.items.length === 0 ? (
        <EmptyState title="还没有 MCP 服务器" description="可新增 stdio 或 HTTP MCP 服务器，或粘贴 Claude Code 的 mcpServers 配置。" />
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
                const hasDetail = Boolean(server.last_error || result?.error || result?.tools)
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
                      <td className="right num">{server.tools?.length ? server.tools.length : <span className="muted">未提供</span>}</td>
                      {canWrite ? (
                        <td className="right">
                          <div className={ext.rowActions}>
                            <Button variant="secondary" className={ui.smallButton} onClick={() => onTest(server)} loading={result?.pending}>测试并列出工具</Button>
                            {server.builtin ? (
                              // Built-in servers follow the platform release: they can
                              // only be switched off and on.
                              <Button variant="secondary" className={ui.smallButton} onClick={() => onToggle(server)}>{server.enabled ? '停用' : '启用'}</Button>
                            ) : (
                              <>
                                <Button variant="secondary" className={ui.smallButton} onClick={() => onEdit(server)}>编辑</Button>
                                <Button variant="danger" className={ui.smallButton} onClick={() => onDelete(server)}>删除</Button>
                              </>
                            )}
                          </div>
                        </td>
                      ) : null}
                    </tr>
                    {hasDetail ? (
                      <tr className={ext.detailRow}>
                        <td colSpan={canWrite ? 6 : 5}>
                          {server.last_error ? <span className={styles.resultError}>{server.last_error}</span> : null}
                          {result?.error ? <span className={styles.resultError} role="status">测试失败：{result.error}</span> : null}
                          {result?.tools ? (
                            <div className={styles.testResult} role="status">
                              {result.tools.length ? (
                                <details className={ui.disclosure}>
                                  <summary>测试成功：{result.tools.length} 个工具</summary>
                                  <ul className={styles.list} style={{ marginTop: 6 }}>
                                    {result.tools.map((tool) => (
                                      <li key={tool.name} className={styles.listItem}>
                                        <div className={styles.listMain}>
                                          <span className="mono small">{tool.name}</span>
                                          {tool.description ? <span className="small muted">{tool.description}</span> : null}
                                        </div>
                                      </li>
                                    ))}
                                  </ul>
                                </details>
                              ) : <span>测试成功，但服务端没有返回工具列表。</span>}
                            </div>
                          ) : null}
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

function SkillsTab({
  canWrite, skills, bindings, onUpload, onDirectory, onToggle, onDelete,
}: {
  canWrite: boolean
  skills: ReturnType<typeof usePaginated<Skill>>
  bindings: BlockBinding[]
  onUpload: () => void
  onDirectory: () => void
  onToggle: (skill: Skill) => void
  onDelete: (skill: Skill) => void
}) {
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
    <Panel title="Skill" aside="首版只支持指令类 Skill，其中的脚本不会被执行。">
      {canWrite ? (
        <div className={styles.actionsRight}>
          <Button variant="secondary" onClick={onDirectory}>登记服务器目录</Button>
          <Button variant="primary" onClick={onUpload}>上传 Skill（zip）</Button>
        </div>
      ) : null}
      {skills.isPending ? <LoadingState /> : null}
      {skills.isError ? <ErrorState error={skills.error} onRetry={skills.retry} /> : null}
      {skills.isSuccess && skills.items.length === 0 ? <EmptyState title="还没有 Skill" description="上传包含 SKILL.md 的 zip，或登记服务器上的目录。" /> : null}
      {skills.items.length || (canWrite && skills.isSuccess) ? (
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
                {canWrite ? (
                  <span className={ext.rowActions} style={{ marginLeft: 'auto' }}>
                    <Button variant="secondary" className={ui.smallButton} onClick={() => onToggle(skill)}>{skill.enabled ? '禁用' : '启用'}</Button>
                    {/* Built-in skills ship with the platform: disable them instead. */}
                    {skill.builtin ? null : <Button variant="secondary" className={ui.smallButton} onClick={() => onDelete(skill)}>删除</Button>}
                  </span>
                ) : null}
              </div>
            </article>
          ))}
          {canWrite ? (
            <button type="button" className={ext.uploadCard} onClick={onUpload}>
              <span className={ext.uploadPlus} aria-hidden="true">+</span>
              上传 Skill zip
            </button>
          ) : null}
        </div>
      ) : null}
      {skills.hasNextPage ? <Button variant="secondary" onClick={skills.fetchNextPage} loading={skills.isFetchingNextPage}>加载更多 Skill</Button> : null}
    </Panel>
  )
}

type BindingDraft = { mcp_tools: string[]; skills: string[] }
type BindingDrafts = { baseline: Record<string, BindingDraft>; local: Record<string, BindingDraft> }
type RestoreTarget = 'all' | string

const emptyDraft: BindingDraft = { mcp_tools: [], skills: [] }

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

function bindingDrafts(items: BlockBinding[]): Record<string, BindingDraft> {
  const result: Record<string, BindingDraft> = {}
  for (const item of items) {
    if (!result[item.block]) {
      result[item.block] = { mcp_tools: [...(item.mcp_tools || [])], skills: [...(item.skills || [])] }
    }
  }
  return result
}

// Merge a server snapshot without discarding drafts. A block keeps its local
// draft when it differs from the previous baseline; clean blocks follow the
// server so pagination, refetches, and successful saves become visible.
function mergeBindingDrafts(current: BindingDrafts, items: BlockBinding[]): BindingDrafts {
  const server = bindingDrafts(items)
  const dirty = (block: string) => !sameDraft(current.baseline[block] || emptyDraft, current.local[block] || emptyDraft)
  const baseline: Record<string, BindingDraft> = {}
  const local: Record<string, BindingDraft> = {}
  for (const [block, value] of Object.entries(server)) {
    baseline[block] = value
    local[block] = dirty(block) ? cloneDraft(current.local[block] || emptyDraft) : value
  }
  for (const [block, value] of Object.entries(current.baseline)) {
    if (server[block] || !dirty(block)) continue
    baseline[block] = value
    local[block] = cloneDraft(current.local[block] || emptyDraft)
  }
  for (const [block, value] of Object.entries(current.local)) {
    if (server[block] || baseline[block] || !dirty(block)) continue
    local[block] = cloneDraft(value)
  }
  return { baseline, local }
}

function BindingsTab({
  canWrite, scope, projectId, bindingState, servers, skills, testResults, onSaved,
}: {
  canWrite: boolean
  scope: 'platform' | 'project'
  projectId?: number
  bindingState: ReturnType<typeof usePaginated<BlockBinding>>
  servers: McpServer[]
  skills: Skill[]
  testResults: Record<string, { tools?: McpTool[]; error?: string; pending?: boolean }>
  onSaved: () => Promise<void> | void
}) {
  const toast = useToast()
  const bindingSnapshot = JSON.stringify(bindingState.items)
  const [drafts, setDrafts] = useState<BindingDrafts>(() => (
    mergeBindingDrafts({ baseline: {}, local: {} }, bindingState.items)
  ))
  const appliedSnapshot = useRef(bindingSnapshot)
  const { baseline, local } = drafts
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<unknown>(null)
  const [restoreTarget, setRestoreTarget] = useState<RestoreTarget | null>(null)
  const [restorePreview, setRestorePreview] = useState<RestoreDefaultsResult | null>(null)
  const [previewPending, setPreviewPending] = useState(false)
  const [previewError, setPreviewError] = useState<unknown>(null)
  const [restorePending, setRestorePending] = useState(false)
  const [restoreError, setRestoreError] = useState<unknown>(null)
  const [restoreConflict, setRestoreConflict] = useState(false)
  const restoreRequest = useRef(0)

  // Merge each distinct server snapshot into the baseline while keeping dirty
  // drafts. The content key avoids merging the same page data repeatedly when
  // usePaginated rebuilds its items array.
  useLayoutEffect(() => {
    if (appliedSnapshot.current === bindingSnapshot) return
    appliedSnapshot.current = bindingSnapshot
    setDrafts((current) => mergeBindingDrafts(current, bindingState.items))
  }, [bindingSnapshot, bindingState.items])

  const rowNames = useMemo(() => {
    const names = [...blocks]
    for (const block of Object.keys(local)) if (!names.includes(block)) names.push(block)
    return names
  }, [local])

  const toolColumns = useMemo(() => {
    const set = new Set<string>()
    for (const binding of bindingState.items) for (const tool of binding.mcp_tools || []) set.add(tool)
    for (const server of servers) {
      for (const tool of server.tools || []) set.add(`mcp__${server.name}__${tool.name}`)
      for (const tool of testResults[server.id]?.tools || []) set.add(`mcp__${server.name}__${tool.name}`)
    }
    return Array.from(set).sort()
  }, [bindingState.items, servers, testResults])

  const skillColumns = useMemo(() => {
    const set = new Set<string>()
    for (const binding of bindingState.items) for (const skill of binding.skills || []) set.add(skill)
    for (const skill of skills) set.add(skill.name)
    return Array.from(set).sort()
  }, [bindingState.items, skills])

  const toggle = (block: string, group: 'mcp_tools' | 'skills', value: string) => {
    if (!canWrite) return
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
    if (!canRestore || dirty || saving) return
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

  const confirmRestore = async () => {
    if (!restoreTarget || !restorePreview || restoreConflict || restorePending) return
    setRestorePending(true)
    setRestoreError(null)
    try {
      const result = await restoreDefaultBindings(restoreTarget === 'all'
        ? { dry_run: false, preview_token: restorePreview.preview_token }
        : { blocks: [restoreTarget], dry_run: false, preview_token: restorePreview.preview_token })
      toast.push('success', result.restart_required
        ? '默认绑定已恢复，重启服务后生效。'
        : '默认绑定已恢复。')
      restoreRequest.current += 1
      setRestoreTarget(null)
      setRestorePreview(null)
      await onSaved()
    } catch (error) {
      // A stale preview is never retried automatically: the admin decides
      // when to preview the new state and confirm again.
      if (error instanceof ApiError && error.status === 409) setRestoreConflict(true)
      else setRestoreError(error)
    } finally {
      setRestorePending(false)
    }
  }

  const save = async () => {
    if (!canWrite) return
    setSaving(true)
    setSaveError(null)
    const submitted = rowNames
      .filter((block) => !sameDraft(baseline[block] || emptyDraft, local[block] || emptyDraft))
      .map((block) => ({ block, draft: cloneDraft(local[block] || emptyDraft) }))
    if (!submitted.length) {
      setSaving(false)
      toast.push('info', '没有需要保存的绑定变更。')
      return
    }
    const results = await Promise.allSettled(submitted.map(({ block, draft }) => saveBinding(block, {
      scope,
      project_id: projectId,
      mcp_tools: draft.mcp_tools,
      skills: draft.skills,
    })))
    const failures = results.filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    if (failures.length) {
      setSaveError(failures[0].reason)
      toast.push('error', `部分绑定未保存（${failures.length}/${submitted.length} 个请求失败）。`)
    } else {
      toast.push('success', '块绑定已保存。')
    }
    // Only a fulfilled write advances that block's baseline. Failed blocks
    // keep both the server baseline and the draft so a retry is possible.
    setDrafts((current) => {
      let next = current.baseline
      submitted.forEach(({ block, draft }, index) => {
        if (results[index].status !== 'fulfilled') return
        if (next === current.baseline) next = { ...next }
        next[block] = draft
      })
      return next === current.baseline ? current : { ...current, baseline: next }
    })
    await onSaved()
    setSaving(false)
  }

  if (bindingState.isPending) return <Panel title="块绑定"><LoadingState /></Panel>
  if (bindingState.isError) return <Panel title="块绑定"><ErrorState error={bindingState.error} onRetry={bindingState.retry} /></Panel>

  return (
    <>
    <Panel
      title="块绑定"
      aside={scope === 'platform' ? '平台默认绑定' : '项目覆盖绑定（未覆盖的块继承平台默认）'}
    >
      {canRestore ? (
        <div className={ext.restoreBar}>
          <div className={ext.restoreText}>
            <span className={ext.restoreTitle}>内置默认授权</span>
            <span>恢复只处理平台级绑定。全部恢复由服务端选择全部可恢复块。</span>
            {dirty ? (
              <span className={ext.restoreWarning} role="status">先保存绑定再恢复默认，未保存的更改会丢失。</span>
            ) : null}
          </div>
          <Button variant="secondary" disabled={dirty || saving} onClick={() => openRestore('all')}>恢复全部默认</Button>
        </div>
      ) : null}
      {!toolColumns.length && !skillColumns.length ? (
        <div className={styles.warning}>
          没有可绑定的工具或 Skill。先添加 MCP 服务器和 Skill，并运行 MCP 测试以发现工具；已保存的绑定名称也会显示为列。
        </div>
      ) : null}
      <>
          <div className={styles.matrixWrap}>
            <table className={styles.matrix}>
              <thead>
                <tr>
                  <th scope="col" style={{ textAlign: 'left' }}>块</th>
                  {canRestore ? <th scope="col" className="right">默认</th> : null}
                  {toolColumns.map((tool) => <th key={tool} scope="col">{tool}</th>)}
                  {skillColumns.map((skill) => <th key={`skill-${skill}`} scope="col">skill: {skill}</th>)}
                </tr>
              </thead>
              <tbody>
                {rowNames.map((block) => {
                  const row = local[block] || emptyDraft
                  return (
                    <tr key={block}>
                      <th scope="row">{block}</th>
                      {canRestore ? (
                        <td className="right">
                          {blocks.includes(block) ? (
                            <Button
                              variant="secondary"
                              className={ui.smallButton}
                              disabled={dirty || saving}
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
                        return (
                          <td key={tool}>
                            <button
                              type="button"
                              className={[styles.matrixCell, on ? styles.matrixCellOn : ''].filter(Boolean).join(' ')}
                              aria-pressed={on}
                              aria-label={`${block} 使用 ${tool}`}
                              disabled={!canWrite}
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
                              disabled={!canWrite}
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
          <span className={styles.matrixNote}>工具名使用 `mcp__server__tool` 形式。测试 MCP 成功后，发现的工具会出现在列中。</span>
      </>
      {saveError ? <ErrorState error={saveError} title="保存绑定失败" /> : null}
      {canWrite ? (
        <div className={styles.actionsRight}>
          <Button variant="primary" onClick={() => { void save() }} loading={saving}>保存绑定</Button>
        </div>
      ) : <span className="small muted">只读账号不能修改块绑定。</span>}
      {bindingState.hasNextPage ? <Button variant="secondary" onClick={bindingState.fetchNextPage} loading={bindingState.isFetchingNextPage}>加载更多绑定</Button> : null}
    </Panel>
    {canRestore ? (
      <RestoreDialog
        target={restoreTarget}
        preview={restorePreview}
        previewPending={previewPending}
        previewError={previewError}
        pending={restorePending}
        error={restoreError}
        conflict={restoreConflict}
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
  target, preview, previewPending, previewError, pending, error, conflict, onClose, onPreview, onConfirm,
}: {
  target: RestoreTarget | null
  preview: RestoreDefaultsResult | null
  previewPending: boolean
  previewError: unknown
  pending: boolean
  error: unknown
  conflict: boolean
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
            <Button variant="secondary" onClick={onPreview} loading={previewPending}>重新预览</Button>
          ) : null}
          {preview && !conflict ? (
            <Button variant="primary" onClick={onConfirm} loading={pending}>确认恢复</Button>
          ) : null}
        </>
      )}
    >
      {previewPending ? <LoadingState label="正在预览默认绑定…" /> : null}
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

function AddMcpDialog({ open, scope, projectId, onClose, onSaved }: { open: boolean; scope: 'platform' | 'project'; projectId?: number; onClose: () => void; onSaved: () => void }) {
  const nameId = useId()
  const configId = useId()
  const [name, setName] = useState('')
  const [configText, setConfigText] = useState('{}')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [fieldError, setFieldError] = useState('')

  useEffect(() => {
    if (!open) {
      setName('')
      setConfigText('{}')
      setError(null)
      setFieldError('')
      setPending(false)
    }
  }, [open])

  const submit = async () => {
    if (!name.trim()) { setFieldError('请输入名称。'); return }
    let config: Record<string, unknown>
    try {
      config = parseJsonObject(configText, '配置')
      setFieldError('')
    } catch (caught) {
      setFieldError(caught instanceof Error ? caught.message : '配置 JSON 无效。')
      return
    }
    setPending(true)
    setError(null)
    try {
      await createMcpServer({ scope, project_id: projectId, name: name.trim(), config })
      onSaved()
    } catch (caught) {
      setError(caught)
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="新增 MCP 服务器"
      description="config 是单个 Claude Code mcpServers 条目。敏感 env 或 header 值不会回显。"
      wide
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>添加</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} /> : null}
      <Field label="名称" htmlFor={nameId} error={fieldError} hint="用于工具名 mcp__名称__工具 和绑定。">
        <input id={nameId} className="input" value={name} onChange={(event) => setName(event.target.value)} autoComplete="off" aria-invalid={Boolean(fieldError)} />
      </Field>
      <Field label="配置 JSON" htmlFor={configId} hint='例如 {"command":"...","args":["..."]} 或 {"url":"https://..."}'>
        <textarea id={configId} className="textarea textareaCode" rows={9} value={configText} onChange={(event) => setConfigText(event.target.value)} spellCheck={false} />
      </Field>
    </Dialog>
  )
}

function ImportMcpDialog({ open, scope, projectId, onClose, onSaved }: { open: boolean; scope: 'platform' | 'project'; projectId?: number; onClose: () => void; onSaved: () => void }) {
  const id = useId()
  const [text, setText] = useState('{\n  "mcpServers": {}\n}')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [parseError, setParseError] = useState('')

  useEffect(() => {
    if (!open) {
      setText('{\n  "mcpServers": {}\n}')
      setPending(false)
      setError(null)
      setParseError('')
    }
  }, [open])

  const submit = async () => {
    let parsed: Record<string, unknown>
    try {
      parsed = parseJsonObject(text, '导入内容')
      if (!parsed.mcpServers || typeof parsed.mcpServers !== 'object' || Array.isArray(parsed.mcpServers)) {
        setParseError('导入内容必须包含 mcpServers 对象。')
        return
      }
      setParseError('')
    } catch (caught) {
      setParseError(caught instanceof Error ? caught.message : 'JSON 无效。')
      return
    }
    setPending(true)
    setError(null)
    try {
      await importMcpServers(scope, projectId, parsed)
      onSaved()
    } catch (caught) {
      setError(caught)
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="导入 MCP 配置"
      description={`粘贴 .mcp.json 中的 mcpServers 对象，导入到${scope === 'platform' ? '平台级' : '当前项目'}。服务端会逐个校验并探测工具；失败项会返回错误。`}
      wide
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>校验并导入</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} /> : null}
      <Field label="mcpServers JSON" htmlFor={id} error={parseError}>
        <textarea id={id} className="textarea textareaCode" rows={12} value={text} onChange={(event) => setText(event.target.value)} spellCheck={false} />
      </Field>
    </Dialog>
  )
}

function EditMcpDialog({ server, onClose, onSaved }: { server: McpServer | null; onClose: () => void; onSaved: () => void }) {
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
      } catch (caught) {
        setFieldError(caught instanceof Error ? caught.message : '配置 JSON 无效。')
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
    } catch (caught) {
      setError(caught)
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={Boolean(server)}
      onClose={onClose}
      title="编辑 MCP 服务器"
      description="默认只修改名称和启用状态。为保护密钥，服务端不返回配置内容。"
      wide
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>保存</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} /> : null}
      <div className="formGrid">
        <Field label="名称" htmlFor={nameId} error={fieldError}>
          <input id={nameId} className="input" value={name} onChange={(event) => setName(event.target.value)} aria-invalid={Boolean(fieldError)} />
        </Field>
        <Field label="启用" htmlFor="mcp-edit-enabled">
          <select id="mcp-edit-enabled" className="select" value={enabled ? 'true' : 'false'} onChange={(event) => setEnabled(event.target.value === 'true')}>
            <option value="true">启用</option>
            <option value="false">停用</option>
          </select>
        </Field>
      </div>
      {server ? (
        <details className={ui.disclosure}>
          <summary>查看服务端返回的配置（已隐藏）</summary>
          <pre className={`pre ${ui.jsonBlock}`}>{formatJson(server.config)}</pre>
        </details>
      ) : null}
      <label className="checkboxRow">
        <input type="checkbox" checked={replaceConfig} onChange={(event) => {
          setReplaceConfig(event.target.checked)
          setConfigText('')
          setFieldError('')
        }} />
        <span>替换完整配置</span>
      </label>
      {replaceConfig ? (
        <>
          <div className={styles.warning}>
            替换会覆盖已有配置。脱敏的 env/header 值不会保留；如配置需要密钥，请重新填写完整值，且不要粘贴脱敏占位符。
          </div>
          <Field label="新的配置 JSON" htmlFor="mcp-replace-config" hint="从空白开始填写完整配置。">
            <textarea id="mcp-replace-config" className="textarea textareaCode" rows={9} value={configText} onChange={(event) => setConfigText(event.target.value)} spellCheck={false} />
          </Field>
        </>
      ) : null}
    </Dialog>
  )
}

function UploadSkillDialog({ open, scope, projectId, onClose, onSaved }: { open: boolean; scope: 'platform' | 'project'; projectId?: number; onClose: () => void; onSaved: () => void }) {
  const id = useId()
  const [file, setFile] = useState<File | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [fieldError, setFieldError] = useState('')

  useEffect(() => {
    if (!open) {
      setFile(null)
      setPending(false)
      setError(null)
      setFieldError('')
    }
  }, [open])

  const submit = async () => {
    if (!file) { setFieldError('请选择 zip 文件。'); return }
    if (!file.name.toLowerCase().endsWith('.zip')) { setFieldError('文件扩展名必须是 .zip。'); return }
    setPending(true)
    setError(null)
    try {
      await uploadSkill({ scope, project_id: projectId, file })
      onSaved()
    } catch (caught) {
      setError(caught)
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="上传 Skill"
      description="zip 根目录或子目录需包含 SKILL.md。服务端会读取名称和描述；其中的脚本不会被执行。"
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>上传</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} /> : null}
      <Field label="Skill zip" htmlFor={id} error={fieldError} hint="只接受 .zip。">
        <input id={id} className="input" type="file" accept=".zip,application/zip" onChange={(event) => setFile(event.target.files?.[0] || null)} />
      </Field>
    </Dialog>
  )
}

function DirectorySkillDialog({ open, scope, projectId, onClose, onSaved }: { open: boolean; scope: 'platform' | 'project'; projectId?: number; onClose: () => void; onSaved: () => void }) {
  const id = useId()
  const [dir, setDir] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [fieldError, setFieldError] = useState('')

  useEffect(() => {
    if (!open) {
      setDir('')
      setPending(false)
      setError(null)
      setFieldError('')
    }
  }, [open])

  const submit = async () => {
    if (!dir.trim()) { setFieldError('请输入服务器目录路径。'); return }
    setPending(true)
    setError(null)
    try {
      await registerSkillDirectory({ scope, project_id: projectId, dir: dir.trim() })
      onSaved()
    } catch (caught) {
      setError(caught)
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="登记 Skill 目录"
      description="目录位于服务端文件系统，必须包含 SKILL.md。"
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>登记</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} /> : null}
      <Field label="目录路径" htmlFor={id} error={fieldError}>
        <input id={id} className="input mono" value={dir} onChange={(event) => setDir(event.target.value)} placeholder="/srv/review/skills/my-skill" aria-invalid={Boolean(fieldError)} />
      </Field>
    </Dialog>
  )
}
