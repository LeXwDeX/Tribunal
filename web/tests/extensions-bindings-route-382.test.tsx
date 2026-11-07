import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient } from '@tanstack/react-query'
import { Link, Route, Routes, useLocation, useNavigate, useParams } from 'react-router-dom'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import type { BlockBinding, Skill } from '../src/api/types'
import { binding, project, skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const boundSkills: Skill[] = [
  skill({ id: 'k-code', scope: 'project', name: 'code-review' }),
  skill({ id: 'k-team', scope: 'project', name: 'team-rules' }),
]
const bindings: BlockBinding[] = [binding({ scope: 'project', block: 'review', skills: ['code-review', 'team-rules'] })]

function setup() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: Infinity } },
  })
  queryClient.setQueryData(['block-bindings', 'project', 42], { pages: [{ bindings }], pageParams: [undefined] })
  queryClient.setQueryData(['skills', 'project', 42], { pages: [{ skills: boundSkills }], pageParams: [undefined] })
  queryClient.setQueryData(['mcp-servers', 'project', 42], { pages: [{ servers: [] }], pageParams: [undefined] })
  queryClient.setQueryData(['project', 42], project({ id: 42 }))
  installFetch((url) => {
    const path = new URL(url, 'http://localhost').pathname
    if (path === '/api/v1/projects/42') return jsonResponse(project({ id: 42 }))
    if (path.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings })
    if (path.startsWith('/api/v1/skills')) return jsonResponse({ skills: boundSkills })
    if (path.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
    return jsonResponse({})
  })
  return queryClient
}

function RouteProbe() {
  const location = useLocation()
  return <output data-testid="route">{location.pathname}</output>
}

function ExtensionsRoute() {
  const { tab = 'bindings' } = useParams()
  const navigate = useNavigate()
  return (
    <ExtensionsManager
      scope="project"
      projectId={42}
      canManage
      projects={[project({ id: 42 })]}
      tab={tab}
      onTabChange={(next, replace) => navigate(`/extensions/${next}`, { replace })}
    />
  )
}

function ExtensionLink({ to }: { to: string }) {
  return <Link to={to}>前往 {to}</Link>
}

describe('block binding drafts across route-driven tab changes', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('restores the routed tab and keeps drafts when a link leaves bindings', async () => {
    const user = userEvent.setup()
    renderWithProviders(
      <>
        <RouteProbe />
        <ExtensionLink to="/extensions/skills" />
        <Routes>
          <Route path="/extensions/:tab" element={<ExtensionsRoute />} />
        </Routes>
      </>,
      { route: '/extensions/bindings', queryClient: setup() },
    )

    const toggle = await screen.findByRole('button', { name: 'review 使用 Skill team-rules' })
    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('link', { name: '前往 /extensions/skills' }))
    const dialog = await screen.findByRole('dialog', { name: '放弃未保存的绑定修改？' })
    expect(screen.getByTestId('route')).toHaveTextContent('/extensions/bindings')
    expect(screen.getByRole('button', { name: 'review 使用 Skill team-rules' })).toHaveAttribute('aria-pressed', 'false')

    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByTestId('route')).toHaveTextContent('/extensions/bindings')

    await user.click(screen.getByRole('link', { name: '前往 /extensions/skills' }))
    await user.click(await screen.findByRole('button', { name: '放弃并切换' }))
    expect(await screen.findByRole('tab', { name: /^Skill/, selected: true })).toBeInTheDocument()
    expect(screen.getByTestId('route')).toHaveTextContent('/extensions/skills')
  })

  it('asks before an outer project tab discards binding drafts', async () => {
    const user = userEvent.setup()
    renderWithProviders(
      <Routes>
        <Route path="/projects/:id/:tab" element={<ProjectDetailPage />} />
      </Routes>,
      { route: '/projects/42/extensions', queryClient: setup() },
    )

    await user.click(await screen.findByRole('tab', { name: '块绑定' }))
    const toggle = await screen.findByRole('button', { name: 'review 使用 Skill team-rules' })
    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('tab', { name: '概览' }))
    const dialog = await screen.findByRole('dialog', { name: '放弃未保存的绑定修改？' })
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(screen.getByRole('button', { name: 'review 使用 Skill team-rules' })).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('tab', { name: '概览' }))
    await user.click(await screen.findByRole('button', { name: '放弃并切换' }))
    expect(await screen.findByRole('tab', { name: '概览', selected: true })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill team-rules' })).not.toBeInTheDocument()
  })
})