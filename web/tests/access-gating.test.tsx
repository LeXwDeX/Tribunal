import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { SettingsPanel } from '../src/features/project/SettingsPanel'
import { WorkspacePanel } from '../src/features/project/WorkspacePanel'
import { ExtensionsPage } from '../src/pages/ExtensionsPage'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { ProjectsPage } from '../src/pages/ProjectsPage'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { OverviewPage } from '../src/pages/OverviewPage'
import { ReviewsPage } from '../src/pages/ReviewsPage'
import { finding, project, review } from './fixtures'
import { adminMe, installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

class FakeEventSource {
  static instances: FakeEventSource[] = []
  url: string
  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

function emptyExtensions(url: string) {
  if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
  if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills: [] })
  if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
  return jsonResponse({})
}

describe('project access gates write controls', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('keeps project settings read-only without a manage grant even for an admin', async () => {
    const fetchMock = installFetch(() => jsonResponse({ profiles: [] }))
    renderWithProviders(<SettingsPanel project={project({ access: 'read' })} />)
    expect(await screen.findByText('项目设置（只读）')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存设置' })).not.toBeInTheDocument()
    expect(fetchMock.mock.calls.some(([input]) => String(input).startsWith('/api/v1/profiles'))).toBe(false)
  })

  it('treats a missing access field as read-only', async () => {
    installFetch(() => jsonResponse({ profiles: [] }))
    renderWithProviders(<SettingsPanel project={project({ access: undefined })} />)
    expect(await screen.findByText('项目设置（只读）')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存设置' })).not.toBeInTheDocument()
  })

  it('shows project settings controls to a viewer with a manage grant', async () => {
    installFetch(() => jsonResponse({ profiles: [{ name: 'default' }] }))
    renderWithProviders(<SettingsPanel project={project({ access: 'manage' })} />, { me: viewerMe })
    expect(await screen.findByRole('button', { name: '保存设置' })).toBeInTheDocument()
    expect(screen.queryByText('项目设置（只读）')).not.toBeInTheDocument()
  })

  it('gates workspace writes on the manage flag', async () => {
    installFetch((url) => {
      if (url.includes('/workspace')) return jsonResponse({ state: 'ready' })
      if (url.includes('/context-docs/current')) return jsonResponse({ error: { message: 'not found' } }, 404)
      if (url.includes('/context-docs')) return jsonResponse({ documents: [] })
      return jsonResponse({})
    })
    const read = renderWithProviders(<WorkspacePanel projectId={42} canManage={false} />)
    await waitFor(() => expect(screen.getByText(/只读账号可以查看工作区状态/)).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '刷新工作区' })).not.toBeInTheDocument()
    read.unmount()

    renderWithProviders(<WorkspacePanel projectId={42} canManage />, { me: viewerMe })
    expect(await screen.findByRole('button', { name: '刷新工作区' })).toBeInTheDocument()
  })

  it('requires both platform admin and the project manage grant for project extension writes', async () => {
    installFetch(emptyExtensions)
    const viewerView = renderWithProviders(
      <ExtensionsManager scope="project" projectId={42} canManage tab="mcp" onTabChange={() => {}} />,
      { me: viewerMe },
    )
    await waitFor(() => expect(screen.getByRole('heading', { name: 'MCP 服务器' })).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '新增 MCP 服务器' })).not.toBeInTheDocument()
    viewerView.unmount()

    const noGrant = renderWithProviders(
      <ExtensionsManager scope="project" projectId={42} canManage={false} tab="mcp" onTabChange={() => {}} />,
      { me: adminMe },
    )
    await waitFor(() => expect(screen.getByRole('heading', { name: 'MCP 服务器' })).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '新增 MCP 服务器' })).not.toBeInTheDocument()
    noGrant.unmount()

    renderWithProviders(
      <ExtensionsManager scope="project" projectId={42} canManage tab="mcp" onTabChange={() => {}} />,
      { me: adminMe },
    )
    expect(await screen.findByRole('button', { name: '新增 MCP 服务器' })).toBeInTheDocument()
  })

  it('opens project scope by default for a viewer and never queries platform extensions', async () => {
    const urls: string[] = []
    installFetch((url) => {
      urls.push(url)
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project({ access: 'read' })] })
      return emptyExtensions(url)
    })
    renderWithProviders(<ExtensionsPage />, { route: '/extensions', me: viewerMe })
    expect(await screen.findByText(/项目级 · group\/backend/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '平台级' })).not.toBeInTheDocument()
    await waitFor(() => expect(urls.some((url) => url.includes('scope=project') && url.includes('project_id=42'))).toBe(true))
    expect(urls.some((url) => url.includes('scope=platform'))).toBe(false)
  })

  it('keeps platform-scope extensions admin-only', async () => {
    installFetch(emptyExtensions)
    renderWithProviders(
      <ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />,
      { me: viewerMe },
    )
    await waitFor(() => expect(screen.getByRole('heading', { name: 'MCP 服务器' })).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '新增 MCP 服务器' })).not.toBeInTheDocument()
  })

  it('gates project enablement switches on each project access', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project({ access: 'read' })] })
      return jsonResponse({})
    })
    const adminView = renderWithProviders(<ProjectsPage />, { route: '/projects' })
    await waitFor(() => expect(screen.getByText('group/backend')).toBeInTheDocument())
    expect(screen.queryByRole('switch', { name: '启用 group/backend' })).not.toBeInTheDocument()
    adminView.unmount()

    installFetch((url) => {
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project({ access: 'manage' })] })
      return jsonResponse({})
    })
    renderWithProviders(<ProjectsPage />, { route: '/projects', me: viewerMe })
    expect(await screen.findByRole('switch', { name: '启用 group/backend' })).toBeInTheDocument()
  })

  it('gates overview manual triggering on manageable projects', async () => {
    const overviewRoutes = (access: 'read' | 'manage') => (url: string) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1 })
      if (parsed.pathname === '/api/v1/reviews') return jsonResponse({ reviews: [] })
      if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [project({ access })] })
      if (parsed.pathname === '/api/v1/providers') return jsonResponse({ providers: [] })
      return jsonResponse({})
    }
    installFetch(overviewRoutes('read'))
    const adminView = renderWithProviders(<OverviewPage />, { route: '/' })
    await waitFor(() => expect(screen.getByText('运行次数')).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '手动触发评审' })).not.toBeInTheDocument()
    adminView.unmount()

    installFetch(overviewRoutes('manage'))
    renderWithProviders(<OverviewPage />, { route: '/', me: viewerMe })
    expect(await screen.findByRole('button', { name: '手动触发评审' })).toBeInTheDocument()
  })

  it('shows manual review triggering only when a project is manageable', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/reviews')) return jsonResponse({ reviews: [] })
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project({ access: 'read' })] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    const adminView = renderWithProviders(<ReviewsPage />, { route: '/reviews' })
    await waitFor(() => expect(screen.getByText('还没有评审记录')).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '手动触发评审' })).not.toBeInTheDocument()
    adminView.unmount()

    installFetch((url) => {
      if (url.startsWith('/api/v1/reviews')) return jsonResponse({ reviews: [] })
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project({ access: 'manage' })] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    renderWithProviders(<ReviewsPage />, { route: '/reviews', me: viewerMe })
    expect(await screen.findByRole('button', { name: '手动触发评审' })).toBeInTheDocument()
  })
})

