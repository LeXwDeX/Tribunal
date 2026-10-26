import { useEffect, useId, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { listProfiles, listProjects, registerProject, updateProject, validateProjectConnection } from '../api/api'
import { ApiError, errorMessage } from '../api/client'
import type { Project, ProjectRegistration, ProjectValidation } from '../api/types'
import { useCanWrite } from '../components/RoleContext'
import {
  Badge, Button, Dialog, EmptyState, ErrorState, ExternalLink, Field, LoadingState, PageHeader, Panel, RelativeTime, Segmented, StatusBadge,
  Switch, Toolbar, ToolbarField, VerdictBadge, useToast,
} from '../components/ui'
import { usePaginated } from '../lib/hooks'
import { formatDate, labelForStatus, labelForWebhookState, webhookTone } from '../lib/format'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'
import page from './projects.module.css'

function tokenExpiry(project: Project): { label: string; tone: 'ok' | 'warn' | 'danger' | 'neutral' } {
  if (!project.token_expires_at) return { label: '未提供', tone: 'neutral' }
  const expires = new Date(project.token_expires_at).getTime()
  if (Number.isNaN(expires)) return { label: '日期无效', tone: 'warn' }
  const days = Math.ceil((expires - Date.now()) / 86_400_000)
  if (days <= 0) return { label: '已过期', tone: 'danger' }
  if (days <= 30) return { label: `${days} 天后到期`, tone: 'warn' }
  return { label: formatDate(project.token_expires_at), tone: 'neutral' }
}

const ENABLED_FILTERS = [
  { value: '', label: '全部' },
  { value: 'true', label: '已启用' },
  { value: 'false', label: '已停用' },
]

const ACCESS_LABELS: Record<string, string> = {
  manage: '可管理',
  read: '只读',
}

const EXPIRY_TONES: Record<string, string | undefined> = {
  warn: page.expiryWarn,
  danger: page.expiryDanger,
}

function fieldErrorsFrom(error: unknown): Record<string, string> {
  if (!(error instanceof ApiError)) return {}
  const result: Record<string, string> = {}
  for (const detail of error.details || []) {
    if (detail.field) result[detail.field] = detail.message || '输入无效。'
  }
  return result
}

const ACCESS_LEVEL_ROLES: Record<number, string> = {
  10: 'Guest',
  20: 'Reporter',
  30: 'Developer',
  40: 'Maintainer',
  50: 'Owner',
}

function accessLevelLabel(level: number): string {
  return `${ACCESS_LEVEL_ROLES[level] || '未知角色'}（${level}）`
}

function RegisterProjectDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const projectId = useId()
  const tokenId = useId()
  const profileId = useId()
  const branchesId = useId()
  const requestRef = useRef(0)
  const [project, setProject] = useState('')
  const [token, setToken] = useState('')
  const [profile, setProfile] = useState('default')
  const [skipDraft, setSkipDraft] = useState(true)
  const [branches, setBranches] = useState('')
  const [validating, setValidating] = useState(false)
  const [saving, setSaving] = useState(false)
  const [validation, setValidation] = useState<ProjectValidation | null>(null)
  const [validationError, setValidationError] = useState<unknown>(null)
  const [error, setError] = useState<unknown>(null)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [result, setResult] = useState<ProjectRegistration | null>(null)
  const pending = validating || saving

  const profiles = useQuery({
    queryKey: ['profiles', 'register'],
    queryFn: () => listProfiles(),
    enabled: open,
  })

  useEffect(() => {
    if (!open) {
      requestRef.current += 1
      setProject('')
      setToken('')
      setProfile('default')
      setSkipDraft(true)
      setBranches('')
      setValidating(false)
      setSaving(false)
      setValidation(null)
      setValidationError(null)
      setError(null)
      setErrors({})
      setResult(null)
    }
  }, [open])

  const close = () => {
    if (pending) return
    requestRef.current += 1
    setToken('')
    setValidation(null)
    setValidationError(null)
    setError(null)
    setErrors({})
    setResult(null)
    onClose()
  }

  const resetValidation = () => {
    setValidation(null)
    setValidationError(null)
    setError(null)
  }

  const changeProject = (value: string) => {
    setProject(value)
    setErrors({})
    resetValidation()
  }

  const changeToken = (value: string) => {
    setToken(value)
    setErrors({})
    resetValidation()
  }

  const validate = async () => {
    const nextErrors: Record<string, string> = {}
    if (!project.trim()) nextErrors.project = '请输入仓库网址、项目路径或 ID。'
    if (!token.trim()) nextErrors.token = '请输入访问 Key。'
    setErrors(nextErrors)
    if (Object.keys(nextErrors).length) return
    const requestId = requestRef.current + 1
    requestRef.current = requestId
    setValidating(true)
    setValidation(null)
    setValidationError(null)
    setError(null)
    try {
      const value = await validateProjectConnection({ project: project.trim(), token: token.trim() })
      if (requestRef.current !== requestId) return
      setValidation(value)
      setErrors({})
    } catch (caught) {
      if (requestRef.current !== requestId) return
      setValidationError(caught)
      setErrors(fieldErrorsFrom(caught))
    } finally {
      if (requestRef.current === requestId) setValidating(false)
    }
  }

  const save = async () => {
    if (!validation || pending) return
    setSaving(true)
    setError(null)
    try {
      const settings: Record<string, unknown> = { skip_draft: skipDraft }
      const targets = branches.split(',').map((value) => value.trim()).filter(Boolean)
      if (targets.length) settings.target_branches = targets
      const registration = await registerProject({
        project: project.trim(),
        token: token.trim(),
        profile: profile.trim() || 'default',
        settings,
      })
      setResult(registration)
      setToken('')
      setValidation(null)
      setErrors({})
      toast.push('success', registration.job_id ? `项目已保存，初始化任务 ${registration.job_id} 已创建。` : '项目已保存，初始化已开始。')
      void queryClient.invalidateQueries({ queryKey: ['projects'] })
      void queryClient.invalidateQueries({ queryKey: ['stats'] })
    } catch (caught) {
      setError(caught)
      setErrors(fieldErrorsFrom(caught))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={close}
      title={result ? '项目已保存，初始化已开始' : '接入 GitLab 项目'}
      description={result
        ? '项目已保存。平台已加密保存访问 Key；后台会拉取仓库并生成评审上下文，可能产生模型费用，但不会修改仓库代码。'
        : '先只读校验连接，确认项目和权限；校验通过后才保存并初始化。保存会加密存储访问 Key，并在后台拉取仓库、生成评审上下文，可能产生模型费用。不会修改仓库代码。'}
      wide
      footer={result ? (
        <Button variant="primary" onClick={close}>完成</Button>
      ) : (
        <>
          <Button variant="secondary" onClick={close} disabled={pending}>取消</Button>
          <Button variant="secondary" onClick={() => { void validate() }} loading={validating} disabled={saving}>校验连接</Button>
          <Button variant="primary" onClick={() => { void save() }} loading={saving} disabled={!validation || validating}>保存并初始化</Button>
        </>
      )}
    >
      {result ? (
        <div className={styles.list}>
          <dl className={ui.definitionGrid}>
            <div><dt>项目</dt><dd className="mono">{result.project?.path_with_namespace || project}</dd></div>
            <div><dt>项目 ID</dt><dd className="tabular">{result.project?.id ?? '—'}</dd></div>
            <div><dt>默认分支</dt><dd className="mono">{result.project?.default_branch || '—'}</dd></div>
            <div><dt>档位</dt><dd>{result.project?.profile || '—'}</dd></div>
            <div><dt>token scope</dt><dd className="mono">{result.project?.token_scopes?.join(', ') || '—'}</dd></div>
            <div><dt>token 过期</dt><dd>{result.project?.token_expires_at ? formatDate(result.project.token_expires_at) : '未提供'}</dd></div>
            <div><dt>初始化任务</dt><dd className="mono">{result.job_id || '—'}</dd></div>
          </dl>
          <div className={styles.notice} role="status">
            <div className={ui.stack}>
              <strong>Webhook 需在项目设置中单独配置</strong>
              <span className="small">注册不会创建、启用或修改 GitLab Webhook。需要自动触发评审时，在项目详情的「设置」中单独启用 Webhook。</span>
              {result.project?.id ? <Link to={`/projects/${result.project.id}/settings`} onClick={close}>前往项目设置</Link> : null}
            </div>
          </div>
        </div>
      ) : (
        <>
          <ol className={page.stepper} aria-label="注册步骤">
            <li className={validation ? page.step : `${page.step} ${page.stepOn}`} aria-current={validation ? undefined : 'step'}><b>1</b>校验连接</li>
            <li className={validation ? `${page.step} ${page.stepOn}` : page.step} aria-current={validation ? 'step' : undefined}><b>2</b>保存并初始化</li>
          </ol>
          {validationError ? <ErrorState error={validationError} title="校验失败" /> : null}
          {error ? <ErrorState error={error} title="保存并初始化失败" /> : null}
          {validation ? (
            <>
              <div className={page.resultOk} role="status">
                <div className={ui.stack}>
                  <strong>校验通过，尚未保存</strong>
                  <span className="small">只读校验未登记项目、未保存 Key、未创建 Webhook、未入队、未调用模型，也未写仓库。</span>
                </div>
              <dl className={ui.definitionGrid}>
                <div>
                  <dt>项目</dt>
                  <dd>
                    <span className="mono">{validation.path_with_namespace}</span>
                    <div><ExternalLink href={validation.web_url}>{validation.web_url}</ExternalLink></div>
                  </dd>
                </div>
                <div><dt>项目 ID</dt><dd className="tabular">{validation.project_id}</dd></div>
                <div><dt>当前身份</dt><dd>{validation.username}</dd></div>
                <div><dt>项目角色</dt><dd>{accessLevelLabel(validation.access_level)}</dd></div>
                <div><dt>默认分支</dt><dd className="mono">{validation.default_branch}</dd></div>
                <div><dt>Key 有效期</dt><dd>{validation.token_expires_at ? formatDate(validation.token_expires_at) : '未提供'}</dd></div>
                <div>
                  <dt>所需 scope</dt>
                  <dd>
                    {validation.token_scopes.length ? (
                      <span className={styles.badgeGroup}>
                        {validation.token_scopes.map((scope) => <Badge key={scope} tone="neutral">{scope}</Badge>)}
                      </span>
                    ) : '未提供'}
                  </dd>
                </div>
                <div><dt>仓库 HEAD</dt><dd className="mono">{validation.git_head}</dd></div>
              </dl>
              </div>
              <div className={styles.notice}>
                这是只读校验：确认项目可读取、当前身份与 scope、Key 有效期，并已读取仓库 HEAD。它不证明评论写入、Webhook 或完整 clone；保存时服务端仍会复核凭据。
              </div>
            </>
          ) : null}
          <div className="formGrid">
            <Field label="仓库地址或项目 ID" htmlFor={projectId} error={errors.project || errors.Project} hint="支持 GitLab 项目完整网址，也兼容 group/project 或项目 ID。">
              <input id={projectId} className="input" value={project} onChange={(event) => changeProject(event.target.value)} placeholder="https://gitlab.example.com/group/project" autoComplete="off" aria-invalid={Boolean(errors.project)} disabled={pending} />
            </Field>
            <Field label="访问 Key" htmlFor={tokenId} error={errors.token || errors.Token} hint="需要 api 和 read_repository scope；只提交给服务端，不写入浏览器存储。">
              <input id={tokenId} className="input" type="password" value={token} onChange={(event) => changeToken(event.target.value)} autoComplete="new-password" aria-invalid={Boolean(errors.token)} disabled={pending} />
            </Field>
          </div>
          <details className={ui.disclosure}>
            <summary>高级设置：档位、分支与 Draft</summary>
            <div className={ui.stack} style={{ marginTop: 8 }}>
              <div className="formGrid">
                <Field label="评审档位" htmlFor={profileId} hint="从平台配置档位中选择，或输入已有档位名。">
                  <input id={profileId} className="input" list="profile-options" value={profile} onChange={(event) => setProfile(event.target.value)} autoComplete="off" disabled={pending} />
                  <datalist id="profile-options">
                    {(profiles.data?.profiles || []).map((item) => <option key={item.name} value={item.name} />)}
                  </datalist>
                </Field>
                <Field label="目标分支" htmlFor={branchesId} hint="逗号分隔；留空沿用平台默认分支设置。">
                  <input id={branchesId} className="input" value={branches} onChange={(event) => setBranches(event.target.value)} placeholder="main, release/*" autoComplete="off" disabled={pending} />
                </Field>
              </div>
              <div className={styles.filterChips} style={{ flexDirection: 'column', alignItems: 'flex-start' }}>
                <label className="checkboxRow">
                  <input type="checkbox" checked={skipDraft} onChange={(event) => setSkipDraft(event.target.checked)} disabled={pending} />
                  <span>跳过 Draft MR</span>
                </label>
              </div>
              {profiles.isError ? <span className="small muted">档位列表加载失败，仍可输入档位名。</span> : null}
            </div>
          </details>
          <div className={page.costNotice}>
            保存并初始化会登记项目、加密保存访问 Key、按档位入队后台拉取与上下文生成，并可能在初始化或评审时产生模型费用；不创建或修改 Webhook，Webhook 在项目设置中单独配置。所有操作都不会修改仓库代码。
          </div>
        </>
      )}
    </Dialog>
  )
}

export function ProjectsPage() {
  const canRegister = useCanWrite()
  const navigate = useNavigate()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [search, setSearch] = useState('')
  const [enabled, setEnabled] = useState('')
  const [debouncedSearch, setDebouncedSearch] = useState('')
  const [registerOpen, setRegisterOpen] = useState(false)
  const [pendingIds, setPendingIds] = useState<Set<number>>(new Set())

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedSearch(search.trim()), 300)
    return () => window.clearTimeout(timer)
  }, [search])

  const projects = usePaginated<Project>(
    ['projects', 'list', debouncedSearch, enabled],
    (cursor) => listProjects({
      q: debouncedSearch || undefined,
      enabled: enabled === '' ? undefined : enabled === 'true',
    }, cursor),
    'projects',
  )

  const toggleProject = async (project: Project, next: boolean) => {
    if (project.access !== 'manage') return
    setPendingIds((current) => new Set(current).add(project.id))
    try {
      await updateProject(project.id, { enabled: next })
      toast.push('success', `${project.path_with_namespace} 已${next ? '启用' : '停用'}。`)
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
    } catch (error) {
      toast.push('error', `更新失败：${errorMessage(error)}`)
    } finally {
      setPendingIds((current) => {
        const nextSet = new Set(current)
        nextSet.delete(project.id)
        return nextSet
      })
    }
  }

  const enabledCount = projects.items.filter((project) => project.enabled).length

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="项目"
          description="每个项目使用自己的 Project Access Token；评审结果以该 token 的 bot 用户身份发布。"
          actions={canRegister ? <Button variant="primary" onClick={() => setRegisterOpen(true)}>注册项目</Button> : undefined}
        />

        <Panel
          title="已注册项目"
          aside={projects.isSuccess ? `已加载 ${projects.items.length}${projects.hasNextPage ? '+' : ''} 个项目 · ${enabledCount} 个启用` : undefined}
        >
          <Toolbar actions={projects.isFetching ? <span className="small muted" role="status">刷新中…</span> : undefined}>
            <ToolbarField label="搜索项目路径" grow>
              <input className="input" name="q" type="search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索项目路径" aria-label="搜索项目路径" />
            </ToolbarField>
            <Segmented
              label="按启用状态筛选"
              options={ENABLED_FILTERS}
              value={enabled}
              onChange={setEnabled}
            />
          </Toolbar>

          {projects.isPending ? <LoadingState /> : null}
          {projects.isError ? <ErrorState error={projects.error} onRetry={projects.retry} /> : null}
          {projects.isSuccess && projects.items.length === 0 ? (
            <EmptyState
              title={debouncedSearch || enabled ? '没有符合条件的项目' : '还没有注册项目'}
              description={debouncedSearch || enabled ? '调整搜索或启用状态筛选。' : '注册第一个 GitLab 项目后，这里会显示工作区、token 和 webhook 状态。'}
              action={canRegister && !debouncedSearch && !enabled ? <Button variant="primary" onClick={() => setRegisterOpen(true)}>注册项目</Button> : undefined}
            />
          ) : null}
          {projects.items.length ? (
            <div className="tableWrap">
              <table className="dataTable" style={{ minWidth: 1060 }}>
                <thead>
                  <tr>
                    <th scope="col">项目</th>
                    <th scope="col">档位</th>
                    <th scope="col">工作区</th>
                    <th scope="col">最近评审</th>
                    <th scope="col">Key 过期</th>
                    <th scope="col">webhook</th>
                    <th scope="col">权限</th>
                    <th scope="col">启用</th>
                  </tr>
                </thead>
                <tbody>
                  {projects.items.map((project) => {
                    const expiry = tokenExpiry(project)
                    const workspaceState = project.workspace?.state || project.workspace_state
                    const webhookState = project.webhook_state || (project.hook_id ? 'unknown' : 'disabled')
                    const lastReview = project.last_review
                    return (
                      <tr
                        key={project.id}
                        className="clickable"
                        onClick={(event) => {
                          // Links, switches and buttons inside the row keep their own behavior.
                          if ((event.target as HTMLElement).closest('a, button, input, label')) return
                          navigate(`/projects/${project.id}`)
                        }}
                      >
                        <td>
                          <div className={styles.tablePrimary}>
                            <Link to={`/projects/${project.id}`} className={page.pathCell}>{project.path_with_namespace}</Link>
                            <span className={styles.tableMeta}>ID {project.id}{project.default_branch ? ` · 默认分支 ${project.default_branch}` : ''}</span>
                          </div>
                        </td>
                        <td>{project.profile ? <Badge plain>{project.profile}</Badge> : '—'}</td>
                        <td>{workspaceState ? <StatusBadge status={workspaceState} label={labelForStatus(workspaceState)} /> : <span className="muted">未提供</span>}</td>
                        <td>
                          {lastReview ? (
                            <div className={styles.tablePrimary}>
                              <span className={page.reviewCell}>
                                {lastReview.verdict ? <VerdictBadge verdict={lastReview.verdict} /> : <StatusBadge status={lastReview.status} />}
                                <Link to={`/reviews/${encodeURIComponent(lastReview.id)}`}>!{lastReview.mr_iid} {lastReview.mr_title || ''}</Link>
                              </span>
                              <span className={styles.tableMeta}>
                                {labelForStatus(lastReview.status)}
                                {lastReview.created_at ? <> · <RelativeTime value={lastReview.created_at} /></> : null}
                              </span>
                            </div>
                          ) : <span className="muted">—</span>}
                        </td>
                        <td><span className={[page.expiry, EXPIRY_TONES[expiry.tone]].filter(Boolean).join(' ')}>{expiry.label}</span></td>
                        <td>
                          <Badge tone={webhookTone(webhookState)}>{labelForWebhookState(webhookState)}</Badge>
                          {project.hook_id ? <span className={styles.tableMeta}> · #{project.hook_id}</span> : null}
                        </td>
                        <td className="muted">{project.access ? ACCESS_LABELS[project.access] || project.access : '—'}</td>
                        <td>
                          {project.access === 'manage' ? (
                            <Switch
                              checked={project.enabled}
                              label={`启用 ${project.path_with_namespace}`}
                              disabled={pendingIds.has(project.id)}
                              onChange={(next) => { void toggleProject(project, next) }}
                            />
                          ) : <Badge tone={project.enabled ? 'ok' : 'neutral'}>{project.enabled ? '启用' : '停用'}</Badge>}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          ) : null}
          {projects.hasNextPage ? (
            <div className={styles.actionsRight}>
              <Button variant="secondary" onClick={projects.fetchNextPage} loading={projects.isFetchingNextPage}>加载更多项目</Button>
            </div>
          ) : null}
        </Panel>
      </div>

      {canRegister ? <RegisterProjectDialog open={registerOpen} onClose={() => setRegisterOpen(false)} /> : null}
    </main>
  )
}

