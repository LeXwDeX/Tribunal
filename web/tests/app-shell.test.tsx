import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { AppShell } from '../src/components/AppShell'
import { ToastProvider } from '../src/components/ui'
import { adminMe, createTestQueryClient, installFetch, jsonResponse } from './test-utils'

const runningKey = ['reviews', 'overview', 'running']

function renderShell(route = '/', queryClient: QueryClient = createTestQueryClient()) {
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[route]}>
        <ToastProvider>
          <AppShell me={adminMe} />
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
  return queryClient
}

const review = (id: string) => ({ id, project_id: 7, project_path: 'group/app', mr_iid: 42, status: 'running' })

describe('AppShell navigation', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('groups navigation into workbench, configuration and insight', () => {
    installFetch(() => jsonResponse({ reviews: [] }))
    renderShell()
    const nav = screen.getByRole('navigation', { name: '主导航' })
    const groups = within(nav).getAllByRole('group')
    expect(groups.map((group) => group.getAttribute('aria-label'))).toEqual(['工作台', '配置', '洞察'])
    const linkNames = (name: string) => within(within(nav).getByRole('group', { name }))
      .getAllByRole('link').map((link) => link.textContent)
    expect(linkNames('工作台')).toEqual(['总览', expect.stringMatching(/^评审/)])
    expect(linkNames('配置')).toEqual(['项目', '扩展', '模型与档位'])
    expect(linkNames('洞察')).toEqual(['统计与反馈', '系统'])
  })

  it('shows the running review count from the overview running query', async () => {
    const fetchMock = installFetch((url) => {
      if (url.includes('/api/v1/reviews') && url.includes('status=running')) {
        return jsonResponse({ reviews: [review('r1'), review('r2')] })
      }
      return jsonResponse({}, 404)
    })
    renderShell()
    expect(await screen.findByRole('link', { name: '评审 2 个运行中' })).toHaveAttribute('href', '/reviews')
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('reads the count from the same cache key the overview page fills', () => {
    installFetch(() => new Promise<Response>(() => {}))
    const client = createTestQueryClient()
    client.setQueryData(runningKey, { pages: [{ reviews: [review('r1'), review('r2'), review('r3')] }], pageParams: [undefined] })
    renderShell('/', client)
    expect(screen.getByRole('link', { name: '评审 3 个运行中' })).toBeInTheDocument()
  })

  it('hides the count when no review is running', async () => {
    const fetchMock = installFetch(() => jsonResponse({ reviews: [] }))
    renderShell()
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalled())
    await screen.findByRole('link', { name: '评审' })
    expect(screen.queryByText(/个运行中/)).not.toBeInTheDocument()
  })
})

describe('AppShell breadcrumbs', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  const crumbs = () => screen.getByRole('navigation', { name: '面包屑' })

  it('names the top-level page', () => {
    installFetch(() => jsonResponse({ reviews: [] }))
    renderShell('/statistics')
    expect(within(crumbs()).getByText('统计与反馈')).toHaveAttribute('aria-current', 'page')
  })

  it('shows project path and MR iid on a review detail page', () => {
    installFetch(() => new Promise<Response>(() => {}))
    const client = createTestQueryClient()
    client.setQueryData(['review', 'r1', 'detail'], review('r1'))
    renderShell('/reviews/r1', client)
    expect(within(crumbs()).getByRole('link', { name: '评审' })).toHaveAttribute('href', '/reviews')
    expect(within(crumbs()).getByText('group/app !42')).toHaveAttribute('aria-current', 'page')
  })

  it('reads the project path from the cached project when the detail API omits it', () => {
    installFetch(() => new Promise<Response>(() => {}))
    const client = createTestQueryClient()
    const { project_path: _omitted, ...detail } = review('r2')
    client.setQueryData(['review', 'r2', 'detail'], detail)
    client.setQueryData(['project', detail.project_id], { id: detail.project_id, path_with_namespace: 'group/cached' })
    renderShell('/reviews/r2', client)
    expect(within(crumbs()).getByText(`group/cached !${detail.mr_iid}`)).toHaveAttribute('aria-current', 'page')
  })

  it('falls back to the review ID before the detail has loaded', () => {
    installFetch(() => new Promise<Response>(() => {}))
    renderShell('/reviews/r9')
    expect(within(crumbs()).getByText('r9')).toHaveAttribute('aria-current', 'page')
  })

  it('shows the project path on a project detail page', () => {
    installFetch(() => new Promise<Response>(() => {}))
    const client = createTestQueryClient()
    client.setQueryData(['project', 7], { id: 7, path_with_namespace: 'group/app' })
    renderShell('/projects/7/settings', client)
    expect(within(crumbs()).getByRole('link', { name: '项目' })).toHaveAttribute('href', '/projects')
    expect(within(crumbs()).getByText('group/app')).toHaveAttribute('aria-current', 'page')
  })

  it('labels unknown routes', () => {
    installFetch(() => jsonResponse({ reviews: [] }))
    renderShell('/nope')
    expect(within(crumbs()).getByText('页面不存在')).toBeInTheDocument()
  })
})
