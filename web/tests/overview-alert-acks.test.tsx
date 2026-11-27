import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { OverviewPage } from '../src/pages/OverviewPage'
import { disabledFingerprint, workspaceFingerprint } from '../src/lib/alertAcks'
import { project, review } from './fixtures'
import { createTestQueryClient, installFetch, jsonResponse, renderWithProviders } from './test-utils'

const disabled = project({ id: 42, path_with_namespace: 'group/disabled', enabled: false, created_at: '2026-01-01T00:00:00Z', revision: 3 })
const broken = project({
  id: 43,
  path_with_namespace: 'group/broken',
  created_at: '2026-01-02T00:00:00Z',
  workspace: { state: 'error', last_error: 'clone failed', last_refresh_at: '2026-02-01T00:00:00Z' },
})

interface Calls {
  writes: { method: string; path: string; body?: unknown }[]
}

function routeOverview(options: {
  projects?: unknown[]
  incomplete?: unknown[]
  acks?: () => Response | Promise<Response>
  put?: (path: string, body: { fingerprint: string }) => Response
  del?: (path: string) => Response | Promise<Response>
}): Calls {
  const calls: Calls = { writes: [] }
  installFetch((url, init) => {
    const parsed = new URL(url, 'http://localhost')
    const method = init?.method || 'GET'
    if (parsed.pathname.startsWith('/api/v1/alert-acks')) {
      if (method === 'GET') return options.acks ? options.acks() : jsonResponse({ acks: [] })
      const body = init?.body ? JSON.parse(String(init.body)) as { fingerprint: string } : undefined
      calls.writes.push({ method, path: parsed.pathname, body })
      if (method === 'PUT') {
        if (options.put) return options.put(parsed.pathname, body!)
        return jsonResponse({ key: decodeURIComponent(parsed.pathname.split('/').pop()!), fingerprint: body!.fingerprint, acked_at: '2026-11-27T10:00:00Z' })
      }
      if (options.del) return options.del(parsed.pathname)
      return new Response(null, { status: 204 })
    }
    if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
    if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: options.projects || [] })
    if (parsed.pathname === '/api/v1/reviews') {
      if (parsed.searchParams.get('verdict') === 'INCOMPLETE') return jsonResponse({ reviews: options.incomplete || [] })
      return jsonResponse({ reviews: [] })
    }
    return jsonResponse({})
  })
  return calls
}

function alertPanel() {
  const panel = screen.getByRole('heading', { name: '告警' }).closest('section')
  if (!panel) throw new Error('alerts panel missing')
  return panel
}

