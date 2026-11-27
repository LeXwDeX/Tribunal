import { useEffect, useId, useMemo, useRef, useState, useSyncExternalStore, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { deleteAlertAck, getStats, listAlertAcks, listModels, listProjects, listReviews, putAlertAck } from '../api/api'
import { errorMessage } from '../api/client'
import type { AlertAckList, ModelHealth, Project, ReviewListItem, ReviewSummary, Stats } from '../api/types'
import { useCanRegisterProject, useCanWrite } from '../components/RoleContext'
import { ReviewTable } from '../components/ReviewTable'
import { TriggerReviewDialog } from '../components/TriggerReviewDialog'
import {
  Badge, Button, ButtonLink, EmptyState, ErrorState, LoadingState, Metric, PageHeader, Panel, Sparkline, classNames, useToast,
} from '../components/ui'
import { elapsedSince, useNow, usePaginated } from '../lib/hooks'
import { readNumber } from '../lib/format'
import { formatMs, formatRatio, statNumber } from '../lib/stats'
import { describeWorkspaceError } from '../lib/diagnostics'
import { REVIEW_STAGES, deriveReviewStage } from '../lib/reviewStage'
import {
  ALERT_KEY_PATTERN, REVIEW_FAILURE_FINGERPRINT, aggregateAckState, disabledFingerprint, isAcked, providerAcked,
  providerFingerprint, publicationFingerprint, retryFingerprint, tokenFingerprint, tokenStage, watermarkFingerprint,
  workspaceFingerprint, type AlertAcks,
} from '../lib/alertAcks'
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
  /** Stored on ack; compared with the stored value to decide read state. */
  fingerprint: string
  acked: boolean
}

const ALERT_ACKS_KEY = ['alert-acks']

type ReviewIssueKind = 'run' | 'publication' | 'retry'

