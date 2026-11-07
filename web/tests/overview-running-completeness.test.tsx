import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { onlineManager } from '@tanstack/react-query'
import { OverviewPage } from '../src/pages/OverviewPage'
import { review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const runningKey = ['reviews', 'overview', 'running']

function setup(running: (url: URL) => Response | Promise<Response>) {
  installFetch((raw) => {
    const url = new URL(raw, 'http://localhost')
    if (url.pathname === '/api/v1/reviews' && url.searchParams.get('status') === 'running') return running(url)
    if (url.pathname === '/api/v1/reviews') return jsonResponse({ reviews: [] })
    if (url.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
    if (url.pathname === '/api/v1/providers') return jsonResponse({ providers: [] })
    return jsonResponse({ runs: 0 })
  })
  return renderWithProviders(<OverviewPage />)
}

function panel() {
  const section = screen.getByRole('heading', { name: '运行中' }).closest('section')
  if (!section) throw new Error('running panel missing')
  return within(section)
}

describe('overview running completeness', () => {
  afterEach(() => {
    onlineManager.setOnline(true)
    vi.unstubAllGlobals()
  })

  it('shows an empty result only after a complete successful response', async () => {
    setup(() => jsonResponse({ reviews: [] }))
    expect(await panel().findByText('当前没有运行中的评审')).toBeInTheDocument()
  })

  it('keeps the empty result visible while refreshing and shows a non-blocking status', async () => {
    let resolveRefresh: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveRefresh = resolve })
    let calls = 0
    const { queryClient } = setup(() => ++calls === 1 ? jsonResponse({ reviews: [] }) : pending)
    await panel().findByText('当前没有运行中的评审')
    act(() => { void queryClient.invalidateQueries({ queryKey: runningKey }) })
    await waitFor(() => expect(panel().getByText(/刷新中/)).toBeInTheDocument())
    expect(panel().getByText('当前没有运行中的评审')).toBeInTheDocument()
    expect(panel().queryByText(/^加载中/)).not.toBeInTheDocument()
    resolveRefresh(jsonResponse({ reviews: [] }))
    expect(await panel().findByText('当前没有运行中的评审')).toBeInTheDocument()
  })

  it('suppresses the empty result while a refresh is paused offline', async () => {
    const { queryClient } = setup(() => jsonResponse({ reviews: [] }))
    await panel().findByText('当前没有运行中的评审')
    act(() => {
      onlineManager.setOnline(false)
      void queryClient.invalidateQueries({ queryKey: runningKey })
    })
    await waitFor(() => expect(panel().queryByText('当前没有运行中的评审')).not.toBeInTheDocument())
    expect(panel().getByRole('status')).toHaveTextContent('等待网络连接')
    act(() => onlineManager.setOnline(true))
    expect(await panel().findByText('当前没有运行中的评审')).toBeInTheDocument()
  })

  it('loads a later running review after an empty first page', async () => {
    setup((url) => url.searchParams.get('cursor')
      ? jsonResponse({ reviews: [review({ id: 'later', mr_title: 'later running review' })] })
      : jsonResponse({ reviews: [], next_cursor: 'next' }))
    const load = await panel().findByRole('button', { name: '加载更多运行中评审' })
    expect(panel().queryByText('当前没有运行中的评审')).not.toBeInTheDocument()
    await userEvent.setup().click(load)
    expect(await panel().findByText('later running review')).toBeInTheDocument()
    expect(panel().queryByText('当前没有运行中的评审')).not.toBeInTheDocument()
  })

  it('keeps known reviews visible while a refresh is pending', async () => {
    let resolveRefresh: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveRefresh = resolve })
    let calls = 0
    const data = { reviews: [review({ id: 'known', mr_title: 'known running review' })] }
    const { queryClient } = setup(() => ++calls === 1 ? jsonResponse(data) : pending)
    await panel().findByText('known running review')
    act(() => { void queryClient.invalidateQueries({ queryKey: runningKey }) })
    await waitFor(() => expect(panel().getByText(/刷新中/)).toBeInTheDocument())
    expect(panel().queryByText(/^加载中/)).not.toBeInTheDocument()
    expect(panel().getByText('known running review')).toBeInTheDocument()
    resolveRefresh(jsonResponse(data))
    await waitFor(() => expect(panel().queryByText(/刷新中/)).not.toBeInTheDocument())
  })

  it('keeps pagination available after a failed refresh is retried', async () => {
    let resolveRefresh: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveRefresh = resolve })
    let calls = 0
    const first = { reviews: [review({ id: 'known', mr_title: 'known running review' })], next_cursor: 'next' }
    const { queryClient } = setup((url) => {
      if (url.searchParams.get('cursor')) return jsonResponse({ reviews: [review({ id: 'later', mr_title: 'later running review' })] })
      calls += 1
      return calls === 2 ? pending : jsonResponse(first)
    })
    const user = userEvent.setup()
    const load = await panel().findByRole('button', { name: '加载更多运行中评审' })
    act(() => { void queryClient.invalidateQueries({ queryKey: runningKey }) })
    await waitFor(() => expect(load).toBeDisabled())
    expect(panel().getByText('known running review')).toBeInTheDocument()
    resolveRefresh(jsonResponse({ error: { message: 'unavailable' } }, 500))
    expect(await panel().findByRole('alert')).toBeInTheDocument()
    expect(load).toBeDisabled()
    expect(panel().queryByText('当前没有运行中的评审')).not.toBeInTheDocument()
    await user.click(panel().getByRole('button', { name: '重试' }))
    await waitFor(() => expect(load).toBeEnabled())
    await user.click(load)
    expect(await panel().findByText('later running review')).toBeInTheDocument()
  })

  it('ticks the elapsed clock inside the running list', async () => {
    setup(() => jsonResponse({
      reviews: [review({ created_at: new Date(Date.now() - 200).toISOString() })],
    }))
    const elapsed = await panel().findByText(/已用时 \d+s/)
    const before = elapsed.textContent
    await waitFor(() => {
      expect(panel().getByText(/已用时 \d+s/).textContent).not.toBe(before)
    }, { timeout: 2500 })
  })
})
