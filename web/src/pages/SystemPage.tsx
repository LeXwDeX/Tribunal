import { useEffect, useId, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  createApiToken, deleteAdminGrant, deleteAdminUserGrant, fetchMe, getSettings, listAdminGrants,
  listAdminProjects, listAdminUserGrants, listAdminUsers, listApiTokens, listProjects, listWebhookDeliveries,
  renewApiToken, revokeAdminUserCredentials, revokeApiToken, setAdminGrant, setAdminUserDisabled, setAdminUserGrant,
  updateAdminUser,
} from '../api/api'
import { ApiError } from '../api/client'
import type {
  AdminProject, AdminUser, ApiToken, CreatedApiToken, Project, ProjectAccess, ProjectGrant, WebhookDeliverySummary,
} from '../api/types'
import { useCanSelfService, useCanWrite } from '../components/RoleContext'
import {
  Badge, Button, ConfirmDialog, DataTable, Dialog, Drawer, EmptyState, ErrorState, Field, LoadingState, NumberField,
  PageHeader, Pagination, Panel, PanelBody, SecretField, Select, TableAction, TableActions, Tabs, Tooltip, classNames, tableCell,
  useToast,
} from '../components/ui'
import { usePaginated } from '../lib/hooks'
import { formatDate, formatNumber, formatRelative } from '../lib/format'
import { ConfigSourcesPanel } from './ConfigSourcesPanel'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'
import insights from './insights.module.css'
import drawer from './userDrawer.module.css'