// The elapsed clock ticks every second; keeping it inside the running list
// means the rest of the overview (stats, alerts, recent reviews) stays put.
function RunningReviewList({ items, projectNames }: {
  items: ReviewSummary[]
  projectNames: Map<number, string>
}) {
  const now = useNow(1000, items.length > 0)
  return (
    <ul className={overview.liveList}>
      {items.map((review) => {
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
  )
}

interface ReviewIssue {
  review: ReviewSummary
  kind: ReviewIssueKind
}

function buildAlerts(
  projects: Project[],
  providers: ModelHealth[],
  reviewIssues: ReviewIssue[],
  reviews: ReviewListItem[],
  incomplete: { items: ReviewListItem[]; hasMore: boolean },
  acks: AlertAcks,
): Alert[] {
  const alerts: Alert[] = []
  const now = Date.now()
  // Single-object alerts are read while the fingerprint is unchanged.
  const push = (alert: Omit<Alert, 'acked'>, acked = isAcked(acks, alert.id, alert.fingerprint)) => {
    alerts.push({ ...alert, acked })
  }
  for (const project of projects) {
    const expires = project.token_expires_at ? new Date(project.token_expires_at).getTime() : NaN
    if (!Number.isNaN(expires)) {
      const days = Math.ceil((expires - now) / 86_400_000)
      if (days <= 30) {
        const stage = tokenStage(days)
        push({
          id: `token-${project.id}`,
          fingerprint: tokenFingerprint(project, stage),
          tone: stage,
          label: days <= 0 ? '已过期' : '提醒',
          text: `${project.path_with_namespace} 的 token ${days <= 0 ? '已过期' : `将在 ${days} 天后过期`}。`,
          href: `/projects/${project.id}/settings`,
          action: '轮换 token',
        })
      }
    }
    const workspaceState = project.workspace?.state || project.workspace_state
    if (workspaceState === 'error') {
      push({
        id: `workspace-${project.id}`,
        fingerprint: workspaceFingerprint(project),
        tone: 'danger',
        label: '工作区',
        text: `${project.path_with_namespace} 工作区异常${project.workspace?.last_error ? `：${describeWorkspaceError(project.workspace.last_error)}` : '。'}`,
        href: `/projects/${project.id}/workspace`,
        action: '查看工作区',
      })
    }
    if (!project.enabled) {
      push({
        id: `disabled-${project.id}`,
        fingerprint: disabledFingerprint(project),
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
      const id = `provider-${provider.id}`
      push({
        id,
        fingerprint: providerFingerprint(errors, now),
        tone: 'warn',
        label: 'Provider',
        text: `${provider.display_name} 近 24 小时有 ${errors} 次错误${typeof rate === 'number' ? `（错误率 ${formatRatio(rate)}）` : ''}。`,
        href: '/models',
        action: '查看模型',
      }, providerAcked(acks.get(id), errors, now))
    }
  }
  for (const { review, kind } of reviewIssues) {
    const title = `!${review.mr_iid} ${review.mr_title || ''}`.trim()
    if (kind === 'run') {
      push({
        id: `review-${review.id}`,
        fingerprint: REVIEW_FAILURE_FINGERPRINT,
        tone: 'danger',
        label: '评审失败',
        text: `${title} 执行失败。`,
        href: `/reviews/${encodeURIComponent(review.id)}`,
        action: '查看评审',
      })
    } else if (kind === 'publication') {
      push({
        id: `publication-${review.id}`,
        fingerprint: publicationFingerprint(review.publication_error),
        tone: 'danger',
        label: '发布失败',
        text: `${title} 发布失败${review.publication_error ? `：${review.publication_error}` : '，原因不可用'}。`,
        href: `/reviews/${encodeURIComponent(review.id)}`,
        action: '查看评审',
      })
    } else {
      const retryReason = review.publication_error || review.error
      push({
        id: `retry-${review.id}`,
        fingerprint: retryFingerprint(retryReason),
        tone: 'warn',
        label: '发布重试中',
        text: `${title} ${review.status === 'failed' ? '执行失败，' : ''}发布正在重试${retryReason ? `：${retryReason}` : '。'}`,
        href: `/reviews/${encodeURIComponent(review.id)}`,
        action: '查看评审',
      })
    }
  }
  // Completed runs can settle without a workflow error and still be
  // incomplete. INCOMPLETE is counted server-side so reviews beyond the
  // loaded recent page are not missed; the first page plus hasNextPage is
  // enough to show "N+". Failed or skipped nodes have no server-side filter,
  // so they stay client-side over the loaded recent reviews and the alert
  // states that scope. Aggregate alerts store a finished_at watermark: after
  // an ack they count only hits that finished later.
  const completed = reviews.filter((item): item is ReviewSummary => 'status' in item && item.status === 'completed')
  const nodeIssues = completed.filter((item) => item.verdict !== 'INCOMPLETE'
    && (item.nodes || []).some((node) => node.status === 'failed' || node.status === 'skipped'))
  if (incomplete.items.length) {
    const stored = acks.get('reviews-incomplete')
    const state = aggregateAckState(incomplete.items, incomplete.hasMore, stored)
    alerts.push({
      id: 'reviews-incomplete',
      fingerprint: watermarkFingerprint(incomplete.items, stored),
      acked: state.acked,
      tone: 'warn',
      label: '评审未完成',
      text: `${state.sinceAck ? '自上次已读后新增 ' : ''}${state.count}${state.plus ? '+' : ''} 个已结束评审为 INCOMPLETE（未完成）：存在未解决的证据缺口或未执行节点，零 Finding 不代表没有问题。`,
      href: '/reviews?verdict=INCOMPLETE',
      action: '查看 INCOMPLETE 评审',
    })
  }
  if (nodeIssues.length) {
    const stored = acks.get('reviews-node-issues')
    const state = aggregateAckState(nodeIssues, false, stored)
    alerts.push({
      id: 'reviews-node-issues',
      fingerprint: watermarkFingerprint(nodeIssues, stored),
      acked: state.acked,
      tone: 'warn',
      label: '评审节点异常',
      text: `${state.sinceAck ? '自上次已读后新增 ' : ''}${state.count} 个已结束评审包含失败或跳过的节点，结果可能不完整（按最近 ${reviews.length} 条评审统计）。`,
      href: '/reviews?status=completed',
      action: '查看评审',
    })
  }
  return alerts
}

function trendPoints(stats: Stats | undefined, field: 'runs'): number[] {
  if (!Array.isArray(stats?.trend)) return []
  return stats.trend.map((day) => Number(day?.[field])).filter((value) => Number.isFinite(value))
}

function TrendNote({ points, children }: { points: number[]; children: ReactNode }) {
  return (
    <>
      {points.length >= 2 ? <Sparkline points={points} /> : null}
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
        <span className={styles.notice}>统计不可用时不显示示例数据。</span>
      </Panel>
    )
  }
  const runs = statNumber(stats, 'runs')
  const blocking = statNumber(stats, 'blocking')
  const blockingRatio = statNumber(stats, 'blocking_ratio')
  const published = statNumber(stats, 'published')
  const overturned = statNumber(stats, 'overturned_ratio')
  const avg = statNumber(stats, 'avg_duration_ms')
  const p95 = statNumber(stats, 'p95_duration_ms')
  return (
    <section aria-label="近 7 天指标" className={classNames(ui.metricStrip, overview.metrics)}>
      <Metric
        label="运行次数"
        value={runs === null ? '不可用' : runs}
        note={(
          <TrendNote points={trendPoints(stats, 'runs')}>
            {blocking === null ? '含评审与 onboarding · BLOCKING 次数不可用' : `含评审与 onboarding · BLOCKING ${blocking} 次`}
          </TrendNote>
        )}
      />
      <Metric label="BLOCKING 比例" value={formatRatio(blockingRatio)} note="BLOCKING 工作流数 / 所有工作流数" />
      <Metric label="发布类问题" value={published === null ? '不可用' : published} note="裁决为 publish 或 mark 的问题数，不等于发布成功。" />
      <Metric label="规则丢弃比例" value={formatRatio(overturned)} note="drop 问题数 / 有裁决的问题数，不是模型准确率" />
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
  // The alert count for INCOMPLETE verdicts must not depend on the loaded
  // recent page. Only the first filtered page and hasNextPage are used, so
  // the alert can show "N+"; the link opens the full filtered list.
  const incompleteReviews = usePaginated<ReviewListItem>(
    ['reviews', 'overview', 'incomplete'],
    (cursor) => listReviews({ status: 'completed', verdict: 'INCOMPLETE' }, cursor),
    'reviews',
  )
  const stats = useQuery({
    queryKey: ['stats', 'overview', range.from, range.to],
    queryFn: () => getStats(range),
  })
  // Acks are the caller's own read marks. Until they load no alert row is
  // rendered; if they fail every alert is shown without ack buttons, so a
  // failure can only show more, never hide an alert.
  const acksQuery = useQuery({ queryKey: ALERT_ACKS_KEY, queryFn: listAlertAcks, retry: false })
  const [showRead, setShowRead] = useState(false)
  const [pendingAcks, setPendingAcks] = useState<ReadonlySet<string>>(() => new Set())
  const alertIdPrefix = useId()
  const ackButtons = useRef(new Map<string, HTMLButtonElement>())
  const readToggle = useRef<HTMLButtonElement>(null)
  // After an ack write settles, focus goes back to the row's button, or to a
  // neighbour when the row was hidden, so keyboard focus never drops to body.
  const focusTarget = useRef<{ id: string; order: string[]; acked: boolean } | null>(null)

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
  const ackMap = useMemo<AlertAcks>(
    // A failed refetch keeps the stale data; only a successful read may hide
    // alerts, so the failure path shows every alert as the notice says.
    () => new Map((acksQuery.isSuccess ? acksQuery.data.acks || [] : []).map((ack) => [ack.key, ack.fingerprint])),
    [acksQuery.isSuccess, acksQuery.data],
  )
  const alerts = useMemo(
    () => buildAlerts(
      projectsPage.items,
      providersPage.data?.models || [],
      reviewIssues,
      recent.items,
      { items: incompleteReviews.items, hasMore: incompleteReviews.hasNextPage },
      ackMap,
    ),
    [projectsPage.items, providersPage.data, reviewIssues, recent.items, incompleteReviews.items, incompleteReviews.hasNextPage, ackMap],
  )
  const acksSettled = !acksQuery.isPending
  const acksReady = acksQuery.isSuccess
  const unreadCount = alerts.filter((alert) => !alert.acked).length
  const readCount = alerts.length - unreadCount
  const showingRead = showRead && readCount > 0
  const visibleAlerts = acksSettled ? alerts.filter((alert) => !alert.acked || showingRead) : []
  useEffect(() => {
    if (readCount === 0 && showRead) setShowRead(false)
  }, [readCount, showRead])
  useEffect(() => {
    const target = focusTarget.current
    if (!target || pendingAcks.has(target.id)) return
    const current = alerts.find((alert) => alert.id === target.id)
    // Wait until the ack cache reflects the write before moving focus.
    if (current && current.acked !== target.acked) return
    focusTarget.current = null
    const active = document.activeElement
    const own = ackButtons.current.get(target.id)
    if (active && active !== document.body && active !== own) return
    if (own) {
      own.focus()
      return
    }
    const index = target.order.indexOf(target.id)
    const neighbours = [...target.order.slice(index + 1), ...target.order.slice(0, Math.max(index, 0)).reverse()]
    for (const id of neighbours) {
      const button = ackButtons.current.get(id)
      if (button && !button.disabled) {
        button.focus()
        return
      }
    }
    readToggle.current?.focus()
  })
  const writeAck = async (alert: Alert, acked: boolean) => {
    const order = visibleAlerts.map((item) => item.id)
    setPendingAcks((current) => new Set(current).add(alert.id))
    try {
      if (acked) {
        const saved = await putAlertAck(alert.id, alert.fingerprint)
        queryClient.setQueryData<AlertAckList>(ALERT_ACKS_KEY, (current) => ({
          acks: [saved, ...(current?.acks || []).filter((ack) => ack.key !== saved.key)],
        }))
        focusTarget.current = { id: alert.id, order, acked: true }
      } else {
        await deleteAlertAck(alert.id)
        queryClient.setQueryData<AlertAckList>(ALERT_ACKS_KEY, (current) => ({
          acks: (current?.acks || []).filter((ack) => ack.key !== alert.id),
        }))
        focusTarget.current = { id: alert.id, order, acked: false }
      }
    } catch (error) {
      focusTarget.current = { id: alert.id, order, acked: alert.acked }
      toast.push('error', `${acked ? '标记已读' : '撤销已读'}失败：${errorMessage(error)}`)
    } finally {
      setPendingAcks((current) => {
        const next = new Set(current)
        next.delete(alert.id)
        return next
      })
    }
  }
  const issuesError = projectsPage.error || failedRuns.error || failedPublications.error
    || pendingPublications.error || incompleteReviews.error || (providersEnabled ? providersPage.error : null)
  const hasIssuesError = projectsPage.isError || failedRuns.isError || failedPublications.isError
    || pendingPublications.isError || incompleteReviews.isError || (providersEnabled && providersPage.isError)
  // Cached queries paused offline are neither pending nor fetching.
  const pausedIssues = useSyncExternalStore(
    queryClient.getQueryCache().subscribe,
    () => [
      ['projects', 'overview'],
      ['reviews', 'overview', 'failed'],
      ['reviews', 'overview', 'publication-failed'],
      ['reviews', 'overview', 'publication-pending'],
      ['reviews', 'overview', 'incomplete'],
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
    || incompleteReviews.isPending || incompleteReviews.isFetching
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
              ? '还没有登记项目，可注册第一个。'
              : '当前账号没有可管理的项目，可注册自己的项目或等待管理员分配。只显示可读数据。'}
            后端会独立校验权限。
          </p>
        ) : null}

        <MetricStrip
          stats={stats.data}
          loading={stats.isPending}
          error={stats.isError ? stats.error : null}
          onRetry={() => { void stats.refetch() }}
        />

          <Panel
            flush
            title="告警"
            aside={(
              <>
                <span>检查范围：token 过期、工作区异常、停用项目{providersEnabled ? '、provider 错误' : ''}、评审执行失败和发布失败、已完成但未完成（服务端计数）或含失败/跳过节点（按最近加载的评审计数）的评审</span>
                {acksSettled && unreadCount ? <span>{unreadCount}{hasMoreIssues ? '+' : ''} 项</span> : null}
                {acksReady && readCount ? (
                  <Button ref={readToggle} variant="ghost" size="sm" aria-pressed={showingRead} onClick={() => setShowRead(!showingRead)}>显示已读（{readCount}）</Button>
                ) : null}
              </>
            )}
          >
            {hasIssuesError ? (
              <div className={overview.state}>
                <ErrorState
                  error={issuesError}
                  onRetry={() => {
                    for (const source of [projectsPage, failedRuns, failedPublications, pendingPublications, incompleteReviews]) {
                      if (source.isError && !source.isFetching && !pausedIssues) source.retry()
                    }
                    if (providersEnabled && providersPage.isError && !providersPage.isFetching && !pausedIssues) void providersPage.refetch()
                  }}
                  title="异常检查未完整加载"
                />
              </div>
            ) : null}
            {acksQuery.isError ? (
              <p role="status" className={overview.ackNotice}>已读记录加载失败，暂时显示全部告警，不能标记已读。</p>
            ) : null}
            {acksSettled && !issuesPending && !hasIssuesError && !hasMoreIssues && alerts.length === 0 ? (
              <p className={overview.compactState}>无异常</p>
            ) : null}
            {acksSettled && !issuesPending && !hasIssuesError && !hasMoreIssues && alerts.length > 0 && unreadCount === 0 && !showingRead ? (
              <p className={overview.compactState}>无未读告警</p>
            ) : null}
            {issuesPending || !acksSettled ? <div className={overview.state}><LoadingState label={pausedIssues ? '等待网络连接以继续异常检查…' : undefined} /></div> : null}
            {visibleAlerts.map((alert) => {
              const textId = `${alertIdPrefix}-${alert.id}`
              const ackable = acksReady && ALERT_KEY_PATTERN.test(alert.id)
              return (
                <div key={alert.id} className={classNames(overview.alert, alert.acked && overview.alertRead)}>
                  <Badge tone={alert.tone}>{alert.label}</Badge>
                  <div className={overview.alertBody}>
                    <span id={textId}>{alert.text}</span>
                    <Link to={alert.href} className={overview.alertAction}>{alert.action}</Link>
                  </div>
                  {ackable ? (
                    <Button
                      ref={(element) => {
                        if (element) ackButtons.current.set(alert.id, element)
                        else ackButtons.current.delete(alert.id)
                      }}
                      variant="secondary"
                      size="sm"
                      className={overview.alertAck}
                      loading={pendingAcks.has(alert.id)}
                      aria-label={`${alert.acked ? '撤销已读' : '标记已读'}：${alert.label}`}
                      aria-describedby={textId}
                      onClick={() => { void writeAck(alert, !alert.acked) }}
                    >
                      {alert.acked ? '撤销已读' : '已读'}
                    </Button>
                  ) : null}
                </div>
              )
            })}
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
                {running.isFetching && !running.isFetchingNextPage && !pausedRunning
                  ? <span className="small muted"> · 刷新中…</span>
                  : null}
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
              <RunningReviewList items={running.items} projectNames={projectNames} />
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
