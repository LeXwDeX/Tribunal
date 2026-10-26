import { useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  cancelReview, getProject, getReview, getReviewGraph, listFindings, listProjects, republishReview, setFindingAnnotation,
} from '../api/api'
import type { Finding, FindingAnnotation, FindingsSummary, ReviewDetail } from '../api/types'
import { Icon } from '../components/Icons'
import { TriggerReviewDialog } from '../components/TriggerReviewDialog'
import {
  Badge, Button, ButtonLink, ConfirmDialog, EmptyState, ErrorState, LoadingState, Panel, PanelBody, PublicationBadge,
  RelativeTime, Segmented, SeverityBadge, StatusBadge, classNames, useToast,
} from '../components/ui'
import { ExecutionGraph } from '../features/review/ExecutionGraph'
import { NodeDrawer } from '../features/review/NodeDrawer'
import { findingFunnel, verdictCardState } from '../features/review/reviewView'
import { useReviewStream } from '../features/review/useReviewStream'
import { usePaginated } from '../lib/hooks'
import {
  formatCost, formatDuration, formatJson, formatNumber, formatTokens, labelForChallenge, labelForDecision,
  labelForTrigger, safeExternalUrl, shortSha, stringifyUnknown,
} from '../lib/format'
import ui from '../styles/ui.module.css'
import rv from '../features/review/review.module.css'
import type { GraphNode } from '../api/types'

const severityOptions = [['', '全部级别'], ['P0', 'P0'], ['P1', 'P1'], ['P2', 'P2'], ['info', 'info']]
const annotationOptions: Array<{ value: FindingAnnotation; label: string }> = [
  { value: 'valid', label: '有效' },
  { value: 'invalid', label: '无效' },
  { value: 'uncertain', label: '未确定' },
  { value: '', label: '未标注' },
]
const annotationLabels: Record<FindingAnnotation, string> = {
  valid: '有效',
  invalid: '无效',
  uncertain: '未确定',
  '': '未标注',
}

function annotationTone(value: FindingAnnotation): 'ok' | 'danger' | 'warn' | 'neutral' {
  if (value === 'valid') return 'ok'
  if (value === 'invalid') return 'danger'
  if (value === 'uncertain') return 'warn'
  return 'neutral'
}

// Chain colors read as the finding's risk: a confirmed challenge or a
// published finding is red, a refuted one green, an uncertain one amber.
const challengeStep: Record<string, string> = {
  confirmed: rv.stepBad,
  refuted: rv.stepOk,
  uncertain: rv.stepMeh,
}
const decisionStep: Record<string, string> = {
  publish: rv.stepBad,
  mark: rv.stepMeh,
}
const challengeDot: Record<string, string> = {
  confirmed: rv.dotBad,
  refuted: rv.dotOk,
  uncertain: rv.dotMeh,
}

function decisionOptions(summary?: FindingsSummary) {
  const bySeverity = summary?.by_severity
  const total = bySeverity ? Object.values(bySeverity).reduce<number>((sum, count) => sum + (count || 0), 0) : undefined
  return [
    { value: '', label: '全部', count: total },
    { value: 'publish', label: '发布', count: summary?.published },
    { value: 'mark', label: '存疑', count: summary?.marked },
    { value: 'drop', label: '丢弃', count: summary?.dropped },
    { value: 'downgrade', label: '降级' },
  ]
}

