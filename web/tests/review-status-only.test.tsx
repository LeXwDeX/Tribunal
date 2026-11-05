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
    expect(within(row).getByText(/没有可评审的变更/)).toBeInTheDocument()
    expect(within(row).getByText('group/backend')).toBeInTheDocument()
    expect(within(row).getByText('abcdef12')).toBeInTheDocument()
    expect(within(row).queryByRole('link')).toBeNull()
    expect(row).not.toHaveClass('clickable')
  })

  it('keeps workflow rows linked next to status-only rows', () => {
    renderTable([emptyDiff, review({ id: 'wf-1', mr_iid: 320, status: 'completed', verdict: 'PASS' })])
    expect(screen.getAllByRole('link')).toHaveLength(1)
    expect(screen.getByRole('link', { name: /!320/ })).toHaveAttribute('href', '/reviews/wf-1')
  })
})
