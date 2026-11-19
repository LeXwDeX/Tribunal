import type { MouseEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { isStatusOnly } from '../api/types'
import type { ReviewListItem, ReviewSummary, StatusOnlyReview } from '../api/types'
import { formatDuration, labelForPublication, publicationTone } from '../lib/format'
import {
  Badge, DataTable, ReasonBadge, RelativeTime, SevCounts, StatusBadge, Tooltip, VerdictBadge, classNames, tableCell,
} from './ui'
import styles from './ReviewTable.module.css'

export function projectLabel(review: Pick<ReviewSummary, 'project_id' | 'project_path'>, names: Map<number, string>): string {
  return review.project_path || names.get(review.project_id) || `项目 ${review.project_id}`
}

export function reviewHref(review: ReviewSummary): string {
  return `/reviews/${encodeURIComponent(review.id)}`
}

// Clicks on inner links, buttons or selected text keep their own behavior.
function isInteractiveClick(event: MouseEvent<HTMLElement>): boolean {
  const target = event.target as HTMLElement
  if (target.closest('a, button, input, select, textarea, label')) return true
  return Boolean(window.getSelection()?.toString())
}

// Persisted reasons of reviews that never created a workflow.
const STATUS_ONLY_REASONS: Record<string, string> = {
  'empty diff': '没有可评审的变更（empty diff）',
  'no reviewable files': '所有变更文件均被忽略规则过滤',
  'snapshot omitted files': '材料快照遗漏了变更文件',
}

export function statusOnlyReasonLabel(reason: string): string {
  return STATUS_ONLY_REASONS[reason] || reason || '未记录原因'
}

// Persisted publication reasons are bounded, sanitized server codes (see
// internal/app/publisher.go and review_service.go). The list shows the Chinese
// text in the badge tooltip; an unknown code is shown as stored.
const PUBLICATION_REASONS: Record<string, string> = {
  'review cancelled': '评审已取消',
  'review superseded': '已被新一轮评审取代',
  'review job no longer active': '评审任务已不再活动',
  'failure note disabled': '已关闭失败说明评论，未发布',
  'retry pending': '发布失败，等待重试',
  'discussion outcome unknown': '行内讨论的发布结果未知',
  'publication is not possible': '无法发布',
  'merge request is not available': '合并请求不可用',
  'publication outcome stayed unknown; review the merge request and publish again': '发布结果长期未知，请检查合并请求后重新发布',
  'repository or stored ignore configuration is invalid': '仓库或已保存的忽略规则无效',
  'ignore policy unavailable; rerun review': '忽略规则不可用，请重新评审',
  'workspace material identity unavailable; rerun review': '工作区材料标识不可用，请重新评审',
  'repository skill provenance unavailable or unsupported; rerun review': '仓库 Skill 来源不可用或不受支持，请重新评审',
}

// A "stale: <cause>" reason marks a finished review that was not published
// because its inputs changed afterwards.
const STALE_REASONS: Record<string, string> = {
  'workflow fingerprint changed': '评审指纹已变化',
  'ignore policy changed': '忽略规则已变化',
  'configuration changed': '评审配置已变化',
  'prompt bundle identity unavailable': '提示词版本标识不可用',
  'prompt bundle changed': '提示词版本已变化',
  'merge request head changed': '合并请求有新提交（head 已变化）',
  'merge request base changed': '合并请求的 base 已变化',
  'ignored finding rejected': '问题位于被忽略的路径',
}

export function publicationReasonLabel(reason: string): string {
  const known = PUBLICATION_REASONS[reason]
  if (known) return known
  if (reason.startsWith('stale:')) {
    const cause = reason.slice('stale:'.length).trim()
    return `结果已过期：${STALE_REASONS[cause] || PUBLICATION_REASONS[cause] || cause}`
  }
  return reason
}

// No workflow means no graph page: the row is plain text, never a link.
function StatusOnlyRow({ review, projectNames }: { review: StatusOnlyReview; projectNames: Map<number, string> }) {
  const reason = statusOnlyReasonLabel(review.reason)
  return (
    <tr>
      <td>
        <div className={styles.primary}>
          <span className={styles.titleText}>!{review.mr_iid}</span>
          <span className={styles.meta}>
            <span className={styles.project}>{projectLabel(review, projectNames)}</span>
            {review.head_sha ? <span className={styles.mrRef}>{review.head_sha.slice(0, 8)}</span> : null}
          </span>
          <span className={styles.reason}>原因：{reason}</span>
        </div>
      </td>
      <td className={tableCell.muted}>—</td>
      <td className={styles.fit}><Badge tone="neutral">未创建评审任务</Badge></td>
      <td className={tableCell.muted}>—</td>
      <td className={styles.fit}><VerdictBadge verdict={review.verdict} /></td>
      <td className={tableCell.muted}>—</td>
      <td className={tableCell.num}>—</td>
      <td className={classNames(tableCell.muted, tableCell.nowrap)}><RelativeTime value={review.created_at} /></td>
    </tr>
  )
}

export function ReviewTable({
  reviews, projectNames, label = '评审列表',
}: { reviews: ReviewListItem[]; projectNames: Map<number, string>; label?: string }) {
  const navigate = useNavigate()
  return (
    <DataTable label={label} minWidth="xl">
      <thead>
        <tr>
          <th scope="col">合并请求</th>
          <th scope="col">档位</th>
          <th scope="col">执行状态</th>
          <th scope="col">发布</th>
          <th scope="col">结论</th>
          <th scope="col">P0 · P1 · P2</th>
          <th scope="col" className={tableCell.num}>用时</th>
          <th scope="col">时间</th>
        </tr>
      </thead>
      <tbody>
        {reviews.map((review) => {
          if (isStatusOnly(review)) {
            return <StatusOnlyRow key={`${review.project_id}-${review.mr_iid}-${review.head_sha || ''}`} review={review} projectNames={projectNames} />
          }
          const href = reviewHref(review)
          // Reviews saved before titles were persisted have no mr_title; fall back to the MR reference.
          const title = review.mr_title || `!${review.mr_iid}`
          const linkLabel = review.mr_title ? `!${review.mr_iid} ${review.mr_title}` : `!${review.mr_iid}`
          return (
            <tr
              key={review.id}
              className={styles.rowLink}
              onClick={(event) => {
                if (isInteractiveClick(event)) return
                navigate(href)
              }}
            >
              <td>
                <div className={styles.primary}>
                  <Tooltip content={title}>
                    <Link to={href} className={styles.titleLink} aria-label={linkLabel}>
                      {title}
                    </Link>
                  </Tooltip>
                  <span className={styles.meta}>
                    <span className={styles.mrRef}>!{review.mr_iid}</span>
                    <span className={styles.project}>{projectLabel(review, projectNames)}</span>
                  </span>
                </div>
              </td>
              <td className={styles.fit}>{review.profile ? <Badge plain>{review.profile}</Badge> : <span className={tableCell.muted}>—</span>}</td>
              <td className={styles.fit}><StatusBadge status={review.status} /></td>
              <td className={styles.fit}>
                {/* The cell holds the badge only; the reason lives in its tooltip. */}
                <ReasonBadge
                  tone={publicationTone(review.publication_state)}
                  reason={review.publication_error ? publicationReasonLabel(review.publication_error) : undefined}
                >
                  {labelForPublication(review.publication_state)}
                </ReasonBadge>
              </td>
              <td className={styles.fit}>{review.verdict ? <VerdictBadge verdict={review.verdict} /> : <span className={tableCell.muted}>—</span>}</td>
              <td className={styles.fit}><SevCounts summary={review.findings_summary} /></td>
              <td className={tableCell.num}>{formatDuration(review.duration_ms)}</td>
              <td className={classNames(tableCell.muted, tableCell.nowrap)}><RelativeTime value={review.created_at} /></td>
            </tr>
          )
        })}
      </tbody>
    </DataTable>
  )
}
