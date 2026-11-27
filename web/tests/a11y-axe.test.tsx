import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'
import axe from 'axe-core'
import { AppShell } from '../src/components/AppShell'
import { OverviewPage } from '../src/pages/OverviewPage'
import { ReviewsPage } from '../src/pages/ReviewsPage'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { ProjectsPage } from '../src/pages/ProjectsPage'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { ExtensionsPage } from '../src/pages/ExtensionsPage'
import { ModelsPage } from '../src/pages/ModelsPage'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { SystemPage } from '../src/pages/SystemPage'
import { edges, finding, mcpServer, nodes, project, review, skill } from './fixtures'
import { adminMe, installFetch, jsonResponse, renderWithProviders } from './test-utils'

// Automated WCAG check (#657): every main page, rendered inside the app shell
// with realistic mocked data, must have no serious or critical axe violation in
// either theme.
//
// Excluded rule:
// - color-contrast: jsdom does not lay out or paint, so axe cannot compute the
//   rendered foreground/background pair and reports every text node as
//   "incomplete". Contrast is checked instead from the colour tokens in
//   tests/contrast-tokens.test.ts.
const AXE_OPTIONS: axe.RunOptions = {
  rules: { 'color-contrast': { enabled: false } },
  resultTypes: ['violations'],
}

