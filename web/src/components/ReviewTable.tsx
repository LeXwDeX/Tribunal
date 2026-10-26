import type { MouseEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import type { ReviewSummary } from '../api/types'
import { formatCost, formatDuration } from '../lib/format'
import { Badge, PublicationBadge, RelativeTime, SevCounts, StatusBadge, VerdictBadge } from './ui'
import styles from './ReviewTable.module.css'

export function projectLabel(review: ReviewSummary, names: Map<number, string>): string {
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

export function ReviewTable({ reviews, projectNames }: { reviews: ReviewSummary[]; projectNames: Map<number, string> }) {
  const navigate = useNavigate()
  return (
    <div className="tableWrap">
      <table className={styles.table}>
        <thead>
          <tr>
            <th scope="col">合并请求</th>
            <th scope="col">档位</th>
            <th scope="col">执行状态</th>
            <th scope="col">发布</th>
            <th scope="col">结论</th>
            <th scope="col">P0 · P1 · P2</th>
            <th scope="col" className="right">用时</th>
            <th scope="col" className="right">成本</th>
            <th scope="col">时间</th>
          </tr>
        </thead>
        <tbody>
          {reviews.map((review) => {
            const href = reviewHref(review)
            // The list API does not return mr_title; fall back to the MR reference.
            const title = review.mr_title || `!${review.mr_iid}`
            const label = review.mr_title ? `!${review.mr_iid} ${review.mr_title}` : `!${review.mr_iid}`
            return (
              <tr
                key={review.id}
                className="clickable"
                onClick={(event) => {
                  if (isInteractiveClick(event)) return
                  navigate(href)
                }}
              >
                <td>
                  <div className={styles.primary}>
                    <Link to={href} className={styles.titleLink} aria-label={label} title={title}>
                      {title}
                    </Link>
                    <span className={styles.meta}>
                      <span className={styles.mrRef}>!{review.mr_iid}</span>
                      <span className={styles.project}>{projectLabel(review, projectNames)}</span>
                    </span>
                  </div>
                </td>
                <td>{review.profile ? <Badge plain>{review.profile}</Badge> : <span className="muted">—</span>}</td>
                <td><StatusBadge status={review.status} /></td>
                <td>
                  <div className={styles.publicationCell}>
                    <PublicationBadge state={review.publication_state} />
                    {review.publication_error ? (
                      <span className={styles.publicationError} title={review.publication_error}>
                        {review.publication_error}
                      </span>
                    ) : null}
                  </div>
                </td>
                <td>{review.verdict ? <VerdictBadge verdict={review.verdict} /> : <span className="muted">—</span>}</td>
                <td><SevCounts summary={review.findings_summary} /></td>
                <td className="right num mono">{formatDuration(review.duration_ms)}</td>
                <td className="right num mono">{formatCost(review.usage?.cost_micros ?? review.cost_micros)}</td>
                <td className="muted nowrap"><RelativeTime value={review.created_at} /></td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