describe('review access gates write controls', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    FakeEventSource.instances = []
  })

  function routeReview(access: 'read' | 'manage' | undefined, status = 'running') {
    installFetch((url) => {
      if (url === '/api/v1/reviews/wf-access') {
        return jsonResponse(review({ id: 'wf-access', status, verdict: status === 'completed' ? 'PASS' : null, access }))
      }
      if (url === '/api/v1/reviews/wf-access/graph') return jsonResponse({ nodes: [], edges: [] })
      if (url.startsWith('/api/v1/reviews/wf-access/findings')) return jsonResponse({ findings: [finding()] })
      if (url === '/api/v1/projects/42') return jsonResponse(project({ access }))
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project({ access })] })
      return jsonResponse({})
    })
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
  }

  function renderReview() {
    return renderWithProviders(
      <Routes>
        <Route path="/reviews/:id" element={<ReviewDetailPage />} />
      </Routes>,
      { route: '/reviews/wf-access' },
    )
  }

  it('hides write actions from an admin without a manage grant', async () => {
    routeReview('read')
    renderReview()
    expect(await screen.findByRole('heading', { name: /!318/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '取消评审' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重新触发评审' })).not.toBeInTheDocument()

    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /展开：并发写 map 未加锁/ }))
    expect(screen.queryByRole('group', { name: '人工标注' })).not.toBeInTheDocument()
  })

  it('treats a missing review access as read-only', async () => {
    routeReview(undefined)
    renderReview()
    expect(await screen.findByRole('heading', { name: /!318/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '取消评审' })).not.toBeInTheDocument()
  })

  it('shows write actions to a viewer with a manage grant', async () => {
    routeReview('manage', 'completed')
    renderReview()
    expect(await screen.findByRole('button', { name: '重新发布' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新触发评审' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '取消评审' })).not.toBeInTheDocument()
  })
})

describe('project detail passes access to write panels', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('renders read-only settings for a read grant', async () => {
    installFetch((url) => {
      if (url === '/api/v1/projects/42') return jsonResponse(project({ access: 'read' }))
      return jsonResponse({})
    })
    renderWithProviders(
      <Routes>
        <Route path="/projects/:id/:tab" element={<ProjectDetailPage />} />
      </Routes>,
      { route: '/projects/42/settings' },
    )
    expect(await screen.findByText('项目设置（只读）')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存设置' })).not.toBeInTheDocument()
  })

  it('renders writable settings for a viewer with a manage grant', async () => {
    installFetch((url) => {
      if (url === '/api/v1/projects/42') return jsonResponse(project({ access: 'manage' }))
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
      return jsonResponse({})
    })
    renderWithProviders(
      <Routes>
        <Route path="/projects/:id/:tab" element={<ProjectDetailPage />} />
      </Routes>,
      { route: '/projects/42/settings', me: viewerMe },
    )
    expect(await screen.findByRole('button', { name: '保存设置' })).toBeInTheDocument()
  })
})
