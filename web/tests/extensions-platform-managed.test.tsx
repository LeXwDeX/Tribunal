import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { setCsrfToken } from '../src/api/client'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { mcpServer, skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const servers = [
  mcpServer({ id: 'builtin', name: 'builtin-mcp', builtin: true }),
  mcpServer({ id: 'builtin-disabled', name: 'builtin-disabled-mcp', builtin: true, enabled: false }),
  mcpServer({ id: 'custom', name: 'custom-mcp' }),
]
const skills = [
  skill({ id: 'builtin', name: 'builtin-skill', builtin: true }),
  skill({ id: 'builtin-disabled', name: 'builtin-disabled-skill', builtin: true, enabled: false }),
  skill({ id: 'custom', name: 'custom-skill' }),
]

afterEach(() => { vi.unstubAllGlobals(); setCsrfToken(null) })

describe.each(['platform', 'project'] as const)('%s managed extensions', (scope) => {
  function renderTab(tab: 'mcp' | 'skills', empty = false) {
    installFetch((url) => {
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: empty ? [] : servers })
      if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills: empty ? [] : skills })
      return jsonResponse({ bindings: [] })
    })
    return renderWithProviders(<ExtensionsManager scope={scope} projectId={scope === 'project' ? 42 : undefined} canManage tab={tab} onTabChange={() => {}} />)
  }

  it('shows MCP creation while keeping built-in toggles hidden and existing actions', async () => {
    renderTab('mcp')
    const builtin = (await screen.findByText('builtin-mcp')).closest('tr') as HTMLElement
    const disabled = screen.getByText('builtin-disabled-mcp').closest('tr') as HTMLElement
    const custom = screen.getByText('custom-mcp').closest('tr') as HTMLElement

    expect(screen.getByRole('button', { name: '新增 MCP 服务器' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '粘贴 Claude Code 配置导入' })).toBeInTheDocument()
    expect(within(builtin).queryByRole('button', { name: '停用' })).not.toBeInTheDocument()
    expect(within(disabled).queryByRole('button', { name: '启用' })).not.toBeInTheDocument()
    expect(within(builtin).getByRole('button', { name: '测试并列出工具' })).toBeInTheDocument()
    expect(within(custom).getByRole('button', { name: '编辑' })).toBeInTheDocument()
    expect(within(custom).getByRole('button', { name: '删除' })).toBeInTheDocument()
    expect(within(custom).getByRole('button', { name: '测试并列出工具' })).toBeInTheDocument()
  })

  it('shows Skill creation while keeping built-in toggles hidden and existing actions', async () => {
    renderTab('skills')
    const builtin = (await screen.findByText('builtin-skill')).closest('article') as HTMLElement
    const disabled = screen.getByText('builtin-disabled-skill').closest('article') as HTMLElement
    const custom = screen.getByText('custom-skill').closest('article') as HTMLElement

    expect(screen.getByRole('button', { name: '上传 Skill zip' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '登记服务器目录' })).toBeInTheDocument()
    expect(within(builtin).queryByRole('button', { name: '禁用' })).not.toBeInTheDocument()
    expect(within(disabled).queryByRole('button', { name: '启用' })).not.toBeInTheDocument()
    expect(within(builtin).getByText('启用')).toBeInTheDocument()
    expect(within(disabled).getByText('已禁用')).toBeInTheDocument()
    expect(within(custom).getByRole('button', { name: '禁用' })).toBeInTheDocument()
    expect(within(custom).getByRole('button', { name: '删除' })).toBeInTheDocument()
  })

  it.each(['mcp', 'skills'] as const)('describes platform management in an empty %s list', async (tab) => {
    renderTab(tab, true)
    expect(await screen.findByText(/由平台管理/)).toBeInTheDocument()
    if (tab === 'mcp') {
      expect(screen.getByRole('button', { name: '新增 MCP 服务器' })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '粘贴 Claude Code 配置导入' })).toBeInTheDocument()
    } else {
      expect(screen.getByRole('button', { name: '上传 Skill zip' })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '登记服务器目录' })).toBeInTheDocument()
    }
  })
})

it.each(['mcp', 'skills'] as const)('hides %s creation entries from viewers', async (tab) => {
  installFetch((url) => {
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers })
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills })
    return jsonResponse({ bindings: [] })
  })
  renderWithProviders(
    <ExtensionsManager scope="platform" tab={tab} onTabChange={() => {}} />,
    { me: viewerMe },
  )
  if (tab === 'mcp') {
    await screen.findByText('builtin-mcp')
    expect(screen.queryByRole('button', { name: '新增 MCP 服务器' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '粘贴 Claude Code 配置导入' })).not.toBeInTheDocument()
  } else {
    await screen.findByText('builtin-skill')
    expect(screen.queryByRole('button', { name: '上传 Skill zip' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '登记服务器目录' })).not.toBeInTheDocument()
  }
})

