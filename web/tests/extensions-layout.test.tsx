import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsPage } from '../src/pages/ExtensionsPage'
import { project } from './fixtures'
import { createTestQueryClient, installFetch, jsonResponse, renderWithProviders } from './test-utils'

function extensionRoutes(projects: () => Response | Promise<Response>) {
  return (url: string) => {
    if (url.startsWith('/api/v1/projects')) return projects()
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills: [] })
    if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
    return jsonResponse({})
  }
}

// Project status messages render in the tab panel below the tabs.
function expectStableLayout() {
  const toolbar = screen.getByRole('toolbar', { name: '扩展作用域与项目' })
  expect(within(toolbar).getByRole('group', { name: '扩展作用域' })).toBeInTheDocument()
  expect(within(toolbar).getByRole('combobox', { name: '扩展作用域项目' })).toBeInTheDocument()
  expect(within(toolbar).queryByRole('status')).not.toBeInTheDocument()
  expect(within(toolbar).queryByRole('alert')).not.toBeInTheDocument()
  expect(screen.getByRole('tablist', { name: '扩展类型' })).toBeInTheDocument()
  return { toolbar, panel: screen.getByRole('tabpanel') }
}

describe('extensions page layout', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the project selector only in project scope', async () => {
    installFetch(extensionRoutes(() => jsonResponse({ projects: [project({ access: 'manage' }), project({ id: 43, path_with_namespace: 'group/frontend' })] })))
    renderWithProviders(<ExtensionsPage />, { route: '/extensions' })

    const platform = screen.getByRole('button', { name: '平台级' })
    expect(platform).toHaveAttribute('aria-pressed', 'true')
    const toolbar = screen.getByRole('toolbar', { name: '扩展作用域与项目' })
    expect(within(toolbar).queryByRole('combobox', { name: '扩展作用域项目' })).not.toBeInTheDocument()
    expect(within(toolbar).queryByText('项目', { exact: true })).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '项目级' }))
    expect(screen.getByRole('button', { name: '项目级' })).toHaveAttribute('aria-pressed', 'true')
    expect(await screen.findByText(/项目级 · group\/backend/)).toBeInTheDocument()
    const selector = screen.getByRole('combobox', { name: '扩展作用域项目' })
    expect(selector).toBeEnabled()
    expect(selector).toHaveTextContent('group/backend')
    await userEvent.click(selector)
    await userEvent.click(await screen.findByRole('option', { name: 'group/frontend' }))
    expect(await screen.findByText(/项目级 · group\/frontend/)).toBeInTheDocument()
    expectStableLayout()

    await userEvent.click(platform)
    expect(within(toolbar).queryByRole('combobox', { name: '扩展作用域项目' })).not.toBeInTheDocument()
    expect(within(toolbar).queryByText('项目', { exact: true })).not.toBeInTheDocument()
  })

  it('shows the project loading state under the tabs', async () => {
    installFetch(extensionRoutes(() => new Promise<Response>(() => {})))
    renderWithProviders(<ExtensionsPage />, { route: '/extensions' })
    await userEvent.click(screen.getByRole('button', { name: '项目级' }))
    const { panel } = expectStableLayout()
    expect(within(panel).getByRole('status')).toHaveTextContent('正在读取项目…')
  })

  it('shows the project list error under the tabs', async () => {
    installFetch(extensionRoutes(() => jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500)))
    renderWithProviders(<ExtensionsPage />, { route: '/extensions' })
    await userEvent.click(screen.getByRole('button', { name: '项目级' }))
    expect(await screen.findByText('项目列表加载失败')).toBeInTheDocument()
    const { panel } = expectStableLayout()
    expect(within(panel).getByRole('alert')).toHaveTextContent('项目列表加载失败')
  })

  it('shows the empty project list under the tabs', async () => {
    installFetch(extensionRoutes(() => jsonResponse({ projects: [] })))
    renderWithProviders(<ExtensionsPage />, { route: '/extensions' })
    await userEvent.click(screen.getByRole('button', { name: '项目级' }))
    expect(await screen.findByText('还没有项目')).toBeInTheDocument()
    const { panel } = expectStableLayout()
    expect(within(panel).getByText('还没有项目')).toBeInTheDocument()
  })
})

describe('extensions page project refresh failure', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('keeps the selector usable and offers a retry when a cached project list fails to refresh', async () => {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(['projects', 'extensions'], { pages: [{ projects: [project({ access: 'manage' }), project({ id: 43, path_with_namespace: 'group/frontend' })] }], pageParams: [undefined] })
    installFetch(extensionRoutes(() => jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500)))
    renderWithProviders(<ExtensionsPage />, { route: '/extensions', queryClient })
    await userEvent.click(screen.getByRole('button', { name: '项目级' }))

    expect(await screen.findByText('项目列表刷新失败')).toBeInTheDocument()
    const { panel } = expectStableLayout()
    expect(within(panel).getByRole('button', { name: '重试' })).toBeInTheDocument()
    const selector = screen.getByRole('combobox', { name: '扩展作用域项目' })
    expect(selector).toBeEnabled()
    await userEvent.click(selector)
    await userEvent.click(await screen.findByRole('option', { name: 'group/frontend' }))
    expect(await screen.findByText(/项目级 · group\/frontend/)).toBeInTheDocument()
    expect(screen.getByText('项目列表刷新失败')).toBeInTheDocument()
  })
})