class FakeEventSource {
  url: string
  constructor(url: string) {
    this.url = url
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

const completed = review({
  id: '0192f0c1-aaaa-bbbb-cccc-000000000002',
  status: 'completed',
  verdict: 'BLOCKING',
  finished_at: '2026-10-22T05:12:00Z',
})
const running = review()
const userModel = {
  id: 'user:aaaaaaaa-1111', display_name: '评审模型', source: 'user', read_only: false,
  protocol: 'openai-chat', base_url: 'https://example.com/v1', model: 'm',
  params: { max_tokens: 100 }, has_api_key: true, available: true, revision: 1,
  referenced_by: [], vote_identity: 'example.com/m',
}

function routes(url: string, init?: RequestInit) {
  const parsed = new URL(url, 'http://localhost')
  const path = parsed.pathname
  if (init?.method === 'POST' && path === '/api/v1/projects/validate') {
    return jsonResponse({
      project_id: 42,
      path_with_namespace: 'group/backend',
      web_url: 'https://gitlab.example.com/group/backend',
      default_branch: 'main',
      username: 'review-bot',
      access_level: 30,
      token_scopes: ['api'],
      token_expires_at: '2027-01-02T00:00:00Z',
      git_head: '0123456789abcdef0123456789abcdef01234567',
    })
  }
  if (path === '/api/v1/stats') {
    return jsonResponse({
      runs: 12,
      completed: 10,
      failed: 1,
      findings: 7,
      trend: [{ day: '2026-10-21', runs: 5, duration_samples: 5, avg_duration_ms: 60000, p95_duration_ms: 90000 }],
    })
  }
  if (path === `/api/v1/reviews/${completed.id}`) return jsonResponse(completed)
  if (path === `/api/v1/reviews/${completed.id}/graph`) return jsonResponse({ nodes, edges })
  if (path === `/api/v1/reviews/${completed.id}/findings`) {
    return jsonResponse({ findings: [finding(), finding({ id: 'f2', severity: 'P2', title: '错误未包装', decision: 'drop' })] })
  }
  if (path === '/api/v1/reviews') return jsonResponse({ reviews: [completed, running] })
  if (path === '/api/v1/projects/42') return jsonResponse(project())
  if (path.startsWith('/api/v1/projects/42/')) return jsonResponse({})
  if (path === '/api/v1/projects') return jsonResponse({ projects: [project(), project({ id: 43, path_with_namespace: 'group/web', enabled: false })] })
  if (path === '/api/v1/profiles') return jsonResponse({ profiles: [{ name: 'default' }] })
  if (path === '/api/v1/models') return jsonResponse({ models: [userModel] })
  if (path === '/api/v1/model-presets') return jsonResponse({ presets: [] })
  if (path === '/api/v1/mcp-servers') return jsonResponse({ servers: [mcpServer()] })
  if (path === '/api/v1/skills') return jsonResponse({ skills: [skill()] })
  if (path === '/api/v1/block-bindings') return jsonResponse({ bindings: [] })
  if (path === '/api/v1/providers') return jsonResponse({ providers: [] })
  if (path === '/api/v1/api-tokens') return jsonResponse({ tokens: [] })
  if (path === '/api/v1/admin/users') return jsonResponse({ users: [] })
  return jsonResponse({})
}

function renderPage(route: string, path: string, page: React.ReactNode) {
  return renderWithProviders(
    <Routes>
      <Route element={<AppShell me={adminMe} />}>
        <Route path={path} element={page} />
      </Route>
    </Routes>,
    { route },
  )
}

async function seriousViolations() {
  const results = await axe.run(document.body, AXE_OPTIONS)
  return results.violations
    .filter((violation) => violation.impact === 'serious' || violation.impact === 'critical')
    .map((violation) => ({
      rule: violation.id,
      impact: violation.impact,
      help: violation.help,
      targets: violation.nodes.map((node) => node.target.join(' ')),
    }))
}

const PAGES: Array<{ name: string; route: string; path: string; page: React.ReactNode; ready: () => Promise<unknown> }> = [
  { name: '总览', route: '/', path: '/', page: <OverviewPage />, ready: () => screen.findByText('运行次数') },
  { name: '评审列表', route: '/reviews', path: '/reviews', page: <ReviewsPage />, ready: () => screen.findAllByText('feat: 支持批量导入') },
  {
    name: '评审详情',
    route: `/reviews/${completed.id}`,
    path: '/reviews/:id',
    page: <ReviewDetailPage />,
    ready: () => screen.findByText('并发写 map 未加锁'),
  },
  { name: '项目列表', route: '/projects', path: '/projects', page: <ProjectsPage />, ready: () => screen.findByText('group/web') },
  {
    name: '项目设置',
    route: '/projects/42/settings',
    path: '/projects/:id/:tab?',
    page: <ProjectDetailPage />,
    ready: () => screen.findAllByText('group/backend'),
  },
  { name: '扩展', route: '/extensions', path: '/extensions/:tab?', page: <ExtensionsPage />, ready: () => screen.findAllByText(mcpServer().name) },
  { name: '模型与档位', route: '/models', path: '/models', page: <ModelsPage />, ready: () => screen.findAllByText('评审模型') },
  { name: '统计与反馈', route: '/statistics', path: '/statistics', page: <StatisticsPage />, ready: () => screen.findAllByRole('heading', { name: '统计与反馈' }) },
  { name: '系统', route: '/system', path: '/system', page: <SystemPage />, ready: () => screen.findAllByRole('heading', { name: '系统' }) },
]

// jsdom implements neither of these; axe probes a canvas for icon ligatures
// and the shell scrolls to the top on navigation. Stub them to keep the run quiet.
function stubUnimplemented() {
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null)
}

describe('axe harness (#657)', () => {
  beforeEach(stubUnimplemented)
  afterEach(() => vi.restoreAllMocks())

  // Guards the check itself: if axe stopped running rules under jsdom, every
  // page below would pass vacuously.
  it('reports serious and critical violations under jsdom', async () => {
    renderWithProviders(<main><button type="button" /><img src="/x.png" /></main>)
    const rules = (await seriousViolations()).map((violation) => [violation.rule, violation.impact])
    expect(rules).toEqual(expect.arrayContaining([['button-name', 'critical'], ['image-alt', 'critical']]))
  })
})

describe.each(['light', 'dark'])('axe on the main pages, %s theme (#657)', (theme) => {
  beforeEach(() => {
    document.documentElement.dataset.theme = theme
    installFetch(routes)
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    stubUnimplemented()
  })

  afterEach(() => {
    delete document.documentElement.dataset.theme
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it.each(PAGES)('$name has no serious or critical violation', async ({ route, path, page, ready }) => {
    renderPage(route, path, page)
    await ready()
    expect(await seriousViolations()).toEqual([])
  })

  it('the register project dialog, with a passed validation, has no serious or critical violation', async () => {
    const user = userEvent.setup()
    renderPage('/projects', '/projects', <ProjectsPage />)
    await user.click((await screen.findAllByRole('button', { name: '注册项目' }))[0])
    await screen.findByRole('dialog')
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), 'https://gitlab.example.com/group/backend')
    await user.type(screen.getByLabelText('访问 Key'), 'test-access-key')
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    expect(await seriousViolations()).toEqual([])
  })
})
