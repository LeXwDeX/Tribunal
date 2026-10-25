import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const firstPage = [
  { id: 1, path_with_namespace: 'group/first' },
  { id: 2, path_with_namespace: 'group/second' },
]
const secondPage = [
  { id: 3, path_with_namespace: 'group/third' },
]

function projectIdFromStatsUrl(url: string) {
  return new URL(url, 'http://localhost').searchParams.get('project_id')
}

function runsMetric() {
  const label = screen.getByText('运行次数')
  const metric = label.closest('div')
  if (!metric) throw new Error('runs metric not found')
  return metric
}

describe('statistics project selector pagination', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('loads the next project page and applies the selected project', async () => {
    const statsCalls: string[] = []
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') {
        if (parsed.searchParams.get('cursor') === 'page-2') return jsonResponse({ projects: secondPage })
        return jsonResponse({ projects: firstPage, next_cursor: 'page-2' })
      }
      if (parsed.pathname === '/api/v1/stats') {
        statsCalls.push(url)
        return jsonResponse({ runs: 1 })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)

    await screen.findByRole('option', { name: 'group/first' })
    expect(screen.queryByRole('option', { name: 'group/third' })).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '加载更多项目' }))
    await screen.findByRole('option', { name: 'group/third' })

    await user.selectOptions(screen.getByRole('combobox', { name: '项目' }), '3')
    await user.click(screen.getByRole('button', { name: '应用' }))

    await waitFor(() => {
      expect(statsCalls.some((url) => projectIdFromStatsUrl(url) === '3')).toBe(true)
    })
  })

  it('keeps the first page while the next page loads or fails, then retries', async () => {
    let resolveSecondPage: (value: Response) => void = () => {}
    const secondPageRequest = new Promise<Response>((resolve) => { resolveSecondPage = resolve })
    let secondPageAttempts = 0
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') {
        if (parsed.searchParams.get('cursor') === 'page-2') {
          secondPageAttempts += 1
          if (secondPageAttempts === 1) return secondPageRequest
          return jsonResponse({ projects: secondPage })
        }
        return jsonResponse({ projects: firstPage, next_cursor: 'page-2' })
      }
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)

    await screen.findByRole('option', { name: 'group/first' })
    await user.click(screen.getByRole('button', { name: '加载更多项目' }))

    await waitFor(() => expect(screen.getByRole('button', { name: '加载更多项目' })).toBeDisabled())
    expect(secondPageAttempts).toBe(1)
    expect(screen.getByRole('option', { name: 'group/first' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'group/second' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: 'group/third' })).not.toBeInTheDocument()

    resolveSecondPage(jsonResponse({ error: { message: 'second page failed' } }, 500))
    await screen.findByRole('button', { name: '重试加载更多项目' })
    expect(screen.getByText('更多项目加载失败；已加载的项目仍可选择。')).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'group/first' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'group/second' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '重试加载更多项目' }))
    await screen.findByRole('option', { name: 'group/third' })
    expect(secondPageAttempts).toBe(2)
    expect(screen.getByRole('option', { name: 'group/first' })).toBeInTheDocument()
    expect(screen.queryByText('更多项目加载失败；已加载的项目仍可选择。')).not.toBeInTheDocument()
  })

  it('retries a failed background refresh when cached projects have no next page', async () => {
    let projectAttempts = 0
    let resolveRefresh: (value: Response) => void = () => {}
    const refreshPending = new Promise<Response>((resolve) => { resolveRefresh = resolve })
    const projectRequests: string[] = []
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') {
        projectAttempts += 1
        projectRequests.push(url)
        if (projectAttempts === 1) return jsonResponse({ projects: firstPage })
        if (projectAttempts === 2) return jsonResponse({ error: { message: 'refresh failed' } }, 500)
        return refreshPending
      }
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    const { queryClient } = renderWithProviders(<StatisticsPage />)

    await screen.findByRole('option', { name: 'group/first' })
    await user.selectOptions(screen.getByRole('combobox', { name: '项目' }), '2')

    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ['projects', 'statistics'] })
    })
    await screen.findByText(/项目列表刷新失败/)
    expect(screen.queryByRole('button', { name: '重试加载更多项目' })).not.toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'group/first' })).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: '项目' })).toHaveValue('2')

    await user.click(screen.getByRole('button', { name: '重试刷新项目' }))
    await waitFor(() => expect(screen.getByRole('button', { name: '重试刷新项目' })).toBeDisabled())
    expect(screen.getByRole('option', { name: 'group/first' })).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: '项目' })).toHaveValue('2')

    await act(async () => { resolveRefresh(jsonResponse({ projects: firstPage })) })
    await waitFor(() => expect(screen.queryByText(/项目列表刷新失败/)).not.toBeInTheDocument())
    expect(screen.getByRole('combobox', { name: '项目' })).toHaveValue('2')
    expect(projectRequests).toHaveLength(3)
    expect(new URL(projectRequests[2], 'http://localhost').searchParams.get('cursor')).toBeNull()
  })

  it('keeps load more separate from a failed refresh and refetches the first page', async () => {
    let projectAttempts = 0
    const projectRequests: string[] = []
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') {
        projectAttempts += 1
        projectRequests.push(url)
        if (parsed.searchParams.get('cursor') === 'page-2') return jsonResponse({ projects: secondPage })
        if (projectAttempts === 2) return jsonResponse({ error: { message: 'refresh failed' } }, 500)
        return jsonResponse({ projects: firstPage, next_cursor: 'page-2' })
      }
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    const { queryClient } = renderWithProviders(<StatisticsPage />)

    await screen.findByRole('option', { name: 'group/first' })
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ['projects', 'statistics'] })
    })
    await screen.findByText(/项目列表刷新失败/)
    expect(screen.queryByRole('button', { name: '重试加载更多项目' })).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '重试刷新项目' }))
    await waitFor(() => expect(screen.queryByText(/项目列表刷新失败/)).not.toBeInTheDocument())
    expect(new URL(projectRequests[2], 'http://localhost').searchParams.get('cursor')).toBeNull()

    await user.click(screen.getByRole('button', { name: '加载更多项目' }))
    await screen.findByRole('option', { name: 'group/third' })
    expect(new URL(projectRequests[3], 'http://localhost').searchParams.get('cursor')).toBe('page-2')
  })

  it('retries the first project page after a load failure', async () => {
    let attempts = 0
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') {
        attempts += 1
        if (attempts === 1) return jsonResponse({ error: { message: 'first page failed' } }, 500)
        return jsonResponse({ projects: firstPage })
      }
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)

    await screen.findByText('项目筛选列表加载失败，仍可查看全部项目统计。')
    expect(screen.queryByRole('option', { name: 'group/first' })).not.toBeInTheDocument()

    const retry = screen.getByRole('button', { name: '重试加载项目' })
    expect(retry.className).toMatch(/secondary/)
    await user.click(retry)

    await screen.findByRole('option', { name: 'group/first' })
    expect(attempts).toBe(2)
    expect(screen.queryByText('项目筛选列表加载失败，仍可查看全部项目统计。')).not.toBeInTheDocument()
  })

  it('does not show phantom zero stats for a project without stats', async () => {
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: firstPage.slice(0, 1) })
      if (parsed.pathname === '/api/v1/stats') {
        return jsonResponse(parsed.searchParams.get('project_id') ? {} : { runs: 7 })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)

    await screen.findByRole('option', { name: 'group/first' })
    await waitFor(() => expect(within(runsMetric()).getByText('7')).toBeInTheDocument())

    await user.selectOptions(screen.getByRole('combobox', { name: '项目' }), '1')
    await user.click(screen.getByRole('button', { name: '应用' }))

    await waitFor(() => expect(within(runsMetric()).getByText('不可用')).toBeInTheDocument())
    expect(within(runsMetric()).queryByText('0')).not.toBeInTheDocument()
  })
})
