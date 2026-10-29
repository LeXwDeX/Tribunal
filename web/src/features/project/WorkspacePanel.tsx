import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  getCurrentContextDoc, getWorkspace, listContextDocs, refreshWorkspace, saveCurrentContextDoc, setContextDocLock,
} from '../../api/api'
import { ApiError, errorMessage } from '../../api/client'
import type { ContextDoc } from '../../api/types'
import {
  Badge, Button, EmptyState, ErrorState, LoadingState, Panel, Segmented, StatusBadge, Switch, useToast,
} from '../../components/ui'
import { usePaginated } from '../../lib/hooks'
import { formatDate, labelForSource, labelForStatus, shortSha } from '../../lib/format'
import ui from '../../styles/ui.module.css'
import styles from '../../pages/pages.module.css'
import page from '../../pages/projects.module.css'

type DocKind = 'agents' | 'claude'
type RefreshAction = 'refresh' | 'reindex' | 'regenerate'

const docKinds: Array<{ key: DocKind; label: string }> = [
  { key: 'agents', label: 'AGENTS' },
  { key: 'claude', label: 'CLAUDE' },
]

const ACTIVE_WORKSPACE_STATES = new Set(['new', 'refreshing', 'onboarding'])
const WORKSPACE_POLL_MS = 2000
const WORKSPACE_TRACK_MS = 5 * 60 * 1000

