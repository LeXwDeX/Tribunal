import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { setCsrfToken } from '../src/api/client'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import type { McpServer } from '../src/api/types'
import { mcpServer } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

function Manager({ scope = 'platform', projectId }: { scope?: 'platform' | 'project'; projectId?: number } = {}) {
  const [tab, setTab] = useState('mcp')
  return <ExtensionsManager scope={scope} projectId={projectId} canManage tab={tab} onTabChange={setTab} />
}

type Write = { url: string; method: string; body: unknown }

const testTools = [
  { name: 'search', description: 'Search the code graph', read_only: true, destructive: false, bindable: true, annotated: true },
  { name: 'index', description: 'Reindex a project', read_only: false, destructive: true, bindable: false, annotated: true },
]

function routes({ writes = [] as Write[], create = undefined as ((body: any) => Response) | undefined } = {}) {
  let servers: McpServer[] = []
  return (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    const body = init?.body && !(init.body instanceof FormData) ? JSON.parse(String(init.body)) : undefined
    if (method !== 'GET') writes.push({ url, method, body })
    if (url.startsWith('/api/v1/mcp-servers:test')) {
      return jsonResponse({ ok: true, latency_ms: 42, tool_count: 2, truncated: false, tools: testTools })
    }
    if (url.startsWith('/api/v1/mcp-servers/import')) {
      return jsonResponse({ servers: [] }, 201)
    }
    if (url === '/api/v1/mcp-servers' && method === 'POST') {
      if (create) return create(body)
      const server: McpServer = mcpServer({ id: `id-${body.name}`, name: body.name, config: {}, enabled: body.enabled !== false })
      servers = [...servers, server]
      return jsonResponse(server, 201)
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers })
    if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
    return jsonResponse({ skills: [] })
  }
}

afterEach(() => { vi.unstubAllGlobals(); setCsrfToken(null) })

describe('MCP creation entries', () => {
  it('shows add and import only to writers', async () => {
    installFetch(routes())
    const writer = renderWithProviders(<Manager />)
    expect(await screen.findByRole('button', { name: '新增 MCP 服务器' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '粘贴 Claude Code 配置导入' })).toBeInTheDocument()
    writer.unmount()

    installFetch(routes())
    renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />, { me: viewerMe })
    await screen.findByText('还没有 MCP 服务器')
    expect(screen.queryByRole('button', { name: '新增 MCP 服务器' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '粘贴 Claude Code 配置导入' })).not.toBeInTheDocument()
  })
})

