import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { render } from '@testing-library/react'
import { ReviewsPage } from '../src/pages/ReviewsPage'
import { ReviewTable } from '../src/components/ReviewTable'
import { review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('review list status filter', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('filters by status through a segmented control', async () => {
    const fetchMock = installFetch((url) => {
      if (url.startsWith('/api/v1/reviews')) return jsonResponse({ reviews: [] })
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<ReviewsPage />, { route: '/reviews' })

    const group = await screen.findByRole('group', { name: '按状态筛选' })
    expect(within(group).getByRole('button', { name: '全部' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.queryByRole('combobox', { name: '按状态筛选' })).not.toBeInTheDocument()

    await user.click(within(group).getByRole('button', { name: '失败' }))
    expect(within(group).getByRole('button', { name: '失败' })).toHaveAttribute('aria-pressed', 'true')
    expect(within(group).getByRole('button', { name: '全部' })).toHaveAttribute('aria-pressed', 'false')
    const reviewCalls = fetchMock.mock.calls.map(([input]) => String(input)).filter((url) => url.startsWith('/api/v1/reviews'))
    expect(reviewCalls.some((url) => new URL(url, 'http://localhost').searchParams.get('status') === 'failed')).toBe(true)

    await user.click(within(group).getByRole('button', { name: '全部' }))
    expect(within(group).getByRole('button', { name: '全部' })).toHaveAttribute('aria-pressed', 'true')
  })
})

function Location() {
  const location = useLocation()
  return <span data-testid="location">{location.pathname}</span>
}

describe('review table v2 rows', () => {
  it('merges MR and project into a primary cell, keeps a focusable link and opens the row on click', async () => {
    const user = userEvent.setup()
    render(
      <MemoryRouter initialEntries={['/reviews']}>
        <Routes>
          <Route path="*" element={<><ReviewTable reviews={[review({ id: 'r-1', mr_iid: 7, mr_title: 'row title', findings_summary: { by_severity: { P0: 1, P1: 0, P2: 2 } } })]} projectNames={new Map()} /><Location /></>} />
        </Routes>
      </MemoryRouter>,
    )
    expect(screen.getByRole('columnheader', { name: '合并请求' })).toBeInTheDocument()
    expect(screen.queryByRole('columnheader', { name: '项目' })).not.toBeInTheDocument()
    const link = screen.getByRole('link', { name: '!7 row title' })
    expect(link).toHaveAttribute('href', '/reviews/r-1')
    const row = link.closest('tr')!
    expect(within(row).getByText('!7')).toBeInTheDocument()
    expect(within(row).getByText('group/backend')).toBeInTheDocument()
    expect(within(row).getByRole('img', { name: 'P0 1' })).toBeInTheDocument()
    expect(within(row).getByRole('img', { name: 'P2 2' })).toBeInTheDocument()

    await user.click(within(row).getByText('group/backend'))
    expect(screen.getByTestId('location')).toHaveTextContent('/reviews/r-1')
  })
})