function FindingAnnotationEditor({
  finding,
  canAnnotate,
  onSave,
}: {
  finding: Finding
  canAnnotate: boolean
  onSave: (annotation: FindingAnnotation) => Promise<Finding>
}) {
  const [draft, setDraft] = useState<FindingAnnotation>(finding.annotation ?? '')
  const [saved, setSaved] = useState({
    annotation: finding.annotation ?? '',
    annotated_by: finding.annotated_by,
    annotated_at: finding.annotated_at,
  })
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)

  useEffect(() => {
    const next = {
      annotation: finding.annotation ?? '',
      annotated_by: finding.annotated_by,
      annotated_at: finding.annotated_at,
    }
    setDraft(next.annotation)
    setSaved(next)
    setError(null)
  }, [finding.id, finding.annotation, finding.annotated_by, finding.annotated_at])

  const save = async () => {
    setPending(true)
    setError(null)
    try {
      const updated = await onSave(draft)
      setSaved({
        annotation: updated.annotation ?? '',
        annotated_by: updated.annotated_by,
        annotated_at: updated.annotated_at,
      })
      setDraft(updated.annotation ?? '')
    } catch (caught) {
      setError(caught)
    } finally {
      setPending(false)
    }
  }

  return (
    <section aria-label="人工标注" className={rv.annotationPanel}>
      <h4>人工标注</h4>
      <p className={rv.annotationIntro}>
        人工标注是评审人员对问题是否有效的评估，不会改变发布结果或评审结论。
      </p>
      <div className={rv.annotationSaved} role="status" aria-label="已保存的人工标注">
        <span>已保存：</span>
        <Badge tone={annotationTone(saved.annotation)}>{annotationLabels[saved.annotation]}</Badge>
        {saved.annotation ? (
          <span className="muted">
            标注人 {saved.annotated_by || '不可用'} · 时间 <RelativeTime value={saved.annotated_at} fallback="不可用" />
          </span>
        ) : null}
      </div>
      {canAnnotate ? (
        <>
          <div role="group" aria-label="人工标注" className={`${ui.segmented} ${rv.annotationOptions}`}>
            {annotationOptions.map((option) => (
              <button
                key={option.value || 'none'}
                type="button"
                aria-pressed={draft === option.value}
                disabled={pending}
                onClick={() => setDraft(option.value)}
              >
                {option.label}
              </button>
            ))}
          </div>
          {draft !== saved.annotation ? <span className="small muted">有未保存的更改。</span> : null}
          <Button variant="primary" className={ui.smallButton} onClick={() => { void save() }} loading={pending}>
            保存标注
          </Button>
          {error ? <ErrorState error={error} onRetry={() => { void save() }} title="标注保存失败" /> : null}
        </>
      ) : null}
    </section>
  )
}

function FindingTimeline({ finding }: { finding: Finding }) {
  const finalSeverity = finding.final_severity && finding.final_severity !== finding.severity ? finding.final_severity : ''
  return (
    <ol className={rv.timeline} aria-label="处理过程">
      <li className={rv.tlItem}>
        <i className={`${rv.tlDot} ${rv.dotInfo}`} aria-hidden="true" />
        <b>提出</b> · {finding.severity}
        <span className={rv.who}>{finding.origin_node}{finding.origin_provider ? ` · ${finding.origin_provider}` : ''}</span>
      </li>
      <li className={rv.tlItem}>
        <i className={classNames(rv.tlDot, challengeDot[finding.challenge_status || ''])} aria-hidden="true" />
        <b>挑战 {labelForChallenge(finding.challenge_status)}</b>
        <span className={rv.who}>{finding.challenge_node || '—'}</span>
      </li>
      <li className={rv.tlItem}>
        <i className={`${rv.tlDot} ${rv.dotText}`} aria-hidden="true" />
        <b>裁决 {labelForDecision(finding.decision)}</b>
        {finalSeverity ? <span className={rv.who}>{finding.severity} → {finalSeverity}</span> : null}
        <div className={rv.tlReason}>{finding.decision_reason || '没有记录裁决理由。'}</div>
      </li>
    </ol>
  )
}

