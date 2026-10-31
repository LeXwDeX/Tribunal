import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes, useNavigate } from 'react-router-dom'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { setCsrfToken } from '../src/api/client'
import { mcpServer, project } from './fixtures'
import { createTestQueryClient, installFetch, jsonResponse, renderWithProviders } from './test-utils'

function setupDetail(route = '/projects/42/extensions', post?: (url: URL, init: RequestInit) => Response | Promise<Response>) {
  const queryClient = createTestQueryClient()
  const servers = [mcpServer({ id: 'a-server', scope: 'project', project_id: 42, name: 'a-server' })]
  for (const id of [42, 43]) {
    queryClient.setQueryData(['project', id], project({ id, path_with_namespace: `group/project-${id}` }))
    queryClient.setQueryData(['mcp-servers', 'project', id], { pages: [{ servers: id === 42 ? servers : [] }], pageParams: [undefined] })
    queryClient.setQueryData(['skills', 'project', id], { pages: [{ skills: [] }], pageParams: [undefined] })
    queryClient.setQueryData(['block-bindings', 'project', id], { pages: [{ bindings: [] }], pageParams: [undefined] })
  }
  installFetch((raw, init) => {
    const url = new URL(raw, 'http://localhost')
    if ((init?.method === 'POST' || init?.method === 'PUT' || init?.method === 'DELETE') && post) return post(url, init)
    if (url.pathname === '/api/v1/mcp-servers') return jsonResponse({ servers: url.searchParams.get('project_id') === '42' ? servers : [] })
    if (url.pathname === '/api/v1/skills') return jsonResponse({ skills: [] })
    if (url.pathname === '/api/v1/block-bindings') return jsonResponse({ bindings: [] })
    if (url.pathname.startsWith('/api/v1/projects/')) {
      const id = Number(url.pathname.split('/').pop())
      return jsonResponse(project({ id, path_with_namespace: `group/project-${id}` }))
    }
    if (url.pathname === '/api/v1/profiles') return jsonResponse({ profiles: [] })
    return jsonResponse({})
  })
  function Switch() {
    const navigate = useNavigate()
    return <button onClick={() => navigate(route.replace('/42/', '/43/'))}>切换到项目 B</button>
  }
  const user = userEvent.setup()
  setCsrfToken('test-csrf')
  renderWithProviders(<><Switch /><Routes><Route path="/projects/:id/:tab" element={<ProjectDetailPage />} /></Routes></>, { route, queryClient })
  return { user, queryClient }
}

