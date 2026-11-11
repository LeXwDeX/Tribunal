import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsPage } from '../src/pages/ExtensionsPage'
import { ReviewsPage } from '../src/pages/ReviewsPage'
import { mcpServer, project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const firstPage = Array.from({ length: 100 }, (_, index) => project({
  id: index + 1, path_with_namespace: `group/project-${index + 1}`, access: 'read',
}))
const laterProjects = [
  project({ id: 101, path_with_namespace: 'group/manage-later', access: 'manage' }),
  project({ id: 102, path_with_namespace: 'group/read-later', access: 'read' }),
]

// The extensions page still renders a native select; ReviewsPage uses the
// Select combobox whose options live in a portal only while it is open.
function isNativeSelect(selector: HTMLElement) {
  return selector.tagName === 'SELECT'
}

async function openProjectOptions(user: ReturnType<typeof userEvent.setup>, selector: HTMLElement) {
  if (isNativeSelect(selector)) return
  if (selector.getAttribute('aria-expanded') !== 'true') await user.click(selector)
}

async function closeProjectOptions(user: ReturnType<typeof userEvent.setup>, selector: HTMLElement) {
  if (isNativeSelect(selector)) return
  if (selector.getAttribute('aria-expanded') === 'true') await user.click(selector)
}

async function selectProject(user: ReturnType<typeof userEvent.setup>, selector: HTMLElement, name: string) {
  await openProjectOptions(user, selector)
  const option = await screen.findByRole('option', { name })
  if (isNativeSelect(selector)) {
    await user.selectOptions(selector, option)
    return
  }
  await user.click(option)
}

function expectSelectedProject(selector: HTMLElement, value: string, label: string) {
  if (isNativeSelect(selector)) expect(selector).toHaveValue(value)
  else expect(selector).toHaveTextContent(label)
}

function otherRoutes(url: string) {
  const path = new URL(url, 'http://localhost').pathname
  if (path === '/api/v1/reviews') return jsonResponse({ reviews: [] })
  if (path === '/api/v1/profiles') return jsonResponse({ profiles: [] })
  if (path === '/api/v1/mcp-servers') {
    const projectId = new URL(url, 'http://localhost').searchParams.get('project_id')
    return jsonResponse({ servers: projectId ? [mcpServer({ id: `s-${projectId}`, name: `server-${projectId}` })] : [] })
  }
  if (path === '/api/v1/skills') return jsonResponse({ skills: [] })
  if (path === '/api/v1/block-bindings') return jsonResponse({ bindings: [] })
  return jsonResponse({})
}

describe.each([
  { page: 'extensions', label: '扩展作用域项目' },
  { page: 'reviews', label: '按项目筛选' },
] as const)('$page project selector', ({ page, label }) => {
  afterEach(() => vi.unstubAllGlobals())

  it('keeps loaded projects on a failed next page, retries, and enforces project permissions', async () => {
    let resolveSecond: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveSecond = resolve })
    let laterAttempts = 0
    const reviewRequests: string[] = []
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') {
        if (parsed.searchParams.get('cursor') === 'next') {
          laterAttempts += 1
          return laterAttempts === 1 ? pending : jsonResponse({ projects: laterProjects })
        }
        return jsonResponse({ projects: firstPage, next_cursor: 'next' })
      }
      if (parsed.pathname === '/api/v1/reviews') reviewRequests.push(url)
      return otherRoutes(url)
    })
    const user = userEvent.setup()
    renderWithProviders(page === 'extensions' ? <ExtensionsPage /> : <ReviewsPage />)
    if (page === 'extensions') await user.click(screen.getByRole('button', { name: '项目级' }))
    const selector = screen.getByRole('combobox', { name: label })
    await openProjectOptions(user, selector)
    expect(await screen.findByRole('option', { name: 'group/project-100' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: 'group/manage-later' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: page === 'extensions' ? '编辑' : '手动触发评审' })).not.toBeInTheDocument()
    await closeProjectOptions(user, selector)

    await user.click(screen.getByRole('button', { name: '加载更多项目' }))
    await waitFor(() => expect(screen.getByRole('button', { name: '加载更多项目' })).toBeDisabled())
    await openProjectOptions(user, selector)
    expect(screen.getByRole('option', { name: 'group/project-100' })).toBeInTheDocument()
    await closeProjectOptions(user, selector)
    resolveSecond(jsonResponse({ error: { message: 'next failed' } }, 500))
    await user.click(await screen.findByRole('button', { name: '重试加载更多项目' }))
    await selectProject(user, selector, 'group/manage-later')
    expect(laterAttempts).toBe(2)
    await openProjectOptions(user, selector)
    expect(screen.getByRole('option', { name: 'group/project-100' })).toBeInTheDocument()
    await closeProjectOptions(user, selector)

    if (page === 'extensions') {
      await screen.findByText('server-101')
      expect(screen.getByRole('button', { name: '编辑' })).toBeInTheDocument()
      await selectProject(user, selector, 'group/read-later')
      await screen.findByText('server-102')
      expect(screen.queryByRole('button', { name: '编辑' })).not.toBeInTheDocument()
    } else {
      await waitFor(() => expect(reviewRequests.some((url) => new URL(url, 'http://localhost').searchParams.get('project_id') === '101')).toBe(true))
      await user.click(screen.getByRole('button', { name: '手动触发评审' }))
      const dialog = screen.getByRole('dialog', { name: '手动触发评审' })
      await user.click(within(dialog).getByRole('combobox', { name: '项目' }))
      expect(await screen.findByRole('option', { name: 'group/manage-later' })).toBeInTheDocument()
      expect(screen.queryByRole('option', { name: 'group/read-later' })).not.toBeInTheDocument()
    }
  })

  it('offers a retry after the first project page fails', async () => {
    let attempts = 0
    installFetch((url) => {
      if (new URL(url, 'http://localhost').pathname === '/api/v1/projects') {
        attempts += 1
        return attempts === 1 ? jsonResponse({ error: { message: 'first failed' } }, 500) : jsonResponse({ projects: [firstPage[0]] })
      }
      return otherRoutes(url)
    })
    const user = userEvent.setup()
    renderWithProviders(page === 'extensions' ? <ExtensionsPage /> : <ReviewsPage />)
    if (page === 'extensions') await user.click(screen.getByRole('button', { name: '项目级' }))
    await user.click(await screen.findByRole('button', { name: page === 'extensions' ? '重试' : '重试加载项目' }))
    await openProjectOptions(user, screen.getByRole('combobox', { name: label }))
    expect(await screen.findByRole('option', { name: 'group/project-1' })).toBeInTheDocument()
    expect(attempts).toBe(2)
  })

  it('keeps the selected project after a cached refresh failure and retries the refresh', async () => {
    let attempts = 0
    installFetch((url) => {
      if (new URL(url, 'http://localhost').pathname === '/api/v1/projects') {
        attempts += 1
        return attempts === 2 ? jsonResponse({ error: { message: 'refresh failed' } }, 500) : jsonResponse({ projects: firstPage.slice(0, 2) })
      }
      return otherRoutes(url)
    })
    const user = userEvent.setup()
    const { queryClient } = renderWithProviders(page === 'extensions' ? <ExtensionsPage /> : <ReviewsPage />)
    if (page === 'extensions') await user.click(screen.getByRole('button', { name: '项目级' }))
    const selector = screen.getByRole('combobox', { name: label })
    await openProjectOptions(user, selector)
    expect(await screen.findByRole('option', { name: 'group/project-2' })).toBeInTheDocument()
    await closeProjectOptions(user, selector)
    await selectProject(user, selector, 'group/project-2')
    await act(async () => { await queryClient.invalidateQueries({ queryKey: ['projects', page === 'extensions' ? 'extensions' : 'filter'] }) })
    expect(await screen.findByText(/项目列表刷新失败/)).toBeInTheDocument()
    expectSelectedProject(selector, '2', 'group/project-2')
    await user.click(screen.getByRole('button', { name: page === 'extensions' ? '重试' : '重试刷新项目' }))
    await waitFor(() => expect(screen.queryByText(/项目列表刷新失败/)).not.toBeInTheDocument())
    expectSelectedProject(selector, '2', 'group/project-2')
    expect(attempts).toBe(3)
  })

  it('retries a failed next page after an empty first page', async () => {
    let nextAttempts = 0
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects') {
        if (!parsed.searchParams.get('cursor')) return jsonResponse({ projects: [], next_cursor: 'next' })
        nextAttempts += 1
        return nextAttempts === 1 ? jsonResponse({ error: { message: 'next failed' } }, 500) : jsonResponse({ projects: laterProjects })
      }
      return otherRoutes(url)
    })
    const user = userEvent.setup()
    renderWithProviders(page === 'extensions' ? <ExtensionsPage /> : <ReviewsPage />)
    if (page === 'extensions') await user.click(screen.getByRole('button', { name: '项目级' }))
    await user.click(await screen.findByRole('button', { name: '加载更多项目' }))
    const retry = await screen.findByRole('button', { name: '重试加载更多项目' })
    expect(screen.queryByRole('button', { name: '重试加载项目' })).not.toBeInTheDocument()
    expect(screen.queryByText('还没有项目')).not.toBeInTheDocument()
    await user.click(retry)
    await openProjectOptions(user, screen.getByRole('combobox', { name: label }))
    expect(await screen.findByRole('option', { name: 'group/manage-later' })).toBeInTheDocument()
    expect(nextAttempts).toBe(2)
    expect(screen.queryByText('还没有项目')).not.toBeInTheDocument()
  })

  it('announces a project list failure to assistive technology', async () => {
    installFetch((url) => new URL(url, 'http://localhost').pathname === '/api/v1/projects'
      ? jsonResponse({ error: { message: 'unavailable' } }, 500) : otherRoutes(url))
    const user = userEvent.setup()
    renderWithProviders(page === 'extensions' ? <ExtensionsPage /> : <ReviewsPage />)
    if (page === 'extensions') await user.click(screen.getByRole('button', { name: '项目级' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/项目.*加载失败/)
  })

  it('keeps keyboard focus on the project selector after the last page loads', async () => {
    let resolveNext: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveNext = resolve })
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (url.pathname === '/api/v1/projects') return url.searchParams.get('cursor') ? pending : jsonResponse({ projects: firstPage.slice(0, 1), next_cursor: 'next' })
      return otherRoutes(raw)
    })
    const user = userEvent.setup()
    renderWithProviders(page === 'extensions' ? <ExtensionsPage /> : <ReviewsPage />)
    if (page === 'extensions') await user.click(screen.getByRole('button', { name: '项目级' }))
    const button = await screen.findByRole('button', { name: '加载更多项目' })
    button.focus()
    await user.keyboard('{Enter}')
    expect(button).toBeDisabled()
    await act(async () => { resolveNext(jsonResponse({ projects: laterProjects })) })
    await waitFor(() => expect(screen.queryByRole('button', { name: '加载更多项目' })).not.toBeInTheDocument())
    expect(screen.getByRole('combobox', { name: label })).toHaveFocus()
  })

  it('does not steal focus if the user moves away during a pending last page', async () => {
    let resolveNext: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveNext = resolve })
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (url.pathname === '/api/v1/projects') return url.searchParams.get('cursor') ? pending : jsonResponse({ projects: firstPage.slice(0, 1), next_cursor: 'next' })
      return otherRoutes(raw)
    })
    const user = userEvent.setup()
    renderWithProviders(page === 'extensions' ? <ExtensionsPage /> : <ReviewsPage />)
    if (page === 'extensions') await user.click(screen.getByRole('button', { name: '项目级' }))
    const button = await screen.findByRole('button', { name: '加载更多项目' })
    button.focus()
    await user.keyboard('{Enter}')
    const other = page === 'extensions' ? screen.getByRole('button', { name: '平台级' }) : screen.getByRole('button', { name: '失败' })
    other.focus()
    await act(async () => { resolveNext(jsonResponse({ projects: laterProjects })) })
    await waitFor(() => expect(screen.queryByRole('button', { name: '加载更多项目' })).not.toBeInTheDocument())
    expect(other).toHaveFocus()
  })
})

