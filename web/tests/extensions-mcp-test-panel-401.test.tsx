import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import type { BlockBinding } from '../src/api/types'
import { binding, mcpServer, revisionToken } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function Manager() {
  const [tab, setTab] = useState('mcp')
  return <ExtensionsManager scope="platform" tab={tab} onTabChange={setTab} />
}

const server = mcpServer({ id: 's-team', name: 'team-search' })

const testTools = [
  { name: 'search', description: 'Search the code graph', read_only: true, destructive: false, bindable: true, annotated: true },
  { name: 'rewrite', description: 'Rewrite a file', read_only: false, destructive: true, bindable: false, annotated: true },
  { name: 'wipe', description: 'Delete a file', read_only: false, destructive: true, bindable: false, annotated: true },
  { name: 'plain', description: '', read_only: false, destructive: false, bindable: true, annotated: false },
]

const boundRewrite: BlockBinding[] = [
  binding({ scope: 'platform', block: 'review', mcp_tools: ['mcp__team-search__rewrite'], revision: revisionToken('bound-rewrite') }),
]

function routes(handlers: { test?: () => Response; bindings?: () => Response } = {}) {
  return (url: string) => {
    if (url === '/api/v1/mcp-servers/s-team/test') {
      return handlers.test ? handlers.test() : jsonResponse({
        ok: true, latency_ms: 37, tool_count: 4, truncated: false, tools: testTools,
      })
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [server] })
    if (url.startsWith('/api/v1/block-bindings')) {
      return handlers.bindings ? handlers.bindings() : jsonResponse({ bindings: [] })
    }
    return jsonResponse({ skills: [] })
  }
}

afterEach(() => vi.unstubAllGlobals())

describe('saved MCP connection test panel', () => {
  it('shows latency, tool annotations and lets an already-bound non-bindable tool be removed', async () => {
    installFetch(routes({ bindings: () => jsonResponse({ bindings: boundRewrite }) }))
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const row = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))

    expect(await screen.findByText('连接成功')).toBeInTheDocument()
    expect(screen.getByText('延迟 37 ms')).toBeInTheDocument()
    expect(screen.getByText('工具 4 个')).toBeInTheDocument()
    const search = screen.getByText('search').closest('tr') as HTMLElement
    expect(within(search).getByText('只读')).toBeInTheDocument()
    expect(within(search).getByText('非破坏性')).toBeInTheDocument()
    expect(within(search).getByText('可绑定')).toBeInTheDocument()
    const rewrite = screen.getByText('rewrite').closest('tr') as HTMLElement
    expect(within(rewrite).getByText('不可绑定')).toBeInTheDocument()
    expect(within(rewrite).getByText('破坏性')).toBeInTheDocument()
    const plain = screen.getByText('plain').closest('tr') as HTMLElement
    expect(within(plain).getByText('未注解（按可绑定处理）')).toBeInTheDocument()
    expect(within(plain).queryByText('非只读')).not.toBeInTheDocument()
    expect(within(plain).queryByText('非破坏性')).not.toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(await screen.findByRole('columnheader', { name: 'mcp__team-search__search' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'review 使用 mcp__team-search__search' })).toBeEnabled()

    // Already-bound tools must stay removable even if a later test marks them
    // destructive; only adding an unbound non-bindable tool is blocked.
    const bound = screen.getByRole('button', { name: 'review 使用 mcp__team-search__rewrite' })
    expect(bound).toHaveAttribute('aria-pressed', 'true')
    expect(bound).toBeEnabled()
    await user.click(bound)
    expect(bound).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: 'review 使用 mcp__team-search__wipe' })).toBeDisabled()
    expect(screen.getByRole('columnheader', { name: /mcp__team-search__rewrite/ })).toHaveTextContent('不可绑定')
  })

  it('shows a fixed failure category and retries the test', async () => {
    let attempts = 0
    installFetch(routes({
      test: () => {
        attempts += 1
        if (attempts === 1) {
          return jsonResponse({ error: { code: 'list_tools_failed', message: 'listing MCP tools failed' } }, 502)
        }
        return jsonResponse({ ok: true, latency_ms: 5, tool_count: 1, truncated: false, tools: [testTools[0]] })
      },
    }))
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const row = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))
    expect(await screen.findByText('测试失败：读取工具失败')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '重试测试' }))
    await waitFor(() => expect(screen.getByText('连接成功')).toBeInTheDocument())
    expect(attempts).toBe(2)
  })

  it.each([
    ['too_many_tools', 422, '工具数量超出上限'],
    ['test_busy', 429, '测试并发已满，请稍后重试'],
    ['forbidden', 403, '没有权限执行此操作'],
    ['internal', 500, '测试服务不可用'],
  ])('maps %s to a fixed failure category', async (code, status, label) => {
    installFetch(routes({
      test: () => jsonResponse({ error: { code, message: 'upstream failure' } }, status),
    }))
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const row = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))
    expect(await screen.findByText(`测试失败：${label}`)).toBeInTheDocument()
  })

  it('reports a network failure without inventing a server error', async () => {
    installFetch((url) => {
      if (url === '/api/v1/mcp-servers/s-team/test') throw new Error('offline')
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [server] })
      if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
      return jsonResponse({ skills: [] })
    })
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const row = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))
    expect(await screen.findByText('测试失败：无法连接服务器')).toBeInTheDocument()
  })

  it('marks a truncated tool list', async () => {
    installFetch(routes({
      test: () => jsonResponse({ ok: true, latency_ms: 11, tool_count: 250, truncated: true, tools: [testTools[0]] }),
    }))
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const row = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))
    expect(await screen.findByText('工具 250 个（列表已截断）')).toBeInTheDocument()
  })

  it('shows a disabled server result without adding its tools to the matrix', async () => {
    const disabled = mcpServer({ id: 's-team', name: 'team-search', enabled: false })
    installFetch((url) => {
      if (url === '/api/v1/mcp-servers/s-team/test') {
        return jsonResponse({ ok: true, latency_ms: 5, tool_count: 1, truncated: false, tools: [testTools[0]] })
      }
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [disabled] })
      if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
      return jsonResponse({ skills: [] })
    })
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const row = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    expect(within(row).getByText('停用')).toBeInTheDocument()
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))
    expect(await screen.findByText('连接成功')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(screen.queryByRole('columnheader', { name: 'mcp__team-search__search' })).not.toBeInTheDocument()
  })

  it('removes tested tools from the matrix after the server is deleted', async () => {
    let deleted = false
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      if (url === '/api/v1/mcp-servers/s-team' && method === 'DELETE') {
        deleted = true
        return new Response(null, { status: 204 })
      }
      if (url === '/api/v1/mcp-servers/s-team/test') {
        return jsonResponse({ ok: true, latency_ms: 5, tool_count: 1, truncated: false, tools: [testTools[0]] })
      }
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: deleted ? [] : [server] })
      if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
      return jsonResponse({ skills: [] })
    })
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const row = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))
    await screen.findByText('连接成功')
    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(await screen.findByRole('columnheader', { name: 'mcp__team-search__search' })).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: /MCP 服务器/ }))
    const refreshed = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    await user.click(within(refreshed).getByRole('button', { name: '删除' }))
    const dialog = screen.getByRole('dialog', { name: '删除 MCP 服务器？' })
    await user.click(within(dialog).getByRole('button', { name: '删除' }))
    await waitFor(() => expect(screen.queryByText('team-search')).not.toBeInTheDocument())

    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(screen.queryByRole('columnheader', { name: 'mcp__team-search__search' })).not.toBeInTheDocument()
  })
})