export function FindingRow({
  finding,
  index,
  expanded,
  onToggle,
  canAnnotate = false,
  onSaveAnnotation,
}: {
  finding: Finding
  index: number
  expanded: boolean
  onToggle: () => void
  canAnnotate?: boolean
  onSaveAnnotation?: (annotation: FindingAnnotation) => Promise<Finding>
}) {
  const bodyId = `finding-body-${index}`
  const finalSeverity = finding.final_severity || finding.severity
  const downgraded = Boolean(finding.final_severity && finding.final_severity !== finding.severity)
  const dropped = finding.decision === 'drop'
  const position = finding.path ? `${finding.path}${finding.line ? `:${finding.line}` : ''}` : ''
  return (
    <>
      <tr>
        <td className={rv.sevCell}>
          <SeverityBadge severity={finalSeverity} />
          {downgraded ? <span className={rv.sevOriginal}><span className="srOnly">原级别 </span>{finding.severity}</span> : null}
        </td>
        <td>
          <div className={rv.titleCell}>
            <button
              type="button"
              className={rv.titleButton}
              aria-expanded={expanded}
              aria-controls={bodyId}
              onClick={onToggle}
              aria-label={`${expanded ? '收起' : '展开'}：${finding.title}`}
            >
              <Icon name="chevron" size={14} aria-hidden="true" className={classNames(rv.chevron, expanded && rv.chevronOpen)} />
              <span className={classNames(rv.titleText, dropped && rv.titleDropped)}>{finding.title}</span>
            </button>
            <span className={rv.filePos}>{[position, finding.category].filter(Boolean).join(' · ') || '—'}</span>
          </div>
        </td>
        <td>
          <span className={rv.chain} role="group" aria-label="证据链">
            <span className={rv.step}>{finding.origin_provider || finding.origin_node}</span>
            <span className={rv.to} aria-hidden="true">›</span>
            <span className={classNames(rv.step, challengeStep[finding.challenge_status || ''])}>{labelForChallenge(finding.challenge_status)}</span>
            <span className={rv.to} aria-hidden="true">›</span>
            <span className={classNames(rv.step, decisionStep[finding.decision || ''])}>{labelForDecision(finding.decision)}</span>
          </span>
        </td>
        <td className={rv.annotationCell}>{annotationLabels[finding.annotation ?? '']}</td>
      </tr>
      {expanded ? (
        <tr>
          <td colSpan={4} className={rv.findingBodyCell} id={bodyId}>
            <div className={rv.findingDetail}>
              <div className={rv.detailMain}>
                <section className={rv.detailSection}>
                  <h4>问题正文</h4>
                  {finding.body ? <p>{finding.body}</p> : <span className="muted small">没有正文记录。</span>}
                </section>
                <section className={rv.detailSection}>
                  <h4>证据</h4>
                  {finding.evidence
                    ? <pre className={`${ui.codeBlock} ${rv.evidence}`}>{stringifyUnknown(finding.evidence)}</pre>
                    : <span className="muted small">没有证据记录。</span>}
                </section>
                {finding.challenge_evidence ? (
                  <section className={rv.detailSection}>
                    <h4>挑战证据</h4>
                    <pre className={`${ui.codeBlock} ${rv.evidence}`}>{stringifyUnknown(finding.challenge_evidence)}</pre>
                  </section>
                ) : null}
                <section className={rv.detailSection}>
                  <h4>处理过程</h4>
                  <FindingTimeline finding={finding} />
                </section>
                <section className={rv.detailSection}>
                  <h4>其他字段</h4>
                  <dl className={`${ui.definitionGrid} ${rv.extraFields}`}>
                    <dt>confidence</dt><dd>{finding.confidence || '—'}</dd>
                    <dt>votes</dt><dd>{typeof finding.votes === 'number' ? finding.votes : '—'}</dd>
                    <dt>position_ok</dt><dd>{typeof finding.position_ok === 'boolean' ? String(finding.position_ok) : '—'}</dd>
                  </dl>
                </section>
              </div>
              <FindingAnnotationEditor
                finding={finding}
                canAnnotate={canAnnotate && Boolean(onSaveAnnotation)}
                onSave={onSaveAnnotation || (async () => finding)}
              />
            </div>
          </td>
        </tr>
      ) : null}
    </>
  )
}

function publicationHint(state?: string, reason?: string): string {
  switch (state) {
    case 'pending':
      return '评审执行已结束，正在等待 GitLab 发布终态。'
    case 'published':
      return 'GitLab 发布已确认完成。'
    case 'skipped':
      if (reason?.startsWith('stale:')) {
        return `发布已跳过：${reason}。这是过期结果，不会自动重发，也不会自动修复。`
      }
      if (reason) return `发布已跳过：${reason}。`
      return '本次发布已跳过。'
    case 'failed':
      return 'GitLab 发布失败，不能视为 PASS。'
    default:
      return '服务端没有返回发布状态；这不表示发布成功。'
  }
}