describe('overview alert acks', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('hides a row after the server stores the ack and counts only unread alerts', async () => {
    const calls = routeOverview({ projects: [disabled, broken] })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    const button = await within(panel).findByRole('button', { name: '标记已读：已停用' })
    expect(within(panel).getByText('2 项')).toBeInTheDocument()
    await userEvent.click(button)

    await waitFor(() => expect(within(panel).queryByText(/group\/disabled 已停用/)).not.toBeInTheDocument())
    expect(calls.writes).toEqual([{ method: 'PUT', path: '/api/v1/alert-acks/disabled-42', body: { fingerprint: disabledFingerprint(disabled) } }])
    expect(within(panel).getByText('1 项')).toBeInTheDocument()
    expect(within(panel).getByText(/group\/broken 工作区异常/)).toBeInTheDocument()
    expect(within(panel).getByRole('button', { name: '显示已读（1）' })).toHaveAttribute('aria-pressed', 'false')
  })

  it('keeps the row and shows a toast when storing the ack fails', async () => {
    routeOverview({
      projects: [disabled],
      put: () => jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500),
    })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    await userEvent.click(await within(panel).findByRole('button', { name: '标记已读：已停用' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('标记已读失败')
    expect(within(panel).getByText(/group\/disabled 已停用/)).toBeInTheDocument()
    expect(within(panel).getByText('1 项')).toBeInTheDocument()
    expect(within(panel).queryByRole('button', { name: /显示已读/ })).not.toBeInTheDocument()
  })

  it('brings an aggregate alert back with only the hits that finished after the watermark', async () => {
    const b = review({ id: 'b', status: 'completed', verdict: 'INCOMPLETE', created_at: '2026-11-27T09:00:00Z', finished_at: '2026-11-27T09:10:00Z' })
    const a = review({ id: 'a', status: 'completed', verdict: 'INCOMPLETE', created_at: '2026-11-27T08:00:00Z', finished_at: '2026-11-27T09:30:00Z' })
    routeOverview({
      incomplete: [b, a],
      acks: () => jsonResponse({ acks: [{ key: 'reviews-incomplete', fingerprint: '2026-11-27T09:10:00.000Z', acked_at: '2026-11-27T09:15:00Z' }] }),
    })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    expect(await within(panel).findByText(/自上次已读后新增 1 个已结束评审为 INCOMPLETE/)).toBeInTheDocument()
    expect(within(panel).getByText('1 项')).toBeInTheDocument()
  })

  it('treats an aggregate alert as read while nothing finished after the watermark', async () => {
    const b = review({ id: 'b', status: 'completed', verdict: 'INCOMPLETE', created_at: '2026-11-27T09:00:00Z', finished_at: '2026-11-27T09:10:00Z' })
    const calls = routeOverview({
      incomplete: [b],
      acks: () => jsonResponse({ acks: [{ key: 'reviews-incomplete', fingerprint: '2026-11-27T09:10:00.000Z', acked_at: '2026-11-27T09:15:00Z' }] }),
    })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    expect(await within(panel).findByText('无未读告警')).toBeInTheDocument()
    expect(within(panel).queryByText(/INCOMPLETE（未完成）/)).not.toBeInTheDocument()
    await userEvent.click(within(panel).getByRole('button', { name: '显示已读（1）' }))
    expect(within(panel).getByText(/1 个已结束评审为 INCOMPLETE/)).toBeInTheDocument()
    expect(calls.writes).toEqual([])
  })

  it('shows read alerts on demand and undoes an ack', async () => {
    const calls = routeOverview({
      projects: [disabled, broken],
      acks: () => jsonResponse({ acks: [{ key: 'disabled-42', fingerprint: disabledFingerprint(disabled), acked_at: '2026-11-27T09:00:00Z' }] }),
    })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    const toggle = await within(panel).findByRole('button', { name: '显示已读（1）' })
    expect(within(panel).queryByText(/group\/disabled 已停用/)).not.toBeInTheDocument()
    expect(within(panel).getByText('1 项')).toBeInTheDocument()

    await userEvent.click(toggle)
    expect(toggle).toHaveAttribute('aria-pressed', 'true')
    expect(within(panel).getByText(/group\/disabled 已停用/)).toBeInTheDocument()
    await userEvent.click(within(panel).getByRole('button', { name: '撤销已读：已停用' }))

    await waitFor(() => expect(calls.writes).toEqual([{ method: 'DELETE', path: '/api/v1/alert-acks/disabled-42', body: undefined }]))
    expect(await within(panel).findByRole('button', { name: '标记已读：已停用' })).toBeInTheDocument()
    expect(within(panel).getByText('2 项')).toBeInTheDocument()
    expect(within(panel).queryByRole('button', { name: /显示已读/ })).not.toBeInTheDocument()
  })

  it('reappears when the acked object changed state', async () => {
    const stale = { ...broken, workspace: { ...broken.workspace, last_error: 'index failed' } }
    routeOverview({
      projects: [broken],
      acks: () => jsonResponse({ acks: [{ key: 'workspace-43', fingerprint: workspaceFingerprint(stale), acked_at: '2026-11-27T09:00:00Z' }] }),
    })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    expect(await within(panel).findByRole('button', { name: '标记已读：工作区' })).toBeInTheDocument()
    expect(within(panel).queryByRole('button', { name: /显示已读/ })).not.toBeInTheDocument()
  })

  it('does not render alert rows until acks load', async () => {
    let release: (response: Response) => void = () => {}
    routeOverview({
      projects: [disabled],
      acks: () => new Promise<Response>((resolve) => { release = resolve }),
    })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    expect(await screen.findByText(/1 个已注册项目/)).toBeInTheDocument()
    expect(within(panel).queryByText(/group\/disabled 已停用/)).not.toBeInTheDocument()
    release(jsonResponse({ acks: [] }))
    expect(await within(panel).findByText(/group\/disabled 已停用/)).toBeInTheDocument()
  })

  it('shows every alert without ack buttons when acks fail to load', async () => {
    routeOverview({
      projects: [disabled, broken],
      acks: () => jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500),
    })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    expect(await within(panel).findByText(/group\/disabled 已停用/)).toBeInTheDocument()
    expect(within(panel).getByText(/group\/broken 工作区异常/)).toBeInTheDocument()
    expect(within(panel).getByText('2 项')).toBeInTheDocument()
    expect(within(panel).queryByRole('button', { name: /标记已读/ })).not.toBeInTheDocument()
    expect(within(panel).getByText(/已读记录加载失败/)).toHaveAttribute('role', 'status')
  })
  it('ignores cached acks after a failed refetch and shows every alert', async () => {
    routeOverview({
      projects: [disabled, broken],
      acks: () => jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500),
    })
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(['alert-acks'], { acks: [{ key: 'disabled-42', fingerprint: disabledFingerprint(disabled), acked_at: '2026-11-27T09:00:00Z' }] })
    renderWithProviders(<OverviewPage />, { route: '/', queryClient })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    expect(await within(panel).findByText(/已读记录加载失败/)).toHaveAttribute('role', 'status')
    expect(within(panel).getByText(/group\/disabled 已停用/)).toBeInTheDocument()
    expect(within(panel).getByText(/group\/broken 工作区异常/)).toBeInTheDocument()
    expect(within(panel).getByText('2 项')).toBeInTheDocument()
    expect(within(panel).queryByRole('button', { name: /标记已读|撤销已读|显示已读/ })).not.toBeInTheDocument()
  })

  it('collapses "show read" after consecutive undos so a new ack hides its row', async () => {
    const releases: (() => void)[] = []
    routeOverview({
      projects: [disabled, broken],
      acks: () => jsonResponse({ acks: [
        { key: 'disabled-42', fingerprint: disabledFingerprint(disabled), acked_at: '2026-11-27T09:00:00Z' },
        { key: 'workspace-43', fingerprint: workspaceFingerprint(broken), acked_at: '2026-11-27T09:00:00Z' },
      ] }),
      del: () => new Promise<Response>((resolve) => { releases.push(() => resolve(new Response(null, { status: 204 }))) }),
    })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    await userEvent.click(await within(panel).findByRole('button', { name: '显示已读（2）' }))
    // Both undos start before either response arrives.
    await userEvent.click(within(panel).getByRole('button', { name: '撤销已读：已停用' }))
    await userEvent.click(within(panel).getByRole('button', { name: '撤销已读：工作区' }))
    await waitFor(() => expect(releases).toHaveLength(2))
    releases.forEach((release) => release())
    await within(panel).findByRole('button', { name: '标记已读：工作区' })
    await waitFor(() => expect(within(panel).queryByRole('button', { name: /显示已读/ })).not.toBeInTheDocument())

    await userEvent.click(within(panel).getByRole('button', { name: '标记已读：已停用' }))
    await waitFor(() => expect(within(panel).queryByText(/group\/disabled 已停用/)).not.toBeInTheDocument())
    expect(within(panel).getByRole('button', { name: '显示已读（1）' })).toHaveAttribute('aria-pressed', 'false')
  })

  it('moves focus to the next ack button, then the previous one, then the toggle', async () => {
    const third = project({ id: 44, path_with_namespace: 'group/third', enabled: false, created_at: '2026-01-03T00:00:00Z', revision: 1 })
    routeOverview({ projects: [disabled, broken, third] })
    renderWithProviders(<OverviewPage />, { route: '/' })
    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    // Rows: disabled-42, workspace-43, disabled-44. Acking the middle row
    // moves focus to the next one.
    await userEvent.click(await within(panel).findByRole('button', { name: '标记已读：工作区' }))
    await waitFor(() => expect(within(panel).queryByText(/group\/broken 工作区异常/)).not.toBeInTheDocument())
    const remaining = within(panel).getAllByRole('button', { name: '标记已读：已停用' })
    expect(remaining).toHaveLength(2)
    expect(remaining[1]).toHaveFocus()
    // The last row has no next row, so focus moves to the previous one.
    await userEvent.click(remaining[1])
    await waitFor(() => expect(within(panel).queryByText(/group\/third 已停用/)).not.toBeInTheDocument())
    const last = within(panel).getByRole('button', { name: '标记已读：已停用' })
    expect(last).toHaveFocus()
    // No row remains, so focus moves to the "show read" toggle.
    await userEvent.click(last)
    await within(panel).findByText('无未读告警')
    expect(within(panel).getByRole('button', { name: '显示已读（3）' })).toHaveFocus()
  })
})
