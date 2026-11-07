import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const users = [
  { id: 7, username: 'alice', name: 'Alice', disabled: false, revision: 1, role: 'admin', role_source: 'granted', global_read: false, grant_count: 0 },
  { id: 8, username: 'bob', name: 'Bob', disabled: false, revision: 2, role: 'viewer', role_source: 'none', global_read: false, grant_count: 0 },
  { id: 9, username: 'carol', name: 'Carol', disabled: false, revision: 1, role: 'admin', role_source: 'config', global_read: false, grant_count: 0 },
]

function mock(writes: Array<{ url: string; method?: string; body?: unknown }>) {
  return installFetch((url, init) => {
    const method = (init?.method || 'GET').toUpperCase()
    const path = new URL(url, 'http://localhost').pathname
    if (method !== 'GET') writes.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
    if (path === '/api/v1/admin/users') return jsonResponse({ users, next_cursor: '' })
    if (path === '/api/v1/admin/users/8/grants') return jsonResponse({ grants: [], next_cursor: '' })
    if (path === '/api/v1/admin/projects/42/grants/8') return jsonResponse({ project_id: 42, user_id: 8, role: 'manage' })
    if (path.startsWith('/api/v1/admin/users/')) return jsonResponse(users[1])
    return jsonResponse({})
  })
}

async function openUser(user: ReturnType<typeof userEvent.setup>, name: string) {
  renderWithProviders(<SystemPage />)
  await user.click(screen.getByRole('tab', { name: '用户与账号' }))
  const row = (await screen.findByText(name)).closest('tr')!
  await user.click(within(row).getByRole('button', { name: '管理' }))
}

describe('user management drawer (#297)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('promotes a user and toggles global read', async () => {
    const writes: Array<{ url: string; method?: string; body?: unknown }> = []
    mock(writes)
    const user = userEvent.setup()
    await openUser(user, 'bob')
    await user.click(screen.getByRole('button', { name: '提升为管理员' }))
    await user.click(screen.getByRole('button', { name: '确认' }))
    await waitFor(() => expect(writes.some((write) => write.method === 'PATCH' && (write.body as { role?: string })?.role === 'admin')).toBe(true))
    await user.click(screen.getByRole('button', { name: '开启全局模式' }))
    await user.click(screen.getByRole('button', { name: '确认' }))
    await waitFor(() => expect(writes.some((write) => write.method === 'PATCH' && (write.body as { global_read?: boolean })?.global_read === true)).toBe(true))
  })

  it('disables self demotion and configured roles', async () => {
    mock([])
    const user = userEvent.setup()
    await openUser(user, 'alice')
    expect(screen.getByRole('button', { name: '降级为普通用户' })).toBeDisabled()
    await user.click(screen.getAllByRole('button', { name: /关闭/ }).at(-1)!)
    const row = (await screen.findByText('carol')).closest('tr')!
    await user.click(within(row).getByRole('button', { name: '管理' }))
    expect(screen.getByRole('button', { name: '降级为普通用户' })).toBeDisabled()
  })

  it('adds a user-scoped project grant', async () => {
    const writes: Array<{ url: string; method?: string; body?: unknown }> = []
    mock(writes)
    const user = userEvent.setup()
    await openUser(user, 'bob')
    expect(screen.getByLabelText('角色')).toHaveValue('manage')
    await user.type(screen.getByLabelText('项目 ID'), '42')
    await user.selectOptions(screen.getByLabelText('角色'), 'manage')
    await user.click(screen.getByRole('button', { name: '添加授权' }))
    await waitFor(() => expect(writes.some((write) => write.method === 'PUT' && write.url.includes('/admin/projects/42/grants/8'))).toBe(true))
    expect(writes.find((write) => write.method === 'PUT')?.body).toEqual({ role: 'manage' })
  })
})
