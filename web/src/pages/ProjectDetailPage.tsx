import { useMemo, useState } from 'react'
import { Link, Navigate, useNavigate, useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { getProject, getStats, listProjects, listReviews } from '../api/api'
import type { Project, ReviewSummary } from '../api/types'
import { ExtensionsManager } from '../features/extensions/ExtensionsManager'
import { SettingsPanel } from '../features/project/SettingsPanel'
import { WorkspacePanel } from '../features/project/WorkspacePanel'
import { ReviewTable } from '../components/ReviewTable'
import {
  Badge, Button, EmptyState, ErrorState, LoadingState, Metric, PageHeader, Panel, StatusBadge, Tabs,
} from '../components/ui'
import { usePaginated } from '../lib/hooks'
import {
  formatCost, formatDate, labelForWebhookState, safeExternalUrl, shortSha, webhookTone,
} from '../lib/format'
import { formatMs, formatRatio, statNumber } from '../lib/stats'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'

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
  const reviews = usePaginated<ReviewSummary>(
    ['project', project.id, 'reviews'],
    (cursor) => listReviews({ project_id: project.id, limit: 20 }, cursor),
    'reviews',
  )
  const projectNames = useMemo(() => new Map([[project.id, project.path_with_namespace]]), [project.id, project.path_with_namespace])
  const workspaceState = project.workspace?.state || project.workspace_state
  const webhookState = project.webhook_state || (project.hook_id ? 'unknown' : 'disabled')

  const runs = statNumber(stats.data, 'runs')
  const blocking = statNumber(stats.data, 'blocking')
  const blockingRatio = statNumber(stats.data, 'blocking_ratio')
  const published = statNumber(stats.data, 'published')
  const overturned = statNumber(stats.data, 'overturned_ratio')
  const cost = statNumber(stats.data, 'cost_micros')
  const avg = statNumber(stats.data, 'avg_duration_ms')
  const p95 = statNumber(stats.data, 'p95_duration_ms')

  return (
    <div className={ui.pageStack}>
      <div className={styles.cards}>
        <section className={styles.infoCard}>
          <h2 className={styles.cardTitle}>项目信息</h2>
          <dl className={ui.definitionGrid}>
            <div><dt>项目 ID</dt><dd className="tabular">{project.id}</dd></div>
            <div><dt>默认分支</dt><dd className="mono">{project.default_branch || '—'}</dd></div>
            <div><dt>档位</dt><dd>{project.profile || '—'}</dd></div>
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
            <Link to={`/projects/${project.id}/workspace`} style={secondaryLinkStyle}>工作区</Link>
            <Link to={`/projects/${project.id}/settings`} style={secondaryLinkStyle}>设置</Link>
          </div>
        </section>
        <section className={styles.infoCard}>
          <h2 className={styles.cardTitle}>工作区摘要 <StatusBadge status={workspaceState} /></h2>
          <dl className={ui.definitionGrid}>
            <div><dt>状态</dt><dd>{workspaceState ? <StatusBadge status={workspaceState} /> : '项目详情未提供'}</dd></div>
            <div><dt>base sha</dt><dd className="mono">{project.workspace?.base_sha ? shortSha(project.workspace.base_sha) : '—'}</dd></div>
            <div><dt>索引状态</dt><dd>{project.workspace?.index_status || '—'}</dd></div>
            <div><dt>最近刷新</dt><dd>{project.workspace?.last_refresh_at ? formatDate(project.workspace.last_refresh_at) : '—'}</dd></div>
          </dl>
          {project.workspace?.last_error ? <span className={styles.resultError}>{project.workspace.last_error}</span> : null}
          <Link to={`/projects/${project.id}/workspace`} className={styles.listMeta}>查看完整工作区与上下文 →</Link>
        </section>
      </div>

      <Panel title="本项目近 7 天" aside="所有数值来自 stats API；缺失时显示不可用">
        {stats.isPending ? <LoadingState label="正在读取项目统计…" /> : null}
        {stats.isError ? <ErrorState error={stats.error} onRetry={() => { void stats.refetch() }} title="项目统计加载失败" /> : null}
        {!stats.isPending && !stats.isError ? (
          <div className={styles.statGrid}>
            <Metric label="运行次数" value={runs === null ? '不可用' : runs} note={`包含评审与 onboarding · BLOCKING ${blocking === null ? '不可用' : blocking} 次`} />
            <Metric label="BLOCKING 比例" value={formatRatio(blockingRatio)} note="来自 stats.blocking_ratio" />
            <Metric label="已发布问题" value={published === null ? '不可用' : published} note="来自 stats.published" />
            <Metric label="规则丢弃比例" value={formatRatio(overturned)} note="规则 drop 占比，不是模型准确率" />
            <Metric label="成本" value={cost === null ? '不可用' : formatCost(cost)} note={`平均 ${formatMs(avg)} · P95 ${formatMs(p95)}`} />
          </div>
        ) : null}
      </Panel>

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

function ProjectExtensionsPanel({ project }: { project: Project }) {
  const [subTab, setSubTab] = useState('mcp')
  return (
    <ExtensionsManager
      scope="project"
      projectId={project.id}
      canManage={project.access === 'manage'}
      projects={[project]}
      tab={subTab}
      onTabChange={setSubTab}
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
          title={<span className={styles.projectTitleMono}>{data.path_with_namespace}</span>}
          description={
            <>
              默认分支 <span className="mono">{data.default_branch || '—'}</span> · 档位 {data.profile || '—'} · 生成的上下文与索引只保存在平台内部
            </>
          }
          actions={
            <>
              {gitlabUrl ? <a href={gitlabUrl} target="_blank" rel="noopener noreferrer" style={secondaryLinkStyle}>在 GitLab 中打开</a> : null}
              {data.enabled ? <Badge tone="ok">已启用</Badge> : <Badge tone="neutral">已停用</Badge>}
            </>
          }
        />

        <Tabs
          items={projectTabs}
          value={tab}
          onChange={(next) => navigate(`/projects/${projectId}/${next}`)}
          label="项目详情"
          idPrefix="project-tab"
        />

        <div id={`project-tabpanel-${tab}`} role="tabpanel" aria-labelledby={`project-tab-${tab}`}>
          {tab === 'overview' ? <ProjectOverview project={data} /> : null}
          {tab === 'workspace' ? <WorkspacePanel projectId={data.id} canManage={data.access === 'manage'} /> : null}
          {tab === 'settings' ? <SettingsPanel project={data} /> : null}
          {tab === 'extensions' ? <ProjectExtensionsPanel project={data} /> : null}
        </div>
      </div>
    </main>
  )
}

const secondaryLinkStyle = {
  display: 'inline-flex',
  alignItems: 'center',
  minHeight: 44,
  padding: '0 16px',
  borderRadius: 8,
  border: '1px solid var(--control-border)',
  background: 'var(--surface)',
  color: 'var(--text)',
  fontWeight: 500,
  textDecoration: 'none',
} as const