// WebhookDeliveriesPanel is the read-only "why was this MR not reviewed" view.
// It shows only the fixed reasons and counts the API returns; the upstream
// request body and the webhook secret are never available here.
function WebhookDeliveriesPanel() {
  const deliveries = usePaginated<WebhookDeliverySummary>(
    ['webhook-deliveries'],
    (cursor) => listWebhookDeliveries({}, cursor),
    'deliveries',
  )
  const projects = useQuery({
    queryKey: ['projects', 'webhook-deliveries'],
    queryFn: () => listProjects({ limit: 200 }),
    retry: false,
  })
  const projectNames = new Map((projects.data?.projects || []).map((item: Project) => [item.id, item.path_with_namespace]))
  const items = deliveries.items
  return (
    <Panel
      flush
      title="Webhook 投递"
      aside={deliveries.isSuccess ? `${items.length}${deliveries.hasNextPage ? '+' : ''} 个 MR` : undefined}
      note="按项目与 MR 聚合最近保留期内的投递。忽略原因来自固定类别；rejected/invalid_secret 表示请求未通过密钥校验，不会把项目标红。只读。"
    >
      {deliveries.isPending ? <PanelBody><LoadingState label="正在读取 Webhook 投递…" /></PanelBody> : null}
      {deliveries.isError ? (
        <PanelBody>
          <ErrorState error={deliveries.error} onRetry={deliveries.retry} title="Webhook 投递加载失败" />
        </PanelBody>
      ) : null}
      {deliveries.isSuccess && !items.length ? (
        <PanelBody>
          <EmptyState title="没有 Webhook 投递" description="保留期内没有记录到投递；旧版服务端也可能没有该只读接口。" />
        </PanelBody>
      ) : null}
      {items.length ? (
        <DataTable label="Webhook 投递" minWidth="xl">
            <thead>
              <tr>
                <th scope="col">项目</th>
                <th scope="col">MR</th>
                <th scope="col">最后投递</th>
                <th scope="col" className={tableCell.num}>投递</th>
                <th scope="col" className={tableCell.num}>入队</th>
                <th scope="col" className={tableCell.num}>忽略</th>
                <th scope="col" className={tableCell.num}>拒绝</th>
                <th scope="col" className={tableCell.num}>无效密钥</th>
                <th scope="col">忽略原因</th>
              </tr>
            </thead>
            <tbody>
              {items.map((row) => {
                const reasons = Object.entries(row.reasons || {}).sort(([a], [b]) => a.localeCompare(b))
                return (
                  <tr key={`${row.project_id}-${row.mr_iid}`}>
                    <td className={tableCell.mono}>{projectNames.get(row.project_id) || `项目 ${row.project_id}`}</td>
                    <td className={classNames(tableCell.mono, tableCell.nowrap)}>{row.mr_iid > 0 ? `!${row.mr_iid}` : '未知 MR'}</td>
                    <td className={tableCell.nowrap}>{formatRelative(row.last_received_at)}</td>
                    <td className={tableCell.num}>{formatNumber(row.deliveries)}</td>
                    <td className={tableCell.num}>{formatNumber(row.enqueued)}</td>
                    <td className={tableCell.num}>{formatNumber(row.ignored)}</td>
                    <td className={tableCell.num}>{formatNumber(row.rejected)}</td>
                    <td className={tableCell.num}>{formatNumber(row.rejected_invalid_secret)}</td>
                    <td>
                      {reasons.length ? (
                        <span className={styles.budgetRow}>
                          {reasons.map(([reason, count]) => <Badge key={reason} plain tone="warn">{reason}: {count}</Badge>)}
                        </span>
                      ) : (
                        <span className="small muted">无</span>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
        </DataTable>
      ) : null}
      {deliveries.hasNextPage ? (
        <PanelBody>
          <Button
            variant="secondary"
            onClick={deliveries.fetchNextPage}
            loading={deliveries.isFetchingNextPage}
          >
            {deliveries.isFetchNextPageError ? '重试加载更多投递' : '加载更多投递'}
          </Button>
        </PanelBody>
      ) : null}
    </Panel>
  )
}

function tokenLabel(token: ApiToken): string {
  return token.name || `token ${token.id}`
}

function TokenCreateDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const nameId = useId()
  const expiresId = useId()
  const [name, setName] = useState('')
  const [days, setDays] = useState('90')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [created, setCreated] = useState<CreatedApiToken | null>(null)
  const [fieldError, setFieldError] = useState('')
  const [notice, setNotice] = useState('')
  const activeRequest = useRef<{ controller: AbortController; abandoned: boolean; stopped: boolean } | null>(null)
  const mounted = useRef(false)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      if (activeRequest.current) {
        activeRequest.current.abandoned = true
        activeRequest.current.controller.abort()
        activeRequest.current = null
      }
    }
  }, [])

  useEffect(() => {
    if (!open) {
      setName('')
      setDays('90')
      setError(null)
      setCreated(null)
      setFieldError('')
    }
  }, [open])

  const close = () => {
    if (activeRequest.current) {
      activeRequest.current.abandoned = true
      setNotice('关闭的请求可能已创建 token，明文无法再次查看。请检查 token 列表，撤销不需要的 token 后再决定是否重试。')
      toast.push('info', '请求仍在进行；关闭后将无法查看明文。请检查 token 列表并撤销不需要的 token。')
    }
    onClose()
  }

  const stopWaiting = () => {
    const request = activeRequest.current
    if (!request) return
    request.abandoned = true
    request.stopped = true
    activeRequest.current = null
    request.controller.abort()
    setPending(false)
    setNotice('已停止等待，但 token 可能已在服务端创建。请刷新并检查 token 列表，撤销不需要的 token 后再决定是否重试。')
    void queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
  }

  const submit = async () => {
    if (activeRequest.current) return
    const parsedDays = Number(days)
    if (!name.trim()) { setFieldError('请输入 token 名称。'); return }
    if (!Number.isInteger(parsedDays) || parsedDays <= 0 || parsedDays > 365) {
      setFieldError('有效天数必须在 1 到 365 之间。')
      return
    }
    const request = { controller: new AbortController(), abandoned: false, stopped: false }
    activeRequest.current = request
    setFieldError('')
    setPending(true)
    setError(null)
    setNotice('')
    try {
      const result = await createApiToken({ name: name.trim(), expires_in_days: parsedDays }, request.controller.signal)
      if (mounted.current && !request.abandoned && activeRequest.current === request) {
        setCreated(result)
        toast.push('success', 'API token 已创建，请立即复制。')
      } else if (mounted.current) {
        toast.push('info', 'API token 已创建，但明文无法再次查看。请在列表中撤销该 token 并重新创建。')
      }
      void queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
    } catch (caught) {
      if (!request.stopped) {
        if (mounted.current && !request.abandoned && activeRequest.current === request) {
          setError(caught instanceof ApiError && caught.status === 0 ? new Error('无法连接服务器。') : caught)
          setNotice('创建结果未知，token 可能已在服务端创建。请刷新并检查 token 列表，撤销不需要的 token 后再决定是否重试。')
        } else if (mounted.current) {
          toast.push('info', '创建结果未知，token 可能已在服务端创建。请刷新并检查 token 列表。')
        }
        void queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
      }
    } finally {
      if (activeRequest.current === request) {
        activeRequest.current = null
        if (mounted.current) setPending(false)
      }
    }
  }

  return (
    <Dialog
      open={open}
      onClose={close}
      title={created ? '复制新 API token' : '创建 API token'}
      description={created
        ? '明文 token 只显示这一次。关闭对话框后将从界面状态中清除。'
        : 'API token 供 tribunalctl 或脚本访问项目级 API。设置名称和有效期。'}
      footer={created ? (
        <Button variant="primary" onClick={close}>我已复制，关闭</Button>
      ) : (
        <>
          <Button variant="secondary" onClick={close}>{pending ? '关闭' : '取消'}</Button>
          {pending ? <Button variant="secondary" onClick={stopWaiting}>停止等待</Button> : null}
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>创建</Button>
        </>
      )}
    >
      {created ? (
        <>
          <SecretField label="API token" value={created.token} />
          <div className={styles.notice}>关闭后不会再次显示明文；如遗失，请撤销并创建新 token。</div>
        </>
      ) : (
        <>
          {error ? <ErrorState error={error} title="创建失败或结果未知" /> : null}
          {notice ? <div className={styles.warning} role={error ? 'status' : 'alert'}>{notice}</div> : null}
          {pending ? <div className={styles.notice} role="status">正在创建。关闭对话框不会取消请求，关闭后无法查看明文；如需停止等待，请先检查 token 列表再决定是否重试。</div> : null}
          <div className="formGrid">
            <Field label="名称" htmlFor={nameId} error={fieldError}>
              <input id={nameId} className="input" value={name} onChange={(event) => setName(event.target.value)} placeholder="例如 ci-tribunalctl" aria-invalid={Boolean(fieldError)} disabled={pending} />
            </Field>
            <Field label="有效天数" htmlFor={expiresId} hint="正整数，最长 365 天。">
              <NumberField id={expiresId} min={1} max={365} value={days} onChange={(event) => setDays(event.target.value)} disabled={pending} />
            </Field>
          </div>
        </>
      )}
    </Dialog>
  )
}

function userLabel(user: AdminUser): string {
  return user.name ? `${user.name} (@${user.username})` : `@${user.username}`
}

const TOKEN_EXPIRY_WARNING_DAYS = 14

// tokenExpiry surfaces the two states an operator must act on: already
// expired and within the two-week warning window.
function tokenExpiry(token: ApiToken): { label: string; tone: 'danger' | 'warn' } | null {
  if (!token.expires_at) return null
  const expires = new Date(token.expires_at).getTime()
  if (Number.isNaN(expires)) return null
  const days = Math.ceil((expires - Date.now()) / 86_400_000)
  if (days <= 0) return { label: '已过期', tone: 'danger' }
  if (days <= TOKEN_EXPIRY_WARNING_DAYS) return { label: `${days} 天后过期`, tone: 'warn' }
  return null
}

// A renewal replaces a credential: an unbounded wait would leave the dialog
// frozen with no way to act, so the client abandons the request after this
// budget and says that the outcome is unknown.
export const RENEW_TIMEOUT_MS = 30_000
const RENEW_TIMEOUT_SECONDS = RENEW_TIMEOUT_MS / 1000

function TokenRenewDialog({ token, open, onClose }: {
  token: ApiToken | null
  open: boolean
  onClose: () => void
}) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const [days, setDays] = useState('90')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [notice, setNotice] = useState('')
  const [renewed, setRenewed] = useState<CreatedApiToken>()
  const activeRequest = useRef<{ controller: AbortController; timedOut: boolean } | null>(null)
  const id = useId()

  useEffect(() => {
    // Clearing on close as well keeps the one-time plaintext out of state once
    // the dialog is gone, matching the "cleared from UI state" copy.
    setDays('90')
    setPending(false)
    setError(undefined)
    setNotice('')
    setRenewed(undefined)
  }, [open, token?.id])

  const submit = async () => {
    if (!token || activeRequest.current) return
    const parsed = Number(days)
    if (!Number.isInteger(parsed) || parsed < 1 || parsed > 365) {
      setError(new Error('有效天数必须是 1 到 365 的整数。'))
      return
    }
    const request = { controller: new AbortController(), timedOut: false }
    activeRequest.current = request
    setPending(true)
    setError(undefined)
    setNotice('')
    const timer = window.setTimeout(() => {
      request.timedOut = true
      request.controller.abort()
    }, RENEW_TIMEOUT_MS)
    try {
      setRenewed(await renewApiToken(token.id, parsed, request.controller.signal))
      toast.push('success', 'API token 已续期；旧明文立即失效。')
      await queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
    } catch (caught) {
      if (request.timedOut) {
        // The server may still have rotated the secret after the client gave up.
        setNotice(`续期请求超过 ${RENEW_TIMEOUT_SECONDS} 秒未完成，已停止等待。结果未知：旧 token 可能已失效，请刷新 token 列表确认后再决定是否重试。`)
        void queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
      } else {
        setError(caught)
      }
    } finally {
      window.clearTimeout(timer)
      if (activeRequest.current === request) {
        activeRequest.current = null
        setPending(false)
      }
    }
  }

  if (!token) return null
  return (
    <Dialog
      open={open}
      onClose={onClose}
      closeDisabled={pending}
      title={renewed ? '复制新 API token' : '续期 API token'}
      description={renewed
        ? '明文 token 只显示这一次。关闭对话框后将从界面状态中清除。'
        : `续期 ${tokenLabel(token)} 会立即签发同名新明文并让旧明文失效。`}
      footer={renewed ? (
        <Button variant="primary" onClick={onClose}>我已复制，关闭</Button>
      ) : (
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending} aria-disabled={pending || undefined}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>续期</Button>
        </>
      )}
    >
      {renewed ? (
        <>
          <SecretField label="API token" value={renewed.token} />
          <div className={styles.notice}>关闭后不会再次显示明文；旧 token 已失效。</div>
        </>
      ) : (
        <>
          {error ? <ErrorState error={error} title="续期失败" /> : null}
          {notice ? <div className={styles.warning} role="alert">{notice}</div> : null}
          {pending ? (
            <div className={styles.notice} role="status">
              正在续期。请求完成前不能关闭对话框；超过 {RENEW_TIMEOUT_SECONDS} 秒会停止等待并提示结果未知。
            </div>
          ) : null}
          <Field label="有效天数" htmlFor={`${id}-days`} hint="正整数，最长 365 天。">
            <NumberField id={`${id}-days`} min={1} max={365} value={days} onChange={(event) => setDays(event.target.value)} disabled={pending} />
          </Field>
        </>
      )}
    </Dialog>
  )
}

