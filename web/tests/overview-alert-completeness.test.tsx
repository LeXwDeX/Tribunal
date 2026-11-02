import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { onlineManager } from '@tanstack/react-query'
import { OverviewPage } from '../src/pages/OverviewPage'
import { project, review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

type Source = 'projects' | 'providers' | 'failed' | 'publication-failed' | 'publication-pending'

function sourceFor(url: URL): Source | undefined {
  if (url.pathname === '/api/v1/projects') return 'projects'
  if (url.pathname === '/api/v1/providers') return 'providers'
  if (url.pathname !== '/api/v1/reviews') return undefined
  if (url.searchParams.get('status') === 'failed') return 'failed'
  if (url.searchParams.get('publication_state') === 'failed') return 'publication-failed'
  if (url.searchParams.get('publication_state') === 'pending') return 'publication-pending'
  return undefined
}

function page(source: Source, items: unknown[] = [], next?: string) {
  const key = source === 'projects' ? 'projects' : source === 'providers' ? 'providers' : 'reviews'
  return jsonResponse({ [key]: items, ...(next ? { next_cursor: next } : {}) })
}

function alertPanel() {
  const panel = screen.getByRole('heading', { name: '需要处理' }).closest('section')
  if (!panel) throw new Error('alerts panel missing')
  return within(panel)
}

function defaultResponse(url: URL) {
  const source = sourceFor(url)
  if (source) return page(source)
  if (url.pathname === '/api/v1/reviews') return jsonResponse({ reviews: [] })
  return jsonResponse({ runs: 1 })
}

describe('overview alert completeness', () => {
  afterEach(() => {
    onlineManager.setOnline(true)
    vi.unstubAllGlobals()
  })

  it.each<Source>(['projects', 'providers', 'failed', 'publication-failed', 'publication-pending'])(
    'never reports a clean result when %s failed, and retries it', async (source) => {
      let attempts = 0
      installFetch((raw) => {
        const url = new URL(raw, 'http://localhost')
        if (sourceFor(url) === source) {
          attempts += 1
          return attempts === 1 ? jsonResponse({ error: { message: 'unavailable' } }, 500) : page(source)
        }
        return defaultResponse(url)
      })
      const user = userEvent.setup()
      renderWithProviders(<OverviewPage />)
      const panel = alertPanel()
      expect(await panel.findByRole('alert')).toHaveTextContent('异常检查未完整加载')
      expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
      await user.click(panel.getByRole('button', { name: '重试' }))
      expect(await panel.findByText('暂无需要处理的异常')).toBeInTheDocument()
      expect(attempts).toBe(2)
    },
  )

  it('waits for an unresolved initial page before showing a clean result', async () => {
    let resolveProjects: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveProjects = resolve })
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      return sourceFor(url) === 'projects' ? pending : defaultResponse(url)
    })
    renderWithProviders(<OverviewPage />)
    const panel = alertPanel()
    expect(panel.getByRole('status')).toHaveTextContent('加载中')
    expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
    resolveProjects(page('projects'))
    expect(await panel.findByText('暂无需要处理的异常')).toBeInTheDocument()
  })

  it('does not show a clean result while a background refresh is unresolved', async () => {
    let resolveRefresh: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveRefresh = resolve })
    let attempts = 0
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (sourceFor(url) === 'projects') {
        attempts += 1
        return attempts === 1 ? page('projects') : pending
      }
      return defaultResponse(url)
    })
    const { queryClient } = renderWithProviders(<OverviewPage />)
    const panel = alertPanel()
    await panel.findByText('暂无需要处理的异常')
    act(() => { void queryClient.invalidateQueries({ queryKey: ['projects', 'overview'] }) })
    await waitFor(() => expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument())
    expect(panel.getByRole('status')).toHaveTextContent('加载中')
    resolveRefresh(page('projects'))
    expect(await panel.findByText('暂无需要处理的异常')).toBeInTheDocument()
  })

  it.each([
    ['projects', 'overview'],
    ['providers', 'overview'],
    ['reviews', 'overview', 'failed'],
    ['reviews', 'overview', 'publication-failed'],
    ['reviews', 'overview', 'publication-pending'],
  ])('does not show a clean result while %s refresh is paused offline', async (...key) => {
    installFetch((raw) => defaultResponse(new URL(raw, 'http://localhost')))
    const { queryClient } = renderWithProviders(<OverviewPage />)
    const panel = alertPanel()
    await panel.findByText('暂无需要处理的异常')
    act(() => {
      onlineManager.setOnline(false)
      void queryClient.invalidateQueries({ queryKey: key })
    })
    await waitFor(() => expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument())
    expect(panel.getByRole('status')).toHaveTextContent('等待网络连接')
    act(() => onlineManager.setOnline(true))
    expect(await panel.findByText('暂无需要处理的异常')).toBeInTheDocument()
  })

  it('retains alerts from successful sources while another source fails', async () => {
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (sourceFor(url) === 'projects') return page('projects', [project({ enabled: false, path_with_namespace: 'group/disabled' })])
      if (sourceFor(url) === 'failed') return jsonResponse({ error: { message: 'unavailable' } }, 500)
      return defaultResponse(url)
    })
    renderWithProviders(<OverviewPage />)
    const panel = alertPanel()
    expect(await panel.findByRole('alert')).toHaveTextContent('异常检查未完整加载')
    expect(await panel.findByText(/group\/disabled/)).toBeInTheDocument()
    expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
  })

  it.each([
    { source: 'projects', key: ['projects', 'overview'], button: '加载更多项目以继续检查' },
    { source: 'providers', key: ['providers', 'overview'], button: '加载更多 Provider 以继续检查' },
    { source: 'failed', key: ['reviews', 'overview', 'failed'], button: '加载更多告警数据' },
    { source: 'publication-failed', key: ['reviews', 'overview', 'publication-failed'], button: '加载更多告警数据' },
    { source: 'publication-pending', key: ['reviews', 'overview', 'publication-pending'], button: '加载更多告警数据' },
  ] as const)('does not allow a next page to hide a failed $source refresh', async ({ source, key, button }) => {
    let refreshes = 0
    let nextPages = 0
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (sourceFor(url) !== source) return defaultResponse(url)
      if (url.searchParams.get('cursor')) {
        nextPages += 1
        return page(source)
      }
      refreshes += 1
      return refreshes === 2 ? jsonResponse({ error: { message: 'refresh failed' } }, 500) : page(source, [], 'next')
    })
    const user = userEvent.setup()
    const { queryClient } = renderWithProviders(<OverviewPage />)
    const panel = alertPanel()
    await panel.findByRole('button', { name: button })
    await act(async () => { await queryClient.invalidateQueries({ queryKey: key }) })
    expect(await panel.findByRole('alert')).toHaveTextContent('异常检查未完整加载')
    const load = panel.getByRole('button', { name: button })
    await user.click(load)
    expect(load).toBeDisabled()
    expect(nextPages).toBe(0)
    expect(panel.getByRole('alert')).toHaveTextContent('异常检查未完整加载')
    expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
    await user.click(panel.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(panel.queryByRole('alert')).not.toBeInTheDocument())
    expect(refreshes).toBe(3)
    await user.click(panel.getByRole('button', { name: button }))
    await waitFor(() => expect(nextPages).toBe(1))
    expect(await panel.findByText('暂无需要处理的异常')).toBeInTheDocument()
  })

  it.each([
    { source: 'projects', key: ['projects', 'overview'], button: '加载更多项目以继续检查' },
    { source: 'providers', key: ['providers', 'overview'], button: '加载更多 Provider 以继续检查' },
    { source: 'failed', key: ['reviews', 'overview', 'failed'], button: '加载更多告警数据' },
    { source: 'publication-failed', key: ['reviews', 'overview', 'publication-failed'], button: '加载更多告警数据' },
    { source: 'publication-pending', key: ['reviews', 'overview', 'publication-pending'], button: '加载更多告警数据' },
  ] as const)('does not cancel an in-flight $source refresh to load the next page', async ({ source, key, button }) => {
    let resolveRefresh: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveRefresh = resolve })
    let refreshes = 0
    let nextPages = 0
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (sourceFor(url) !== source) return defaultResponse(url)
      if (url.searchParams.get('cursor')) { nextPages += 1; return page(source) }
      refreshes += 1
      return refreshes === 1 ? page(source, [], 'next') : pending
    })
    const user = userEvent.setup()
    const { queryClient } = renderWithProviders(<OverviewPage />)
    const panel = alertPanel()
    await panel.findByRole('button', { name: button })
    act(() => { void queryClient.invalidateQueries({ queryKey: key }) })
    await waitFor(() => expect(panel.getByRole('status')).toHaveTextContent('加载中'))
    const load = panel.getByRole('button', { name: button })
    await user.click(load)
    expect(load).toBeDisabled()
    expect(nextPages).toBe(0)
    expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
    await act(async () => { resolveRefresh(page(source, [], 'next')) })
    await waitFor(() => expect(load).toBeEnabled())
  })

  it.each([
    { source: 'projects', button: '加载更多项目以继续检查', item: project({ enabled: false, path_with_namespace: 'group/disabled-later' }), text: /group\/disabled-later/ },
    { source: 'providers', button: '加载更多 Provider 以继续检查', item: { name: 'provider-later', stats_24h: { errors: 2 } }, text: /provider-later/ },
    { source: 'failed', button: '加载更多告警数据', item: review({ id: 'failed-later', mr_title: 'failed later', status: 'failed' }), text: /failed later/ },
    { source: 'publication-failed', button: '加载更多告警数据', item: review({ id: 'publication-later', mr_title: 'publication later', publication_state: 'failed' }), text: /publication later/ },
    { source: 'publication-pending', button: '加载更多告警数据', item: review({ id: 'retry-later', mr_title: 'retry later', publication_state: 'pending', publication_error: 'retrying' }), text: /retry later/ },
  ] as const)('loads $source beyond an empty first page and retries a page failure', async ({ source, button, item, text }) => {
    let resolveLater: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveLater = resolve })
    let laterAttempts = 0
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (sourceFor(url) === source) {
        if (url.searchParams.get('cursor') === 'next') {
          laterAttempts += 1
          return laterAttempts === 1 ? pending : page(source, [item])
        }
        return page(source, [], 'next')
      }
      return defaultResponse(url)
    })
    const user = userEvent.setup()
    renderWithProviders(<OverviewPage />)
    const panel = alertPanel()
    const load = await panel.findByRole('button', { name: button })
    expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
    await user.click(load)
    await waitFor(() => expect(panel.getByRole('button', { name: button })).toBeDisabled())
    expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
    resolveLater(jsonResponse({ error: { message: 'later page failed' } }, 500))
    expect(await panel.findByRole('alert')).toHaveTextContent('异常检查未完整加载')
    expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
    await user.click(panel.getByRole('button', { name: '重试' }))
    expect(await panel.findByText(text)).toBeInTheDocument()
    expect(laterAttempts).toBe(2)
    expect(panel.queryByText('暂无需要处理的异常')).not.toBeInTheDocument()
  })

  it('shows a clean result for a viewer without requesting admin-only providers', async () => {
    const requests: string[] = []
    installFetch((raw) => {
      requests.push(raw)
      return defaultResponse(new URL(raw, 'http://localhost'))
    })
    renderWithProviders(<OverviewPage />, { me: viewerMe })
    const panel = alertPanel()
    expect(await panel.findByText('暂无需要处理的异常')).toBeInTheDocument()
    expect(panel.queryByText(/provider 错误/)).not.toBeInTheDocument()
    expect(requests.some((raw) => new URL(raw, 'http://localhost').pathname === '/api/v1/providers')).toBe(false)
  })
})
