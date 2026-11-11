import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { mcpServer } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const builtinServer = mcpServer({ id: 's-builtin', name: 'codebase-memory', builtin: true })
const customServer = mcpServer({ id: 's-team', name: 'team-search' })

function routes(writes: Array<{ url: string; init?: RequestInit }>) {
  return (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    if (method !== 'GET') {
      writes.push({ url, init })
      return jsonResponse({ tools: [{ name: 'search' }] })
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [builtinServer, customServer] })
    if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
    return jsonResponse({ skills: [] })
  }
}

function card(name: string): HTMLElement {
  return screen.getByText(name).closest('tr') as HTMLElement
}

describe('built-in MCP servers in the extensions console', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('marks built-in servers, hides edit, delete and toggle, and tests them', async () => {
    const writes: Array<{ url: string; init?: RequestInit }> = []
    installFetch(routes(writes))
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    await screen.findByText('codebase-memory')
    const builtin = card('codebase-memory')
    const custom = card('team-search')
    expect(within(builtin).getByText('内置')).toBeInTheDocument()
    expect(within(builtin).queryByRole('button', { name: '删除' })).not.toBeInTheDocument()
    expect(within(builtin).queryByRole('button', { name: '编辑' })).not.toBeInTheDocument()
    expect(within(builtin).queryByRole('button', { name: '停用' })).not.toBeInTheDocument()
    expect(within(builtin).queryByRole('button', { name: '启用' })).not.toBeInTheDocument()
    expect(within(builtin).getByRole('button', { name: '测试并列出工具' })).toBeInTheDocument()
    expect(within(custom).queryByText('内置')).not.toBeInTheDocument()
    expect(within(custom).getByRole('button', { name: '删除' })).toBeInTheDocument()

    await userEvent.click(within(builtin).getByRole('button', { name: '测试并列出工具' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0].url).toBe('/api/v1/mcp-servers/s-builtin/test')
    expect(writes[0].init?.method).toBe('POST')
    expect(await screen.findByText('连接成功')).toBeInTheDocument()
    expect(screen.getByText('工具 1 个')).toBeInTheDocument()
    await userEvent.click(within(custom).getByRole('button', { name: '测试并列出工具' }))
    await waitFor(() => expect(writes).toHaveLength(2))
    expect(writes[1].url).toBe('/api/v1/mcp-servers/s-team/test')
    expect(writes[1].init?.method).toBe('POST')
  })

  it('shows the marker but no actions to a viewer', async () => {
    installFetch(routes([]))
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />, { me: viewerMe })
    await screen.findByText('codebase-memory')
    const builtin = card('codebase-memory')
    expect(within(builtin).getByText('内置')).toBeInTheDocument()
    expect(within(builtin).queryByRole('button')).not.toBeInTheDocument()
  })
})
