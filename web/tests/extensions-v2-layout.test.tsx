import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { mcpServer, skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

function routes({ moreServers = false } = {}) {
  return (url: string) => {
    if (url.startsWith('/api/v1/mcp-servers')) {
      return jsonResponse({
        servers: [mcpServer(), mcpServer({ id: 's2', name: 'sourcegraph', enabled: false, tools: [{ name: 'find' }] })],
        ...(moreServers ? { next_cursor: 'c2' } : {}),
      })
    }
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills: [skill(), skill({ id: 'k2', name: 'sec-checklist' }), skill({ id: 'k3', name: 'payments' })] })
    if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
    return jsonResponse({})
  }
}

describe('extensions v2 layout', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows tab counts only for fully loaded lists', async () => {
    installFetch(routes({ moreServers: true }))
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    await screen.findByText('sourcegraph')
    const tabs = screen.getByRole('tablist', { name: '扩展类型' })
    expect(await within(tabs).findByRole('tab', { name: 'Skill 3' })).toBeInTheDocument()
    expect(within(tabs).getByRole('tab', { name: 'MCP 服务器' })).toBeInTheDocument()
    expect(within(tabs).getByRole('tab', { name: '块绑定' })).toBeInTheDocument()
  })

  it('lists MCP servers as table rows with scope, command, status and tool count', async () => {
    installFetch(routes())
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    const row = (await screen.findByText('sourcegraph')).closest('tr') as HTMLElement
    expect(row).not.toBeNull()
    expect(within(row).getByText('平台级')).toBeInTheDocument()
    expect(within(row).getByText('停用')).toBeInTheDocument()
    expect(within(row).getByText('1')).toBeInTheDocument()
    expect(within(screen.getByRole('table')).getByRole('columnheader', { name: '命令' })).toBeInTheDocument()
    expect(await screen.findByRole('tab', { name: 'MCP 服务器 2' })).toBeInTheDocument()
  })

  it('renders skills as cards with an upload card for writers only', async () => {
    installFetch(routes())
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    const card = (await screen.findByText('sec-checklist')).closest('article') as HTMLElement
    expect(within(card).getByText('平台级')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /上传 Skill zip/ }))
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('hides the upload card from viewers', async () => {
    installFetch(routes())
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />, { me: viewerMe })

    await screen.findByText('sec-checklist')
    expect(screen.queryByRole('button', { name: /上传 Skill zip/ })).not.toBeInTheDocument()
  })
})
