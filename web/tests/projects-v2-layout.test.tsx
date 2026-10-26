import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes, useLocation } from 'react-router-dom'
import { ProjectsPage } from '../src/pages/ProjectsPage'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function LocationProbe() {
  const location = useLocation()
  return <span data-testid="location">{location.pathname}</span>
}

function renderProjects() {
  return renderWithProviders(
    <Routes>
      <Route path="/projects" element={<ProjectsPage />} />
      <Route path="*" element={<LocationProbe />} />
    </Routes>,
    { route: '/projects' },
  )
}

const soon = new Date(Date.now() + 10 * 86_400_000).toISOString()

describe('projects list v2 layout', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('filters by enabled state with a segmented control', async () => {
    const listUrls: string[] = []
    installFetch((url) => {
      if (url.startsWith('/api/v1/projects')) {
        listUrls.push(url)
        return jsonResponse({ projects: [project()] })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderProjects()

    const group = await screen.findByRole('group', { name: '按启用状态筛选' })
    expect(within(group).getByRole('button', { name: '全部' })).toHaveAttribute('aria-pressed', 'true')
    await user.click(within(group).getByRole('button', { name: '已停用' }))
    expect(within(group).getByRole('button', { name: '已停用' })).toHaveAttribute('aria-pressed', 'true')
    await waitFor(() => expect(listUrls.some((url) => url.includes('enabled=false'))).toBe(true))
    await user.click(within(group).getByRole('button', { name: '已启用' }))
    await waitFor(() => expect(listUrls.some((url) => url.includes('enabled=true'))).toBe(true))
  })

  it('shows profile, last review verdict, key expiry days and access per row', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/projects')) {
        return jsonResponse({
          projects: [project({
            profile: 'strict',
            token_expires_at: soon,
            last_review: {
              id: 'r1', project_id: 42, mr_iid: 318, mr_title: 'feat: 批量导入', status: 'completed', verdict: 'BLOCKING',
              created_at: new Date().toISOString(),
            },
          }), project({ id: 43, path_with_namespace: 'group/readonly', access: 'read' })],
        })
      }
      return jsonResponse({})
    })
    renderProjects()

    const row = (await screen.findByRole('link', { name: 'group/backend' })).closest('tr') as HTMLElement
    expect(within(row).getByText('strict')).toBeInTheDocument()
    expect(within(row).getByText('BLOCKING')).toBeInTheDocument()
    expect(within(row).getByRole('link', { name: /!318/ })).toBeInTheDocument()
    expect(within(row).getByText(/10 天后到期/)).toBeInTheDocument()
    expect(within(row).getByText('可管理')).toBeInTheDocument()
    const readonly = screen.getByRole('link', { name: 'group/readonly' }).closest('tr') as HTMLElement
    expect(within(readonly).getByText('只读')).toBeInTheDocument()
  })

  it('opens the project when the row is clicked but not when its switch is toggled', async () => {
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/projects/42') && init?.method === 'PATCH') return jsonResponse(project({ enabled: false }))
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project()] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderProjects()

    const row = (await screen.findByRole('link', { name: 'group/backend' })).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('switch', { name: '启用 group/backend' }))
    expect(screen.queryByTestId('location')).not.toBeInTheDocument()
    await user.click(within(row).getByText('default'))
    expect(await screen.findByTestId('location')).toHaveTextContent('/projects/42')
  })

  it('marks the registration steps as validation progresses', async () => {
    installFetch((url, init) => {
      if (url === '/api/v1/projects/validate' && init?.method === 'POST') {
        return jsonResponse({
          project_id: 42, path_with_namespace: 'group/backend', web_url: 'https://gitlab.example.com/group/backend', default_branch: 'main',
          username: 'review-bot', access_level: 40, token_scopes: ['api'], git_head: 'abc',
        })
      }
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderProjects()
    await user.click((await screen.findAllByRole('button', { name: '注册项目' }))[0])

    const steps = screen.getByRole('list', { name: '注册步骤' })
    expect(within(steps).getByText('校验连接').closest('li')).toHaveAttribute('aria-current', 'step')
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), 'group/backend')
    await user.type(screen.getByLabelText('访问 Key'), 'glpat-x')
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    expect(within(steps).getByText('保存并初始化').closest('li')).toHaveAttribute('aria-current', 'step')
    expect(within(steps).getByText('校验连接').closest('li')).not.toHaveAttribute('aria-current')
  })
})