describe('reviews direct project filter', () => {
  afterEach(() => vi.unstubAllGlobals())

  it.each(['101', '999'])('shows a safe selected placeholder for unloaded or inaccessible project %s', async (projectId) => {
    const requests: string[] = []
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      requests.push(raw)
      if (url.pathname === '/api/v1/projects') return jsonResponse({ projects: firstPage, next_cursor: 'next' })
      return otherRoutes(raw)
    })
    const user = userEvent.setup()
    renderWithProviders(<ReviewsPage />, { route: `/reviews?project_id=${projectId}` })
    const select = screen.getByRole('combobox', { name: '按项目筛选' })
    await user.click(select)
    expect(await screen.findByRole('option', { name: 'group/project-100' })).toBeInTheDocument()
    expect(select).toHaveTextContent('指定项目（未加载或不可访问）')
    const placeholder = screen.getByRole('option', { name: '指定项目（未加载或不可访问）' })
    expect(placeholder).toHaveAttribute('aria-selected', 'true')
    expect(placeholder).toHaveAttribute('aria-disabled', 'true')
    await waitFor(() => expect(requests.some((raw) => {
      const url = new URL(raw, 'http://localhost')
      return url.pathname === '/api/v1/reviews' && url.searchParams.get('project_id') === projectId
    })).toBe(true))
    expect(requests.some((raw) => new URL(raw, 'http://localhost').pathname === `/api/v1/projects/${projectId}`)).toBe(false)
  })

  it('replaces the safe placeholder with the actual name after its page loads', async () => {
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (url.pathname === '/api/v1/projects') return url.searchParams.get('cursor') ? jsonResponse({ projects: laterProjects }) : jsonResponse({ projects: firstPage, next_cursor: 'next' })
      return otherRoutes(raw)
    })
    const user = userEvent.setup()
    renderWithProviders(<ReviewsPage />, { route: '/reviews?project_id=101' })
    const select = screen.getByRole('combobox', { name: '按项目筛选' })
    await user.click(await screen.findByRole('button', { name: '加载更多项目' }))
    expect(select).toHaveTextContent('group/manage-later')
    await user.click(select)
    expect(await screen.findByRole('option', { name: 'group/manage-later' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.queryByRole('option', { name: '指定项目（未加载或不可访问）' })).not.toBeInTheDocument()
  })
})

describe('empty project selector focus', () => {
  afterEach(() => vi.unstubAllGlobals())
  it('returns focus to a scope control if an empty extension list has no selectable project', async () => {
    installFetch((raw) => {
      const url = new URL(raw, 'http://localhost')
      if (url.pathname === '/api/v1/projects') return url.searchParams.get('cursor') ? jsonResponse({ projects: [] }) : jsonResponse({ projects: [], next_cursor: 'next' })
      return otherRoutes(raw)
    })
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsPage />)
    await user.click(screen.getByRole('button', { name: '项目级' }))
    const button = await screen.findByRole('button', { name: '加载更多项目' })
    button.focus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(screen.queryByRole('button', { name: '加载更多项目' })).not.toBeInTheDocument())
    expect(screen.getByRole('button', { name: '平台级' })).toHaveFocus()
  })
})