describe('cached project detail scope isolation', () => {
  afterEach(() => { vi.unstubAllGlobals(); setCsrfToken(null) })

  it('isolates project drafts even when a mounted manager receives a new project prop', async () => {
    installFetch((raw) => {
      const path = new URL(raw, 'http://localhost').pathname
      return path === '/api/v1/mcp-servers' ? jsonResponse({ servers: [] })
        : path === '/api/v1/skills' ? jsonResponse({ skills: [] }) : jsonResponse({ bindings: [] })
    })
    function Harness() {
      const [id, setId] = useState(42)
      return <><button onClick={() => setId(43)}>切换项目</button><ExtensionsManager scope="project" projectId={id} canManage tab="mcp" onTabChange={() => {}} /></>
    }
    const user = userEvent.setup()
    renderWithProviders(<Harness />)
    await user.click(await screen.findByRole('button', { name: '粘贴 Claude Code 配置导入' }))
    fireEvent.change(screen.getByRole('textbox', { name: 'mcpServers JSON' }), { target: { value: JSON.stringify({ mcpServers: { a: { token: 'a-secret' } } }) } })
    await user.click(screen.getByRole('button', { name: '切换项目' }))
    expect(screen.queryByRole('dialog', { name: '导入 MCP 配置' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '粘贴 Claude Code 配置导入' }))
    expect((screen.getByRole('textbox', { name: 'mcpServers JSON' }) as HTMLTextAreaElement).value).not.toContain('a-secret')
  })

  it('discards an import draft on real navigation and never sends project A secrets to B', async () => {
    const writes: Array<{ project: string | null; body: unknown }> = []
    const { user } = setupDetail(undefined, (url, init) => {
      writes.push({ project: url.searchParams.get('project_id'), body: JSON.parse(String(init.body)) })
      return jsonResponse({})
    })
    await user.click(await screen.findByRole('button', { name: '粘贴 Claude Code 配置导入' }))
    const input = screen.getByRole('textbox', { name: 'mcpServers JSON' })
    fireEvent.change(input, { target: { value: JSON.stringify({ mcpServers: { a: { env: { TOKEN: 'a-secret' } } } }) } })
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    expect(screen.queryByRole('dialog', { name: '导入 MCP 配置' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '粘贴 Claude Code 配置导入' }))
    expect((screen.getByRole('textbox', { name: 'mcpServers JSON' }) as HTMLTextAreaElement).value).not.toContain('a-secret')
    await user.click(screen.getByRole('button', { name: '校验并导入' }))
    await waitFor(() => expect(writes).toEqual([{ project: '43', body: { mcpServers: {} } }]))
  })

  it.each(['directory', 'upload', 'edit', 'delete'] as const)('discards the %s selection on cached navigation', async (kind) => {
    const { user } = setupDetail()
    if (kind === 'directory' || kind === 'upload') await user.click(await screen.findByRole('tab', { name: /^Skill/ }))
    if (kind === 'directory') {
      await user.click(screen.getByRole('button', { name: '登记服务器目录' }))
      await user.type(screen.getByRole('textbox', { name: '目录路径' }), '/secret/project-a')
    } else if (kind === 'upload') {
      await user.click(screen.getByRole('button', { name: '上传 Skill（zip）' }))
      await user.upload(screen.getByLabelText('Skill zip'), new File(['a'], 'a-secret.zip', { type: 'application/zip' }))
    } else if (kind === 'edit') {
      await user.click(await screen.findByRole('button', { name: '编辑' }))
    } else {
      await user.click(await screen.findByRole('button', { name: '删除' }))
    }
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('does not let a completed A import close B, but invalidates the A cache', async () => {
    let resolveA: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveA = resolve })
    const { user, queryClient } = setupDetail(undefined, (url) => url.searchParams.get('project_id') === '42' ? pending : jsonResponse({}))
    await user.click(await screen.findByRole('button', { name: '粘贴 Claude Code 配置导入' }))
    await user.click(screen.getByRole('button', { name: '校验并导入' }))
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    await user.click(screen.getByRole('button', { name: '粘贴 Claude Code 配置导入' }))
    const draft = screen.getByRole('textbox', { name: 'mcpServers JSON' })
    await user.type(draft, 'b-draft')
    await act(async () => { resolveA(jsonResponse({})) })
    expect(screen.getByRole('dialog', { name: '导入 MCP 配置' })).toBeInTheDocument()
    expect((draft as HTMLTextAreaElement).value).toContain('b-draft')
    await waitFor(() => expect(queryClient.getQueryState(['mcp-servers', 'project', 42])?.isInvalidated).toBe(true))
  })

  it.each(['directory', 'upload'] as const)('does not let a completed A %s operation close B and invalidates A', async (kind) => {
    let resolveA: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveA = resolve })
    const writes: Array<{ project: string; kind: string }> = []
    const { user, queryClient } = setupDetail(undefined, (_url, init) => {
      if (init.body instanceof FormData) writes.push({ project: String(init.body.get('project_id')), kind: 'upload' })
      else writes.push({ project: String(JSON.parse(String(init.body)).project_id), kind: 'directory' })
      return pending
    })
    await user.click(await screen.findByRole('tab', { name: /^Skill/ }))
    if (kind === 'directory') {
      await user.click(screen.getByRole('button', { name: '登记服务器目录' }))
      await user.type(screen.getByRole('textbox', { name: '目录路径' }), '/project-a')
      await user.click(screen.getByRole('button', { name: '登记' }))
    } else {
      await user.click(screen.getByRole('button', { name: '上传 Skill（zip）' }))
      await user.upload(screen.getByLabelText('Skill zip'), new File(['a'], 'a.zip', { type: 'application/zip' }))
      await user.click(screen.getByRole('button', { name: '上传' }))
    }
    await waitFor(() => expect(writes).toEqual([{ project: '42', kind }]))
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    if (!screen.queryByRole('button', { name: '登记服务器目录' })) await user.click(screen.getByRole('tab', { name: /^Skill/ }))
    if (kind === 'directory') {
      await user.click(screen.getByRole('button', { name: '登记服务器目录' }))
      await user.type(screen.getByRole('textbox', { name: '目录路径' }), '/project-b')
    } else {
      await user.click(screen.getByRole('button', { name: '上传 Skill（zip）' }))
      await user.upload(screen.getByLabelText('Skill zip'), new File(['b'], 'b.zip', { type: 'application/zip' }))
    }
    await act(async () => { resolveA(jsonResponse({})) })
    const dialog = screen.getByRole('dialog', { name: kind === 'directory' ? '登记 Skill 目录' : '上传 Skill' })
    if (kind === 'directory') expect(within(dialog).getByRole('textbox', { name: '目录路径' })).toHaveValue('/project-b')
    else expect((within(dialog).getByLabelText('Skill zip') as HTMLInputElement).files?.[0].name).toBe('b.zip')
    await waitFor(() => expect(queryClient.getQueryState(['skills', 'project', 42])?.isInvalidated).toBe(true))
  })

  it('does not carry a project deletion confirmation to a different cached settings page', async () => {
    const { user } = setupDetail('/projects/42/settings')
    await user.click(await screen.findByRole('button', { name: '注销项目' }))
    expect(screen.getByRole('dialog', { name: '注销项目？' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    expect(screen.queryByRole('dialog', { name: '注销项目？' })).not.toBeInTheDocument()
  })

  it('does not redirect away from project B when an old project A deletion completes', async () => {
    let resolveDelete: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveDelete = resolve })
    const { user, queryClient } = setupDetail('/projects/42/settings', () => pending)
    await user.click(await screen.findByRole('button', { name: '注销项目' }))
    await user.click(within(screen.getByRole('dialog', { name: '注销项目？' })).getByRole('button', { name: '注销项目' }))
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    await act(async () => { resolveDelete(new Response(null, { status: 204 })) })
    expect(screen.getByRole('heading', { name: 'group/project-43' })).toBeInTheDocument()
    expect(queryClient.getQueryState(['project', 42])).toBeDefined()
  })

  it('invalidates project A after a pending key update even if project B is now open', async () => {
    let resolveSave: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveSave = resolve })
    const { user, queryClient } = setupDetail('/projects/42/settings', (url) => {
      if (url.pathname === '/api/v1/projects/validate') return jsonResponse({ project_id: 42, path_with_namespace: 'group/project-42', username: 'bot', access_level: 40, token_scopes: [], git_head: 'abc' })
      return pending
    })
    await user.click(await screen.findByRole('button', { name: '更新访问 Key' }))
    await user.type(screen.getByLabelText('新的访问 Key'), 'a-secret')
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    await user.click(screen.getByRole('button', { name: '保存访问 Key' }))
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    await act(async () => { resolveSave(new Response(null, { status: 204 })) })
    expect(screen.getByRole('heading', { name: 'group/project-43' })).toBeInTheDocument()
    await waitFor(() => expect(queryClient.getQueryState(['project', 42])?.isInvalidated).toBe(true))
  })

  it.each(['Escape', 'icon', 'backdrop'] as const)('keeps a reopened add draft when the old request succeeds after %s', async (close) => {
    let resolveA: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveA = resolve })
    const { user, queryClient } = setupDetail(undefined, () => pending)
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries')
    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'old')
    await user.click(screen.getByRole('button', { name: '添加' }))
    const dialog = screen.getByRole('dialog', { name: '新增 MCP 服务器' })
    if (close === 'Escape') await user.keyboard('{Escape}')
    else if (close === 'icon') await user.click(within(dialog).getByRole('button', { name: '关闭对话框' }))
    else await user.pointer({ target: dialog.parentElement!, keys: '[MouseLeft]' })
    await user.click(screen.getByRole('button', { name: '新增 MCP 服务器' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'new draft')
    await act(async () => { resolveA(jsonResponse({})) })
    expect(screen.getByRole('textbox', { name: '名称' })).toHaveValue('new draft')
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['mcp-servers', 'project', 42] }))
  })

  it('leaves a B add draft open when an A request succeeds after real navigation', async () => {
    let resolveA: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveA = resolve })
    const { user, queryClient } = setupDetail(undefined, () => pending)
    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'old')
    await user.click(screen.getByRole('button', { name: '添加' }))
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    await user.click(screen.getByRole('button', { name: '新增 MCP 服务器' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'new')
    await act(async () => { resolveA(jsonResponse({})) })
    expect(screen.getByRole('textbox', { name: '名称' })).toHaveValue('new')
    await waitFor(() => expect(queryClient.getQueryState(['mcp-servers', 'project', 42])?.isInvalidated).toBe(true))
  })

  it.each(['close', 'switch'] as const)('reports a failed old add after %s without leaking its secret', async (transition) => {
    let rejectA: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { rejectA = resolve })
    const { user } = setupDetail(undefined, () => pending)
    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'old')
    await user.click(screen.getByRole('button', { name: '添加' }))
    if (transition === 'close') {
      await user.keyboard('{Escape}')
      await user.click(screen.getByRole('button', { name: '新增 MCP 服务器' }))
      await user.type(screen.getByRole('textbox', { name: '名称' }), 'new draft')
    } else {
      await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
      await screen.findByRole('heading', { name: 'group/project-43' })
    }
    await act(async () => { rejectA(jsonResponse({ error: { message: 'server leaked a-secret' } }, 500)) })
    expect(await screen.findByRole('alert')).toHaveTextContent(/新增 MCP 服务器.*失败/)
    expect(screen.queryByText(/a-secret/)).not.toBeInTheDocument()
    if (transition === 'close') expect(screen.getByRole('textbox', { name: '名称' })).toHaveValue('new draft')
  })

  it('shows an in-dialog add failure without echoing a server-supplied secret', async () => {
    const { user } = setupDetail(undefined, () => jsonResponse({ error: { message: 'leaked a-secret' } }, 500))
    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'server')
    await user.click(screen.getByRole('button', { name: '添加' }))
    expect(await within(screen.getByRole('dialog')).findByRole('alert')).toHaveTextContent('新增 MCP 服务器失败')
    expect(screen.queryByText(/a-secret/)).not.toBeInTheDocument()
  })

  it('explains a network failure without claiming the write was rejected', async () => {
    const { user } = setupDetail(undefined, () => Promise.reject(new Error('transport failure with a-secret')))
    await user.click(await screen.findByRole('button', { name: '新增 MCP 服务器' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'server')
    await user.click(screen.getByRole('button', { name: '添加' }))
    expect(await within(screen.getByRole('dialog')).findByRole('alert')).toHaveTextContent('无法连接服务器')
    expect(screen.getByRole('alert')).toHaveTextContent('结果可能不确定')
    expect(screen.queryByText(/a-secret/)).not.toBeInTheDocument()
  })

  it.each([
    ['import', 'success'], ['import', 'failure'], ['edit', 'success'], ['edit', 'failure'],
    ['directory', 'success'], ['directory', 'failure'], ['upload', 'success'], ['upload', 'failure'],
  ] as const)('isolates a reopened %s draft from an old %s', async (kind, outcome) => {
    let resolveOld: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveOld = resolve })
    const { user, queryClient } = setupDetail(undefined, () => pending)
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries')
    if (kind === 'directory' || kind === 'upload') await user.click(await screen.findByRole('tab', { name: /^Skill/ }))
    const trigger = kind === 'import' ? '粘贴 Claude Code 配置导入' : kind === 'edit' ? '编辑' : kind === 'directory' ? '登记服务器目录' : '上传 Skill（zip）'
    const title = kind === 'import' ? '导入 MCP 配置' : kind === 'edit' ? '编辑 MCP 服务器' : kind === 'directory' ? '登记 Skill 目录' : '上传 Skill'
    const submit = kind === 'import' ? '校验并导入' : kind === 'edit' ? '保存' : kind === 'directory' ? '登记' : '上传'
    const fillDraft = async (draft: string) => {
      if (kind === 'import') fireEvent.change(screen.getByRole('textbox', { name: 'mcpServers JSON' }), { target: { value: JSON.stringify({ mcpServers: { [draft]: {} } }) } })
      else if (kind === 'upload') await user.upload(screen.getByLabelText('Skill zip'), new File([draft], `${draft}.zip`, { type: 'application/zip' }))
      else {
        const input = screen.getByRole('textbox', { name: kind === 'edit' ? '名称' : '目录路径' })
        await user.clear(input)
        await user.type(input, draft)
      }
    }
    await user.click(await screen.findByRole('button', { name: trigger }))
    await fillDraft('old')
    await user.click(screen.getByRole('button', { name: submit }))
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: trigger }))
    await fillDraft('new')
    await act(async () => { resolveOld(outcome === 'success' ? jsonResponse({}) : jsonResponse({ error: { message: 'leaked old-secret' } }, 500)) })
    const dialog = screen.getByRole('dialog', { name: title })
    if (kind === 'import') expect((within(dialog).getByRole('textbox', { name: 'mcpServers JSON' }) as HTMLTextAreaElement).value).toContain('new')
    else if (kind === 'upload') expect((within(dialog).getByLabelText('Skill zip') as HTMLInputElement).files?.[0].name).toBe('new.zip')
    else expect(within(dialog).getByRole('textbox', { name: kind === 'edit' ? '名称' : '目录路径' })).toHaveValue('new')
    if (outcome === 'success') {
      const key = kind === 'directory' || kind === 'upload' ? 'skills' : 'mcp-servers'
      await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: [key, 'project', 42] }))
    } else {
      expect(await screen.findByRole('alert')).toHaveTextContent('失败')
      expect(screen.queryByText(/old-secret/)).not.toBeInTheDocument()
    }
  })
})
