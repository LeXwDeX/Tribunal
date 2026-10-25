import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ProjectsPage } from '../src/pages/ProjectsPage'
import { usePaginated } from '../src/lib/hooks'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

type ProbeItem = { id: number }

function PaginationProbe({ fetchPage }: { fetchPage: (cursor?: string) => Promise<{ items: ProbeItem[]; next_cursor?: string }> }) {
  const page = usePaginated<ProbeItem>(['probe'], fetchPage, 'items')
  return (
    <div>
      <span data-testid="items">{page.items.map((item) => item.id).join(',')}</span>
      <span data-testid="state">{page.isError ? 'error' : page.isFetching ? 'fetching' : 'idle'}</span>
      <button onClick={page.fetchNextPage}>next</button>
      <button onClick={page.retry}>retry</button>
      <button onClick={page.refetch}>refetch</button>
    </div>
  )
}

describe('usePaginated retry', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('retries a failed next page with the same cursor and keeps cached items', async () => {
    const calls: Array<string | undefined> = []
    let nextPageAttempts = 0
    const fetchPage = async (cursor?: string) => {
      calls.push(cursor)
      if (!cursor) return { items: [{ id: 1 }], next_cursor: 'p2' }
      nextPageAttempts += 1
      if (nextPageAttempts === 1) throw new Error('next page failed')
      return { items: [{ id: 2 }] }
    }
    const user = userEvent.setup()
    renderWithProviders(<PaginationProbe fetchPage={fetchPage} />)

    await screen.findByText('1')
    await user.click(screen.getByRole('button', { name: 'next' }))
    await waitFor(() => expect(screen.getByTestId('state')).toHaveTextContent('error'), { timeout: 1500 })
    expect(screen.getByTestId('items')).toHaveTextContent('1')

    await user.click(screen.getByRole('button', { name: 'retry' }))
    await waitFor(() => expect(screen.getByTestId('items')).toHaveTextContent('1,2'), { timeout: 1500 })
    expect(calls).toEqual([undefined, 'p2', 'p2'])
  })

  it('refetch reloads every loaded page instead of jumping to the next cursor', async () => {
    const calls: Array<string | undefined> = []
    const fetchPage = async (cursor?: string) => {
      calls.push(cursor)
      if (!cursor) return { items: [{ id: 1 }], next_cursor: 'p2' }
      return { items: [{ id: 2 }] }
    }
    const user = userEvent.setup()
    renderWithProviders(<PaginationProbe fetchPage={fetchPage} />)

    await screen.findByText('1')
    await user.click(screen.getByRole('button', { name: 'next' }))
    await waitFor(() => expect(screen.getByTestId('items')).toHaveTextContent('1,2'))

    await user.click(screen.getByRole('button', { name: 'refetch' }))
    await waitFor(() => expect(calls).toHaveLength(4))
    expect([...calls.slice(2)].sort()).toEqual(['p2', undefined].sort())
    expect(calls.slice(2)).toContain(undefined)
    expect(screen.getByTestId('items')).toHaveTextContent('1,2')
  })

  it('retries a failed first page with a fresh first-page request', async () => {
    const calls: Array<string | undefined> = []
    let attempts = 0
    const fetchPage = async (cursor?: string) => {
      calls.push(cursor)
      attempts += 1
      if (attempts === 1) throw new Error('first page failed')
      return { items: [{ id: 1 }] }
    }
    const user = userEvent.setup()
    renderWithProviders(<PaginationProbe fetchPage={fetchPage} />)

    await waitFor(() => expect(screen.getByTestId('state')).toHaveTextContent('error'), { timeout: 1500 })
    await user.click(screen.getByRole('button', { name: 'retry' }))
    await waitFor(() => expect(screen.getByTestId('items')).toHaveTextContent('1'), { timeout: 1500 })
    expect(calls).toEqual([undefined, undefined])
  })
})

describe('ProjectsPage pagination recovery', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('retries a failed next page with its cursor while keeping the first page', async () => {
    const calls: Array<string | null> = []
    let nextPageAttempts = 0
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') {
        const cursor = parsed.searchParams.get('cursor')
        calls.push(cursor)
        if (cursor === 'p2') {
          nextPageAttempts += 1
          if (nextPageAttempts === 1) return jsonResponse({ error: { message: 'next page failed' } }, 500)
          return jsonResponse({ projects: [project({ id: 2, path_with_namespace: 'group/second' })] })
        }
        return jsonResponse({ projects: [project({ id: 1, path_with_namespace: 'group/first' })], next_cursor: 'p2' })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<ProjectsPage />, { route: '/projects' })

    await screen.findByRole('link', { name: 'group/first' })
    await user.click(screen.getByRole('button', { name: '加载更多项目' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('加载失败')
    expect(screen.getByRole('link', { name: 'group/first' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '重试' }))
    await screen.findByRole('link', { name: 'group/second' }, { timeout: 1500 })
    expect(screen.getByRole('link', { name: 'group/first' })).toBeInTheDocument()
    expect(calls).toEqual([null, 'p2', 'p2'])
  })
})