function commitStatusTone(status?: string): 'ok' | 'danger' | 'info' | 'neutral' {
  if (status === 'success') return 'ok'
  if (status === 'failed') return 'danger'
  if (status === 'pending' || status === 'running') return 'info'
  return 'neutral'
}

function PublicationsPanel({ review, mrUrl }: { review: ReviewDetail; mrUrl?: string }) {
  const publications = review.publications
  const commitNote = publications?.commit_status === 'failed'
    ? review.publication_state === 'published' && review.verdict === 'BLOCKING'
      ? 'commit status 为 failed，表示评审结论为 BLOCKING（阻断合并）；发布本身已成功。'
      : review.publication_state === 'failed'
        ? '发布失败，不能视为 PASS。'
        : 'commit status 为 failed，请结合发布状态判断，不能直接视为 PASS。'
    : ''
  return (
    <Panel title="发布结果" flush>
      <div className={rv.publicationGrid}>
        <div className={publications ? undefined : rv.publicationWide}>
          <span className={rv.publicationLabel}>发布状态</span>
          <span className={rv.publicationValue}><PublicationBadge state={review.publication_state} /></span>
          <span className={rv.publicationHint}>{publicationHint(review.publication_state, review.publication_error)}</span>
        </div>
        {publications ? (
          <>
            <div>
              <span className={rv.publicationLabel}>commit status</span>
              <span className={rv.publicationValue}>
                {publications.commit_status
                  ? <Badge tone={commitStatusTone(publications.commit_status)}>{publications.commit_status}</Badge>
                  : '不可用'}
              </span>
            </div>
            <div>
              <span className={rv.publicationLabel}>汇总评论</span>
              <span className={rv.publicationValue}>
                {publications.summary_note_id ? `note #${publications.summary_note_id}` : '尚未发布'}
              </span>
              {publications.summary_note_id ? (
                <span className={rv.publicationHint}>
                  {mrUrl ? <a href={`${mrUrl}#note_${publications.summary_note_id}`} target="_blank" rel="noopener noreferrer">在 MR 中查看</a> : 'note ID 来自评审详情'}
                </span>
              ) : null}
            </div>
            <div>
              <span className={rv.publicationLabel}>行内讨论</span>
              <span className={`${rv.publicationValue} tabular`}>
                {typeof publications.discussions === 'number' ? `${publications.discussions} 条` : '不可用'}
              </span>
            </div>
            {Object.entries(publications)
              .filter(([key]) => !['summary_note_id', 'discussions', 'commit_status'].includes(key))
              .map(([key, value]) => (
                <div key={key}>
                  <span className={rv.publicationLabel}>{key}</span>
                  <span className={rv.publicationValue}>{typeof value === 'object' ? formatJson(value) : String(value ?? '—')}</span>
                </div>
              ))}
          </>
        ) : null}
      </div>
      {review.publication_error ? (
        <div className={rv.publicationAlert} role="alert">
          <strong>{review.publication_state === 'failed'
            ? '发布失败原因'
            : review.publication_state === 'skipped' ? '跳过原因' : '发布说明'}</strong>
          <p>{review.publication_error}</p>
        </div>
      ) : null}
      {!publications ? (
        <PanelBody className={rv.publicationEmpty}>
          <EmptyState title="尚无发布结果记录" description="服务端详情没有 publications 字段；这不表示发布成功。" />
        </PanelBody>
      ) : null}
      {commitNote ? <div className={ui.tableNote}>{commitNote}</div> : null}
    </Panel>
  )
}

function UsagePanel({ review }: { review: ReviewDetail }) {
  const usage = review.usage
  const tokens = (value?: number) => (typeof value === 'number' ? `${formatNumber(value)} tokens` : '不可用')
  return (
    <Panel title="用量">
      <dl className={`${ui.definitionGrid} ${rv.usageGrid}`}>
        <dt>输入</dt><dd className="mono">{tokens(usage?.tokens_in)}</dd>
        <dt>输出</dt><dd className="mono">{tokens(usage?.tokens_out)}</dd>
        <dt>成本</dt><dd className="mono">{typeof usage?.cost_micros === 'number' ? formatCost(usage.cost_micros) : '不可用'}</dd>
        <dt>用时</dt><dd className="mono">{formatDuration(review.duration_ms)}</dd>
      </dl>
    </Panel>
  )
}

