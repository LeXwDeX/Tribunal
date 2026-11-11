import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { setCsrfToken } from '../src/api/client'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import type { McpServer } from '../src/api/types'
import { mcpServer } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function Manager({ scope = 'platform', projectId }: { scope?: 'platform' | 'project'; projectId?: number } = {}) {
  const [tab, setTab] = useState('mcp')
  return <ExtensionsManager scope={scope} projectId={projectId} canManage tab={tab} onTabChange={setTab} />
}

type Write = { url: string; method: string; body: unknown }

const document = {
  mcpServers: {
    alpha: { command: 'npx', env: { TOKEN: 'secret-canary' } },
    beta: { url: 'https://mcp.example.com', headers: { Authorization: 'Bearer import-canary' } },
  },
}

function routes({
  writes = [] as Write[],
  importResponse = undefined as (() => Response) | undefined,
  testResponse = undefined as ((body: any) => Response | Promise<Response> | undefined) | undefined,
} = {}) {
  let servers: McpServer[] = []
  return (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    const body = init?.body && !(init.body instanceof FormData) ? JSON.parse(String(init.body)) : undefined
    if (method !== 'GET') writes.push({ url, method, body })
    if (url.startsWith('/api/v1/mcp-servers:test')) {
      const custom = testResponse?.(body)
      if (custom) return custom
      if (body?.name === 'beta') {
        return jsonResponse({ error: { code: 'connect_failed', message: 'MCP server connection failed' } }, 502)
      }
      return jsonResponse({
        ok: true,
        latency_ms: 8,
        tool_count: 1,
        truncated: false,
        tools: [{ name: 'search', description: '', read_only: true, destructive: false, bindable: true, annotated: true }],
      })
    }
    if (url.startsWith('/api/v1/mcp-servers/import')) {
      if (importResponse) return importResponse()
      servers = Object.keys(document.mcpServers).map((name) => mcpServer({ id: `id-${name}`, name, config: {} }))
      return jsonResponse({ servers }, 201)
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers })
    if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
    return jsonResponse({ skills: [] })
  }
}

async function openImport(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: '粘贴 Claude Code 配置导入' }))
  return screen.getByRole('dialog', { name: '导入 MCP 服务器' })
}

afterEach(() => { vi.unstubAllGlobals(); setCsrfToken(null) })

