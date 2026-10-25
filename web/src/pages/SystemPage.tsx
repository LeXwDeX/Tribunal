import { useEffect, useId, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  createApiToken, deleteAdminGrant, getSettings, listAdminGrants, listAdminProjects, listAdminUsers,
  listApiTokens, revokeAdminUserCredentials, revokeApiToken, setAdminGrant, setAdminUserDisabled,
} from '../api/api'
import type {
  AdminProject, AdminUser, ApiToken, CreatedApiToken, ProjectAccess, ProjectGrant,
} from '../api/types'
import { useCanSelfService, useCanWrite } from '../components/RoleContext'
import {
  Badge, Button, ConfirmDialog, Dialog, EmptyState, ErrorState, Field, LoadingState, PageHeader, Panel, SecretField, Tabs, useToast,
} from '../components/ui'
import { usePaginated } from '../lib/hooks'
import { formatDate } from '../lib/format'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'

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

  useEffect(() => {
    if (!open) {
      setName('')
      setDays('90')
      setPending(false)
      setError(null)
      setCreated(null)
      setFieldError('')
    }
  }, [open])

  const submit = async () => {
    const parsedDays = Number(days)
    if (!name.trim()) { setFieldError('请输入 token 名称。'); return }
    if (!Number.isInteger(parsedDays) || parsedDays <= 0 || parsedDays > 365) {
      setFieldError('有效天数必须在 1 到 365 之间。')
      return
    }
    setFieldError('')
    setPending(true)
    setError(null)
    try {
      const result = await createApiToken({ name: name.trim(), expires_in_days: parsedDays })
      setCreated(result)
      toast.push('success', 'API token 已创建，请立即复制。')
      await queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
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
      title={created ? '复制新 API token' : '创建 API token'}
      description={created
        ? '明文 token 只显示这一次。关闭对话框后将从界面状态中清除。'
        : 'API token 用于 reviewctl 或脚本访问管理 API。请设置名称和有效期。'}
      footer={created ? (
        <Button variant="primary" onClick={onClose}>我已复制，关闭</Button>
      ) : (
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
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
          {error ? <ErrorState error={error} title="创建失败" /> : null}
          <div className="formGrid">
            <Field label="名称" htmlFor={nameId} error={fieldError}>
              <input id={nameId} className="input" value={name} onChange={(event) => setName(event.target.value)} placeholder="例如 ci-reviewctl" aria-invalid={Boolean(fieldError)} />
            </Field>
            <Field label="有效天数" htmlFor={expiresId} hint="正整数，最长 365 天。">
              <input id={expiresId} className="input" type="number" min={1} max={365} value={days} onChange={(event) => setDays(event.target.value)} />
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

  const toggleDisabled = async () => {
    if (!disableTarget) return
    setDisablePending(true)
    setDisableError(null)
    try {
      const next = !disableTarget.disabled
      await setAdminUserDisabled(disableTarget.id, next)
      setDisableTarget(null)
      toast.push('success', next ? '账号已停用。' : '账号已启用。')
      await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
    } catch (error) {
      setDisableError(error)
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
    } catch (error) {
      setRevokeError(error)
    } finally {
      setRevokePending(false)
    }
  }

  return (
    <>
      <Panel
        title="用户与账号"
        aside={users.isSuccess ? `${users.items.length}${users.hasNextPage ? '+' : ''} 个用户` : undefined}
      >
        <div className={styles.notice}>停用后该账号不能登录、使用现有会话或 API token，也不能获得新凭据。撤销凭据会立即使全部会话与 token 失效。</div>
        {users.isPending ? <LoadingState label="正在读取用户…" /> : null}
        {users.isError ? <ErrorState error={users.error} onRetry={users.retry} /> : null}
        {users.isSuccess && users.items.length === 0 ? <EmptyState title="还没有本地用户" description="用户首次通过 GitLab 登录后会出现在这里。" /> : null}
        {users.items.length ? (
          <div className="tableWrap">
            <table className="dataTable" style={{ minWidth: 760 }}>
              <thead>
                <tr>
                  <th scope="col">用户名</th>
                  <th scope="col">显示名</th>
                  <th scope="col">状态</th>
                  <th scope="col">最近登录</th>
                  <th scope="col">操作</th>
                </tr>
              </thead>
              <tbody>
                {users.items.map((user) => (
                  <tr key={user.id}>
                    <td className="mono">{user.username}</td>
                    <td>{user.name || '—'}</td>
                    <td><Badge tone={user.disabled ? 'danger' : 'ok'}>{user.disabled ? '已停用' : '启用'}</Badge></td>
                    <td>{user.last_login_at ? formatDate(user.last_login_at) : '从未登录'}</td>
                    <td>
                      <div className={styles.cardActions}>
                        <Button variant="secondary" onClick={() => setDisableTarget(user)}>
                          {user.disabled ? '启用账号' : '停用账号'}
                        </Button>
                        <Button variant="danger" onClick={() => setRevokeTarget(user)}>撤销全部凭据</Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
        {users.hasNextPage ? <Button variant="secondary" onClick={users.fetchNextPage} loading={users.isFetchingNextPage}>加载更多用户</Button> : null}
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
  const [role, setRole] = useState<ProjectAccess>('read')
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
      <Panel title="项目授权" aside={selectedProject ? selectedProject.path_with_namespace : '未选择项目'}>
        <div className={styles.notice}>
          管理员也不会自动获得项目业务数据；请在这里为账号授予 read 或 manage。项目列表来自管理面，不需要管理员自身拥有 grant。
        </div>
        <div className={styles.inlineFilters}>
          <label className={styles.field} style={{ flex: '1 1 280px' }}>
            <span className="small">选择项目</span>
            <select
              className="select"
              aria-label="选择项目"
              value={projectId}
              onChange={(event) => setProjectId(event.target.value)}
              disabled={projects.isPending}
            >
              <option value="">选择项目</option>
              {projects.items.map((project) => (
                <option key={project.id} value={project.id}>{project.path_with_namespace}</option>
              ))}
            </select>
          </label>
          {projects.hasNextPage ? (
            <Button variant="secondary" onClick={projects.fetchNextPage} loading={projects.isFetchingNextPage}>加载更多项目</Button>
          ) : null}
        </div>
        {projects.isError ? <ErrorState error={projects.error} onRetry={projects.retry} title="项目列表加载失败" /> : null}

        {projectId ? (
          <>
            <div className="formGrid" style={{ marginTop: 16 }}>
              <Field label="选择用户" htmlFor="grant-user">
                <select id="grant-user" className="select" value={userId} onChange={(event) => setUserId(event.target.value)} disabled={users.isPending}>
                  <option value="">选择用户</option>
                  {users.items.map((user) => <option key={user.id} value={user.id}>{userLabel(user)}</option>)}
                </select>
              </Field>
              <Field label="权限角色" htmlFor="grant-role">
                <select id="grant-role" className="select" value={role} onChange={(event) => setRole(event.target.value as ProjectAccess)}>
                  <option value="read">只读（read）</option>
                  <option value="manage">管理（manage）</option>
                </select>
              </Field>
            </div>
            {users.hasNextPage ? <Button variant="secondary" onClick={users.fetchNextPage} loading={users.isFetchingNextPage}>加载更多用户</Button> : null}
            {users.isError ? <ErrorState error={users.error} onRetry={users.retry} title="用户列表加载失败" /> : null}
            {saveError ? <ErrorState error={saveError} title="保存授权失败" /> : null}
            <div className={styles.actionsRight} style={{ marginTop: 12 }}>
              <Button variant="primary" onClick={() => { void grant() }} loading={saving}>授予权限</Button>
            </div>

            <h3 style={{ marginTop: 20 }}>当前授权</h3>
            {grants.isPending ? <LoadingState label="正在读取项目授权…" /> : null}
            {grants.isError ? <ErrorState error={grants.error} onRetry={grants.retry} /> : null}
            {grants.isSuccess && grants.items.length === 0 ? (
              <EmptyState title="该项目还没有授权" description="使用上方表单为账号授予 read 或 manage。" />
            ) : null}
            {grants.items.length ? (
              <div className="tableWrap">
                <table className="dataTable" style={{ minWidth: 680 }}>
                  <thead>
                    <tr>
                      <th scope="col">用户</th>
                      <th scope="col">用户 ID</th>
                      <th scope="col">角色</th>
                      <th scope="col">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {grants.items.map((item) => (
                      <tr key={`${item.project_id}-${item.user_id}`}>
                        <td>{item.username || `用户 ${item.user_id}`}</td>
                        <td className="tabular">{item.user_id}</td>
                        <td>{item.role === 'manage' ? '管理（manage）' : '只读（read）'}</td>
                        <td><Button variant="danger" onClick={() => setDeleteTarget(item)}>撤销</Button></td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : null}
            {grants.hasNextPage ? (
              <Button variant="secondary" onClick={grants.fetchNextPage} loading={grants.isFetchingNextPage}>加载更多授权</Button>
            ) : null}
          </>
        ) : <EmptyState title="请选择项目" description="选择项目后可以查看并维护该项目的用户授权。" />}
      </Panel>

      <ConfirmDialog
        open={Boolean(deleteTarget)}
        onClose={() => { setDeleteTarget(null); setDeleteError(null) }}
        onConfirm={() => { void removeGrant() }}
        title="撤销项目授权？"
        description={deleteTarget
          ? `将撤销用户 ${deleteTarget.username || deleteTarget.user_id} 在项目 ${deleteTarget.project_id} 的 ${deleteTarget.role} 权限。该用户会立即失去对应的项目访问能力。`
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
    { key: 'tokens', label: 'API Token' },
    ...(isAdmin ? [
      { key: 'settings', label: '平台配置（只读）' },
      { key: 'users', label: '用户与账号' },
      { key: 'access', label: '项目授权' },
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
            ? '管理个人 API token、平台配置只读视图、用户账号和项目授权。'
            : '管理当前用户的 API token。平台配置与用户管理仅管理员可见。'}
        />
        <Tabs
          items={tabs}
          value={tab}
          onChange={setTab}
          label="系统设置"
        />

        {tab === 'tokens' ? (
          <Panel
            title="我的 API token"
            aside={canSelfService ? '登录用户可管理自己的 token；写操作由 CSRF 保护' : '当前会话不能创建或撤销 token'}
          >
            {canSelfService ? (
              <div className={styles.actionsRight}>
                <Button variant="primary" onClick={() => setCreateOpen(true)}>创建 token</Button>
              </div>
            ) : null}
            {tokens.isPending ? <LoadingState /> : null}
            {tokens.isError ? <ErrorState error={tokens.error} onRetry={tokens.retry} /> : null}
            {tokens.isSuccess && tokens.items.length === 0 ? <EmptyState title="还没有 API token" description={canSelfService ? '创建一个 token 供 reviewctl 或脚本使用。' : '当前账号没有 API token。'} /> : null}
            {tokens.items.length ? (
              <div className="tableWrap">
                <table className="dataTable" style={{ minWidth: 760 }}>
                  <thead>
                    <tr>
                      <th scope="col">名称</th>
                      <th scope="col">创建时间</th>
                      <th scope="col">过期时间</th>
                      <th scope="col">最近使用</th>
                      {canSelfService ? <th scope="col">操作</th> : null}
                    </tr>
                  </thead>
                  <tbody>
                    {tokens.items.map((token) => (
                      <tr key={token.id}>
                        <td>{tokenLabel(token)}</td>
                        <td>{token.created_at ? formatDate(token.created_at) : '—'}</td>
                        <td>{token.expires_at ? formatDate(token.expires_at) : '—'}</td>
                        <td>{token.last_used_at ? formatDate(token.last_used_at) : '从未使用或未提供'}</td>
                        {canSelfService ? <td><Button variant="danger" onClick={() => setRevokeTarget(token)}>撤销</Button></td> : null}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : null}
            {tokens.hasNextPage ? <Button variant="secondary" onClick={tokens.fetchNextPage} loading={tokens.isFetchingNextPage}>加载更多</Button> : null}
          </Panel>
        ) : null}

        {tab === 'settings' && isAdmin ? (
          <Panel
            title="平台配置"
            aside="服务端脱敏只读视图"
          >
            {settings.isPending ? <LoadingState label="正在读取平台配置…" /> : null}
            {settings.isError ? <ErrorState error={settings.error} onRetry={() => { void settings.refetch() }} /> : null}
            {settings.isSuccess ? (
              <>
                <div className={styles.notice}>此页面不提供编辑；密钥和凭据由服务端脱敏，界面不显示其值。</div>
                {Object.keys(settings.data || {}).length ? (
                  <pre className="pre" style={settingsStyle}>{JSON.stringify(settings.data, null, 2)}</pre>
                ) : <EmptyState title="平台配置为空" description="服务端返回了空对象；这不代表配置缺失或加载成功。" />}
              </>
            ) : null}
          </Panel>
        ) : null}

        {tab === 'users' && isAdmin ? <AdminUsersPanel /> : null}
        {tab === 'access' && isAdmin ? <AdminGrantsPanel /> : null}
      </div>

      {canSelfService ? <TokenCreateDialog open={createOpen} onClose={() => setCreateOpen(false)} /> : null}
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

const settingsStyle = {
  background: 'var(--surface-2)',
  border: '1px solid var(--border)',
  borderRadius: 8,
  padding: 12,
  color: 'var(--text)',
  maxHeight: 520,
  overflow: 'auto',
} as const
