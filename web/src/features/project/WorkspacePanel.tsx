import { useEffect, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  getCurrentContextDoc, getWorkspace, listContextDocs, refreshWorkspace, saveCurrentContextDoc, setContextDocLock,
} from '../../api/api'
import { ApiError, errorMessage } from '../../api/client'
import type { ContextDoc } from '../../api/types'
import {
  Button, EmptyState, ErrorState, LoadingState, Panel, StatusBadge, Switch, useToast,
} from '../../components/ui'
import { usePaginated } from '../../lib/hooks'
import { formatDate, labelForSource, labelForStatus, shortSha } from '../../lib/format'
import ui from '../../styles/ui.module.css'
import styles from '../../pages/pages.module.css'

type DocKind = 'agents' | 'claude'
type RefreshAction = 'refresh' | 'reindex' | 'regenerate'

const docKinds: Array<{ key: DocKind; label: string }> = [
  { key: 'agents', label: 'AGENTS' },
  { key: 'claude', label: 'CLAUDE' },
]

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

  const workspace = useQuery({
    queryKey: ['project', projectId, 'workspace'],
    queryFn: () => getWorkspace(projectId),
  })
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
  const display = selected || current.data
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
      await saveCurrentContextDoc(projectId, kind, draft, Boolean(display.locked))
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

  return (
    <div className={styles.split}>
      <div className={styles.mainColumn}>
        <Panel title="工作区状态" aside={workspace.data?.state ? <StatusBadge status={workspace.data.state} /> : undefined}>
          {workspace.isPending ? <LoadingState label="正在读取工作区…" /> : null}
          {workspace.isError ? <ErrorState error={workspace.error} onRetry={() => { void workspace.refetch() }} title="工作区状态加载失败" /> : null}
          {workspace.data ? (
            <>
              <dl className={ui.definitionGrid}>
                <div><dt>状态</dt><dd>{workspace.data.state ? labelForStatus(workspace.data.state) : '不可用'}</dd></div>
                <div><dt>base sha</dt><dd className="mono">{workspace.data.base_sha ? shortSha(workspace.data.base_sha) : '—'}</dd></div>
                <div><dt>索引 sha</dt><dd className="mono">{workspace.data.index_sha ? shortSha(workspace.data.index_sha) : '—'}</dd></div>
                <div><dt>上下文 sha</dt><dd className="mono">{workspace.data.context_sha ? shortSha(workspace.data.context_sha) : '—'}</dd></div>
                <div><dt>最近刷新</dt><dd>{workspace.data.last_refresh_at ? formatDate(workspace.data.last_refresh_at) : '—'}</dd></div>
                <div><dt>索引状态</dt><dd>{workspace.data.index_status || '—'}</dd></div>
              </dl>
              {workspace.data.last_error ? <div className={styles.warning} role="alert">最近错误：{workspace.data.last_error}</div> : null}
            </>
          ) : null}
          {canWrite ? (
            <div className={styles.workspaceActions}>
              <Button variant="secondary" onClick={() => { void refresh('refresh') }} loading={refreshAction === 'refresh'} disabled={Boolean(refreshAction)}>刷新工作区</Button>
              <Button variant="secondary" onClick={() => { void refresh('reindex') }} loading={refreshAction === 'reindex'} disabled={Boolean(refreshAction)}>重建索引</Button>
              <Button variant="secondary" onClick={() => { void refresh('regenerate') }} loading={refreshAction === 'regenerate'} disabled={Boolean(refreshAction)}>重新生成上下文</Button>
            </div>
          ) : <span className="small muted">只读账号可以查看工作区状态，不能触发刷新或修改上下文。</span>}
          {refreshError ? <ErrorState error={refreshError} title="工作区任务提交失败" /> : null}
        </Panel>

        <Panel
          title="上下文文档"
          aside={
            <div className={styles.filterChips} role="group" aria-label="上下文文档类型">
              {docKinds.map((item) => (
                <button
                  key={item.key}
                  type="button"
                  className={[styles.chip, kind === item.key ? styles.chipActive : ''].filter(Boolean).join(' ')}
                  aria-pressed={kind === item.key}
                  onClick={() => setKind(item.key)}
                >
                  {item.label}
                </button>
              ))}
            </div>
          }
        >
          {current.isPending ? <LoadingState label="正在读取当前文档…" /> : null}
          {current.isError && !currentMissing ? <ErrorState error={current.error} onRetry={() => { void current.refetch() }} title="当前上下文加载失败" /> : null}
          {currentMissing && !display ? (
            <EmptyState title="当前分支没有该类型上下文文档" description="服务端没有返回当前文档；可在工作区刷新或生成上下文后再查看。" />
          ) : null}
          {display ? (
            <>
              <div className={styles.docHeader}>
                <div className={styles.docMeta}>
                  <span>版本 <code>{display.id}</code></span>
                  <span>来源 {labelForSource(display.source)}</span>
                  <span>base <code>{display.base_sha ? shortSha(display.base_sha) : '—'}</code></span>
                  <span>{display.created_at ? formatDate(display.created_at) : '时间不可用'}</span>
                  {isCurrent ? <span>当前生效</span> : <span>历史版本（只读）</span>}
                  {display.locked ? <span>已锁定</span> : <span>未锁定</span>}
                </div>
                {canWrite && isCurrent ? (
                  <div className={styles.docHeader} style={{ gap: 8 }}>
                    <span className="small">锁定（刷新时保留）</span>
                    <Switch checked={Boolean(display.locked)} onChange={(next) => { void toggleLock(next) }} label="锁定当前上下文文档" disabled={lockPending} />
                  </div>
                ) : null}
              </div>
              {editing ? (
                <>
                  <label className="srOnly" htmlFor="context-doc-editor">编辑上下文文档</label>
                  <textarea
                    id="context-doc-editor"
                    className={`textarea ${styles.docEditor}`}
                    value={draft}
                    onChange={(event) => setDraft(event.target.value)}
                    spellCheck={false}
                  />
                  {saveError ? <ErrorState error={saveError} title="保存失败" /> : null}
                  <div className={styles.docActions}>
                    <Button variant="secondary" onClick={() => { setEditing(false); setSaveError(null) }} disabled={saving}>取消</Button>
                    <Button variant="primary" onClick={() => { void save() }} loading={saving}>保存为新版本</Button>
                  </div>
                </>
              ) : (
                <>
                  <pre className="pre" style={docPreStyle}>{display.content}</pre>
                  {canWrite && isCurrent ? (
                    <div className={styles.docActions}>
                      <Button variant="secondary" onClick={startEdit}>编辑</Button>
                    </div>
                  ) : null}
                </>
              )}
            </>
          ) : null}
        </Panel>
      </div>

      <aside className={styles.sideColumn} aria-label="上下文版本历史">
        <Panel title="版本历史" aside={history.isSuccess ? `${versions.length}${history.hasNextPage ? '+' : ''} 个版本` : undefined}>
          {history.isPending ? <LoadingState label="正在读取版本…" /> : null}
          {history.isError ? <ErrorState error={history.error} onRetry={history.retry} /> : null}
          {history.isSuccess && versions.length === 0 ? <EmptyState title="没有历史版本" /> : null}
          <div className={styles.list}>
            {versions.map((doc) => (
              <button
                key={doc.id}
                type="button"
                className={[styles.historyItem, display?.id === doc.id ? styles.historyItemActive : ''].filter(Boolean).join(' ')}
                onClick={() => {
                  setSelected(doc)
                  setEditing(false)
                  setSaveError(null)
                }}
              >
                <span className={styles.historyTop}>
                  <code>{shortSha(doc.base_sha)}</code>
                  <span>{labelForSource(doc.source)}</span>
                  {doc.locked ? <span>锁定</span> : null}
                </span>
                <span className={styles.tableMeta}>
                  {doc.created_at ? formatDate(doc.created_at) : '时间不可用'}{doc.id === current.data?.id ? ' · 当前' : ''}
                </span>
              </button>
            ))}
          </div>
          {history.hasNextPage ? (
            <Button variant="secondary" onClick={history.fetchNextPage} loading={history.isFetchingNextPage}>加载更多版本</Button>
          ) : null}
        </Panel>
      </aside>
    </div>
  )
}

const docPreStyle = {
  margin: 0,
  padding: 14,
  background: 'var(--preview-bg)',
  border: '1px solid var(--border)',
  borderRadius: 8,
  color: 'var(--text)',
  maxHeight: 480,
  overflow: 'auto',
} as const
