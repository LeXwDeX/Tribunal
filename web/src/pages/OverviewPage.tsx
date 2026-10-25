import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { getStats, listProjects, listProviders, listReviews } from '../api/api'
import type { Project, Provider, ReviewSummary, Stats } from '../api/types'
import { useCanWrite } from '../components/RoleContext'
import { ReviewTable } from '../components/ReviewTable'
import { TriggerReviewDialog } from '../components/TriggerReviewDialog'
import {
  Badge, Button, EmptyState, ErrorState, LoadingState, Metric, PageHeader, Panel, useToast,
} from '../components/ui'
import { elapsedSince, useNow, usePaginated } from '../lib/hooks'
import { formatCost, labelForStatus, readNumber } from '../lib/format'
import { formatMs, formatRatio, statNumber } from '../lib/stats'
import styles from './pages.module.css'
import ui from '../styles/ui.module.css'

interface Alert {
  id: string
  tone: 'danger' | 'warn'
  label: string
  text: string
  href: string
  action: string
}

type ReviewIssueKind = 'run' | 'publication' | 'retry'

interface ReviewIssue {
  review: ReviewSummary
  kind: ReviewIssueKind
}

function buildAlerts(projects: Project[], providers: Provider[], reviewIssues: ReviewIssue[]): Alert[] {
  const alerts: Alert[] = []
  const now = Date.now()
  for (const project of projects) {
    const expires = project.token_expires_at ? new Date(project.token_expires_at).getTime() : NaN
    if (!Number.isNaN(expires)) {
      const days = Math.ceil((expires - now) / 86_400_000)
      if (days <= 30) {
        alerts.push({
          id: `token-${project.id}`,
          tone: days <= 0 ? 'danger' : 'warn',
          label: days <= 0 ? '已过期' : '提醒',
          text: `${project.path_with_namespace} 的 token ${days <= 0 ? '已过期' : `将在 ${days} 天后过期`}。`,
          href: `/projects/${project.id}/settings`,
          action: '轮换 token',
        })
      }
    }
    const workspaceState = project.workspace?.state || project.workspace_state
    if (workspaceState === 'error') {
      alerts.push({
        id: `workspace-${project.id}`,
        tone: 'danger',
        label: '工作区',
        text: `${project.path_with_namespace} 工作区异常${project.workspace?.last_error ? `：${project.workspace.last_error}` : '。'}`,
        href: `/projects/${project.id}/workspace`,
        action: '查看工作区',
      })
    }
    if (!project.enabled) {
      alerts.push({
        id: `disabled-${project.id}`,
        tone: 'warn',
        label: '已停用',
        text: `${project.path_with_namespace} 已停用，webhook 事件将被忽略。`,
        href: `/projects/${project.id}/settings`,
        action: '查看设置',
      })
    }
  }
  for (const provider of providers) {
    const errors = readNumber(provider.stats_24h, ['errors']) ?? readNumber(provider as unknown as Record<string, unknown>, ['errors_24h'])
    const rate = readNumber(provider.stats_24h, ['error_rate']) ?? readNumber(provider as unknown as Record<string, unknown>, ['error_rate_24h'])
    if (errors && errors > 0) {
      alerts.push({
        id: `provider-${provider.name}`,
        tone: 'warn',
        label: 'Provider',
        text: `${provider.name} 近 24 小时有 ${errors} 次错误${typeof rate === 'number' ? `（错误率 ${formatRatio(rate)}）` : ''}。`,
        href: '/models',
        action: '查看模型',
      })
    }
  }
  for (const { review, kind } of reviewIssues) {
    const title = `!${review.mr_iid} ${review.mr_title || ''}`.trim()
    if (kind === 'run') {
      alerts.push({
        id: `review-${review.id}`,
        tone: 'danger',
        label: '评审失败',
        text: `${title} 执行失败。`,
        href: `/reviews/${encodeURIComponent(review.id)}`,
        action: '查看评审',
      })
    } else if (kind === 'publication') {
      alerts.push({
        id: `publication-${review.id}`,
        tone: 'danger',
        label: '发布失败',
        text: `${title} 发布失败${review.publication_error ? `：${review.publication_error}` : '，原因不可用'}。`,
        href: `/reviews/${encodeURIComponent(review.id)}`,
        action: '查看评审',
      })
    } else {
      const retryReason = review.publication_error || review.error
      alerts.push({
        id: `retry-${review.id}`,
        tone: 'warn',
        label: '发布重试中',
        text: `${title} ${review.status === 'failed' ? '执行失败，' : ''}发布正在重试${retryReason ? `：${retryReason}` : '。'}`,
        href: `/reviews/${encodeURIComponent(review.id)}`,
        action: '查看评审',
      })
    }
  }
  return alerts
}