// Built only from findings_summary; nothing here is inferred.
function summarySentence(summary?: FindingsSummary): string {
  if (!summary) return ''
  const parts: string[] = []
  if (summary.published) parts.push(`已发布 ${summary.published} 条`)
  if (summary.marked) parts.push(`存疑 ${summary.marked} 条`)
  if (summary.dropped) parts.push(`丢弃 ${summary.dropped} 条`)
  const severities = (['P0', 'P1', 'P2', 'info'] as const)
    .filter((key) => summary.by_severity?.[key])
    .map((key) => `${key} ×${summary.by_severity?.[key]}`)
  if (!parts.length && !severities.length) return summary.by_severity ? '没有记录 Finding。' : ''
  return [parts.join(' · '), severities.length ? `级别 ${severities.join(' · ')}` : ''].filter(Boolean).join('；')
}

function FindingsFunnel({ findings, partial }: { findings: Finding[]; partial: boolean }) {
  const counts = findingFunnel(findings)
  return (
    <div className={rv.funnel} role="group" aria-label="Findings 漏斗">
      <span>提出 <b>{counts.proposed}</b></span>
      <span className={rv.arrow} aria-hidden="true">→</span>
      <span>
        挑战确认 <b>{counts.challenge.confirmed}</b> · 推翻 <b>{counts.challenge.refuted}</b> · 存疑 <b>{counts.challenge.uncertain}</b>
        {counts.challenge.skipped ? <> · 未挑战 <b>{counts.challenge.skipped}</b></> : null}
      </span>
      <span className={rv.arrow} aria-hidden="true">→</span>
      <span>
        发布 <b>{counts.decision.publish}</b> · 丢弃 <b>{counts.decision.drop}</b> · 标记 <b>{counts.decision.mark}</b>
        {counts.decision.downgrade ? <> · 降级 <b>{counts.decision.downgrade}</b></> : null}
      </span>
      <span>（{partial ? '按当前筛选和已加载的 Finding 计数' : '按已加载的 Finding 计数'}）</span>
    </div>
  )
}

function VerdictCard({
  review, findings, findingsLoaded, partial,
}: { review: ReviewDetail; findings: Finding[]; findingsLoaded: boolean; partial: boolean }) {
  const state = verdictCardState(review)
  const summary = summarySentence(review.findings_summary)
  return (
    <section className={rv.verdictCard} data-verdict={state.tone} aria-label="评审结论">
      <div className={rv.verdictMark}>
        <span>结论</span>
        <b>{state.mark}</b>
      </div>
      <div className={rv.verdictReason}>
        {review.error ? <span className={rv.verdictAlert} role="alert">执行失败：{review.error}</span> : null}
        {state.tone === 'FAILED' && !review.error ? <span className={rv.verdictAlert} role="alert">执行失败，服务端没有返回错误原因。</span> : null}
        {review.cancel_reason ? <span className={rv.verdictAlert}>取消原因：{review.cancel_reason}</span> : null}
        {state.tone === 'CANCELLED' && !review.cancel_reason ? <span className={rv.verdictAlert}>评审已取消，结果不会被视为 PASS。</span> : null}
        {summary ? <span>{summary}</span> : null}
        {findingsLoaded && findings.length ? <FindingsFunnel findings={findings} partial={partial} /> : null}
      </div>
      <div className={rv.verdictStats}>
        <div><span className={rv.statLabel}>发布</span><PublicationBadge state={review.publication_state} /></div>
        <div><span className={rv.statLabel}>Tokens</span><b>{formatTokens(review.usage?.tokens_in, review.usage?.tokens_out)}</b></div>
        <div><span className={rv.statLabel}>成本</span><b>{formatCost(review.usage?.cost_micros)}</b></div>
      </div>
    </section>
  )
}

