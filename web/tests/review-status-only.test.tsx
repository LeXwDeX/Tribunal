import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { ReviewTable } from '../src/components/ReviewTable'
import type { StatusOnlyReview } from '../src/api/types'
import { review } from './fixtures'

const emptyDiff: StatusOnlyReview = {
  kind: 'status_only',
  project_id: 42,
  mr_iid: 331,
  head_sha: 'abcdef1234567890',
  verdict: 'INCOMPLETE',
  reason: 'empty diff',
  created_at: '2026-10-22T05:00:00Z',
}

function renderTable(items: Parameters<typeof ReviewTable>[0]['reviews']) {
  return render(
    <MemoryRouter>
      <ReviewTable reviews={items} projectNames={new Map([[42, 'group/backend']])} />
    </MemoryRouter>,
  )
}

describe('review list status-only rows', () => {
  it('shows INCOMPLETE with the empty-diff reason and no link', () => {
    renderTable([emptyDiff])
    const row = screen.getByText('!331').closest('tr') as HTMLElement
    expect(within(row).getByText('INCOMPLETE（未完成）')).toBeInTheDocument()
    // The reason sits in the status badge tooltip, not as a second line.
    expect(within(row).queryByText(/没有可评审的变更/)).not.toBeInTheDocument()
    const status = within(row).getByText('未创建评审任务').closest('[tabindex]') as HTMLElement
    expect(status).toHaveAccessibleDescription(/没有可评审的变更/)
    expect(within(row).getByText('group/backend')).toBeInTheDocument()
    expect(within(row).getByText('abcdef12')).toBeInTheDocument()
    expect(within(row).queryByRole('link')).toBeNull()
    expect(row.className).not.toMatch(/rowLink/)
  })

  it('keeps workflow rows linked next to status-only rows', () => {
    renderTable([emptyDiff, review({ id: 'wf-1', mr_iid: 320, status: 'completed', verdict: 'PASS' })])
    expect(screen.getAllByRole('link')).toHaveLength(1)
    expect(screen.getByRole('link', { name: /!320/ })).toHaveAttribute('href', '/reviews/wf-1')
  })
})

describe('review list columns', () => {
  it('fits desktop widths: profile joins the MR meta line and time stacks over duration', () => {
    renderTable([review({ id: 'wf-9', mr_iid: 9, status: 'completed', verdict: 'PASS', profile: 'deep', duration_ms: 65_000 })])
    const table = screen.getByRole('table', { name: '评审列表' })
    expect(within(table).getAllByRole('columnheader').map((cell) => cell.textContent)).toEqual([
      '合并请求', '执行状态', '发布', '结论', 'P0 · P1 · P2', '时间 / 用时',
    ])
    const row = screen.getByRole('link', { name: /!9/ }).closest('tr') as HTMLElement
    expect(within(row).getByText('deep')).toBeInTheDocument()
    expect(within(row).getByText('1m 5s')).toBeInTheDocument()
  })
})