function MetricStrip({ stats, loading, error, onRetry }: { stats?: Stats; loading: boolean; error: unknown; onRetry: () => void }) {
  if (loading) return <LoadingState label="正在加载近 7 天统计…" />
  if (error) {
    return (
      <Panel>
        <ErrorState error={error} onRetry={onRetry} title="统计数据加载失败" />
        <span className={styles.notice}>统计不可用时不会用示例数据代替。</span>
      </Panel>
    )
  }
  const runs = statNumber(stats, 'runs')
  const blocking = statNumber(stats, 'blocking')
  const blockingRatio = statNumber(stats, 'blocking_ratio')
  const published = statNumber(stats, 'published')
  const overturned = statNumber(stats, 'overturned_ratio')
  const cost = statNumber(stats, 'cost_micros')
  const avg = statNumber(stats, 'avg_duration_ms')
  const p95 = statNumber(stats, 'p95_duration_ms')
  return (
    <section aria-label="近 7 天指标" className={styles.statGrid}>
      <Metric label="运行次数" value={runs === null ? '不可用' : runs} note={blocking === null ? '包含评审与 onboarding · BLOCKING 次数不可用' : `包含评审与 onboarding · 其中 BLOCKING ${blocking} 次`} />
      <Metric label="BLOCKING 比例" value={formatRatio(blockingRatio)} note="按已结束评审计" />
      <Metric label="已发布问题" value={published === null ? '不可用' : published} note="GitLab 已发布条目" />
      <Metric label="规则丢弃比例" value={formatRatio(overturned)} note="规则 drop 占比，不是模型准确率" />
      <Metric label="成本" value={cost === null ? '不可用' : formatCost(cost)} note={`平均 ${formatMs(avg)} · P95 ${formatMs(p95)}`} />
    </section>
  )
}