it.each([true, false])('keeps editing and toggling an existing custom MCP server (enabled=%s)', async (enabled) => {
  const writes: Array<{ url: string; body: unknown }> = []
  installFetch((url, init) => {
    if (init?.method === 'PUT') {
      writes.push({ url, body: JSON.parse(String(init.body)) })
      return jsonResponse({})
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: servers.map((server) => server.id === 'custom' ? { ...server, enabled } : server) })
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills })
    return jsonResponse({ bindings: [] })
  })
  setCsrfToken('csrf-test')
  const user = userEvent.setup()
  renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)
  const custom = (await screen.findByText('custom-mcp')).closest('tr') as HTMLElement
  await user.click(within(custom).getByRole('button', { name: '编辑' }))
  const dialog = screen.getByRole('dialog', { name: '编辑 MCP 服务器' })
  await user.click(within(dialog).getByRole('combobox', { name: '启用' }))
  await user.click(await screen.findByRole('option', { name: enabled ? '停用' : '启用' }))
  await user.click(within(dialog).getByRole('button', { name: '保存' }))
  await waitFor(() => expect(writes).toEqual([{ url: '/api/v1/mcp-servers/custom', body: { name: 'custom-mcp', enabled: !enabled } }]))
})

it('replaces the full MCP config when the replacement JSON is valid', async () => {
  const writes: Array<{ url: string; body: unknown }> = []
  installFetch((url, init) => {
    if (init?.method === 'PUT') {
      writes.push({ url, body: JSON.parse(String(init.body)) })
      return jsonResponse({})
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers })
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills })
    return jsonResponse({ bindings: [] })
  })
  setCsrfToken('csrf-test')
  const user = userEvent.setup()
  renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)
  const custom = (await screen.findByText('custom-mcp')).closest('tr') as HTMLElement
  await user.click(within(custom).getByRole('button', { name: '编辑' }))
  const dialog = screen.getByRole('dialog', { name: '编辑 MCP 服务器' })
  await user.click(within(dialog).getByRole('checkbox', { name: '替换完整配置' }))
  fireEvent.change(within(dialog).getByLabelText('新的配置 JSON'), {
    target: { value: '{"command":"new-mcp","args":["--x"]}' },
  })
  await user.click(within(dialog).getByRole('button', { name: '保存' }))
  await waitFor(() => expect(writes).toEqual([{
    url: '/api/v1/mcp-servers/custom',
    body: { name: 'custom-mcp', enabled: true, config: { command: 'new-mcp', args: ['--x'] } },
  }]))
})

it('rejects invalid replacement JSON without submitting', async () => {
  const writes: Array<{ url: string; body: unknown }> = []
  installFetch((url, init) => {
    if (init?.method === 'PUT') {
      writes.push({ url, body: JSON.parse(String(init.body)) })
      return jsonResponse({})
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers })
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills })
    return jsonResponse({ bindings: [] })
  })
  setCsrfToken('csrf-test')
  const user = userEvent.setup()
  renderWithProviders(<ExtensionsManager scope="platform" tab="mcp" onTabChange={() => {}} />)
  const custom = (await screen.findByText('custom-mcp')).closest('tr') as HTMLElement
  await user.click(within(custom).getByRole('button', { name: '编辑' }))
  const dialog = screen.getByRole('dialog', { name: '编辑 MCP 服务器' })
  await user.click(within(dialog).getByRole('checkbox', { name: '替换完整配置' }))
  fireEvent.change(within(dialog).getByLabelText('新的配置 JSON'), { target: { value: 'not json' } })
  await user.click(within(dialog).getByRole('button', { name: '保存' }))
  expect(await within(dialog).findByText('替换配置 JSON 无效或不是对象。')).toBeInTheDocument()
  expect(writes).toHaveLength(0)
})

it.each(['mcp', 'skills'] as const)('keeps deleting existing custom %s extensions', async (tab) => {
  const writes: string[] = []
  installFetch((url, init) => {
    if (init?.method === 'DELETE') { writes.push(url); return jsonResponse({}) }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers })
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills })
    return jsonResponse({ bindings: [] })
  })
  setCsrfToken('csrf-test')
  const user = userEvent.setup()
  renderWithProviders(<ExtensionsManager scope="platform" tab={tab} onTabChange={() => {}} />)
  const custom = (await screen.findByText(tab === 'mcp' ? 'custom-mcp' : 'custom-skill')).closest(tab === 'mcp' ? 'tr' : 'article') as HTMLElement
  await user.click(within(custom).getByRole('button', { name: '删除' }))
  const dialog = screen.getByRole('dialog', { name: tab === 'mcp' ? '删除 MCP 服务器？' : '删除 Skill？' })
  await user.click(within(dialog).getByRole('button', { name: '删除' }))
  await waitFor(() => expect(writes).toEqual([`/api/v1/${tab === 'mcp' ? 'mcp-servers' : 'skills'}/custom`]))
})
