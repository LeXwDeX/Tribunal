import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes, useNavigate } from 'react-router-dom'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { setCsrfToken } from '../src/api/client'
import { mcpServer, project } from './fixtures'
import { createTestQueryClient, installFetch, jsonResponse, renderWithProviders } from './test-utils'

function setupDetail(route = '/projects/42/extensions', post?: (url: URL, init: RequestInit) => Response | Promise<Response>) {
  const queryClient = createTestQueryClient()
  const servers = (id: number) => [mcpServer({ id: `${id}-server`, scope: 'project', project_id: id, name: `${id}-server` })]
  for (const id of [42, 43]) {
    queryClient.setQueryData(['project', id], project({ id, path_with_namespace: `group/project-${id}` }))
    queryClient.setQueryData(['mcp-servers', 'project', id], { pages: [{ servers: servers(id) }], pageParams: [undefined] })
    queryClient.setQueryData(['skills', 'project', id], { pages: [{ skills: [] }], pageParams: [undefined] })
    queryClient.setQueryData(['block-bindings', 'project', id], { pages: [{ bindings: [] }], pageParams: [undefined] })
  }
  installFetch((raw, init) => {
    const url = new URL(raw, 'http://localhost')
    if ((init?.method === 'POST' || init?.method === 'PUT' || init?.method === 'DELETE') && post) return post(url, init)
    if (url.pathname === '/api/v1/mcp-servers') return jsonResponse({ servers: servers(Number(url.searchParams.get('project_id'))) })
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
      const url = new URL(raw, 'http://localhost')
      const path = url.pathname
      const id = Number(url.searchParams.get('project_id'))
      return path === '/api/v1/mcp-servers' ? jsonResponse({ servers: [mcpServer({ id: `${id}-server`, name: `${id}-server`, scope: 'project', project_id: id })] })
        : path === '/api/v1/skills' ? jsonResponse({ skills: [] }) : jsonResponse({ bindings: [] })
    })
    function Harness() {
      const [id, setId] = useState(42)
      return <><button onClick={() => setId(43)}>切换项目</button><ExtensionsManager scope="project" projectId={id} canManage tab="mcp" onTabChange={() => {}} /></>
    }
    const user = userEvent.setup()
    renderWithProviders(<Harness />)
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.clear(screen.getByRole('textbox', { name: '名称' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'a-secret')
    await user.click(screen.getByRole('button', { name: '切换项目' }))
    expect(screen.queryByRole('dialog', { name: '编辑 MCP 服务器' })).not.toBeInTheDocument()
    await screen.findByText('43-server')
    await user.click(screen.getByRole('button', { name: '编辑' }))
    expect(screen.getByRole('textbox', { name: '名称' })).toHaveValue('43-server')
  })

  it('discards an edit draft on real navigation and never sends project A secrets to B', async () => {
    const writes: Array<{ path: string; body: unknown }> = []
    const { user } = setupDetail(undefined, (url, init) => {
      writes.push({ path: url.pathname, body: JSON.parse(String(init.body)) })
      return jsonResponse({})
    })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    const input = screen.getByRole('textbox', { name: '名称' })
    await user.clear(input)
    await user.type(input, 'a-secret')
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    expect(screen.queryByRole('dialog', { name: '编辑 MCP 服务器' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '编辑' }))
    expect(screen.getByRole('textbox', { name: '名称' })).toHaveValue('43-server')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(writes).toEqual([{ path: '/api/v1/mcp-servers/43-server', body: { name: '43-server', enabled: true } }]))
  })

  it.each(['edit', 'delete'] as const)('discards the %s selection on cached navigation', async (kind) => {
    const { user } = setupDetail()
    if (kind === 'edit') {
      await user.click(await screen.findByRole('button', { name: '编辑' }))
    } else {
      await user.click(await screen.findByRole('button', { name: '删除' }))
    }
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
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

  it.each(['Escape', 'icon', 'backdrop'] as const)('keeps a reopened edit draft when the old request succeeds after %s', async (close) => {
    let resolveA: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveA = resolve })
    const { user, queryClient } = setupDetail(undefined, () => pending)
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries')
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.clear(screen.getByRole('textbox', { name: '名称' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'old')
    await user.click(screen.getByRole('button', { name: '保存' }))
    const dialog = screen.getByRole('dialog', { name: '编辑 MCP 服务器' })
    if (close === 'Escape') await user.keyboard('{Escape}')
    else if (close === 'icon') await user.click(within(dialog).getByRole('button', { name: '关闭对话框' }))
    else await user.pointer({ target: dialog.parentElement!, keys: '[MouseLeft]' })
    await user.click(screen.getByRole('button', { name: '编辑' }))
    await user.clear(screen.getByRole('textbox', { name: '名称' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'new draft')
    await act(async () => { resolveA(jsonResponse({})) })
    expect(screen.getByRole('textbox', { name: '名称' })).toHaveValue('new draft')
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['mcp-servers', 'project', 42] }))
  })

  it('leaves a B edit draft open when an A request succeeds after real navigation', async () => {
    let resolveA: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveA = resolve })
    const { user, queryClient } = setupDetail(undefined, () => pending)
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.clear(screen.getByRole('textbox', { name: '名称' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'old')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
    await screen.findByRole('heading', { name: 'group/project-43' })
    await user.click(screen.getByRole('button', { name: '编辑' }))
    await user.clear(screen.getByRole('textbox', { name: '名称' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'new')
    await act(async () => { resolveA(jsonResponse({})) })
    expect(screen.getByRole('textbox', { name: '名称' })).toHaveValue('new')
    await waitFor(() => expect(queryClient.getQueryState(['mcp-servers', 'project', 42])?.isInvalidated).toBe(true))
  })

  it.each(['close', 'switch'] as const)('reports a failed old edit after %s without leaking its secret', async (transition) => {
    let rejectA: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { rejectA = resolve })
    const { user } = setupDetail(undefined, () => pending)
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.clear(screen.getByRole('textbox', { name: '名称' }))
    await user.type(screen.getByRole('textbox', { name: '名称' }), 'old')
    await user.click(screen.getByRole('button', { name: '保存' }))
    if (transition === 'close') {
      await user.keyboard('{Escape}')
      await user.click(screen.getByRole('button', { name: '编辑' }))
      await user.clear(screen.getByRole('textbox', { name: '名称' }))
      await user.type(screen.getByRole('textbox', { name: '名称' }), 'new draft')
    } else {
      await user.click(screen.getByRole('button', { name: '切换到项目 B' }))
      await screen.findByRole('heading', { name: 'group/project-43' })
    }
    await act(async () => { rejectA(jsonResponse({ error: { message: 'server leaked a-secret' } }, 500)) })
    expect(await screen.findByRole('alert')).toHaveTextContent(/编辑 MCP 服务器.*失败/)
    expect(screen.queryByText(/a-secret/)).not.toBeInTheDocument()
    if (transition === 'close') expect(screen.getByRole('textbox', { name: '名称' })).toHaveValue('new draft')
  })

  it('shows an in-dialog edit failure without echoing a server-supplied secret', async () => {
    const { user } = setupDetail(undefined, () => jsonResponse({ error: { message: 'leaked a-secret' } }, 500))
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(await within(screen.getByRole('dialog')).findByRole('alert')).toHaveTextContent('编辑 MCP 服务器失败')
    expect(screen.queryByText(/a-secret/)).not.toBeInTheDocument()
  })

  it('explains a network failure without claiming the write was rejected', async () => {
    const { user } = setupDetail(undefined, () => Promise.reject(new Error('transport failure with a-secret')))
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(await within(screen.getByRole('dialog')).findByRole('alert')).toHaveTextContent('无法连接服务器')
    expect(screen.getByRole('alert')).toHaveTextContent('结果可能不确定')
    expect(screen.queryByText(/a-secret/)).not.toBeInTheDocument()
  })

  it.each(['success', 'failure'] as const)('isolates a reopened edit draft from an old %s', async (outcome) => {
    let resolveOld: (response: Response) => void = () => {}
    const pending = new Promise<Response>((resolve) => { resolveOld = resolve })
    const { user, queryClient } = setupDetail(undefined, () => pending)
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries')
    const fillDraft = async (draft: string) => {
      const input = screen.getByRole('textbox', { name: '名称' })
      await user.clear(input)
      await user.type(input, draft)
    }
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await fillDraft('old')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: '编辑' }))
    await fillDraft('new')
    await act(async () => { resolveOld(outcome === 'success' ? jsonResponse({}) : jsonResponse({ error: { message: 'leaked old-secret' } }, 500)) })
    const dialog = screen.getByRole('dialog', { name: '编辑 MCP 服务器' })
    expect(within(dialog).getByRole('textbox', { name: '名称' })).toHaveValue('new')
    if (outcome === 'success') {
      await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ['mcp-servers', 'project', 42] }))
    } else {
      expect(await screen.findByRole('alert')).toHaveTextContent('失败')
      expect(screen.queryByText(/old-secret/)).not.toBeInTheDocument()
    }
  })
})
