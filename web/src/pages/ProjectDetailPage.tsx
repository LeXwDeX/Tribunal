import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, Navigate, useNavigate, useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { getProject, getStats, listProjects, listReviews } from '../api/api'
import type { Project, ReviewListItem } from '../api/types'
import { ExtensionsManager } from '../features/extensions/ExtensionsManager'
import { SettingsPanel } from '../features/project/SettingsPanel'
import { WorkspacePanel } from '../features/project/WorkspacePanel'
import { ReviewTable } from '../components/ReviewTable'
import {
  ActionHint, Badge, Button, ButtonLink, ConfirmDialog, EmptyState, ErrorState, LoadingState, Metric, PageHeader, Panel,
  StatusBadge, Tabs,
} from '../components/ui'
import { usePaginated } from '../lib/hooks'
import {
  formatDate, labelForWebhookState, safeExternalUrl, shortSha, webhookTone,
} from '../lib/format'
import { formatMs, formatRatio, statNumber } from '../lib/stats'
import { describeWorkspaceError } from '../lib/diagnostics'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'
import page from './projects.module.css'

const projectTabs = [
  { key: 'overview', label: '概览', panelId: 'project-tabpanel-overview' },
  { key: 'workspace', label: '工作区与上下文', panelId: 'project-tabpanel-workspace' },
  { key: 'settings', label: '设置', panelId: 'project-tabpanel-settings' },
  { key: 'extensions', label: '扩展绑定', panelId: 'project-tabpanel-extensions' },
]