export function OverviewPage() {
  const canRegister = useCanWrite()
  const toast = useToast()
  const now = useNow(1000)
  const [triggerOpen, setTriggerOpen] = useState(false)
  const [range] = useState(() => {
    const to = new Date()
    const from = new Date(to.getTime() - 7 * 24 * 60 * 60 * 1000)
    return { from: from.toISOString(), to: to.toISOString() }
  })

  const running = usePaginated<ReviewSummary>(
    ['reviews', 'overview', 'running'],
    (cursor) => listReviews({ status: 'running' }, cursor),
    'reviews',
    { refetchInterval: 10_000 },
  )
  const projectsPage = usePaginated<Project>(
    ['projects', 'overview'],
    (cursor) => listProjects({}, cursor),
    'projects',
  )
  // Provider health is a platform-admin view. Ordinary users must not receive
  // a false 403 alert from a query they cannot run.
  const providersEnabled = canRegister
  const providersPage = usePaginated<Provider>(
    ['providers', 'overview'],
    (cursor) => listProviders(cursor),
    'providers',
    { enabled: providersEnabled },
  )
  const failedRuns = usePaginated<ReviewSummary>(
    ['reviews', 'overview', 'failed'],
    (cursor) => listReviews({ status: 'failed' }, cursor),
    'reviews',
  )
  const failedPublications = usePaginated<ReviewSummary>(
    ['reviews', 'overview', 'publication-failed'],
    (cursor) => listReviews({ publication_state: 'failed' }, cursor),
    'reviews',
  )
  const pendingPublications = usePaginated<ReviewSummary>(
    ['reviews', 'overview', 'publication-pending'],
    (cursor) => listReviews({ publication_state: 'pending' }, cursor),
    'reviews',
  )
  const recent = usePaginated<ReviewSummary>(
    ['reviews', 'overview', 'recent'],
    (cursor) => listReviews({}, cursor),
    'reviews',
  )
  const stats = useQuery({
    queryKey: ['stats', 'overview', range.from, range.to],
    queryFn: () => getStats(range),
  })

  const projectNames = useMemo(
    () => new Map(projectsPage.items.map((project) => [project.id, project.path_with_namespace])),
    [projectsPage.items],
  )
  const reviewIssues = useMemo(() => {
    const issues = new Map<string, ReviewIssue>()
    for (const item of failedRuns.items) issues.set(item.id, { review: item, kind: 'run' })
    for (const item of failedPublications.items) issues.set(item.id, { review: item, kind: 'publication' })
    // A retryable publication failure stays pending with a bounded reason.
    // Completed workflows have an empty execution error, so the reason is the
    // only reliable signal. Normal pending publications have no reason.
    for (const item of pendingPublications.items) {
      if (item.publication_error) issues.set(item.id, { review: item, kind: 'retry' })
    }
    return Array.from(issues.values())
  }, [failedRuns.items, failedPublications.items, pendingPublications.items])
  const alerts = useMemo(
    () => buildAlerts(projectsPage.items, providersPage.items, reviewIssues),
    [projectsPage.items, providersPage.items, reviewIssues],
  )
  const issuesError = projectsPage.error || failedRuns.error || failedPublications.error
    || pendingPublications.error || (providersEnabled ? providersPage.error : null)
  const hasIssuesError = projectsPage.isError || failedRuns.isError || failedPublications.isError
    || pendingPublications.isError || (providersEnabled && providersPage.isError)
  const issuesPending = projectsPage.isPending || failedRuns.isPending || failedPublications.isPending
    || pendingPublications.isPending || (providersEnabled && providersPage.isPending)
  const reviewAlertsFetching = failedRuns.isFetchingNextPage || failedPublications.isFetchingNextPage || pendingPublications.isFetchingNextPage
  const hasMoreReviewAlerts = failedRuns.hasNextPage || failedPublications.hasNextPage || pendingPublications.hasNextPage
  const loadMoreReviewAlerts = () => {
    if (failedRuns.hasNextPage) failedRuns.fetchNextPage()
    if (failedPublications.hasNextPage) failedPublications.fetchNextPage()
    if (pendingPublications.hasNextPage) pendingPublications.fetchNextPage()
  }
  const runningCount = running.items.length
  const manageableProjects = projectsPage.items.filter((project) => project.access === 'manage')
  const canTrigger = manageableProjects.length > 0

  const onTriggered = (jobId?: string) => {
    setTriggerOpen(false)
    toast.push('success', jobId ? `已创建评审任务 ${jobId}。` : '已创建评审任务。')
    void running.refetch()
    void stats.refetch()
  }

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="总览"
          description={
            <>
              7 天统计 · {projectsPage.isPending ? '正在读取项目…' : `${projectsPage.items.length}${projectsPage.hasNextPage ? '+' : ''} 个已注册项目`} · {running.isPending ? '正在读取运行状态…' : `${runningCount} 个评审正在运行`}
            </>
          }
          actions={canRegister || canTrigger ? (
            <>
              {canRegister ? <Link to="/projects" style={secondaryLinkStyle}>注册项目</Link> : null}
              {canTrigger ? <Button variant="primary" onClick={() => setTriggerOpen(true)} disabled={projectsPage.isPending}>手动触发评审</Button> : null}
            </>
          ) : undefined}
        />

        {!canRegister && !canTrigger ? <p className={styles.notice}>当前账号没有平台管理权限，也没有可管理的项目；只显示可读数据。后端会独立校验权限。</p> : null}

        <MetricStrip
          stats={stats.data}
          loading={stats.isPending}
          error={stats.isError ? stats.error : null}
          onRetry={() => { void stats.refetch() }}
        />

        <div className={styles.split}>
          <Panel
            title="运行中"
            aside={<span>{runningCount}{running.hasNextPage ? '+' : ''} 个 · 每 10 秒刷新</span>}
            className={styles.mainColumn}
          >
            {running.isPending ? <LoadingState /> : null}
            {running.isError ? <ErrorState error={running.error} onRetry={running.retry} /> : null}
            {running.isSuccess && running.items.length === 0 ? <EmptyState title="当前没有运行中的评审" description="新的 webhook 或手动触发会出现在这里。" /> : null}
            <ul className={styles.list}>
              {running.items.map((review) => {
                const activeNode = review.nodes?.find((node) => node.status === 'running')
                return (
                  <li key={review.id}>
                    <Link className={styles.listItem} to={`/reviews/${encodeURIComponent(review.id)}`}>
                      <div className={styles.listMain}>
                        <span className={styles.listTitle}>!{review.mr_iid} {review.mr_title || '未命名 MR'}</span>
                        <span className={styles.listMeta}>
                          <span className="mono">{review.project_path || projectNames.get(review.project_id) || `项目 ${review.project_id}`}</span>
                          {' · '}{review.profile || '默认档位'}{' · '}{labelForStatus(review.status)}
                        </span>
                      </div>
                      <div className={styles.listMeta}>
                        {activeNode ? `节点 ${activeNode.id}` : '阶段未提供'}
                      </div>
                      <span className={styles.listMeta + ' tabular'}>已用时 {elapsedSince(review.created_at, now)}</span>
                    </Link>
                  </li>
                )
              })}
            </ul>
            <div className={styles.actionsRight}>
              {running.hasNextPage ? <Button variant="secondary" onClick={running.fetchNextPage} loading={running.isFetchingNextPage}>加载更多运行中评审</Button> : null}
              <Link to="/reviews" style={secondaryLinkStyle}>查看全部评审</Link>
            </div>
          </Panel>

          <Panel title="需要处理" className={styles.sideColumn}>
            {hasIssuesError ? (
              <ErrorState
                error={issuesError}
                onRetry={() => {
                  projectsPage.retry()
                  failedRuns.retry()
                  failedPublications.retry()
                  pendingPublications.retry()
                  if (providersEnabled) providersPage.retry()
                }}
                title="异常检查未完整加载"
              />
            ) : null}
            {alerts.length === 0 && !issuesPending ? (
              <EmptyState title="暂无需要处理的异常" description="检查了 token 过期、工作区状态、停用项目、provider 错误、评审执行失败和发布失败。" />
            ) : null}
            {issuesPending ? <LoadingState /> : null}
            {alerts.map((alert) => (
              <div key={alert.id} className={styles.alert}>
                <Badge tone={alert.tone}>{alert.label}</Badge>
                <div className={styles.alertBody}>
                  <span>{alert.text}</span>
                  <Link to={alert.href} className={styles.alertAction}>{alert.action}</Link>
                </div>
              </div>
            ))}
            {projectsPage.hasNextPage ? (
              <Button variant="secondary" className="smallButton" onClick={projectsPage.fetchNextPage} loading={projectsPage.isFetchingNextPage}>加载更多项目以继续检查</Button>
            ) : null}
            {hasMoreReviewAlerts ? (
              <Button variant="secondary" className="smallButton" onClick={loadMoreReviewAlerts} loading={reviewAlertsFetching}>加载更多告警数据</Button>
            ) : null}
          </Panel>
        </div>

        <Panel
          title="最近评审"
          aside={<Link to="/reviews">查看全部</Link>}
        >
          {recent.isPending ? <LoadingState /> : null}
          {recent.isError ? <ErrorState error={recent.error} onRetry={recent.retry} /> : null}
          {recent.isSuccess && recent.items.length === 0 ? <EmptyState title="还没有评审记录" description="注册项目并打开 MR，或在评审页手动触发。" /> : null}
          {recent.items.length ? <ReviewTable reviews={recent.items} projectNames={projectNames} /> : null}
          {recent.hasNextPage ? (
            <Button variant="secondary" onClick={recent.fetchNextPage} loading={recent.isFetchingNextPage}>加载更多</Button>
          ) : null}
        </Panel>
      </div>

      {canTrigger ? (
        <TriggerReviewDialog
          open={triggerOpen}
          onClose={() => setTriggerOpen(false)}
          projects={manageableProjects}
          onTriggered={onTriggered}
        />
      ) : null}
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
