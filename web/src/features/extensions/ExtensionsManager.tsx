import { useEffect, useId, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  createMcpServer, deleteMcpServer, deleteSkill, importMcpServers, listBindings, listMcpServers,
  listSkills, registerSkillDirectory, saveBinding, testMcpServer, updateMcpServer, uploadSkill,
} from '../../api/api'
import { ApiError, errorMessage } from '../../api/client'
import type { BlockBinding, McpServer, McpTool, Project, Skill } from '../../api/types'
import { useCanWrite } from '../../components/RoleContext'
import {
  Badge, Button, ConfirmDialog, Dialog, EmptyState, ErrorState, Field, LoadingState, Panel, Tabs, useToast,
} from '../../components/ui'
import { usePaginated } from '../../lib/hooks'
import { formatJson } from '../../lib/format'
import ui from '../../styles/ui.module.css'
import styles from '../../pages/pages.module.css'

const blocks = ['code-review', 'review', 'reasoner', 'verify', 'synthesize', 'onboarding']
const tabs = [
  { key: 'mcp', label: 'MCP 服务器' },
  { key: 'skills', label: 'Skill' },
  { key: 'bindings', label: '块绑定' },
]

const mcpCommand = (server: McpServer): string => {
  const config = server.config || {}
  if (typeof config.command === 'string') {
    const args = Array.isArray(config.args) ? config.args.map(String) : []
    return [config.command, ...args].join(' ')
  }
  if (typeof config.url === 'string') return config.url
  return '配置未提供 command 或 url'
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
  scope, projectId, tab, onTabChange, projects = [], canManage = false,
}: {
  scope: 'platform' | 'project'
  projectId?: number
  tab: string
  onTabChange: (tab: string) => void
  projects?: Project[]
  canManage?: boolean
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

  return (
    <div className={ui.pageStack}>
      <Tabs items={tabs} value={tab} onChange={onTabChange} label="扩展类型" />
      <div role="tabpanel" id={`tabpanel-${tab}`} aria-labelledby={`tab-${tab}`} className={ui.pageStack}>
      {!projectEnabled ? (
        <EmptyState title="请选择项目" description="项目级扩展需要指定项目后才能读取。" />
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
          onDelete={setDeleteSkillTarget}
        />
      ) : null}

      {projectEnabled && tab === 'bindings' ? (
        <BindingsTab
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
  canWrite, scope, projectName, servers, testResults, onTest, onAdd, onImport, onEdit, onDelete,
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
        <div className={styles.cards}>
          {servers.items.map((server) => {
            const result = testResults[server.id]
            return (
              <article key={server.id} className={styles.infoCard}>
                <div className={styles.cardTitle}>
                  <span className="mono">{server.name}</span>
                  <Badge tone={server.enabled ? 'ok' : 'neutral'}>{server.enabled ? '启用' : '停用'}</Badge>
                </div>
                <div className={styles.cardMeta}>
                  <span>{server.scope === 'platform' ? '平台级' : `项目级 · ${projectName || `#${server.project_id}`}`}</span>
                  <span>{server.status || '状态未提供'}</span>
                  <span>{server.tools?.length ? `${server.tools.length} 个工具` : '工具数未提供'}</span>
                </div>
                <code className={styles.commandLine}>{mcpCommand(server)}</code>
                {server.last_error ? <span className={styles.resultError}>{server.last_error}</span> : null}
                <div className={styles.cardActions}>
                  {canWrite ? (
                    <Button variant="secondary" onClick={() => onTest(server)} loading={result?.pending}>测试并列出工具</Button>
                  ) : null}
                  {canWrite ? (
                    <>
                      <Button variant="secondary" onClick={() => onEdit(server)}>编辑</Button>
                      <Button variant="danger" onClick={() => onDelete(server)}>删除</Button>
                    </>
                  ) : null}
                </div>
                {result?.error ? <span className={styles.resultError} role="status">测试失败：{result.error}</span> : null}
                {result?.tools ? (
                  <div className={styles.testResult} role="status">
                    {result.tools.length ? (
                      <details>
                        <summary style={{ minHeight: 44, display: 'flex', alignItems: 'center', cursor: 'pointer' }}>测试成功：{result.tools.length} 个工具</summary>
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
              </article>
            )
          })}
        </div>
      ) : null}
      {servers.hasNextPage ? <Button variant="secondary" onClick={servers.fetchNextPage} loading={servers.isFetchingNextPage}>加载更多 MCP 服务器</Button> : null}
    </Panel>
  )
}

function SkillsTab({
  canWrite, skills, bindings, onUpload, onDirectory, onDelete,
}: {
  canWrite: boolean
  skills: ReturnType<typeof usePaginated<Skill>>
  bindings: BlockBinding[]
  onUpload: () => void
  onDirectory: () => void
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
      {skills.items.length ? (
        <div className="tableWrap">
          <table className="dataTable" style={{ minWidth: 820 }}>
            <thead>
              <tr>
                <th scope="col">名称</th>
                <th scope="col">描述</th>
                <th scope="col">作用域</th>
                <th scope="col">来源目录</th>
                <th scope="col">挂载到</th>
                {canWrite ? <th scope="col">操作</th> : null}
              </tr>
            </thead>
            <tbody>
              {skills.items.map((skill) => (
                <tr key={skill.id}>
                  <td className="mono">{skill.name}</td>
                  <td>{skill.description || '—'}</td>
                  <td>{skill.scope === 'platform' ? '平台级' : `项目级${skill.project_id ? ` #${skill.project_id}` : ''}`}</td>
                  <td className="mono small">{skill.dir || '—'}</td>
                  <td className="mono small">{usage.get(skill.name)?.join(', ') || '—'}</td>
                  {canWrite ? (
                    <td><Button variant="secondary" onClick={() => onDelete(skill)}>删除</Button></td>
                  ) : null}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {skills.hasNextPage ? <Button variant="secondary" onClick={skills.fetchNextPage} loading={skills.isFetchingNextPage}>加载更多 Skill</Button> : null}
    </Panel>
  )
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
  const [local, setLocal] = useState<Record<string, { mcp_tools: string[]; skills: string[] }>>({})
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<unknown>(null)

  const initial = useMemo(() => {
    const result: Record<string, { mcp_tools: string[]; skills: string[] }> = {}
    for (const binding of bindingState.items) {
      if (!result[binding.block]) {
        result[binding.block] = { mcp_tools: [...(binding.mcp_tools || [])], skills: [...(binding.skills || [])] }
      }
    }
    return result
  }, [bindingState.items])

  useEffect(() => {
    setLocal(initial)
  }, [initial])

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
    setLocal((current) => {
      const existing = current[block] || { mcp_tools: [], skills: [] }
      const values = new Set(existing[group])
      if (values.has(value)) values.delete(value)
      else values.add(value)
      return { ...current, [block]: { ...existing, [group]: Array.from(values) } }
    })
  }

  const save = async () => {
    if (!canWrite) return
    setSaving(true)
    setSaveError(null)
    const changes = rowNames.filter((block) => {
      const before = initial[block] || { mcp_tools: [], skills: [] }
      const after = local[block] || { mcp_tools: [], skills: [] }
      return before.mcp_tools.slice().sort().join('\u0000') !== after.mcp_tools.slice().sort().join('\u0000')
        || before.skills.slice().sort().join('\u0000') !== after.skills.slice().sort().join('\u0000')
    })
    if (!changes.length) {
      setSaving(false)
      toast.push('info', '没有需要保存的绑定变更。')
      return
    }
    const results = await Promise.allSettled(changes.map((block) => saveBinding(block, {
      scope,
      project_id: projectId,
      mcp_tools: local[block]?.mcp_tools || [],
      skills: local[block]?.skills || [],
    })))
    const failures = results.filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    if (failures.length) {
      setSaveError(failures[0].reason)
      toast.push('error', `部分绑定未保存（${failures.length}/${changes.length} 个请求失败）。`)
    } else {
      toast.push('success', '块绑定已保存。')
    }
    await onSaved()
    setSaving(false)
  }

  if (bindingState.isPending) return <Panel title="块绑定"><LoadingState /></Panel>
  if (bindingState.isError) return <Panel title="块绑定"><ErrorState error={bindingState.error} onRetry={bindingState.retry} /></Panel>

  return (
    <Panel
      title="块绑定"
      aside={scope === 'platform' ? '平台默认绑定' : '项目覆盖绑定（未覆盖的块继承平台默认）'}
    >
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
                  {toolColumns.map((tool) => <th key={tool} scope="col">{tool}</th>)}
                  {skillColumns.map((skill) => <th key={`skill-${skill}`} scope="col">skill: {skill}</th>)}
                </tr>
              </thead>
              <tbody>
                {rowNames.map((block) => {
                  const row = local[block] || { mcp_tools: [], skills: [] }
                  return (
                    <tr key={block}>
                      <th scope="row">{block}</th>
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
      description="默认只修改名称和启用状态。服务端返回的配置可能已脱敏，不能直接原样提交。"
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
        <details>
          <summary style={{ minHeight: 44, display: 'flex', alignItems: 'center', cursor: 'pointer' }}>查看服务端返回的配置（可能脱敏）</summary>
          <pre className="pre" style={{ background: 'var(--surface-2)', border: '1px solid var(--border)', borderRadius: 8, padding: 12, color: 'var(--text)' }}>{formatJson(server.config)}</pre>
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