describe('add MCP server dialog', () => {
  it('tests a stdio draft before saving and adds the tools to the binding matrix', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    expect(dialog.querySelector('.formGrid')).not.toBeNull()
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'team-search')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.type(within(dialog).getByRole('textbox', { name: '参数' }), '-y\n@team/mcp')
    fireEvent.change(within(dialog).getByRole('textbox', { name: '环境变量' }), { target: { value: '{"TOKEN":"${MCP_TOKEN}"}' } })

    await user.click(within(dialog).getByRole('button', { name: '测试连接' }))
    expect(await within(dialog).findByText('连接成功')).toBeInTheDocument()
    expect(within(dialog).getByText('延迟 42 ms')).toBeInTheDocument()
    expect(within(dialog).getByText('工具 2 个')).toBeInTheDocument()
    expect(within(dialog).getByText('search')).toBeInTheDocument()
    expect(within(dialog).getByText('只读')).toBeInTheDocument()
    expect(within(dialog).getByText('不可绑定')).toBeInTheDocument()
    expect(writes.find((write) => write.url === '/api/v1/mcp-servers:test')?.body).toEqual({
      scope: 'platform',
      name: 'team-search',
      enabled: true,
      config: { command: 'npx', args: ['-y', '@team/mcp'], env: { TOKEN: '${MCP_TOKEN}' } },
    })

    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '新增 MCP 服务器' })).not.toBeInTheDocument())
    expect(writes.find((write) => write.url === '/api/v1/mcp-servers' && write.method === 'POST')?.body).toEqual({
      scope: 'platform',
      name: 'team-search',
      enabled: true,
      config: { command: 'npx', args: ['-y', '@team/mcp'], env: { TOKEN: '${MCP_TOKEN}' } },
    })
    expect(await screen.findByText('MCP 服务器 team-search 已创建。')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(await screen.findByRole('columnheader', { name: 'mcp__team-search__search' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'review 使用 mcp__team-search__search' })).toBeEnabled()
  })

  it('creates an http server and fills the form from pasted JSON', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.click(within(dialog).getByText('粘贴单个服务器 JSON 配置'))
    fireEvent.change(within(dialog).getByRole('textbox', { name: '服务器 JSON' }), { target: { value: '{"name":"remote-mcp","url":"https://mcp.example.com","headers":{"Authorization":"Bearer ${MCP_TOKEN}"}}' } })
    await user.click(within(dialog).getByRole('button', { name: '填入表单' }))

    expect(within(dialog).getByRole('textbox', { name: '名称' })).toHaveValue('remote-mcp')
    expect(within(dialog).getByRole('combobox', { name: '传输方式' })).toHaveTextContent('http')
    expect(within(dialog).getByRole('textbox', { name: '服务地址' })).toHaveValue('https://mcp.example.com')
    expect(within(dialog).getByRole('textbox', { name: '请求头' })).toHaveValue('{\n  "Authorization": "Bearer ${MCP_TOKEN}"\n}')

    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    await waitFor(() => expect(writes.some((write) => write.url === '/api/v1/mcp-servers' && write.method === 'POST')).toBe(true))
    expect(writes.find((write) => write.url === '/api/v1/mcp-servers')?.body).toEqual({
      scope: 'platform',
      name: 'remote-mcp',
      enabled: true,
      config: { url: 'https://mcp.example.com', headers: { Authorization: 'Bearer ${MCP_TOKEN}' } },
    })
  })

  it('maps a 400 field error and a 409 name conflict without closing the dialog', async () => {
    let status = 400
    const writes: Write[] = []
    installFetch(routes({
      writes,
      create: () => status === 400
        ? jsonResponse({
          error: {
            code: 'validation_failed',
            message: 'invalid request',
            details: [{ field: 'name', message: 'name must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$' }],
          },
        }, 400)
        : jsonResponse({ error: { code: 'conflict', message: 'mcp server already exists' } }, 409),
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'team-search')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    expect(await within(dialog).findByText(/name must match/)).toBeInTheDocument()
    expect(within(dialog).getByRole('textbox', { name: '名称' })).toHaveAttribute('aria-invalid', 'true')

    status = 409
    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    expect(await within(dialog).findByText('同名 MCP 服务器已存在，请更换名称。')).toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: '新增 MCP 服务器' })).toBeInTheDocument()
    expect(writes.filter((write) => write.url === '/api/v1/mcp-servers' && write.method === 'POST')).toHaveLength(2)
  })

  it('reports a fixed test error category and allows retrying', async () => {
    let attempts = 0
    installFetch((url) => {
      if (url.startsWith('/api/v1/mcp-servers:test')) {
        attempts += 1
        if (attempts === 1) {
          return jsonResponse({ error: { code: 'connect_timeout', message: 'MCP server connection timed out' } }, 504)
        }
        return jsonResponse({ ok: true, latency_ms: 7, tool_count: 0, truncated: false, tools: [] })
      }
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
      if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
      return jsonResponse({ skills: [] })
    })
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'flaky')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.click(within(dialog).getByRole('button', { name: '测试连接' }))
    expect(await within(dialog).findByText('测试失败：连接超时')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: '重试测试' }))
    expect(await within(dialog).findByText('连接成功')).toBeInTheDocument()
    expect(within(dialog).getByText('测试成功，但服务端没有返回工具列表。')).toBeInTheDocument()
    expect(attempts).toBe(2)
  })

  it('invalidates a successful draft test when the configuration changes', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'team-search')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.click(within(dialog).getByRole('button', { name: '测试连接' }))
    expect(await within(dialog).findByText('连接成功')).toBeInTheDocument()

    fireEvent.change(within(dialog).getByRole('textbox', { name: '启动命令' }), { target: { value: 'uvx' } })
    expect(within(dialog).queryByText('连接成功')).not.toBeInTheDocument()

    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '新增 MCP 服务器' })).not.toBeInTheDocument())
    expect(writes.find((write) => write.url === '/api/v1/mcp-servers')?.body).toMatchObject({ config: { command: 'uvx' } })

    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(screen.getByRole('tab', { name: '块绑定' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.queryByRole('columnheader', { name: 'mcp__team-search__search' })).not.toBeInTheDocument()
  })

  it('rejects consecutive underscores in the name before sending', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'a__b')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))

    expect(await within(dialog).findByText('名称不得包含连续下划线（__）。')).toBeInTheDocument()
    expect(writes.filter((write) => write.url === '/api/v1/mcp-servers' && write.method === 'POST')).toHaveLength(0)
  })

  it('creates a project-scoped server with the project id', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager scope="project" projectId={42} />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'project-mcp')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))

    await waitFor(() => expect(writes.some((write) => write.url === '/api/v1/mcp-servers' && write.method === 'POST')).toBe(true))
    expect(writes.find((write) => write.url === '/api/v1/mcp-servers')?.body).toMatchObject({
      scope: 'project',
      project_id: 42,
      name: 'project-mcp',
    })
  })

  it('shows a banner for validation details on fields the form does not map', async () => {
    installFetch(routes({
      create: () => jsonResponse({
        error: {
          code: 'validation_failed',
          message: 'invalid request',
          details: [{ field: 'scope', message: 'scope is invalid' }],
        },
      }, 400),
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'team-search')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    expect(await within(dialog).findByText(/scope: scope is invalid/)).toBeInTheDocument()
  })

  it('warns that a lost create response is uncertain', async () => {
    installFetch(routes({
      create: () => jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500),
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'team-search')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    expect(await within(dialog).findByText(/请求结果可能不确定/)).toBeInTheDocument()
  })

  it('maps draft test validation details onto the config field', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/mcp-servers:test')) {
        return jsonResponse({
          error: {
            code: 'validation_failed',
            message: 'invalid request',
            details: [{ field: 'config', message: 'config is invalid' }],
          },
        }, 400)
      }
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
      if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
      return jsonResponse({ skills: [] })
    })
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<Manager />)

    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    await user.type(within(dialog).getByRole('textbox', { name: '名称' }), 'team-search')
    await user.type(within(dialog).getByRole('textbox', { name: '启动命令' }), 'npx')
    await user.click(within(dialog).getByRole('button', { name: '测试连接' }))
    expect(await within(dialog).findByText('测试失败：配置校验未通过，请检查标记的字段。')).toBeInTheDocument()
    expect(within(dialog).getByText('config is invalid')).toBeInTheDocument()
  })
})