const ROLE_SOURCE_LABELS: Record<string, string> = { config: '配置文件', granted: '控制台授权', none: '默认' }

function UserDrawer({ user, open, currentUserId, onClose }: {
  user: AdminUser | null
  open: boolean
  currentUserId?: number | string
  onClose: () => void
}) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const headingId = useId()
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [addError, setAddError] = useState<unknown>(null)
  const [projectID, setProjectID] = useState('')
  const [grantRole, setGrantRole] = useState<ProjectAccess>('manage')
  const [confirm, setConfirm] = useState<{ role?: 'admin' | 'viewer'; global?: boolean } | null>(null)
  const [removeTarget, setRemoveTarget] = useState<{ projectId: number; role: string; label: string } | null>(null)
  const grants = usePaginated<ProjectGrant>(
    ['admin-user-grants', user?.id],
    (cursor) => listAdminUserGrants(user!.id, cursor),
    'grants',
    { enabled: open && Boolean(user) && user?.role !== 'admin' },
  )
  // Grant rows carry only the project ID. Names come from the admin project
  // list (shared cache with the 项目授权 tab); unmatched IDs fall back to #ID.
  const projects = usePaginated<AdminProject>(
    ['admin-projects'],
    (cursor) => listAdminProjects(cursor),
    'projects',
    { enabled: open && Boolean(user) && user?.role !== 'admin' },
  )
  const projectNames = new Map(projects.items.map((project) => [project.id, project.path_with_namespace]))

  const saved = async () => {
    await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
    await queryClient.invalidateQueries({ queryKey: ['admin-user-grants', user?.id] })
    // A user write can rotate the current account's revision and CSRF token.
    await queryClient.invalidateQueries({ queryKey: ['me'] })
  }

  const patch = async (body: { role?: 'admin' | 'viewer'; global_read?: boolean }) => {
    if (!user) return
    setPending(true)
    setError(null)
    try {
      await updateAdminUser(user.id, { ...body, revision: user.revision })
      toast.push('success', '用户设置已更新。')
      setConfirm(null)
      await saved()
    } catch (err) {
      setError(err)
    } finally {
      setPending(false)
    }
  }

  const addGrant = async () => {
    if (!user || !projectID.trim()) return
    setPending(true)
    setAddError(null)
    try {
      await setAdminUserGrant(user.id, projectID.trim(), grantRole)
      setProjectID('')
      toast.push('success', '项目授权已添加。')
      await saved()
    } catch (err) {
      setAddError(err)
    } finally {
      setPending(false)
    }
  }

  const removeGrant = async (projectId: number) => {
    if (!user) return
    setPending(true)
    setError(null)
    try {
      await deleteAdminUserGrant(user.id, projectId)
      setRemoveTarget(null)
      toast.push('success', '项目授权已删除。')
      await saved()
    } catch (err) {
      setError(err)
    } finally {
      setPending(false)
    }
  }

  if (!user) return null
  const configured = user.role_source === 'config'
  const self = String(user.id) === String(currentUserId)
  const admin = user.role === 'admin'
  const roleLock = configured
    ? '由配置文件提供，修改需改配置并重启。'
    : self && admin ? '当前登录账号，不能降级自己。' : null
  const roleId = `${headingId}-role`
  const globalId = `${headingId}-global`
  const grantsId = `${headingId}-grants`
  const projectLabel = (projectId: number) => projectNames.get(projectId) ?? `项目 #${projectId}`
  return (
    <Drawer open={open} onClose={onClose} title={userLabel(user)} description="角色与全局模式变更在下次请求生效；全局模式只读。">
      <div className={drawer.sections}>
        <section className={drawer.section} aria-labelledby={roleId}>
          <div className={drawer.sectionHead}>
            <h3 className={drawer.sectionTitle} id={roleId}>平台角色</h3>
            <div className={drawer.badges}>
              <Badge tone={admin ? 'info' : 'neutral'}>{admin ? '管理员' : '普通用户'}</Badge>
              <Badge tone="neutral" plain>来源：{ROLE_SOURCE_LABELS[user.role_source ?? 'none'] ?? '默认'}</Badge>
            </div>
          </div>
          {roleLock ? <p className={drawer.note}>{roleLock}</p> : (
            <>
              <p className={drawer.note}>
                {admin ? '管理员可管理全部项目、用户与平台配置。' : '提升后该用户可管理全部项目、用户与平台配置。'}
              </p>
              <div className={drawer.actions}>
                <Button variant="secondary" disabled={pending} onClick={() => { setError(null); setConfirm({ role: admin ? 'viewer' : 'admin' }) }}>
                  {admin ? '降级为普通用户' : '提升为管理员'}
                </Button>
              </div>
            </>
          )}
        </section>

        <section className={drawer.section} aria-labelledby={globalId}>
          <div className={drawer.sectionHead}>
            <h3 className={drawer.sectionTitle} id={globalId}>全局模式</h3>
            <Badge tone={user.global_read ? 'ok' : 'neutral'}>{user.global_read ? '已开启' : '未开启'}</Badge>
          </div>
          <p className={drawer.note}>开启后该用户可查看全部项目和全部评审（只读）。</p>
          <div className={drawer.actions}>
            <Button variant="secondary" disabled={pending} onClick={() => { setError(null); setConfirm({ global: !user.global_read }) }}>
              {user.global_read ? '关闭全局模式' : '开启全局模式'}
            </Button>
          </div>
        </section>

        {admin ? (
          <section className={drawer.section} aria-labelledby={grantsId}>
            <div className={drawer.sectionHead}>
              <h3 className={drawer.sectionTitle} id={grantsId}>项目访问</h3>
              <Badge tone="info">全部项目</Badge>
            </div>
            <p className={drawer.note}>
              平台管理员对全部已登记项目拥有 manage 权限，无需单独授权。
              {user.grant_count ? ` 该账号保留的 ${user.grant_count} 条项目授权不生效，降级为普通用户后恢复生效。` : ''}
            </p>
          </section>
        ) : (
          <section className={drawer.section} aria-labelledby={grantsId}>
            <div className={drawer.sectionHead}>
              <h3 className={drawer.sectionTitle} id={grantsId}>项目授权</h3>
              {grants.isSuccess ? <span className={drawer.sectionMeta}>{grants.items.length}{grants.hasNextPage ? '+' : ''} 项</span> : null}
            </div>
            <form
              className={drawer.grantForm}
              onSubmit={(event) => { event.preventDefault(); void addGrant() }}
            >
              <Field label="项目 ID" htmlFor={`grant-project-${user.id}`}>
                <input
                  id={`grant-project-${user.id}`}
                  className={ui.input}
                  inputMode="numeric"
                  autoComplete="off"
                  value={projectID}
                  onChange={(event) => { setProjectID(event.target.value); setAddError(null) }}
                />
              </Field>
              <Field label="角色" htmlFor={`grant-role-${user.id}`}>
                <Select
                  id={`grant-role-${user.id}`}
                  value={grantRole}
                  options={[{ value: 'read', label: 'read' }, { value: 'manage', label: 'manage' }]}
                  onChange={(value) => { setGrantRole(value as ProjectAccess); setAddError(null) }}
                />
              </Field>
              <Button type="submit" variant="primary" disabled={!projectID.trim()} loading={pending}>添加授权</Button>
            </form>
            {addError ? <ErrorState error={addError} title="添加授权失败" /> : null}
            <DataTable
              label="项目授权列表"
              minWidth="none"
              fixed
              isEmpty={grants.items.length === 0}
              loading={grants.isPending}
              error={grants.isError ? grants.error : undefined}
              onRetry={grants.retry}
              errorTitle="授权列表加载失败"
              empty={<EmptyState title="没有项目授权" description="输入项目 ID 并选择角色即可添加 read 或 manage 授权。" />}
            >
              <thead>
                <tr>
                  <th scope="col">项目</th>
                  <th scope="col" className={drawer.roleCol}>角色</th>
                  <th scope="col" className={classNames(tableCell.actions, drawer.actionsCol)}>操作</th>
                </tr>
              </thead>
              <tbody>
                {grants.items.map((grant) => {
                  const name = projectNames.get(grant.project_id)
                  const label = projectLabel(grant.project_id)
                  return (
                    <tr key={grant.project_id}>
                      <td>
                        {name ? (
                          <>
                            <span className={drawer.projectName}>{name}</span>
                            <span className={drawer.projectId}>#{grant.project_id}</span>
                          </>
                        ) : <span className={drawer.projectName}>{label}</span>}
                      </td>
                      <td><Badge tone={grant.role === 'manage' ? 'info' : 'neutral'} plain>{grant.role}</Badge></td>
                      <td className={classNames(tableCell.actions, drawer.actionsCol)}>
                        <TableActions>
                          <TableAction
                            tone="danger"
                            disabled={pending}
                            aria-label={`删除 ${label} 的授权`}
                            onClick={() => { setError(null); setRemoveTarget({ projectId: grant.project_id, role: grant.role, label }) }}
                          >
                            删除
                          </TableAction>
                        </TableActions>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </DataTable>
            {grants.hasNextPage ? (
              <Pagination
                hasNextPage={grants.hasNextPage}
                loading={grants.isFetchingNextPage}
                onLoadMore={grants.fetchNextPage}
                label="加载更多授权"
              />
            ) : null}
          </section>
        )}
      </div>
      <ConfirmDialog
        open={Boolean(confirm)}
        onClose={() => setConfirm(null)}
        onConfirm={() => { void patch(confirm?.role ? { role: confirm.role } : { global_read: confirm?.global }) }}
        title={confirm?.role ? (confirm.role === 'admin' ? '提升为管理员？' : '降级为普通用户？') : (confirm?.global ? '开启全局模式？' : '关闭全局模式？')}
        description={confirm?.role
          ? `将修改 ${userLabel(user)} 的平台角色。`
          : `将修改 ${userLabel(user)} 的全局只读权限。`}
        confirmLabel="确认"
        pending={pending}
        danger={confirm?.role === 'viewer'}
        error={error}
      />
      <ConfirmDialog
        open={Boolean(removeTarget)}
        onClose={() => setRemoveTarget(null)}
        onConfirm={() => { if (removeTarget) void removeGrant(removeTarget.projectId) }}
        title="删除项目授权？"
        description={removeTarget
          ? `将移除 ${userLabel(user)} 在 ${removeTarget.label} 的 ${removeTarget.role} 授权。删除后该用户不再通过此授权访问该项目。`
          : ''}
        confirmLabel="删除"
        pending={pending}
        danger
        error={error}
      />
    </Drawer>
  )
}

function AdminUsersPanel() {
  const toast = useToast()
  const queryClient = useQueryClient()
  const users = usePaginated<AdminUser>(['admin-users'], (cursor) => listAdminUsers(cursor), 'users')
  const [disableTarget, setDisableTarget] = useState<AdminUser | null>(null)
  const [disablePending, setDisablePending] = useState(false)
  const [disableError, setDisableError] = useState<unknown>(null)
  const [revokeTarget, setRevokeTarget] = useState<AdminUser | null>(null)
  const [revokePending, setRevokePending] = useState(false)
  const [revokeError, setRevokeError] = useState<unknown>(null)
  const [manageTargetId, setManageTargetId] = useState<number | null>(null)
  const me = useQuery({ queryKey: ['me'], queryFn: fetchMe, retry: false })
  // The drawer reads the refetched list instead of keeping a snapshot: a
  // successful write advances the server-side revision, and the next write
  // must send that new revision. Stale snapshots made consecutive edits 409.
  const manageTarget = manageTargetId == null
    ? null
    : users.items.find((user) => user.id === manageTargetId) ?? null

  const toggleDisabled = async () => {
    if (!disableTarget) return
    setDisablePending(true)
    setDisableError(null)
    try {
      const next = !disableTarget.disabled
      await setAdminUserDisabled(disableTarget.id, next, disableTarget.revision)
      setDisableTarget(null)
      toast.push('success', next ? '账号已停用。' : '账号已启用。')
      await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
      await queryClient.invalidateQueries({ queryKey: ['me'] })
    } catch (error) {
      if (error instanceof ApiError && error.code === 'stale_revision') {
        setDisableTarget(null)
        setDisableError(null)
        toast.push('error', '用户信息已被其他管理员更新，请刷新后重试。')
        await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
        await queryClient.invalidateQueries({ queryKey: ['me'] })
      } else {
        setDisableError(error)
      }
    } finally {
      setDisablePending(false)
    }
  }

  const revoke = async () => {
    if (!revokeTarget) return
    setRevokePending(true)
    setRevokeError(null)
    try {
      await revokeAdminUserCredentials(revokeTarget.id)
      setRevokeTarget(null)
      toast.push('success', '该账号的全部凭据已撤销。')
      await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
      await queryClient.invalidateQueries({ queryKey: ['me'] })
    } catch (error) {
      setRevokeError(error)
    } finally {
      setRevokePending(false)
    }
  }

  return (
    <>
      <Panel
        flush
        title="用户与账号"
        aside={users.isSuccess ? `${users.items.length}${users.hasNextPage ? '+' : ''} 个用户` : undefined}
        note="停用后该账号不能登录、使用现有会话或 API token，也不能获得新凭据。撤销凭据会立即使全部会话与 token 失效。"
      >
        <PanelBody>
          {users.isPending ? <LoadingState label="正在读取用户…" /> : null}
          {users.isError ? <ErrorState error={users.error} onRetry={users.retry} /> : null}
          {users.isSuccess && users.items.length === 0 ? <EmptyState title="还没有本地用户" description="用户首次通过 GitLab 登录后会出现在这里。" /> : null}
        </PanelBody>
        {users.items.length ? (
          <DataTable label="用户与账号" minWidth="xl">
            <thead>
              <tr>
                <th scope="col">用户</th>
                <th scope="col">角色</th>
                <th scope="col">全局模式</th>
                <th scope="col" className={tableCell.num}>项目授权数</th>
                <th scope="col">状态 / 最近登录</th>
                <th scope="col" className={tableCell.actions}>操作</th>
              </tr>
            </thead>
            <tbody>
              {users.items.map((user) => {
                const self = String(user.id) === String(me.data?.id)
                const admin = user.role === 'admin'
                const disableBlocked = user.role_source === 'config' || self
                const disableReason = user.role_source === 'config'
                  ? '配置来源的管理员由启动配置（bootstrap_admins）决定，不能在此停用。'
                  : self ? '不能停用当前登录账号，请使用其他管理员账号操作。' : null
                return (
                  <tr key={user.id}>
                    <td>
                      <span className={drawer.userCell}>
                        {user.name ? <span>{user.name}</span> : null}
                        <span className={drawer.userName}>{user.username}</span>
                      </span>
                    </td>
                    <td>
                      {user.role ? (
                        <span className={drawer.roleCell}>
                          <Badge tone={admin ? 'info' : 'neutral'}>{admin ? '管理员' : '普通用户'}</Badge>
                          {user.role_source ? <span className={drawer.roleSource}>{ROLE_SOURCE_LABELS[user.role_source] ?? user.role_source}</span> : null}
                        </span>
                      ) : '—'}
                    </td>
                    <td className={tableCell.nowrap}>{user.global_read == null ? '—' : user.global_read ? '已开启' : '未开启'}</td>
                    <td className={tableCell.num}>{admin ? '全部' : user.grant_count == null ? '—' : formatNumber(user.grant_count)}</td>
                    <td>
                      <span className={drawer.userCell}>
                        <span><Badge tone={user.disabled ? 'danger' : 'ok'}>{user.disabled ? '已停用' : '启用'}</Badge></span>
                        <span className={drawer.lastLogin}>{user.last_login_at ? formatDate(user.last_login_at) : '从未登录'}</span>
                      </span>
                    </td>
                    <td className={tableCell.actions}>
                      <TableActions>
                        <TableAction onClick={() => setManageTargetId(user.id)}>管理</TableAction>
                        <Tooltip content={disableReason}>
                          <TableAction
                            aria-label={user.disabled ? '启用账号' : '停用账号'}
                            aria-disabled={disableBlocked || undefined}
                            onClick={() => { if (!disableBlocked) setDisableTarget(user) }}
                          >
                            {user.disabled ? '启用' : '停用'}
                          </TableAction>
                        </Tooltip>
                        <TableAction tone="danger" onClick={() => setRevokeTarget(user)}>撤销凭据</TableAction>
                      </TableActions>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </DataTable>
        ) : null}
        {users.hasNextPage ? (
          <div className={insights.flushLoadMore}>
            <Button variant="secondary" onClick={users.fetchNextPage} loading={users.isFetchingNextPage}>加载更多用户</Button>
          </div>
        ) : null}
      </Panel>

      <ConfirmDialog
        open={Boolean(disableTarget)}
        onClose={() => { setDisableTarget(null); setDisableError(null) }}
        onConfirm={() => { void toggleDisabled() }}
        title={`${disableTarget?.disabled ? '启用账号' : '停用账号'}？`}
        description={disableTarget
          ? `${disableTarget.disabled ? '启用' : '停用'} ${userLabel(disableTarget)}。${disableTarget.disabled ? '账号恢复后可以重新登录并使用 API token。' : '停用后现有会话与 token 立即失效，且不能重新登录。'}`
          : ''}
        confirmLabel={disableTarget?.disabled ? '启用' : '停用'}
        danger={!disableTarget?.disabled}
        pending={disablePending}
        error={disableError}
      />

      <UserDrawer
        key={manageTarget?.id ?? 'none'}
        user={manageTarget}
        open={Boolean(manageTarget)}
        currentUserId={me.data?.id}
        onClose={() => setManageTargetId(null)}
      />

      <ConfirmDialog
        open={Boolean(revokeTarget)}
        onClose={() => { setRevokeTarget(null); setRevokeError(null) }}
        onConfirm={() => { void revoke() }}
        title="撤销全部凭据？"
        description={revokeTarget
          ? `将撤销 ${userLabel(revokeTarget)} 的全部会话和 API token。该账号必须重新登录并重新创建 token。此操作不可撤销。`
          : ''}
        confirmLabel="撤销"
        pending={revokePending}
        error={revokeError}
      />
    </>
  )
}

function AdminGrantsPanel() {
  const toast = useToast()
  const queryClient = useQueryClient()
  const projects = usePaginated<AdminProject>(['admin-projects'], (cursor) => listAdminProjects(cursor), 'projects')
  const users = usePaginated<AdminUser>(['admin-users'], (cursor) => listAdminUsers(cursor), 'users')
  const [projectId, setProjectId] = useState('')
  const [userId, setUserId] = useState('')
  const [role, setRole] = useState<ProjectAccess>('manage')
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<unknown>(null)
  const [deleteTarget, setDeleteTarget] = useState<ProjectGrant | null>(null)
  const [deletePending, setDeletePending] = useState(false)
  const [deleteError, setDeleteError] = useState<unknown>(null)

  const grants = usePaginated<ProjectGrant>(
    ['admin-grants', projectId],
    (cursor) => listAdminGrants(projectId, cursor),
    'grants',
    { enabled: Boolean(projectId) },
  )

  const selectedProject = projects.items.find((project) => String(project.id) === projectId)

  const grant = async () => {
    if (!projectId || !userId) {
      setSaveError(new Error('请选择项目和用户。'))
      return
    }
    setSaving(true)
    setSaveError(null)
    try {
      await setAdminGrant(projectId, userId, role)
      toast.push('success', '项目授权已保存。')
      setUserId('')
      await queryClient.invalidateQueries({ queryKey: ['admin-grants', projectId] })
    } catch (error) {
      setSaveError(error)
    } finally {
      setSaving(false)
    }
  }

  const removeGrant = async () => {
    if (!deleteTarget) return
    setDeletePending(true)
    setDeleteError(null)
    try {
      await deleteAdminGrant(deleteTarget.project_id, deleteTarget.user_id)
      setDeleteTarget(null)
      toast.push('success', '项目授权已撤销。')
      await queryClient.invalidateQueries({ queryKey: ['admin-grants', projectId] })
    } catch (error) {
      setDeleteError(error)
    } finally {
      setDeletePending(false)
    }
  }

  return (
    <>
      <Panel
        flush
        title="项目授权"
        aside={selectedProject ? selectedProject.path_with_namespace : '未选择项目'}
        note="平台管理员对全部已登记项目隐式拥有 manage，无需在这里授权；此处用于给普通用户分配 read 或 manage（manage 可删除项目）。项目列表来自管理面，不需要管理员自身拥有 grant。"
      >
        <PanelBody className={insights.grantForm}>
          <div className={insights.grantPicker}>
            <label className={insights.field} htmlFor="grant-project">
              <span className="small">选择项目</span>
              <Select
                id="grant-project"
                value={projectId}
                options={[
                  { value: '', label: '选择项目' },
                  ...projects.items.map((project) => ({ value: String(project.id), label: project.path_with_namespace })),
                ]}
                onChange={setProjectId}
                disabled={projects.isPending}
                aria-label="选择项目"
                emptyLabel="选择项目"
              />
            </label>
            {projects.hasNextPage ? (
              <Button variant="secondary" onClick={projects.fetchNextPage} loading={projects.isFetchingNextPage}>加载更多项目</Button>
            ) : null}
          </div>
          {projects.isError ? <ErrorState error={projects.error} onRetry={projects.retry} title="项目列表加载失败" /> : null}

          {projectId ? (
            <>
              <div className="formGrid">
                <Field label="选择用户" htmlFor="grant-user">
                  <Select
                    id="grant-user"
                    value={userId}
                    options={[
                      { value: '', label: '选择用户' },
                      ...users.items.filter((user) => user.role !== 'admin').map((user) => ({ value: String(user.id), label: userLabel(user) })),
                    ]}
                    onChange={setUserId}
                    disabled={users.isPending}
                    emptyLabel="选择用户"
                  />
                </Field>
                <Field label="权限角色" htmlFor="grant-role">
                  <Select
                    id="grant-role"
                    value={role}
                    options={[{ value: 'read', label: '只读（read）' }, { value: 'manage', label: '管理（manage）' }]}
                    onChange={(value) => setRole(value as ProjectAccess)}
                  />
                </Field>
              </div>
              {users.hasNextPage ? <Button variant="secondary" onClick={users.fetchNextPage} loading={users.isFetchingNextPage}>加载更多用户</Button> : null}
              {users.isError ? <ErrorState error={users.error} onRetry={users.retry} title="用户列表加载失败" /> : null}
              {saveError ? <ErrorState error={saveError} title="保存授权失败" /> : null}
              <div className={insights.formActions}>
                <Button variant="primary" onClick={() => { void grant() }} loading={saving}>授予权限</Button>
              </div>
            </>
          ) : <EmptyState title="请选择项目" description="选择项目后可以查看并维护该项目的用户授权。" />}
        </PanelBody>

        {projectId ? (
          <>
            <div className={insights.flushSection}>
              <h3 className={insights.sectionTitle}>当前授权</h3>
              {grants.isPending ? <LoadingState label="正在读取项目授权…" /> : null}
              {grants.isError ? <ErrorState error={grants.error} onRetry={grants.retry} /> : null}
              {grants.isSuccess && grants.items.length === 0 ? (
                <EmptyState title="该项目还没有授权" description="使用上方表单为账号授予 read 或 manage。" />
              ) : null}
            </div>
            {grants.items.length ? (
              <DataTable label="当前项目授权" minWidth="md">
                <thead>
                  <tr>
                    <th scope="col">用户</th>
                    <th scope="col" className={tableCell.num}>用户 ID</th>
                    <th scope="col">角色</th>
                    <th scope="col" className={tableCell.actions}>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {grants.items.map((item) => (
                    <tr key={`${item.project_id}-${item.user_id}`}>
                      <td>{item.username || `用户 ${item.user_id}`}</td>
                      <td className={tableCell.num}>{item.user_id}</td>
                      <td><Badge tone={item.role === 'manage' ? 'info' : 'neutral'} plain>{item.role === 'manage' ? '管理（manage）' : '只读（read）'}</Badge></td>
                      <td className={tableCell.actions}>
                        <TableActions>
                          <TableAction tone="danger" aria-label={`撤销 ${item.username || `用户 ${item.user_id}`} 的授权`} onClick={() => setDeleteTarget(item)}>撤销</TableAction>
                        </TableActions>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </DataTable>
            ) : null}
            {grants.hasNextPage ? (
              <div className={insights.flushLoadMore}>
                <Button variant="secondary" onClick={grants.fetchNextPage} loading={grants.isFetchingNextPage}>加载更多授权</Button>
              </div>
            ) : null}
          </>
        ) : null}
      </Panel>

      <ConfirmDialog
        open={Boolean(deleteTarget)}
        onClose={() => { setDeleteTarget(null); setDeleteError(null) }}
        onConfirm={() => { void removeGrant() }}
        title="撤销项目授权？"
        description={deleteTarget
          ? `将撤销用户 ${deleteTarget.username || deleteTarget.user_id} 在项目 ${deleteTarget.project_id} 的 ${deleteTarget.role} 权限。撤销后该用户不再通过此授权访问该项目。`
          : ''}
        confirmLabel="撤销授权"
        pending={deletePending}
        error={deleteError}
      />
    </>
  )
}

export function SystemPage() {
  const isAdmin = useCanWrite()
  const canSelfService = useCanSelfService()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [tab, setTab] = useState('tokens')
  const [createOpen, setCreateOpen] = useState(false)
  const [revokeTarget, setRevokeTarget] = useState<ApiToken | null>(null)
  const [renewTarget, setRenewTarget] = useState<ApiToken | null>(null)
  const [revokePending, setRevokePending] = useState(false)
  const [revokeError, setRevokeError] = useState<unknown>(null)

  const tokens = usePaginated<ApiToken>(
    ['api-tokens'],
    (cursor) => listApiTokens(cursor),
    'tokens',
  )
  const settings = useQuery({
    queryKey: ['settings'],
    queryFn: getSettings,
    enabled: isAdmin && tab === 'settings',
  })

  const tabs = [
    { key: 'tokens', label: 'API Token', panelId: 'system-tabpanel-tokens' },
    { key: 'deliveries', label: 'Webhook 投递', panelId: 'system-tabpanel-deliveries' },
    ...(isAdmin ? [
      { key: 'settings', label: '平台配置（只读）', panelId: 'system-tabpanel-settings' },
      { key: 'users', label: '用户与账号', panelId: 'system-tabpanel-users' },
      { key: 'access', label: '项目授权', panelId: 'system-tabpanel-access' },
    ] : []),
  ]

  const revoke = async () => {
    if (!revokeTarget) return
    setRevokePending(true)
    setRevokeError(null)
    try {
      await revokeApiToken(revokeTarget.id)
      setRevokeTarget(null)
      toast.push('success', 'API token 已撤销。')
      await queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
    } catch (error) {
      setRevokeError(error)
    } finally {
      setRevokePending(false)
    }
  }

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="系统"
          description={isAdmin
            ? '管理个人 API token、平台配置只读视图、用户账号与项目授权。'
            : '管理自己的 API token；平台配置与用户管理仅管理员可见。'}
        />
        <Tabs
          items={tabs}
          value={tab}
          onChange={setTab}
          label="系统设置"
          idPrefix="system-tab"
        />

        <div id={`system-tabpanel-${tab}`} role="tabpanel" aria-labelledby={`system-tab-${tab}`}>
          {tab === 'tokens' ? (
            <Panel
              flush
              title="我的 API token"
              aside={canSelfService ? (
                <Button variant="primary" className={ui.smallButton} onClick={() => setCreateOpen(true)}>创建 token</Button>
              ) : undefined}
              note={(
                <>
                  {canSelfService ? '登录用户可管理自己的 token；写操作由 CSRF 保护。' : '当前会话不能创建或撤销 token。'}
                  用户 API token 由登录用户在控制台创建，供 tribunalctl 或脚本访问项目级 API，明文只在创建时显示一次。
                  到期前 14 天会在此提示；续期会立即签发同名新明文，旧明文随即失效。
                  <br />
                  <code>REVIEW_ADMIN_TOKEN</code> 是部署方在环境变量配置的平台管理面 bootstrap Bearer，无用户 ID 与项目 grant，不能读取项目业务数据，也不能在控制台创建或轮换；修改它需改环境变量并重启。
                </>
              )}
            >
              {tokens.isPending || tokens.isError || (tokens.isSuccess && tokens.items.length === 0) ? (
                <PanelBody>
                  {tokens.isPending ? <LoadingState /> : null}
                  {tokens.isError ? <ErrorState error={tokens.error} onRetry={tokens.retry} /> : null}
                  {tokens.isSuccess && tokens.items.length === 0 ? <EmptyState title="还没有 API token" description={canSelfService ? '创建一个 token 供 tribunalctl 或脚本使用。' : '当前账号没有 API token。'} /> : null}
                </PanelBody>
              ) : null}
              {tokens.items.length ? (
                <DataTable label="API token" minWidth="lg">
                  <thead>
                    <tr>
                      <th scope="col">名称</th>
                      <th scope="col">创建时间</th>
                      <th scope="col">过期时间</th>
                      <th scope="col">最近使用</th>
                      {canSelfService ? <th scope="col" className={tableCell.actions}>操作</th> : null}
                    </tr>
                  </thead>
                  <tbody>
                    {tokens.items.map((token) => {
                      const expiry = tokenExpiry(token)
                      return (
                        <tr key={token.id}>
                          <td><b>{tokenLabel(token)}</b></td>
                          <td className={classNames(tableCell.muted, tableCell.nowrap)}>{token.created_at ? formatDate(token.created_at) : '—'}</td>
                          <td>
                            <span className={drawer.roleCell}>
                              <span className={tableCell.nowrap}>{token.expires_at ? formatDate(token.expires_at) : '—'}</span>
                              {expiry ? <Badge tone={expiry.tone}>{expiry.label}</Badge> : null}
                            </span>
                          </td>
                          <td className={tableCell.muted}>{token.last_used_at ? formatDate(token.last_used_at) : '从未使用或未提供'}</td>
                          {canSelfService ? (
                            <td className={tableCell.actions}>
                              <TableActions>
                                <TableAction onClick={() => setRenewTarget(token)}>续期</TableAction>
                                <TableAction tone="danger" onClick={() => setRevokeTarget(token)}>撤销</TableAction>
                              </TableActions>
                            </td>
                          ) : null}
                        </tr>
                      )
                    })}
                  </tbody>
                </DataTable>
              ) : null}
              {tokens.hasNextPage ? (
                <div className={insights.flushLoadMore}>
                  <Button variant="secondary" onClick={tokens.fetchNextPage} loading={tokens.isFetchingNextPage}>加载更多</Button>
                </div>
              ) : null}
            </Panel>
          ) : null}

          {tab === 'deliveries' ? <WebhookDeliveriesPanel /> : null}
          {tab === 'settings' && isAdmin ? (
            <>
              <ConfigSourcesPanel enabled={isAdmin && tab === 'settings'} />
              <Panel
                title="平台配置"
                aside="服务端脱敏只读视图"
                note="此页面不提供编辑；密钥和凭据由服务端脱敏，界面不显示其值。"
              >
                {settings.isPending ? <LoadingState label="正在读取平台配置…" /> : null}
                {settings.isError ? <ErrorState error={settings.error} onRetry={() => { void settings.refetch() }} /> : null}
                {settings.isSuccess ? (
                  <>
                    {Object.keys(settings.data || {}).length ? (
                      <pre className={`pre ${ui.jsonBlock} ${ui.jsonBlockTall}`}>{JSON.stringify(settings.data, null, 2)}</pre>
                    ) : <EmptyState title="平台配置为空" description="服务端返回了空对象；这不代表配置缺失或加载成功。" />}
                  </>
                ) : null}
              </Panel>
            </>
          ) : null}

          {tab === 'users' && isAdmin ? <AdminUsersPanel /> : null}
          {tab === 'access' && isAdmin ? <AdminGrantsPanel /> : null}
        </div>
      </div>

      {canSelfService ? <TokenCreateDialog open={createOpen} onClose={() => setCreateOpen(false)} /> : null}
      {canSelfService ? (
        <TokenRenewDialog token={renewTarget} open={Boolean(renewTarget)} onClose={() => setRenewTarget(null)} />
      ) : null}

      <ConfirmDialog
        open={Boolean(revokeTarget)}
        onClose={() => setRevokeTarget(null)}
        onConfirm={() => { void revoke() }}
        title="撤销 API token？"
        description={`撤销后，使用 ${revokeTarget ? tokenLabel(revokeTarget) : '该 token'} 的脚本将立即失去访问权限。`}
        confirmLabel="撤销"
        pending={revokePending}
        error={revokeError}
      />
    </main>
  )
}
