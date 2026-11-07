import { useMemo, useState, useSyncExternalStore, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { getStats, listModels, listProjects, listReviews } from '../api/api'
import type { ModelHealth, Project, ReviewListItem, ReviewSummary, Stats } from '../api/types'
import { useCanRegisterProject, useCanWrite } from '../components/RoleContext'
import { ReviewTable } from '../components/ReviewTable'
import { TriggerReviewDialog } from '../components/TriggerReviewDialog'
import {
  Badge, Button, ButtonLink, EmptyState, ErrorState, LoadingState, Metric, PageHeader, Panel, Sparkline, classNames, useToast,
} from '../components/ui'
import { elapsedSince, useNow, usePaginated } from '../lib/hooks'
import { formatCost, readNumber } from '../lib/format'
import { formatMs, formatRatio, statNumber } from '../lib/stats'
import { describeWorkspaceError } from '../lib/diagnostics'
import { REVIEW_STAGES, deriveReviewStage } from '../lib/reviewStage'
import styles from './pages.module.css'
import ui from '../styles/ui.module.css'
import overview from './OverviewPage.module.css'

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

function buildAlerts(projects: Project[], providers: ModelHealth[], reviewIssues: ReviewIssue[]): Alert[] {
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
        text: `${project.path_with_namespace} 工作区异常${project.workspace?.last_error ? `：${describeWorkspaceError(project.workspace.last_error)}` : '。'}`,
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
    const errors = readNumber(provider.stats_24h, ['errors'])
    const rate = readNumber(provider.stats_24h, ['error_rate'])
    if (errors && errors > 0) {
      alerts.push({
        id: `provider-${provider.id}`,
        tone: 'warn',
        label: 'Provider',
        text: `${provider.display_name} 近 24 小时有 ${errors} 次错误${typeof rate === 'number' ? `（错误率 ${formatRatio(rate)}）` : ''}。`,
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

function trendPoints(stats: Stats | undefined, field: 'runs' | 'cost_micros'): number[] {
  if (!Array.isArray(stats?.trend)) return []
  return stats.trend.map((day) => Number(day?.[field])).filter((value) => Number.isFinite(value))
}

function TrendNote({ points, color, children }: { points: number[]; color?: string; children: ReactNode }) {
  return (
    <>
      {points.length >= 2 ? <Sparkline points={points} color={color} /> : null}
      <span className={overview.metricNoteText}>{children}</span>
    </>
  )
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
    <section aria-label="近 7 天指标" className={classNames(ui.metricStrip, overview.metrics)}>
      <Metric
        label="运行次数"
        value={runs === null ? '不可用' : runs}
        note={(
          <TrendNote points={trendPoints(stats, 'runs')}>
            {blocking === null ? '包含评审与 onboarding · BLOCKING 次数不可用' : `包含评审与 onboarding · 其中 BLOCKING ${blocking} 次`}
          </TrendNote>
        )}
      />
      <Metric label="BLOCKING 比例" value={formatRatio(blockingRatio)} note="BLOCKING 工作流数 / 所有工作流数" />
      <Metric label="发布类问题" value={published === null ? '不可用' : published} note="裁决为 publish 或 mark 的问题数，不代表实际发布成功" />
      <Metric label="规则丢弃比例" value={formatRatio(overturned)} note="drop 问题数 / 有裁决的问题数，不是模型准确率" />
      <Metric
        label="成本"
        value={cost === null ? '不可用' : formatCost(cost)}
        note={<TrendNote points={trendPoints(stats, 'cost_micros')} color="var(--warn-dot)"><span title="接口无法区分未配置价格和实际零成本">需在 provider 配置 price 才统计成本；无法区分未配置价格和实际零成本</span></TrendNote>}
      />
      <Metric label="耗时 平均 / P95" value={avg === null ? '不可用' : formatMs(avg)} note={`有有效起止时间的工作流 · P95 ${formatMs(p95)}`} />
    </section>
  )
}

function StageTrack({ stage }: { stage: number }) {
  return (
    <ol className={overview.stageTrack} aria-label={`阶段：${REVIEW_STAGES[stage]}`}>
      {REVIEW_STAGES.map((name, index) => (
        <li
          key={name}
          className={classNames(overview.stage, index < stage && overview.stageDone, index === stage && overview.stageNow)}
          aria-current={index === stage ? 'step' : undefined}
        >
          <i aria-hidden="true" />
          {name}
        </li>
      ))}
    </ol>
  )
}

export function OverviewPage() {
  const canRegister = useCanRegisterProject()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [triggerOpen, setTriggerOpen] = useState(false)
  const [range] = useState(() => {
    const to = new Date()
    const from = new Date(to.getTime() - 7 * 24 * 60 * 60 * 1000)
    return { from: from.toISOString(), to: to.toISOString() }
  })

  // AppShell owns the 10s polling for this shared query key; adding another
  // refetchInterval here would create a second observer timer.
  const running = usePaginated<ReviewSummary>(
    ['reviews', 'overview', 'running'],
    (cursor) => listReviews({ status: 'running' }, cursor),
    'reviews',
  )
  // Only tick the elapsed clock while at least one review is running.
  const now = useNow(1000, running.items.length > 0)
  const projectsPage = usePaginated<Project>(
    ['projects', 'overview'],
    (cursor) => listProjects({}, cursor),
    'projects',
  )
  // Provider health is a platform-admin view. Ordinary users must not receive
  // a false 403 alert from a query they cannot run.
  const providersEnabled = useCanWrite()
  const providersPage = useQuery({ queryKey: ['models', 'overview'], queryFn: listModels, enabled: providersEnabled, retry: false })
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
  const recent = usePaginated<ReviewListItem>(
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
    () => buildAlerts(projectsPage.items, providersPage.data?.models || [], reviewIssues),
    [projectsPage.items, providersPage.data, reviewIssues],
  )
  const issuesError = projectsPage.error || failedRuns.error || failedPublications.error
    || pendingPublications.error || (providersEnabled ? providersPage.error : null)
  const hasIssuesError = projectsPage.isError || failedRuns.isError || failedPublications.isError
    || pendingPublications.isError || (providersEnabled && providersPage.isError)
  // Cached queries paused offline are neither pending nor fetching.
  const pausedIssues = useSyncExternalStore(
    queryClient.getQueryCache().subscribe,
    () => [
      ['projects', 'overview'],
      ['reviews', 'overview', 'failed'],
      ['reviews', 'overview', 'publication-failed'],
      ['reviews', 'overview', 'publication-pending'],
      ...(providersEnabled ? [['models', 'overview']] : []),
    ].some((key) => queryClient.getQueryState(key)?.fetchStatus === 'paused'),
    () => false,
  )
  const pausedRunning = useSyncExternalStore(
    queryClient.getQueryCache().subscribe,
    () => queryClient.getQueryState(['reviews', 'overview', 'running'])?.fetchStatus === 'paused',
    () => false,
  )
  const issuesPending = pausedIssues || projectsPage.isPending || projectsPage.isFetching || failedRuns.isPending || failedRuns.isFetching
    || failedPublications.isPending || failedPublications.isFetching || pendingPublications.isPending || pendingPublications.isFetching
    || (providersEnabled && (providersPage.isPending || providersPage.isFetching))
  const reviewAlertsFetching = failedRuns.isFetchingNextPage || failedPublications.isFetchingNextPage || pendingPublications.isFetchingNextPage
  const reviewAlertsBlocked = pausedIssues || [failedRuns, failedPublications, pendingPublications]
    .some((source) => source.isFetching || source.isRefetchError)
  const hasMoreReviewAlerts = failedRuns.hasNextPage || failedPublications.hasNextPage || pendingPublications.hasNextPage
  const hasMoreIssues = projectsPage.hasNextPage || hasMoreReviewAlerts
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
              7 天统计 · {projectsPage.isPending ? '正在读取项目…' : `${projectsPage.items.length}${projectsPage.hasNextPage ? '+' : ''} 个已注册项目`} · {running.isPending ? '正在读取运行状态…' : `已加载 ${runningCount}${running.hasNextPage ? '+' : ''} 个运行中的评审`}
            </>
          }
          actions={canRegister || canTrigger ? (
            <>
              {canRegister ? <ButtonLink to="/projects" variant="primary">注册项目</ButtonLink> : null}
              {canTrigger ? <Button variant="primary" onClick={() => setTriggerOpen(true)} disabled={projectsPage.isPending}>手动触发评审</Button> : null}
            </>
          ) : undefined}
        />

        {projectsPage.isSuccess && !canTrigger ? (
          <p className={styles.notice}>
            {providersEnabled
              ? '还没有登记项目；可以注册第一个项目。'
              : '当前账号没有可管理的项目；可以注册自己的项目，或等待管理员分配。只显示可读数据。'}
            后端会独立校验权限。
          </p>
        ) : null}

        <MetricStrip
          stats={stats.data}
          loading={stats.isPending}
          error={stats.isError ? stats.error : null}
          onRetry={() => { void stats.refetch() }}
        />

          <Panel flush title="告警" aside={<><span>检查范围：token 过期、工作区异常、停用项目{providersEnabled ? '、provider 错误' : ''}、评审执行失败和发布失败</span>{alerts.length ? <span>{alerts.length}{hasMoreIssues ? '+' : ''} 项</span> : null}</>}>
            {hasIssuesError ? (
              <div className={overview.state}>
                <ErrorState
                  error={issuesError}
                  onRetry={() => {
                    for (const source of [projectsPage, failedRuns, failedPublications, pendingPublications]) {
                      if (source.isError && !source.isFetching && !pausedIssues) source.retry()
                    }
                    if (providersEnabled && providersPage.isError && !providersPage.isFetching && !pausedIssues) void providersPage.refetch()
                  }}
                  title="异常检查未完整加载"
                />
              </div>
            ) : null}
            {alerts.length === 0 && !issuesPending && !hasIssuesError && !hasMoreIssues ? (
              <p className={overview.compactState}>无异常</p>
            ) : null}
            {issuesPending ? <div className={overview.state}><LoadingState label={pausedIssues ? '等待网络连接以继续异常检查…' : undefined} /></div> : null}
            {alerts.map((alert) => (
              <div key={alert.id} className={overview.alert}>
                <Badge tone={alert.tone}>{alert.label}</Badge>
                <div className={overview.alertBody}>
                  <span>{alert.text}</span>
                  <Link to={alert.href} className={overview.alertAction}>{alert.action}</Link>
                </div>
              </div>
            ))}
            {hasMoreIssues ? (
              <div className={overview.panelFooter}>
                {projectsPage.hasNextPage ? (
                  <Button variant="secondary" className={ui.smallButton} onClick={projectsPage.fetchNextPage} loading={projectsPage.isFetchingNextPage} disabled={pausedIssues || projectsPage.isFetching || projectsPage.isRefetchError}>加载更多项目以继续检查</Button>
                ) : null}
                {hasMoreReviewAlerts ? (
                  <Button variant="secondary" className={ui.smallButton} onClick={loadMoreReviewAlerts} loading={reviewAlertsFetching} disabled={reviewAlertsBlocked}>加载更多告警数据</Button>
                ) : null}
              </div>
            ) : null}
          </Panel>

          <Panel
            flush
            title="运行中"
            aside={(
              <span>
                已加载 {runningCount}{running.hasNextPage ? '+' : ''} 个 · 每 10 秒刷新
                {running.isFetching && running.isSuccess && !pausedRunning ? <span role="status"> · 刷新中…</span> : null}
              </span>
            )}
          >
            {running.isPending || pausedRunning ? <div className={overview.state}><LoadingState label={pausedRunning ? '等待网络连接以读取运行状态…' : undefined} /></div> : null}
            {running.isError ? <div className={overview.state}><ErrorState error={running.error} onRetry={running.retry} /></div> : null}
            {running.isSuccess && !pausedRunning && !running.hasNextPage && running.items.length === 0 ? (
              <p className={overview.compactState}>当前没有运行中的评审</p>
            ) : null}
            {running.isSuccess && !pausedRunning && running.hasNextPage && running.items.length === 0 ? (
              <p className={overview.compactState}>还有未加载的运行中评审数据。</p>
            ) : null}
            {running.items.length ? (
              <ul className={overview.liveList}>
                {running.items.map((review) => {
                  const stage = deriveReviewStage(review.nodes)
                  const title = review.mr_title || `!${review.mr_iid}`
                  return (
                    <li key={review.id} className={overview.liveItem}>
                      <div className={overview.primary}>
                        <Link className={overview.liveTitle} to={`/reviews/${encodeURIComponent(review.id)}`} title={title}>{title}</Link>
                        <span className={overview.meta}>
                          <span className={overview.mrRef}>!{review.mr_iid}</span>
                          <span className="mono">{review.project_path || projectNames.get(review.project_id) || `项目 ${review.project_id}`}</span>
                          <span aria-hidden="true">·</span>
                          <Badge plain>{review.profile || '默认档位'}</Badge>
                        </span>
                      </div>
                      <span className={overview.elapsed}>已用时 {elapsedSince(review.created_at, now)}</span>
                      {stage === null ? null : <StageTrack stage={stage} />}
                    </li>
                  )
                })}
              </ul>
            ) : null}
            <div className={overview.panelFooter}>
              {running.hasNextPage ? <Button variant="secondary" className={ui.smallButton} onClick={running.fetchNextPage} loading={running.isFetchingNextPage} disabled={running.isFetching || pausedRunning || running.isRefetchError}>加载更多运行中评审</Button> : null}
              <ButtonLink to="/reviews" className={ui.smallButton}>查看全部评审</ButtonLink>
            </div>
          </Panel>

        <Panel
          flush
          title="最近评审"
          aside={<Link to="/reviews">查看全部</Link>}
        >
          {recent.isPending ? <div className={overview.state}><LoadingState /></div> : null}
          {recent.isError ? <div className={overview.state}><ErrorState error={recent.error} onRetry={recent.retry} /></div> : null}
          {recent.isSuccess && recent.items.length === 0 ? (
            <div className={overview.state}><EmptyState title="还没有评审记录" description="注册项目并打开 MR，或在评审页手动触发。" /></div>
          ) : null}
          {recent.items.length ? <ReviewTable reviews={recent.items} projectNames={projectNames} /> : null}
          {recent.hasNextPage ? (
            <div className={ui.loadMore}>
              <Button variant="secondary" className={ui.smallButton} onClick={recent.fetchNextPage} loading={recent.isFetchingNextPage}>加载更多</Button>
            </div>
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