describe('import MCP servers dialog', () => {
  it('lists parsed names, tests each draft and imports without echoing config values', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    const dialog = await openImport(user)
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Claude Code 配置' }), { target: { value: JSON.stringify(document) } })
    expect(await within(dialog).findByText('已解析 2 个服务器：alpha、beta')).toBeInTheDocument()

    await user.click(within(dialog).getByRole('button', { name: '逐项测试全部' }))
    const alpha = within(dialog).getByText('alpha').closest('li') as HTMLElement
    const beta = within(dialog).getByText('beta').closest('li') as HTMLElement
    await waitFor(() => expect(within(alpha).getByText('连接成功')).toBeInTheDocument())
    expect(within(alpha).getByText('延迟 8 ms')).toBeInTheDocument()
    expect(within(beta).getByText('测试失败：连接失败')).toBeInTheDocument()

    expect(screen.queryAllByText(/secret-canary|import-canary/).filter((element) => !(element instanceof HTMLTextAreaElement))).toEqual([])

    await user.click(within(dialog).getByRole('button', { name: '导入 2 个服务器' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '导入 MCP 服务器' })).not.toBeInTheDocument())
    expect(writes.find((write) => write.url === '/api/v1/mcp-servers/import?scope=platform')?.body).toEqual(document)
    expect(await screen.findByText('已导入 2 个 MCP 服务器。')).toBeInTheDocument()
  })

  it('shows a parse error instead of guessing at an invalid document', async () => {
    installFetch(routes())
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    const dialog = await openImport(user)
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Claude Code 配置' }), { target: { value: '{"servers":{}}' } })
    expect(await within(dialog).findByText('缺少 mcpServers 对象。')).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '导入' })).toBeDisabled()
  })

  it('keeps the dialog open and shows an actionable conflict without writing', async () => {
    let imports = 0
    installFetch(routes({
      importResponse: () => {
        imports += 1
        return jsonResponse({ error: { code: 'conflict', message: 'mcp server alpha already exists' } }, 409)
      },
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    const dialog = await openImport(user)
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Claude Code 配置' }), { target: { value: JSON.stringify(document) } })
    await user.click(within(dialog).getByRole('button', { name: '导入 2 个服务器' }))
    expect(await within(dialog).findByText('导入失败：存在同名服务器，整批未写入。请修改名称后重试。')).toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: '导入 MCP 服务器' })).toBeInTheDocument()
    expect(screen.queryAllByText(/secret-canary|import-canary/).filter((element) => !(element instanceof HTMLTextAreaElement))).toEqual([])
    expect(imports).toBe(1)
  })

  it('shows backend field validation without rendering the rejected configuration', async () => {
    installFetch(routes({
      importResponse: () => jsonResponse({
        error: {
          code: 'validation_failed',
          message: 'invalid request',
          details: [{ field: 'mcpServers.beta', message: 'config must be a non-empty JSON object' }],
        },
      }, 400),
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    const dialog = await openImport(user)
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Claude Code 配置' }), { target: { value: JSON.stringify(document) } })
    await user.click(within(dialog).getByRole('button', { name: '导入 2 个服务器' }))
    expect(await within(dialog).findByText(/mcpServers\.beta: config must be a non-empty JSON object/)).toBeInTheDocument()
    expect(screen.queryAllByText(/secret-canary|import-canary/).filter((element) => !(element instanceof HTMLTextAreaElement))).toEqual([])
  })

  it('disables the document while a probe runs and clears results after an edit', async () => {
    let resolveTest!: (value: Response) => void
    const pending = new Promise<Response>((resolve) => { resolveTest = resolve })
    installFetch(routes({ testResponse: (body) => body?.name === 'alpha' ? pending : undefined }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)

    const dialog = await openImport(user)
    const textarea = within(dialog).getByRole('textbox', { name: 'Claude Code 配置' })
    fireEvent.change(textarea, { target: { value: JSON.stringify(document) } })
    const alpha = within(dialog).getByText('alpha').closest('li') as HTMLElement
    await user.click(within(alpha).getByRole('button', { name: '测试' }))
    expect(textarea).toBeDisabled()

    await act(async () => {
      resolveTest(jsonResponse({
        ok: true,
        latency_ms: 6,
        tool_count: 1,
        truncated: false,
        tools: [{ name: 'search', description: '', read_only: true, destructive: false, bindable: true, annotated: true }],
      }))
    })
    expect(await within(alpha).findByText('连接成功')).toBeInTheDocument()

    fireEvent.change(textarea, {
      target: { value: JSON.stringify({ mcpServers: { ...document.mcpServers, gamma: { command: 'uvx' } } }) },
    })
    expect(within(dialog).queryByText('连接成功')).not.toBeInTheDocument()
    expect(within(dialog).getByText('已解析 3 个服务器：alpha、beta、gamma')).toBeInTheDocument()
  })

  it('imports project-scoped servers with the project id', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="project" projectId={42} canManage tab="mcp" onTabChange={() => {}} />)

    const dialog = await openImport(user)
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Claude Code 配置' }), { target: { value: JSON.stringify(document) } })
    await user.click(within(dialog).getByRole('button', { name: '导入 2 个服务器' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '导入 MCP 服务器' })).not.toBeInTheDocument())
    expect(writes.find((write) => write.method === 'POST' && write.url.includes('/api/v1/mcp-servers/import'))?.url)
      .toBe('/api/v1/mcp-servers/import?scope=project&project_id=42')
  })

  it('drops a stale tested tool list when an import replaces the server', async () => {
    const server = mcpServer({ id: 's-team', name: 'team-search' })
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      if (url === '/api/v1/mcp-servers/s-team/test') {
        return jsonResponse({
          ok: true,
          latency_ms: 4,
          tool_count: 1,
          truncated: false,
          tools: [{ name: 'search', description: '', read_only: true, destructive: false, bindable: true, annotated: true }],
        })
      }
      if (method === 'POST' && url.startsWith('/api/v1/mcp-servers/import')) {
        return jsonResponse({ servers: [{ ...server, config: {} }] }, 201)
      }
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [server] })
      if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
      return jsonResponse({ skills: [] })
    })
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const row = (await screen.findByText('team-search')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: '测试并列出工具' }))
    await screen.findByText('连接成功')
    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(await screen.findByRole('columnheader', { name: 'mcp__team-search__search' })).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: /MCP 服务器/ }))
    const dialog = await openImport(user)
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Claude Code 配置' }), {
      target: { value: '{"mcpServers":{"team-search":{"command":"npx","args":["--new"]}}}' },
    })
    await user.click(within(dialog).getByRole('button', { name: '导入 1 个服务器' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '导入 MCP 服务器' })).not.toBeInTheDocument())

    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(screen.queryByRole('columnheader', { name: 'mcp__team-search__search' })).not.toBeInTheDocument()
  })

  it('does not grant tools from a disabled imported server after a successful probe', async () => {
    const disabled = mcpServer({ id: 's-disabled', name: 'alpha', enabled: false })
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      if (url.startsWith('/api/v1/mcp-servers:test')) {
        return jsonResponse({
          ok: true,
          latency_ms: 4,
          tool_count: 1,
          truncated: false,
          tools: [{ name: 'search', description: '', read_only: true, destructive: false, bindable: true, annotated: true }],
        })
      }
      if (method === 'POST' && url.startsWith('/api/v1/mcp-servers/import')) {
        return jsonResponse({ servers: [disabled] }, 201)
      }
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [disabled] })
      if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
      return jsonResponse({ skills: [] })
    })
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    const dialog = await openImport(user)
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Claude Code 配置' }), {
      target: { value: '{"mcpServers":{"alpha":{"command":"npx"}}}' },
    })
    const item = within(dialog).getByText('alpha').closest('li') as HTMLElement
    await user.click(within(item).getByRole('button', { name: '测试' }))
    expect(await within(item).findByText('连接成功')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: '导入 1 个服务器' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '导入 MCP 服务器' })).not.toBeInTheDocument())

    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(screen.queryByRole('columnheader', { name: 'mcp__alpha__search' })).not.toBeInTheDocument()
  })
})
