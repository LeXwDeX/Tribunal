import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { disableWebhook, enableWebhook, rebuildWebhook } from '../../api/api'
import type { Project, ProjectWebhookState } from '../../api/types'
import { Badge, Button, ConfirmDialog, ErrorState, Panel, useToast } from '../../components/ui'
import { formatDate, labelForWebhookState, webhookTone } from '../../lib/format'
import ui from '../../styles/ui.module.css'
import styles from '../../pages/pages.module.css'

type WebhookAction = 'enable' | 'rebuild' | 'disable'

const WEBHOOK_EVENT_LABELS: Record<string, string> = {
  push_events: 'Push',
  merge_requests_events: 'Merge request',
  note_events: 'Note',
}

export function webhookEventLabel(event: string): string {
  return WEBHOOK_EVENT_LABELS[event] || event
}

export function webhookManagedByLabel(managedBy?: string, hookID?: number): string {
  if (managedBy === 'crc') return '本服务'
  if (hookID) return '待确认'
  return '未配置'
}

const PENDING_LABELS: Record<WebhookAction, string> = {
  enable: '正在启用 Webhook…',
  rebuild: '正在重建 Webhook…',
  disable: '正在禁用 Webhook…',
}

const SUCCESS_LABELS: Record<WebhookAction, string> = {
  enable: 'Webhook 已启用',
  rebuild: 'Webhook 已重建',
  disable: 'Webhook 已禁用',
}

interface WebhookView {
  state: string
  hookId?: number
  events: string[]
  error: string
  managedBy?: string
  updatedAt?: string
}

function viewFromProject(project: Project): WebhookView {
  return {
    state: project.webhook_state || (project.hook_id ? 'unknown' : 'disabled'),
    hookId: project.hook_id || undefined,
    events: project.webhook_events || [],
    error: project.webhook_error || '',
    managedBy: project.webhook_managed_by,
    updatedAt: project.webhook_updated_at,
  }
}

function viewFromResponse(response: ProjectWebhookState): WebhookView {
  return {
    state: response.state || 'unknown',
    hookId: response.hook_id || undefined,
    events: response.events || [],
    error: response.last_error || '',
    managedBy: response.managed_by,
    updatedAt: response.updated_at,
  }
}

function isNewer(candidate?: string, reference?: string): boolean {
  if (!candidate) return false
  if (!reference) return true
  return new Date(candidate).getTime() > new Date(reference).getTime()
}

