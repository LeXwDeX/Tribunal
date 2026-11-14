import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import { OverviewPage } from '../src/pages/OverviewPage'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function routeOverview(reviews: unknown[]) {
  return installFetch((url) => {
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
    if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
    if (parsed.pathname === '/api/v1/models') return jsonResponse({ models: [] })
    if (parsed.pathname === '/api/v1/reviews') {
      if (parsed.searchParams.get('status') || parsed.searchParams.get('publication_state')) return jsonResponse({ reviews: [] })
      return jsonResponse({ reviews })
    }
    return jsonResponse({})
  })
}

function alertPanel() {
  const title = screen.getByRole('heading', { name: '告警' })
  const panel = title.closest('section')
  if (!panel) throw new Error('alerts panel missing')
  return panel
}

describe('overview incomplete review alerts', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('aggregates INCOMPLETE reviews and completed reviews with failed or skipped nodes', async () => {
    routeOverview([
      review({ id: 'incomplete-1', status: 'completed', verdict: 'INCOMPLETE', nodes: [] }),
      review({ id: 'incomplete-2', status: 'completed', verdict: 'INCOMPLETE', nodes: [] }),
      review({
        id: 'node-failed', status: 'completed', verdict: 'BLOCKING',
        nodes: [{ id: 'verify.f1', kind: 'verify', status: 'failed' }],
      }),
      review({
        id: 'node-skipped', status: 'completed', verdict: 'PASS',
        nodes: [{ id: 'verify.f2', kind: 'verify', status: 'skipped' }],
      }),
      review({ id: 'clean', status: 'completed', verdict: 'PASS', nodes: [{ id: 'p', kind: 'plan', status: 'completed' }] }),
    ])
    renderWithProviders(<OverviewPage />, { route: '/' })

    await screen.findByRole('heading', { name: '告警' })
    const panel = alertPanel()
    await waitFor(() => expect(within(panel).getByText('评审未完成')).toBeInTheDocument())
    expect(within(panel).getByText(/2 个已结束评审为 INCOMPLETE/)).toBeInTheDocument()
    expect(within(panel).getByRole('link', { name: '查看 INCOMPLETE 评审' })).toHaveAttribute('href', '/reviews?verdict=INCOMPLETE')
    expect(within(panel).getByText('评审节点异常')).toBeInTheDocument()
    expect(within(panel).getByText(/2 个已结束评审包含失败或跳过的节点/)).toBeInTheDocument()
    expect(within(panel).getByRole('link', { name: '查看评审' })).toHaveAttribute('href', '/reviews?status=completed')
    expect(within(panel).getByText(/检查范围：token 过期、工作区异常、停用项目/)).toBeInTheDocument()
  })

  it('does not alert for a clean completed review', async () => {
    routeOverview([review({ id: 'clean', status: 'completed', verdict: 'PASS', nodes: [{ id: 'p', kind: 'plan', status: 'completed' }] })])
    renderWithProviders(<OverviewPage />, { route: '/' })
    const panel = await screen.findByRole('heading', { name: '告警' }).then((title) => title.closest('section')!)
    await waitFor(() => expect(within(panel).getByText('无异常')).toBeInTheDocument())
    expect(within(panel).queryByText('评审未完成')).not.toBeInTheDocument()
    expect(within(panel).queryByText('评审节点异常')).not.toBeInTheDocument()
  })
})

function renderProject(workspace: Record<string, unknown>) {
  installFetch((url) => {
    const path = new URL(url, 'http://localhost').pathname
    if (path === '/api/v1/projects/42') {
      return jsonResponse({
        id: 42,
        path_with_namespace: 'group/backend',
        profile: 'default',
        settings: {},
        enabled: true,
        access: 'manage',
        workspace,
      })
    }
    if (path === '/api/v1/stats') return jsonResponse({ runs: 0 })
    if (path === '/api/v1/reviews') return jsonResponse({ reviews: [] })
    return jsonResponse({})
  })
  return renderWithProviders(
    <Routes><Route path="/projects/:id/:tab" element={<ProjectDetailPage />} /></Routes>,
    { route: '/projects/42/overview' },
  )
}

function workspacePanel() {
  const title = screen.getByRole('heading', { name: '工作区摘要' })
  return title.closest('section')!
}

describe('project overview workspace diagnostics', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('renders an ignore-policy degraded index as safe Chinese guidance, not a red error', async () => {
    renderProject({ state: 'ready', index_status: 'degraded', last_error: 'index: blocked by ignore policy' })
    await screen.findByRole('heading', { name: '工作区摘要' })
    const panel = workspacePanel()
    const message = await within(panel).findByText(/这是安全配置，不是故障/)
    expect(message.className).not.toMatch(/resultError/)
  })

  it('keeps a real workspace error red and actionable', async () => {
    renderProject({ state: 'error', index_status: 'degraded', last_error: 'workspace operation failed' })
    await screen.findByRole('heading', { name: '工作区摘要' })
    const panel = workspacePanel()
    const message = await within(panel).findByText(/请查看服务日志中的 error_class/)
    expect(message.className).toMatch(/resultError/)
  })
})