export function WorkspacePanel({ projectId, canManage = false }: { projectId: number; canManage?: boolean }) {
  const canWrite = canManage
  const toast = useToast()
  const queryClient = useQueryClient()
  const [kind, setKind] = useState<DocKind>('agents')
  const [selected, setSelected] = useState<ContextDoc | null>(null)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const [saveError, setSaveError] = useState<unknown>(null)
  const [saving, setSaving] = useState(false)
  const [lockPending, setLockPending] = useState(false)
  const [refreshAction, setRefreshAction] = useState<RefreshAction | null>(null)
  const [refreshError, setRefreshError] = useState<unknown>(null)
  const [trackingJob, setTrackingJob] = useState(false)
  const trackingTimeout = useRef<number | undefined>(undefined)

  const clearTrackingTimeout = () => {
    if (trackingTimeout.current !== undefined) {
      window.clearTimeout(trackingTimeout.current)
      trackingTimeout.current = undefined
    }
  }
  // The worker may not have written "refreshing" yet when the POST returns.
  // Keep polling for a bounded window so a fast old "ready" cannot stop the refresh.
  const startTracking = () => {
    clearTrackingTimeout()
    setTrackingJob(true)
    trackingTimeout.current = window.setTimeout(() => setTrackingJob(false), WORKSPACE_TRACK_MS)
  }
  useEffect(() => () => clearTrackingTimeout(), [])

  const workspace = useQuery({
    queryKey: ['project', projectId, 'workspace'],
    queryFn: () => getWorkspace(projectId),
    refetchInterval: (query) => {
      const state = query.state.data?.state
      return trackingJob || (state && ACTIVE_WORKSPACE_STATES.has(state)) ? WORKSPACE_POLL_MS : false
    },
  })
  const workspaceState = workspace.data?.state
  useEffect(() => {
    if (!workspaceState || ACTIVE_WORKSPACE_STATES.has(workspaceState)) return
    clearTrackingTimeout()
    setTrackingJob(false)
    void (async () => {
      for (const key of ['context-doc-current', 'context-docs']) {
        await queryClient.cancelQueries({ queryKey: ['project', projectId, key] })
        await queryClient.invalidateQueries({ queryKey: ['project', projectId, key] })
      }
    })()
  }, [workspaceState, projectId, queryClient])
  const current = useQuery({
    queryKey: ['project', projectId, 'context-doc-current', kind],
    queryFn: () => getCurrentContextDoc(projectId, kind),
    retry: false,
  })
  const history = usePaginated<ContextDoc>(
    ['project', projectId, 'context-docs', kind],
    (cursor) => listContextDocs(projectId, kind, cursor),
    'documents',
  )

  useEffect(() => {
    setSelected(null)
    setEditing(false)
    setSaveError(null)
  }, [kind])

  useEffect(() => {
    if (!editing) setSelected(null)
  }, [current.data?.id, editing])

  const currentMissing = current.error instanceof ApiError && current.error.status === 404
  const display = current.data && selected?.id === current.data.id ? current.data : selected || current.data
  const isCurrent = Boolean(display && current.data && display.id === current.data.id)
  const versions = useMemo(() => {
    const items = [...history.items]
    const currentDoc = current.data
    if (currentDoc && !items.some((item) => item.id === currentDoc.id)) items.unshift(currentDoc)
    return items
  }, [history.items, current.data])

  const invalidateDocs = async () => {
    await queryClient.invalidateQueries({ queryKey: ['project', projectId, 'context-doc-current', kind] })
    await queryClient.invalidateQueries({ queryKey: ['project', projectId, 'context-docs', kind] })
  }

  const refresh = async (action: RefreshAction) => {
    if (!canWrite) return
    setRefreshAction(action)
    setRefreshError(null)
    try {
      const response = await refreshWorkspace(projectId, {
        reindex: action === 'reindex' || undefined,
        regenerate_context: action === 'regenerate' || undefined,
      })
      toast.push('success', response.job_id ? `已创建任务 ${response.job_id}。` : '已提交工作区任务。')
      startTracking()
      await queryClient.invalidateQueries({ queryKey: ['project', projectId, 'workspace'] })
      await queryClient.invalidateQueries({ queryKey: ['project', projectId, 'context-docs'] })
    } catch (error) {
      setRefreshError(error)
    } finally {
      setRefreshAction(null)
    }
  }

  const startEdit = () => {
    if (!display || !isCurrent) return
    setDraft(display.content)
    setEditing(true)
    setSaveError(null)
  }

  const save = async () => {
    if (!canWrite || !display) return
    if (!draft.trim()) {
      setSaveError(new Error('内容不能为空。'))
      return
    }
    setSaving(true)
    setSaveError(null)
    try {
      await saveCurrentContextDoc(projectId, kind, draft)
      setEditing(false)
      setSelected(null)
      toast.push('success', '已生成一条人工上下文版本。')
      await invalidateDocs()
    } catch (error) {
      setSaveError(error)
    } finally {
      setSaving(false)
    }
  }

  const toggleLock = async (locked: boolean) => {
    if (!canWrite || !current.data) return
    setLockPending(true)
    try {
      await setContextDocLock(projectId, kind, locked)
      toast.push('success', locked ? '已锁定当前上下文文档。' : '已解除锁定。')
      await invalidateDocs()
    } catch (error) {
      toast.push('error', `锁定状态更新失败：${errorMessage(error)}`)
    } finally {
      setLockPending(false)
    }
  }

  const ws = workspace.data

  return (
    <div className={ui.pageStack}>
      <Panel flush>
        {workspace.isPending ? <div className={ui.panelBody}><LoadingState label="正在读取工作区…" /></div> : null}
        {workspace.isError ? <div className={ui.panelBody}><ErrorState error={workspace.error} onRetry={() => { void workspace.refetch() }} title="工作区状态加载失败" /></div> : null}
        {ws ? (
          <>
            <div className={page.statusTiles} role="group" aria-label="工作区状态">
              <div>
                <span className={page.tileLabel}>工作区</span>
                {ws.state ? <StatusBadge status={ws.state} label={labelForStatus(ws.state)} /> : <span className="muted">不可用</span>}
              </div>
              <div>
                <span className={page.tileLabel}>base sha</span>
                <code className={page.tileValue}>{ws.base_sha ? shortSha(ws.base_sha) : '—'}</code>
              </div>
              <div>
                <span className={page.tileLabel}>索引状态</span>
                {ws.index_status ? <Badge plain>{ws.index_status}</Badge> : <span className="muted">—</span>}
              </div>
              <div>
                <span className={page.tileLabel}>最近刷新</span>
                <span>{ws.last_refresh_at ? formatDate(ws.last_refresh_at) : '—'}</span>
              </div>
            </div>
            <div className={page.tileMeta}>
              <span>索引 sha <code>{ws.index_sha ? shortSha(ws.index_sha) : '—'}</code></span>
              <span>上下文 sha <code>{ws.context_sha ? shortSha(ws.context_sha) : '—'}</code></span>
            </div>
            {ws.last_error ? <div className={`${styles.warning} ${page.tileAlert}`} role="alert">最近错误：{ws.last_error}</div> : null}
          </>
        ) : null}
        <div className={page.tileFooter}>
          {canWrite ? (
            <>
              <Button variant="secondary" className={ui.smallButton} onClick={() => { void refresh('refresh') }} loading={refreshAction === 'refresh'} disabled={Boolean(refreshAction)}>刷新工作区</Button>
              <Button variant="secondary" className={ui.smallButton} onClick={() => { void refresh('reindex') }} loading={refreshAction === 'reindex'} disabled={Boolean(refreshAction)}>重建索引</Button>
              <Button variant="secondary" className={ui.smallButton} onClick={() => { void refresh('regenerate') }} loading={refreshAction === 'regenerate'} disabled={Boolean(refreshAction)}>重新生成上下文</Button>
              <span className={page.hint}>重新生成上下文会调用模型，可能产生费用。</span>
            </>
          ) : <span className="small muted">只读账号可以查看工作区状态，不能触发刷新或修改上下文。</span>}
        </div>
        {refreshError ? <div className={ui.panelBody}><ErrorState error={refreshError} title="工作区任务提交失败" /></div> : null}
      </Panel>

      <Panel flush title="上下文文档" aside={history.isSuccess ? `${versions.length}${history.hasNextPage ? '+' : ''} 个版本` : undefined}>
        <div className={page.docLayout}>
          <nav className={page.docList} aria-label="上下文版本历史">
            <div className={page.docListHeader}>
              <Segmented
                label="上下文文档类型"
                options={docKinds.map((item) => ({ value: item.key, label: item.label }))}
                value={kind}
                onChange={(next) => setKind(next as DocKind)}
              />
            </div>
            {history.isPending ? <div className={page.docListFooter}><LoadingState label="正在读取版本…" /></div> : null}
            {history.isError ? <div className={page.docListFooter}><ErrorState error={history.error} onRetry={history.retry} /></div> : null}
            {history.isSuccess && versions.length === 0 ? <div className={page.docListFooter}><EmptyState title="没有历史版本" /></div> : null}
            {versions.map((doc) => (
              <button
                key={doc.id}
                type="button"
                className={[page.docItem, display?.id === doc.id ? page.docItemActive : ''].filter(Boolean).join(' ')}
                aria-current={display?.id === doc.id ? 'true' : undefined}
                onClick={() => {
                  setSelected(doc)
                  setEditing(false)
                  setSaveError(null)
                }}
              >
                <span className={page.docItemTop}>
                  <code>{shortSha(doc.base_sha)}</code>
                  <span>{labelForSource(doc.source)}</span>
                  {doc.locked ? <span>锁定</span> : null}
                </span>
                <span className={styles.tableMeta}>
                  {doc.created_at ? formatDate(doc.created_at) : '时间不可用'}{doc.id === current.data?.id ? ' · 当前' : ''}
                </span>
              </button>
            ))}
            {history.hasNextPage ? (
              <div className={page.docListFooter}>
                <Button variant="secondary" className={ui.smallButton} onClick={history.fetchNextPage} loading={history.isFetchingNextPage}>加载更多版本</Button>
              </div>
            ) : null}
          </nav>

          <div className={page.docBody}>
            {current.isPending ? <LoadingState label="正在读取当前文档…" /> : null}
            {current.isError && !currentMissing ? <ErrorState error={current.error} onRetry={() => { void current.refetch() }} title="当前上下文加载失败" /> : null}
            {currentMissing && !display ? (
              <EmptyState title="当前分支没有该类型上下文文档" description="服务端没有返回当前文档；可在工作区刷新或生成上下文后再查看。" />
            ) : null}
            {display ? (
              <>
                <div className={page.docBodyHeader}>
                  <div className={page.docMeta}>
                    <span>版本 <code>{display.id}</code></span>
                    <Badge plain>{labelForSource(display.source)}</Badge>
                    <span>base <code>{display.base_sha ? shortSha(display.base_sha) : '—'}</code></span>
                    <span>{display.created_at ? formatDate(display.created_at) : '时间不可用'}</span>
                    {isCurrent ? <span>当前生效</span> : <span>历史版本（只读）</span>}
                    {display.locked ? <span>已锁定</span> : <span>未锁定</span>}
                  </div>
                  {canWrite && isCurrent ? (
                    <div className={page.docControls}>
                      <span className="small">锁定（刷新时保留）</span>
                      <Switch checked={Boolean(display.locked)} onChange={(next) => { void toggleLock(next) }} label="锁定当前上下文文档" disabled={lockPending} />
                      {editing ? (
                        <>
                          <Button variant="secondary" className={ui.smallButton} onClick={() => { setEditing(false); setSaveError(null) }} disabled={saving}>取消</Button>
                          <Button variant="primary" className={ui.smallButton} onClick={() => { void save() }} loading={saving}>保存为新版本</Button>
                        </>
                      ) : <Button variant="secondary" className={ui.smallButton} onClick={startEdit}>编辑</Button>}
                    </div>
                  ) : null}
                </div>
                {editing ? (
                  <>
                    <label className="srOnly" htmlFor="context-doc-editor">编辑上下文文档</label>
                    <textarea
                      id="context-doc-editor"
                      className={`textarea ${page.docTextarea}`}
                      value={draft}
                      onChange={(event) => setDraft(event.target.value)}
                      spellCheck={false}
                    />
                    {saveError ? <ErrorState error={saveError} title="保存失败" /> : null}
                  </>
                ) : <pre className={`pre ${page.docPreview}`}>{display.content}</pre>}
              </>
            ) : null}
          </div>
        </div>
      </Panel>
    </div>
  )
}
