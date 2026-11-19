import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { binding, mcpServer, skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const manyTools = Array.from({ length: 25 }, (_, index) => ({
  name: `tool-${String(index + 1).padStart(2, '0')}`,
  description: `第 ${index + 1} 个工具`,
  read_only: true,
  destructive: false,
  bindable: true,
  annotated: true,
}))

function routes() {
  return (url: string) => {
    if (url === '/api/v1/mcp-servers/s-ready/test') {
      return jsonResponse({ ok: true, latency_ms: 12, tool_count: 25, truncated: false, tools: manyTools })
    }
    if (url.startsWith('/api/v1/mcp-servers')) {
      return jsonResponse({
        servers: [
          mcpServer({ id: 's-ready', name: 'ready-mcp', builtin: true, status: 'ready', tools: [{ name: 'a' }, { name: 'b' }] }),
          mcpServer({ id: 's-idle', name: 'idle-mcp', enabled: false }),
        ],
      })
    }
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills: [skill({ id: 'k1', name: 'code-review' })] })
    if (url.startsWith('/api/v1/block-bindings')) {
      return jsonResponse({
        bindings: [binding({ block: 'review', mcp_tools: ['mcp__team-search__search'], skills: ['code-review'] })],
      })
    }
    return jsonResponse({})
  }
}

// The extensions tables render through the shared DataTable primitive
// (docs/design/07-web-console.md, section 4.3).
describe('extensions tables on the shared table primitive', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('names the MCP table, centres one status badge per row and keeps the reason in a tooltip', async () => {
    installFetch(routes())
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    const table = await screen.findByRole('table', { name: 'MCP 服务器' })
    expect(within(table).getAllByRole('columnheader').map((header) => header.textContent))
      .toEqual(['名称', '作用域', '命令', '状态', '工具', '操作'])

    const ready = within(table).getByText('ready-mcp').closest('tr') as HTMLElement
    expect(within(ready).getByText('2')).toBeInTheDocument()
    expect(within(ready).queryByText('ready')).not.toBeInTheDocument()
    const readyBadge = within(ready).getByText('启用').closest('[tabindex]') as HTMLElement
    expect(document.getElementById(readyBadge.getAttribute('aria-describedby') as string)).toHaveTextContent('ready')

    const idle = within(table).getByText('idle-mcp').closest('tr') as HTMLElement
    expect(within(idle).getByText('未提供')).toBeInTheDocument()
    expect(within(idle).queryByText('状态未提供')).not.toBeInTheDocument()
    const idleBadge = within(idle).getByText('停用').closest('[tabindex]') as HTMLElement
    expect(document.getElementById(idleBadge.getAttribute('aria-describedby') as string)).toHaveTextContent('状态未提供')

    // Row actions: a built-in server can only be tested; a custom one can be edited and deleted.
    expect(within(ready).getAllByRole('button').map((button) => button.textContent)).toEqual(['测试并列出工具'])
    expect(within(idle).getAllByRole('button').map((button) => button.textContent)).toEqual(['测试并列出工具', '编辑', '删除'])
  })

  it('drops the operations column for viewers', async () => {
    installFetch(routes())
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />, { me: viewerMe })

    const table = await screen.findByRole('table', { name: 'MCP 服务器' })
    expect(within(table).getAllByRole('columnheader').map((header) => header.textContent))
      .toEqual(['名称', '作用域', '命令', '状态', '工具'])
    expect(within(table).queryByRole('button')).not.toBeInTheDocument()
  })

  it('pages a long tool list 20 rows at a time instead of growing the table', async () => {
    installFetch(routes())
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    const row = (await screen.findByText('ready-mcp')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))

    const tools = await screen.findByRole('table', { name: 'MCP 工具' })
    expect(within(tools).getAllByRole('columnheader').map((header) => header.textContent)).toEqual(['工具', '描述', '注解'])
    expect(within(tools).getAllByRole('row')).toHaveLength(21)
    expect(within(tools).getByText('tool-20')).toBeInTheDocument()
    expect(within(tools).queryByText('tool-21')).not.toBeInTheDocument()
    expect(screen.getByText('第 1 / 2 页 · 共 25 条')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '下一页' }))
    expect(within(tools).getAllByRole('row')).toHaveLength(6)
    expect(within(tools).getByText('tool-25')).toBeInTheDocument()
    expect(within(tools).queryByText('tool-01')).not.toBeInTheDocument()
    expect(screen.getByText('第 2 / 2 页 · 共 25 条')).toBeInTheDocument()
  })

  it('keeps row and column headers on the binding matrix table', async () => {
    installFetch(routes())
    renderWithProviders(<ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />)

    const table = await screen.findByRole('table', { name: '块绑定矩阵' })
    expect(await within(table).findByRole('columnheader', { name: 'mcp__team-search__search' })).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: '块' })).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: '默认' })).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: 'skill: code-review' })).toBeInTheDocument()
    expect(within(table).getByRole('rowheader', { name: 'review' })).toBeInTheDocument()
    expect(within(table).getByRole('button', { name: '恢复 review 的默认绑定' })).toBeInTheDocument()
    expect(within(table).getByRole('button', { name: 'review 使用 mcp__team-search__search' })).toHaveAttribute('aria-pressed', 'true')
    expect(within(table).getByRole('button', { name: 'review 使用 Skill code-review' })).toHaveAttribute('aria-pressed', 'true')
  })
})
