import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function routes(reviewStats: Record<string, unknown>) {
  return installFetch((url) => {
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/stats/reviews') return jsonResponse(reviewStats)
    if (parsed.pathname === '/api/v1/stats') return jsonResponse({})
    if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
    return jsonResponse({})
  })
}

const stats = {
  runs: 14,
  completed: 13,
  incomplete: 13,
  blocking: 1,
  failed: 1,
  cancelled: 0,
  blocking_ratio: 1 / 14,
  incomplete_ratio: 13 / 14,
  all_runs: 25,
  all_blocking: 1,
  all_blocking_ratio: 1 / 25,
  llm_call_retention_hours: 720,
  model_details_purged: false,
}

describe('statistics review scope (#458)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows review-only runs, INCOMPLETE count and the dual blocking denominator', async () => {
    routes(stats)
    renderWithProviders(<StatisticsPage />)
    const title = await screen.findByRole('heading', { name: '评审口径' })
    const panel = title.closest('section')!
    expect(within(panel).getByText('评审运行')).toBeInTheDocument()
    expect(within(panel).getByText('14')).toBeInTheDocument()
    expect(within(panel).getByText('INCOMPLETE')).toBeInTheDocument()
    expect(within(panel).getByText('13')).toBeInTheDocument()
    expect(within(panel).getByText(/评审分母/)).toHaveTextContent('1/14')
    expect(within(panel).getByText(/全量含 onboarding/)).toHaveTextContent('1/25')
    expect(within(panel).queryByText(/明细已删除/)).not.toBeInTheDocument()
  })

  it('warns that model details are purged when the range predates the retention window', async () => {
    routes({ ...stats, model_details_purged: true })
    renderWithProviders(<StatisticsPage />)
    const title = await screen.findByRole('heading', { name: '评审口径' })
    const panel = title.closest('section')!
    const notice = await within(panel).findByText(/明细已删除/)
    expect(notice).toHaveTextContent('720 小时')
    expect(notice).toHaveAttribute('role', 'status')
  })
})