function ProjectOverview({ project }: { project: Project }) {
  const [range] = useState(() => {
    const to = new Date()
    const from = new Date(to.getTime() - 7 * 24 * 60 * 60 * 1000)
    return { from: from.toISOString(), to: to.toISOString() }
  })
  const stats = useQuery({
    queryKey: ['stats', 'project', project.id, range.from, range.to],
    queryFn: () => getStats({ ...range, project_id: project.id }),
  })
  const reviews = usePaginated<ReviewListItem>(
    ['project', project.id, 'reviews'],
    (cursor) => listReviews({ project_id: project.id, limit: 20 }, cursor),
    'reviews',
  )
  const projectNames = useMemo(() => new Map([[project.id, project.path_with_namespace]]), [project.id, project.path_with_namespace])
  const workspace = project.workspace
  const workspaceState = workspace?.state || project.workspace_state
  // The indexer refusing a path by policy is a safe configuration outcome, not
  // a fault. Only a workspace state of error uses the danger color.
  const workspaceErrorText = workspace?.last_error ? describeWorkspaceError(workspace.last_error) : null
  const workspaceErrorClass = workspace?.state === 'error' ? styles.resultError : styles.resultHint
  const webhookState = project.webhook_state || (project.hook_id ? 'unknown' : 'disabled')

  const runs = statNumber(stats.data, 'runs')
  const blocking = statNumber(stats.data, 'blocking')
  const blockingRatio = statNumber(stats.data, 'blocking_ratio')
  const published = statNumber(stats.data, 'published')
  const overturned = statNumber(stats.data, 'overturned_ratio')
  const avg = statNumber(stats.data, 'avg_duration_ms')
  const p95 = statNumber(stats.data, 'p95_duration_ms')

  return (
    <div className={ui.pageStack}>
      <section className={ui.stack} aria-label="本项目近 7 天">
        {stats.isPending ? <LoadingState label="正在读取项目统计…" /> : null}
        {stats.isError ? <ErrorState error={stats.error} onRetry={() => { void stats.refetch() }} title="项目统计加载失败" /> : null}
        {!stats.isPending && !stats.isError ? (
          <>
            <div className={ui.metricStrip}>
            <Metric label="运行次数" value={runs === null ? '不可用' : runs} note={`含评审与 onboarding · BLOCKING ${blocking === null ? '不可用' : blocking} 次`} />
            <Metric label="BLOCKING 比例" value={formatRatio(blockingRatio)} note="来自 stats.blocking_ratio" />
            <Metric label="已发布问题" value={published === null ? '不可用' : published} note="来自 stats.published" />
            <Metric label="规则丢弃比例" value={formatRatio(overturned)} note="规则 drop 占比，不是模型准确率" />
            <Metric label="耗时 平均 / P95" value={avg === null ? '不可用' : formatMs(avg)} note={`有有效起止时间的工作流 · P95 ${formatMs(p95)}`} />
            </div>
            <ActionHint>近 7 天 · 数值来自 stats API，缺失显示不可用。</ActionHint>
          </>
        ) : null}
      </section>

      <div className={page.overviewCards}>
          <Panel title="基本信息">
            <dl className={ui.definitionGrid}>
              <div><dt>项目 ID</dt><dd className="tabular">{project.id}</dd></div>
              <div><dt>默认分支</dt><dd className="mono">{project.default_branch || '—'}</dd></div>
              <div><dt>档位</dt><dd>{project.profile ? <Badge plain>{project.profile}</Badge> : '—'}</dd></div>
              <div><dt>启用</dt><dd>{project.enabled ? '启用' : '停用'}</dd></div>
              <div><dt>token 过期</dt><dd>{project.token_expires_at ? formatDate(project.token_expires_at) : '未提供'}</dd></div>
              <div><dt>token scopes</dt><dd className="mono">{project.token_scopes?.join(', ') || '不可用'}</dd></div>
              <div>
                <dt>webhook</dt>
                <dd>
                  <Badge tone={webhookTone(webhookState)}>{labelForWebhookState(webhookState)}</Badge>
                  {project.hook_id ? <span className="mono"> #{project.hook_id}</span> : null}
                  <span className="muted"> · {project.webhook_managed_by === 'crc' ? '本服务' : '手动或外部'}</span>
                </dd>
              </div>
              <div><dt>更新时间</dt><dd>{project.updated_at ? formatDate(project.updated_at) : '—'}</dd></div>
            </dl>
            {project.webhook_error ? <span className={styles.resultError}>{project.webhook_error}</span> : null}
            <div className={styles.cardActions}>
              <ButtonLink to={`/projects/${project.id}/workspace`}>工作区</ButtonLink>
              <ButtonLink to={`/projects/${project.id}/settings`}>设置</ButtonLink>
            </div>
          </Panel>
          <Panel title="工作区摘要" aside={workspaceState ? <StatusBadge status={workspaceState} /> : undefined}>
            <dl className={ui.definitionGrid}>
              <div><dt>状态</dt><dd>{workspaceState ? <StatusBadge status={workspaceState} /> : '项目详情未提供'}</dd></div>
              <div><dt>base sha</dt><dd className="mono">{project.workspace?.base_sha ? shortSha(project.workspace.base_sha) : '—'}</dd></div>
              <div><dt>索引状态</dt><dd>{project.workspace?.index_status || '—'}</dd></div>
              <div><dt>最近刷新</dt><dd>{project.workspace?.last_refresh_at ? formatDate(project.workspace.last_refresh_at) : '—'}</dd></div>
            </dl>
            {workspaceErrorText ? <span className={workspaceErrorClass}>{workspaceErrorText}</span> : null}
            <Link to={`/projects/${project.id}/workspace`} className={styles.listMeta}>查看完整工作区与上下文 →</Link>
          </Panel>
      </div>
      <Panel title="最近评审" aside={<Link to={`/reviews?project_id=${project.id}`}>查看全部</Link>}>
        {reviews.isPending ? <LoadingState /> : null}
        {reviews.isError ? <ErrorState error={reviews.error} onRetry={reviews.retry} /> : null}
        {reviews.isSuccess && reviews.items.length === 0 ? <EmptyState title="这个项目还没有评审记录" description="打开 MR 或手动触发一次评审。" /> : null}
        {reviews.items.length ? <ReviewTable reviews={reviews.items} projectNames={projectNames} /> : null}
        {reviews.hasNextPage ? <Button variant="secondary" onClick={reviews.fetchNextPage} loading={reviews.isFetchingNextPage}>加载更多评审</Button> : null}
      </Panel>
    </div>
  )
}

function ProjectExtensionsPanel({ project, onBindingsDirtyChange }: {
  project: Project
  onBindingsDirtyChange: (dirty: boolean) => void
}) {
  const [subTab, setSubTab] = useState('mcp')
  return (
    <ExtensionsManager
      key={project.id}
      scope="project"
      projectId={project.id}
      canManage={project.access === 'manage'}
      projects={[project]}
      tab={subTab}
      onTabChange={setSubTab}
      onBindingsDirtyChange={onBindingsDirtyChange}
    />
  )
}

export function ProjectDetailPage() {
  const { id = '', tab = 'overview' } = useParams()
  const navigate = useNavigate()
  const projectId = Number(id)
  const validTab = projectTabs.some((item) => item.key === tab)
  const project = useQuery({
    queryKey: ['project', projectId],
    queryFn: () => getProject(projectId),
    enabled: Number.isInteger(projectId) && projectId > 0,
  })

  // Keep the extensions panel mounted while a route-driven tab change would
  // discard unsaved block binding drafts, and restore the URL until confirmed.
  const [shownTab, setShownTab] = useState(tab)
  // The dirty flag belongs to the project that produced it: navigating straight
  // from another project's extensions tab must not trip this project's guard.
  const [dirtyExtensionsProject, setDirtyExtensionsProject] = useState<number | null>(null)
  const [pendingOuterTab, setPendingOuterTab] = useState<string | null>(null)
  const guardExtensions = shownTab === 'extensions' && dirtyExtensionsProject === projectId
  const onExtensionsDirtyChange = useCallback((dirty: boolean) => {
    setDirtyExtensionsProject(dirty ? projectId : null)
  }, [projectId])

  useEffect(() => {
    if (tab === shownTab) return
    if (guardExtensions) {
      // Push the routed tab back so the URL matches the panel on screen while
      // the requested entry stays in history, then ask before discarding.
      setPendingOuterTab(tab)
      navigate(`/projects/${projectId}/${shownTab}`)
      return
    }
    setShownTab(tab)
  }, [tab, shownTab, guardExtensions, navigate, projectId])

  const changeTab = (next: string) => {
    if (next === shownTab) return
    if (guardExtensions) {
      setPendingOuterTab(next)
      return
    }
    setShownTab(next)
    navigate(`/projects/${projectId}/${next}`)
  }
  const discardExtensionsAndChangeTab = () => {
    const next = pendingOuterTab
    setPendingOuterTab(null)
    setDirtyExtensionsProject(null)
    if (!next) return
    setShownTab(next)
    navigate(`/projects/${projectId}/${next}`)
  }

  if (!Number.isInteger(projectId) || projectId <= 0) {
    return <main className={ui.page}><ErrorState error={new Error('URL 中的项目 ID 无效。')} title="无法打开项目" /></main>
  }
  if (!validTab) return <Navigate to={`/projects/${projectId}/overview`} replace />

  if (project.isPending) return <main className={ui.page}><LoadingState label="正在读取项目详情…" /></main>
  if (project.isError) {
    return (
      <main className={ui.page}>
        <div className={ui.pageStack}>
          <Link to="/projects">← 返回项目列表</Link>
          <ErrorState error={project.error} onRetry={() => { void project.refetch() }} title="项目详情加载失败" />
        </div>
      </main>
    )
  }

  const data = project.data
  const gitlabUrl = safeExternalUrl(data.web_url)

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <div className={styles.docMeta}>
          <Link to="/projects">项目</Link>
          <span>/</span>
          <span className="mono">{data.path_with_namespace}</span>
        </div>

        <PageHeader
          title={<span className={page.projectTitle}>{data.path_with_namespace}</span>}
          description={
            <>
              默认分支 <span className="mono">{data.default_branch || '—'}</span> · 档位 {data.profile || '—'}
              {data.access ? <> · {data.access === 'manage' ? '可管理' : '只读'}</> : null} · 生成的上下文与索引只保存在平台内部
            </>
          }
          meta={data.enabled ? <Badge tone="ok">已启用</Badge> : <Badge tone="neutral">已停用</Badge>}
          actions={gitlabUrl ? <ButtonLink href={gitlabUrl} target="_blank" rel="noopener noreferrer">在 GitLab 中打开</ButtonLink> : undefined}
        />

        <Tabs
          items={projectTabs}
          value={shownTab}
          onChange={changeTab}
          label="项目详情"
          idPrefix="project-tab"
        />

        <div id={`project-tabpanel-${shownTab}`} role="tabpanel" aria-labelledby={`project-tab-${shownTab}`}>
          {shownTab === 'overview' ? <ProjectOverview project={data} /> : null}
          {shownTab === 'workspace' ? <WorkspacePanel key={data.id} projectId={data.id} canManage={data.access === 'manage'} /> : null}
          {shownTab === 'settings' ? <SettingsPanel key={data.id} project={data} /> : null}
          {shownTab === 'extensions' ? (
            <ProjectExtensionsPanel project={data} onBindingsDirtyChange={onExtensionsDirtyChange} />
          ) : null}
        </div>
      </div>

      <ConfirmDialog
        open={Boolean(pendingOuterTab)}
        onClose={() => setPendingOuterTab(null)}
        onConfirm={discardExtensionsAndChangeTab}
        title="放弃未保存的绑定修改？"
        description="切换标签会丢弃块绑定矩阵中尚未保存的修改。"
        confirmLabel="放弃并切换"
      />
    </main>
  )
}