export function ReviewDetailPage() {
  const { id = '' } = useParams()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [decision, setDecision] = useState('')
  const [severity, setSeverity] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [selectedNode, setSelectedNode] = useState<GraphNode | null>(null)
  const [cancelOpen, setCancelOpen] = useState(false)
  const [cancelPending, setCancelPending] = useState(false)
  const [cancelError, setCancelError] = useState<unknown>(null)
  const [republishing, setRepublishing] = useState(false)
  const [triggerOpen, setTriggerOpen] = useState(false)

  const detail = useQuery({
    queryKey: ['review', id, 'detail'],
    queryFn: () => getReview(id),
    enabled: Boolean(id),
  })
  // Route access comes from the review detail. A missing grant or a missing
  // field never grants write controls.
  const canWrite = detail.data?.access === 'manage'
  const graph = useQuery({
    queryKey: ['review', id, 'graph'],
    queryFn: () => getReviewGraph(id),
    enabled: Boolean(id),
  })
  const findings = usePaginated<Finding>(
    ['review', id, 'findings', decision, severity],
    (cursor) => listFindings(id, { decision: decision || undefined, severity: severity || undefined }, cursor),
    'findings',
    { enabled: Boolean(id) },
  )
  const project = useQuery({
    queryKey: ['project', detail.data?.project_id],
    queryFn: () => getProject(detail.data!.project_id),
    enabled: Boolean(detail.data?.project_id),
  })
  const allProjects = useQuery({
    queryKey: ['projects', 'review-trigger'],
    queryFn: () => listProjects({ limit: 100 }),
    enabled: canWrite,
  })

  const stream = useReviewStream(id, detail.data)
  const projectNames = useMemo(
    () => new Map((allProjects.data?.projects || []).map((item) => [item.id, item.path_with_namespace])),
    [allProjects.data],
  )

  useEffect(() => {
    setExpanded(new Set())
  }, [id, decision, severity])

  if (!id) return <main className={ui.page}><ErrorState error={new Error('URL 中没有评审 ID。')} title="无法打开评审" /></main>
  if (detail.isPending) return <main className={ui.page}><LoadingState label="正在读取评审详情…" /></main>
  if (detail.isError) {
    return (
      <main className={ui.page}>
        <div className={ui.pageStack}>
          <Link to="/reviews">← 返回评审列表</Link>
          <ErrorState error={detail.error} onRetry={() => { void detail.refetch() }} title="评审详情加载失败" />
        </div>
      </main>
    )
  }

  const review = detail.data
  const projectPath = project.data?.path_with_namespace || review.project_path || projectNames.get(review.project_id) || `项目 ${review.project_id}`
  const mrUrl = safeExternalUrl(
    project.data?.web_url
      ? `${project.data.web_url.replace(/\/$/, '')}/-/merge_requests/${review.mr_iid}`
      : undefined,
  )
  const canCancel = canWrite && (review.status === 'running' || review.status === 'pending')
  // Republish covers completed and failed executions once the previous
  // publication is terminal and repairable. Pending waits for the backend
  // retry, and a stale skipped result cannot be repaired by republishing.
  const canRepublish = canWrite
    && (review.status === 'completed' || review.status === 'failed')
    && review.publication_state !== 'skipped'
    && (review.publication_state !== 'pending' || Boolean(review.publication_error))

  const onCancel = async () => {
    if (!canWrite) return
    setCancelPending(true)
    setCancelError(null)
    try {
      await cancelReview(id)
      setCancelOpen(false)
      toast.push('success', '已请求取消评审。')
      await queryClient.invalidateQueries({ queryKey: ['review', id] })
      void queryClient.invalidateQueries({ queryKey: ['reviews'] })
    } catch (error) {
      setCancelError(error)
    } finally {
      setCancelPending(false)
    }
  }

  const onRepublish = async () => {
    if (!canWrite) return
    setRepublishing(true)
    try {
      const result = await republishReview(id)
      toast.push('success', typeof result?.job_id === 'string' ? `已创建重新发布任务 ${result.job_id}。` : '已请求重新发布。')
      await queryClient.invalidateQueries({ queryKey: ['review', id] })
    } catch (error) {
      toast.push('error', error instanceof Error ? error.message : '重新发布失败。')
    } finally {
      setRepublishing(false)
    }
  }

  const annotateFinding = async (target: Finding, annotation: FindingAnnotation) => {
    if (!canWrite) throw new Error('当前账号没有项目管理权限。')
    const updated = await setFindingAnnotation(id, target.id, annotation)
    toast.push('success', annotation ? '已保存人工标注。' : '已清除人工标注。')
    void queryClient.invalidateQueries({ queryKey: ['review', id, 'findings'] })
    void queryClient.invalidateQueries({ queryKey: ['stats'] })
    return updated
  }

  const filtered = Boolean(decision || severity)
  const findingsCount = findings.isSuccess ? `${findings.items.length}${findings.hasNextPage ? '+' : ''} 条` : ''

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <header className={rv.reviewHeader}>
          <div className={rv.reviewHeading}>
            <div className={rv.reviewTitleRow}>
              <h1 className={rv.reviewTitle}>
                <span className={rv.mrRef}>!{review.mr_iid}</span>
                {review.mr_title || '未命名 MR'}
              </h1>
              <StatusBadge status={review.status} />
            </div>
            <div className={rv.reviewMeta}>
              <span>
                <Icon name="projects" size={14} aria-hidden="true" />
                <Link to={`/projects/${review.project_id}`} className="mono">{projectPath}</Link>
              </span>
              <span>
                head <code>{shortSha(review.head_sha)}</code>
                {review.base_sha ? <>→ base <code>{shortSha(review.base_sha)}</code></> : null}
              </span>
              {review.profile ? <span><Badge plain>{review.profile}</Badge></span> : <span>档位 —</span>}
              <span>{labelForTrigger(review.trigger)}触发</span>
              <span>
                创建 <RelativeTime value={review.created_at} /> · 用时 <span className="tabular">{formatDuration(review.duration_ms)}</span>
              </span>
              <span>
                <Badge plain tone={review.access === 'manage' ? 'info' : 'neutral'}>{review.access === 'manage' ? '可管理' : '只读'}</Badge>
              </span>
            </div>
            <div className={rv.streamStatus} role="status">
              {stream.state === 'open' ? <span className={rv.streamOk}>实时更新已连接</span> : null}
              {stream.state === 'connecting' ? <span>正在连接实时更新…</span> : null}
              {stream.state === 'closed' ? <span>{stream.terminal ? '评审已结束，实时连接已关闭。' : '实时更新未连接。'}</span> : null}
              {stream.state === 'error' ? (
                <span className={rv.streamError}>
                  {stream.error || '实时更新中断。'}{' '}
                  <button type="button" className={rv.linkButton} onClick={stream.retry}>重试连接</button>
                </span>
              ) : null}
            </div>
          </div>
          <div className={rv.headerActions}>
            {mrUrl ? (
              <ButtonLink href={mrUrl} target="_blank" rel="noopener noreferrer">
                在 GitLab 中打开 <Icon name="external" size={14} aria-hidden="true" />
              </ButtonLink>
            ) : null}
            {canWrite ? <Button variant="secondary" onClick={() => setTriggerOpen(true)}>重新触发评审</Button> : null}
            {canRepublish ? <Button variant="secondary" onClick={() => { void onRepublish() }} loading={republishing}>重新发布</Button> : null}
            {canCancel ? <Button variant="primary" onClick={() => setCancelOpen(true)}>取消评审</Button> : null}
          </div>
        </header>

        <VerdictCard
          review={review}
          findings={findings.items}
          findingsLoaded={findings.isSuccess}
          partial={filtered || findings.hasNextPage}
        />

        <Panel
          title={<>执行图 {graph.isSuccess ? <small>{graph.data.nodes.length} 个节点</small> : null}</>}
          aside={<span className={ui.hint}>点击节点查看输出与模型调用</span>}
          flush
        >
          {graph.isPending ? <PanelBody><LoadingState label="正在读取执行图…" /></PanelBody> : null}
          {graph.isError ? <PanelBody><ErrorState error={graph.error} onRetry={() => { void graph.refetch() }} title="执行图加载失败" /></PanelBody> : null}
          {graph.isSuccess ? (
            graph.data.nodes.length ? (
              <ExecutionGraph
                nodes={graph.data.nodes}
                edges={graph.data.edges}
                selectedId={selectedNode?.id}
                onSelect={(node) => setSelectedNode(node)}
              />
            ) : (
              <PanelBody>
                <ExecutionGraph nodes={[]} edges={[]} onSelect={() => {}} />
              </PanelBody>
            )
          ) : null}
        </Panel>

        <div className={rv.split}>
          <div className={rv.mainColumn}>
            <Panel
              title={<>Findings {findingsCount ? <small>{findingsCount}</small> : null}</>}
              aside={(
                <div className={rv.findingFilters}>
                  <Segmented
                    label="按处置过滤"
                    options={decisionOptions(review.findings_summary)}
                    value={decision}
                    onChange={setDecision}
                  />
                  <select className="select" name="severity" value={severity} onChange={(event) => setSeverity(event.target.value)} aria-label="按严重级别过滤">
                    {severityOptions.map(([value, label]) => <option key={value || 'all'} value={value}>{label}</option>)}
                  </select>
                </div>
              )}
              flush
            >
              {findings.isPending ? <PanelBody><LoadingState label="正在读取问题…" /></PanelBody> : null}
              {findings.isError ? <PanelBody><ErrorState error={findings.error} onRetry={findings.retry} /></PanelBody> : null}
              {findings.isSuccess && findings.items.length === 0 ? (
                <PanelBody><EmptyState title="没有符合条件的问题" description="调整处置或级别筛选；这不表示评审没有产生过问题。" /></PanelBody>
              ) : null}
              {findings.items.length ? (
                <div className="tableWrap">
                  <table className={`dataTable ${rv.findingsTable}`}>
                    <thead>
                      <tr>
                        <th scope="col">级别</th>
                        <th scope="col">标题</th>
                        <th scope="col">提出 → 挑战 → 裁决</th>
                        <th scope="col">人工</th>
                      </tr>
                    </thead>
                    <tbody>
                      {findings.items.map((finding, index) => (
                        <FindingRow
                          key={finding.id}
                          finding={finding}
                          index={index}
                          expanded={expanded.has(finding.id)}
                          canAnnotate={canWrite}
                          onSaveAnnotation={(annotation) => annotateFinding(finding, annotation)}
                          onToggle={() => {
                            setExpanded((current) => {
                              const next = new Set(current)
                              if (next.has(finding.id)) next.delete(finding.id)
                              else next.add(finding.id)
                              return next
                            })
                          }}
                        />
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : null}
              {findings.hasNextPage ? (
                <div className={rv.loadMore}>
                  <Button variant="secondary" className={ui.smallButton} onClick={findings.fetchNextPage} loading={findings.isFetchingNextPage}>加载更多问题</Button>
                </div>
              ) : null}
              <div className={ui.tableNote}>正文与证据以纯文本展示。</div>
            </Panel>
          </div>
          <div className={rv.sideColumn}>
            <PublicationsPanel review={review} mrUrl={mrUrl} />
            <UsagePanel review={review} />
          </div>
        </div>
      </div>

      <NodeDrawer reviewId={id} node={selectedNode} onClose={() => setSelectedNode(null)} />

      <ConfirmDialog
        open={cancelOpen}
        onClose={() => { setCancelOpen(false); setCancelError(null) }}
        onConfirm={() => { void onCancel() }}
        title="取消评审？"
        description={`将请求取消 !${review.mr_iid} 的当前评审。工作流可能已有部分结果，取消后不会被视为 PASS。`}
        confirmLabel="取消评审"
        pending={cancelPending}
        error={cancelError}
      />

      {canWrite ? (
        <TriggerReviewDialog
          open={triggerOpen}
          onClose={() => setTriggerOpen(false)}
          projects={allProjects.data?.projects || []}
          presetProjectId={review.project_id}
          presetIid={review.mr_iid}
          presetProjectLabel={projectPath}
          onTriggered={(jobId) => {
            setTriggerOpen(false)
            toast.push('success', jobId ? `已创建评审任务 ${jobId}。` : '已创建评审任务。')
            void queryClient.invalidateQueries({ queryKey: ['reviews'] })
          }}
        />
      ) : null}
    </main>
  )
}
