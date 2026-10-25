import { Link } from 'react-router-dom'
import type { ReviewSummary } from '../api/types'
import { formatCost, formatDuration, formatNumber } from '../lib/format'
import { PublicationBadge, RelativeTime, SeverityBadge, StatusBadge, VerdictBadge } from './ui'

export function projectLabel(review: ReviewSummary, names: Map<number, string>): string {
  return review.project_path || names.get(review.project_id) || `项目 ${review.project_id}`
}

export function ReviewTable({ reviews, projectNames }: { reviews: ReviewSummary[]; projectNames: Map<number, string> }) {
  return (
    <div className="tableWrap">
      <table className="dataTable" style={{ minWidth: 1140 }}>
        <thead>
          <tr>
            <th scope="col">MR</th>
            <th scope="col">项目</th>
            <th scope="col">档位</th>
            <th scope="col">执行状态</th>
            <th scope="col">发布</th>
            <th scope="col">结论</th>
            <th scope="col">P0 / P1 / P2</th>
            <th scope="col">用时</th>
            <th scope="col">成本</th>
            <th scope="col">时间</th>
          </tr>
        </thead>
        <tbody>
          {reviews.map((review) => {
            const counts = review.findings_summary?.by_severity
            return (
              <tr key={review.id}>
                <td>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: 210 }}>
                    <Link to={`/reviews/${encodeURIComponent(review.id)}`} style={{ fontWeight: 500 }}>
                      !{review.mr_iid}{review.mr_title ? ` ${review.mr_title}` : ''}
                    </Link>
                    <span className="mono small muted">{review.id}</span>
                  </div>
                </td>
                <td><span className="mono small">{projectLabel(review, projectNames)}</span></td>
                <td>{review.profile || '—'}</td>
                <td><StatusBadge status={review.status} /></td>
                <td>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: 150 }}>
                    <PublicationBadge state={review.publication_state} />
                    {review.publication_error ? (
                      <span className="small" style={{ color: 'var(--danger-fg)', maxWidth: 240, overflowWrap: 'anywhere' }} title={review.publication_error}>
                        {review.publication_error}
                      </span>
                    ) : null}
                  </div>
                </td>
                <td><VerdictBadge verdict={review.verdict} /></td>
                <td className="tabular">
                  {counts ? (
                    <span style={{ display: 'inline-flex', gap: 10, flexWrap: 'wrap' }}>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 3 }}><SeverityBadge severity="P0" />{formatNumber(counts.P0 ?? 0)}</span>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 3 }}><SeverityBadge severity="P1" />{formatNumber(counts.P1 ?? 0)}</span>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 3 }}><SeverityBadge severity="P2" />{formatNumber(counts.P2 ?? 0)}</span>
                    </span>
                  ) : '—'}
                </td>
                <td className="tabular">{formatDuration(review.duration_ms)}</td>
                <td className="tabular">{formatCost(review.usage?.cost_micros)}</td>
                <td className="muted"><RelativeTime value={review.created_at} /></td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
