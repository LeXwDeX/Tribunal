import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

function apiTokensEmpty() {
  return jsonResponse({ tokens: [] })
}

describe('personal token expiry', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('accepts at most 365 days and rejects a longer expiry before sending', async () => {
    const user = userEvent.setup()
    const writes: string[] = []
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') {
        writes.push(url)
        return jsonResponse({ id: 't1', token: 'crc_once' })
      }
      if (url.startsWith('/api/v1/api-tokens')) return apiTokensEmpty()
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />, { me: viewerMe })
    await user.click(await screen.findByRole('button', { name: '创建 token' }))
    await user.type(screen.getByLabelText('名称'), 'ci')
    const days = screen.getByLabelText('有效天数')
    expect(days).toHaveAttribute('max', '365')
    await user.clear(days)
    await user.type(days, '366')
    const createButtons = screen.getAllByRole('button', { name: '创建', exact: true })
    await user.click(createButtons[createButtons.length - 1])
    expect(await screen.findByText('有效天数必须在 1 到 365 之间。')).toBeInTheDocument()
    expect(writes).toHaveLength(0)
  })
})

describe('admin user and grant management', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('pages users and confirms disable and credential revocation', async () => {
    const user = userEvent.setup()
    const writes: Array<{ url: string; method?: string; body?: unknown }> = []
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/admin/users' && method === 'GET') {
        const cursor = parsed.searchParams.get('cursor')
        if (cursor === 'u2') {
          return jsonResponse({ users: [{ id: 2, username: 'bob', name: 'Bob', disabled: true }], next_cursor: '' })
        }
        return jsonResponse({ users: [{ id: 1, username: 'alice', name: 'Alice', disabled: false }], next_cursor: 'u2' })
      }
      if (parsed.pathname === '/api/v1/admin/users/1' && method === 'PATCH') {
        writes.push({ url: parsed.pathname, method, body: JSON.parse(String(init?.body)) })
        return jsonResponse({ id: 1, disabled: true })
      }
      if (parsed.pathname === '/api/v1/admin/users/1/credentials' && method === 'DELETE') {
        writes.push({ url: parsed.pathname, method })
        return new Response(null, { status: 204 })
      }
      if (url.startsWith('/api/v1/api-tokens')) return apiTokensEmpty()
      return jsonResponse({})
    })

    renderWithProviders(<SystemPage />)
    await user.click(await screen.findByRole('tab', { name: '用户与账号' }))

    expect(await screen.findByText('alice')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '加载更多用户' }))
    expect(await screen.findByText('bob')).toBeInTheDocument()

    await user.click(screen.getAllByRole('button', { name: '停用账号' })[0])
    const disableDialog = await screen.findByRole('dialog', { name: '停用账号？' })
    await user.click(within(disableDialog).getByRole('button', { name: '停用' }))
    await waitFor(() => expect(writes).toContainEqual({
      url: '/api/v1/admin/users/1', method: 'PATCH', body: { disabled: true },
    }))

    await user.click(screen.getAllByRole('button', { name: '撤销全部凭据' })[0])
    const revokeDialog = await screen.findByRole('dialog', { name: '撤销全部凭据？' })
    await user.click(within(revokeDialog).getByRole('button', { name: '撤销' }))
    await waitFor(() => expect(writes).toContainEqual({
      url: '/api/v1/admin/users/1/credentials', method: 'DELETE',
    }))
  })

  it('picks projects from the admin list even without a grant and manages grants', async () => {
    const user = userEvent.setup()
    const writes: Array<{ url: string; method?: string; body?: unknown }> = []
    const grantReads: string[] = []
    let projectList = 0
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/admin/projects' && method === 'GET') {
        projectList += 1
        if (parsed.searchParams.get('cursor') === 'p2') {
          return jsonResponse({ projects: [{ id: 8, path_with_namespace: 'group/second' }], next_cursor: '' })
        }
        return jsonResponse({ projects: [{ id: 7, path_with_namespace: 'group/first' }], next_cursor: 'p2' })
      }
      if (parsed.pathname === '/api/v1/admin/projects/7/grants' && method === 'GET') {
        grantReads.push(parsed.pathname)
        return jsonResponse({
          grants: [{ project_id: 7, user_id: 2, role: 'read', username: 'bob' }],
          next_cursor: '',
        })
      }
      if (parsed.pathname === '/api/v1/admin/projects/7/grants/1' && method === 'PUT') {
        writes.push({ url: parsed.pathname, method, body: JSON.parse(String(init?.body)) })
        return jsonResponse({ project_id: 7, user_id: 1, role: 'manage' })
      }
      if (parsed.pathname === '/api/v1/admin/projects/7/grants/2' && method === 'DELETE') {
        writes.push({ url: parsed.pathname, method })
        return new Response(null, { status: 204 })
      }
      if (parsed.pathname === '/api/v1/admin/users' && method === 'GET') {
        return jsonResponse({
          users: [
            { id: 1, username: 'alice', name: 'Alice', disabled: false },
            { id: 2, username: 'bob', name: 'Bob', disabled: false },
          ],
          next_cursor: '',
        })
      }
      if (url.startsWith('/api/v1/api-tokens')) return apiTokensEmpty()
      return jsonResponse({})
    })

    renderWithProviders(<SystemPage />)
    await user.click(await screen.findByRole('tab', { name: '项目授权' }))

    const projectSelect = await screen.findByLabelText('选择项目')
    expect(await within(projectSelect).findByRole('option', { name: 'group/first' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '加载更多项目' }))
    expect(await within(projectSelect).findByRole('option', { name: 'group/second' })).toBeInTheDocument()

    await user.selectOptions(projectSelect, '7')
    await waitFor(() => expect(grantReads).toContain('/api/v1/admin/projects/7/grants'))
    expect(await screen.findByText('bob')).toBeInTheDocument()

    await user.selectOptions(screen.getByLabelText('选择用户'), '1')
    await user.selectOptions(screen.getByLabelText('权限角色'), 'manage')
    await user.click(screen.getByRole('button', { name: '授予权限' }))
    await waitFor(() => expect(writes).toContainEqual({
      url: '/api/v1/admin/projects/7/grants/1', method: 'PUT', body: { role: 'manage' },
    }))

    await user.click(screen.getByRole('button', { name: '撤销' }))
    const deleteDialog = await screen.findByRole('dialog', { name: '撤销项目授权？' })
    await user.click(within(deleteDialog).getByRole('button', { name: '撤销授权' }))
    await waitFor(() => expect(writes).toContainEqual({
      url: '/api/v1/admin/projects/7/grants/2', method: 'DELETE',
    }))
    expect(projectList).toBeGreaterThanOrEqual(2)
  })
})