// WebhookPanel owns the explicit enable / rebuild / disable flow. Its state is
// independent from the project settings form: an action refreshes only the
// project queries and never touches unsaved settings.
export function WebhookPanel({ project }: { project: Project }) {
  const canWrite = project.access === 'manage'
  const toast = useToast()
  const queryClient = useQueryClient()
  const requestRef = useRef(0)
  const [pending, setPending] = useState<WebhookAction | null>(null)
  const [done, setDone] = useState<WebhookAction | null>(null)
  const [latest, setLatest] = useState<WebhookView | null>(null)
  const [enableError, setEnableError] = useState<unknown>(null)
  const [dialog, setDialog] = useState<'rebuild' | 'disable' | null>(null)
  const [dialogError, setDialogError] = useState<unknown>(null)

  useEffect(() => {
    requestRef.current += 1
    setPending(null)
    setDone(null)
    setLatest(null)
    setEnableError(null)
    setDialog(null)
    setDialogError(null)
  }, [project.id])

  const stored = viewFromProject(project)
  // The latest action response is server truth until a newer project read arrives.
  const view = latest && !isNewer(stored.updatedAt, latest.updatedAt) ? latest : stored

  const run = async (action: WebhookAction) => {
    if (!canWrite || pending) return
    const requestId = requestRef.current + 1
    requestRef.current = requestId
    setPending(action)
    setDone(null)
    if (action === 'enable') setEnableError(null)
    else setDialogError(null)
    try {
      const call = action === 'enable' ? enableWebhook : action === 'rebuild' ? rebuildWebhook : disableWebhook
      const response = await call(project.id)
      if (requestRef.current !== requestId) return
      if (response) setLatest(viewFromResponse(response))
      setDone(action)
      setDialog(null)
      const hookId = response?.hook_id
      toast.push('success', action === 'disable'
        ? 'GitLab webhook 已禁用；未删除手动或外部 hook。'
        : `GitLab webhook 已${action === 'enable' ? '启用' : '重建'}${hookId ? `（#${hookId}）` : ''}。`)
      await queryClient.invalidateQueries({ queryKey: ['project', project.id] })
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
    } catch (error) {
      if (requestRef.current !== requestId) return
      if (action === 'enable') setEnableError(error)
      else setDialogError(error)
    } finally {
      if (requestRef.current === requestId) setPending(null)
    }
  }

  const openDialog = (next: 'rebuild' | 'disable') => {
    setDialogError(null)
    setDialog(next)
  }

  // Closing only discards this dialog's error; a completed action stays applied.
  const closeDialog = () => {
    if (pending === 'rebuild' || pending === 'disable') return
    setDialog(null)
    setDialogError(null)
  }

  const busy = pending !== null
  const enabled = view.state === 'enabled'

  return (
    <Panel title="GitLab Webhook" aside={<Badge tone={webhookTone(view.state)}>{labelForWebhookState(view.state)}</Badge>}>
      <div className={ui.stack}>
        <span className="small muted">
          托管方：{webhookManagedByLabel(view.managedBy, view.hookId)}
          {view.hookId ? ` · hook #${view.hookId}` : ''}
        </span>
        <span className="small muted">
          事件：{view.events.length ? view.events.map(webhookEventLabel).join(' · ') : '未记录'}
        </span>
        {view.updatedAt ? <span className="small muted">最近更新：{formatDate(view.updatedAt)}</span> : null}
        {view.error ? <span className={styles.resultError}>{view.error}</span> : null}

        {canWrite ? (
          <>
            {pending ? <span className="small" role="status">{PENDING_LABELS[pending]}</span> : null}
            {!pending && done ? <span role="status"><Badge tone="ok">{SUCCESS_LABELS[done]}</Badge></span> : null}
            <div className={ui.row}>
              {enabled ? (
                <>
                  <Button variant="secondary" onClick={() => openDialog('rebuild')} disabled={busy}>重建 webhook</Button>
                  <Button variant="danger" onClick={() => openDialog('disable')} disabled={busy}>禁用 webhook</Button>
                </>
              ) : (
                <Button
                  variant="primary"
                  onClick={() => { void run('enable') }}
                  loading={pending === 'enable'}
                  disabled={busy || view.state === 'pending'}
                >
                  {view.state === 'failed' ? '重试启用 GitLab Activity' : '启用 GitLab Activity'}
                </Button>
              )}
              {!enabled && view.hookId ? (
                <Button variant="secondary" onClick={() => openDialog('rebuild')} disabled={busy}>重建 webhook</Button>
              ) : null}
            </div>
            {enableError ? <ErrorState error={enableError} title="启用 Webhook 失败" /> : null}
            <span className="small muted">
              启用会在 GitLab 创建或复用本服务记录的 hook；同 URL 的手动 hook 不会被接管或删除。保存项目设置、校验或更新访问 Key 都不会修改 Webhook。
            </span>
          </>
        ) : null}
      </div>

      <ConfirmDialog
        open={dialog === 'rebuild'}
        onClose={closeDialog}
        onConfirm={() => { void run('rebuild') }}
        title="重建 GitLab webhook？"
        description="服务端会轮换 webhook secret，并优先更新本服务记录的 hook；记录不存在时才重新创建。手动或外部 hook 不受影响。"
        confirmLabel="重建 webhook"
        danger={false}
        pending={pending === 'rebuild'}
        error={dialog === 'rebuild' ? dialogError : null}
      />

      <ConfirmDialog
        open={dialog === 'disable'}
        onClose={closeDialog}
        onConfirm={() => { void run('disable') }}
        title="禁用 GitLab webhook？"
        description="只删除本服务创建并记录的 hook；手动或外部 hook 不受影响。项目与历史评审保留，可稍后重新启用。"
        confirmLabel="禁用 webhook"
        danger
        pending={pending === 'disable'}
        error={dialog === 'disable' ? dialogError : null}
      />
    </Panel>
  )
}
