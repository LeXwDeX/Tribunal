import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { OverviewPage } from '../src/pages/OverviewPage'
import { review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

function routeOverview(overrides: {
  byStatus?: Record<string, unknown[]>
  byPublication?: Record<string, unknown[]>
  all?: unknown[]
  onRequest?: (url: string) => void
}) {
  return installFetch((url) => {
    overrides.onRequest?.(url)
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
    if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
    if (parsed.pathname === '/api/v1/providers') return jsonResponse({ providers: [] })
    if (parsed.pathname === '/api/v1/reviews') {
      const status = parsed.searchParams.get('status')
      const publication = parsed.searchParams.get('publication_state')
      if (status && overrides.byStatus?.[status]) return jsonResponse({ reviews: overrides.byStatus[status] })
      if (publication && overrides.byPublication?.[publication]) return jsonResponse({ reviews: overrides.byPublication[publication] })
      return jsonResponse({ reviews: overrides.all || [] })
    }
    return jsonResponse({})
  })
}

function alertPanel() {
  const title = screen.getByRole('heading', { name: '需要处理' })
  const panel = title.closest('section')
  if (!panel) throw new Error('alerts panel missing')
  return panel
}

describe('overview publication alerts', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('surfaces failed publications and failed runs retrying publication without false positives', async () => {
    const runFailure = review({ id: 'run-failure', mr_title: 'run failure', status: 'failed' })
    const publicationFailure = review({
      id: 'publication-failure',
      mr_title: 'publication failure',
      status: 'completed',
      publication_state: 'failed',
      publication_error: 'merge request is not available',
    })
    const pendingOk = review({
      id: 'pending-ok',
      mr_title: 'pending normal',
      status: 'completed',
      publication_state: 'pending',
    })
    const retrying = review({
      id: 'retry-pending',
      mr_title: 'pending retry',
      status: 'completed',
      publication_state: 'pending',
      publication_error: 'retry pending',
    })
    routeOverview({
      byStatus: { failed: [runFailure] },
      byPublication: { failed: [publicationFailure, runFailure], pending: [pendingOk, retrying] },
    })
    renderWithProviders(<OverviewPage />, { route: '/' })

    await screen.findByRole('heading', { name: '需要处理' })
    const panel = alertPanel()
    await waitFor(() => expect(within(panel).getAllByText('发布失败')).toHaveLength(2))
    expect(within(panel).queryByText('评审失败')).not.toBeInTheDocument()
    expect(await within(panel).findByText('发布重试中')).toBeInTheDocument()
    expect(within(panel).getByText(/pending retry/)).toBeInTheDocument()
    expect(within(panel).getByText(/retry pending/)).toBeInTheDocument()
    expect(within(panel).queryByText(/执行失败/)).not.toBeInTheDocument()
    expect(within(panel).getByText(/merge request is not available/)).toBeInTheDocument()
    expect(within(panel).queryByText(/pending normal/)).not.toBeInTheDocument()
    expect(within(panel).getAllByText(/run failure/)).toHaveLength(1)
  })

  it('keeps pagination for the publication failure query', async () => {
    let failedPage = 0
    const urls: string[] = []
    installFetch((url) => {
      urls.push(url)
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
      if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
      if (parsed.pathname === '/api/v1/providers') return jsonResponse({ providers: [] })
      if (parsed.pathname === '/api/v1/reviews' && parsed.searchParams.get('publication_state') === 'failed') {
        failedPage += 1
        if (parsed.searchParams.get('cursor') === 'next-publication') {
          return jsonResponse({ reviews: [review({ id: 'publication-page-2', mr_title: 'second publication page', publication_state: 'failed' })] })
        }
        return jsonResponse({
          reviews: [review({ id: 'publication-page-1', mr_title: 'first publication page', publication_state: 'failed' })],
          next_cursor: 'next-publication',
        })
      }
      if (parsed.pathname === '/api/v1/reviews') return jsonResponse({ reviews: [] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<OverviewPage />, { route: '/' })

    expect(await screen.findByText(/first publication page/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '加载更多告警数据' }))
    expect(await screen.findByText(/second publication page/)).toBeInTheDocument()
    expect(failedPage).toBe(2)
    expect(urls.some((url) => url.includes('publication_state=failed') && url.includes('cursor=next-publication'))).toBe(true)
  })

  it('keeps loading review alerts visible beyond the first twelve entries', async () => {
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
      if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
      if (parsed.pathname === '/api/v1/providers') return jsonResponse({ providers: [] })
      if (parsed.pathname === '/api/v1/reviews' && parsed.searchParams.get('publication_state') === 'failed') {
        if (parsed.searchParams.get('cursor') === 'more') {
          return jsonResponse({ reviews: [review({ id: 'alert-twelve', mr_title: 'alert twelve', publication_state: 'failed' })] })
        }
        return jsonResponse({
          reviews: Array.from({ length: 12 }, (_, index) => review({
            id: `alert-${index}`,
            mr_title: `alert ${index}`,
            publication_state: 'failed',
          })),
          next_cursor: 'more',
        })
      }
      if (parsed.pathname === '/api/v1/reviews') return jsonResponse({ reviews: [] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<OverviewPage />, { route: '/' })
    expect(await screen.findByText(/alert 11/)).toBeInTheDocument()
    expect(screen.queryByText(/alert twelve/)).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '加载更多告警数据' }))
    expect(await screen.findByText(/alert twelve/)).toBeInTheDocument()
  })

  it('does not query providers for a viewer', async () => {
    const urls: string[] = []
    installFetch((url) => {
      urls.push(url)
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
      if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
      if (parsed.pathname === '/api/v1/providers') return jsonResponse({ error: { message: 'platform admin role required' } }, 403)
      if (parsed.pathname === '/api/v1/reviews') return jsonResponse({ reviews: [] })
      return jsonResponse({})
    })
    renderWithProviders(<OverviewPage />, { route: '/', me: viewerMe })
    expect(await screen.findByText('运行次数')).toBeInTheDocument()
    expect(urls.some((url) => url.startsWith('/api/v1/providers'))).toBe(false)
    expect(screen.queryByText(/异常检查未完整加载/)).not.toBeInTheDocument()
    expect(screen.queryByText(/platform admin role required/)).not.toBeInTheDocument()
  })
})
