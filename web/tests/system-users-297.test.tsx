import { afterEach, describe, expect, it, vi } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const users = [
  { id: 7, username: 'alice', name: 'Alice', disabled: false, revision: 1, role: 'admin', role_source: 'granted', global_read: false, grant_count: 0 },
  { id: 8, username: 'bob', name: 'Bob', disabled: false, revision: 2, role: 'viewer', role_source: 'none', global_read: false, grant_count: 0 },
  { id: 9, username: 'carol', name: 'Carol', disabled: false, revision: 1, role: 'admin', role_source: 'config', global_read: false, grant_count: 0 },
]

function mock(
  writes: Array<{ url: string; method?: string; body?: unknown }>,
  grants: Array<{ project_id: number; user_id: number; role: string }> = [],
) {
  return installFetch((url, init) => {
    const method = (init?.method || 'GET').toUpperCase()
    const path = new URL(url, 'http://localhost').pathname
    if (method !== 'GET') writes.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
    if (path === '/api/v1/admin/users') return jsonResponse({ users, next_cursor: '' })
    if (path === '/api/v1/admin/users/8/grants') return jsonResponse({ grants, next_cursor: '' })
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

async function chooseOption(user: ReturnType<typeof userEvent.setup>, trigger: HTMLElement, name: string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name }))
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

  it('refreshes the current user after a user write', async () => {
    const state = { meReads: 0 }
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const path = new URL(url, 'http://localhost').pathname
      if (path === '/api/v1/me') {
        state.meReads += 1
        return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
      }
      if (path === '/api/v1/admin/users' && method === 'GET') return jsonResponse({ users, next_cursor: '' })
      if (path === '/api/v1/admin/users/8/grants') return jsonResponse({ grants: [], next_cursor: '' })
      if (path.startsWith('/api/v1/admin/users/')) return jsonResponse(users[1])
      return jsonResponse({})
    })
    const user = userEvent.setup()
    await openUser(user, 'bob')
    const before = state.meReads
    await user.click(screen.getByRole('button', { name: '提升为管理员' }))
    await user.click(screen.getByRole('button', { name: '确认' }))
    await waitFor(() => expect(state.meReads).toBeGreaterThan(before))
  })

  it('disables the disable action for config-sourced and self accounts with the reason', async () => {
    mock([])
    const user = userEvent.setup()
    renderWithProviders(<SystemPage />)
    await user.click(screen.getByRole('tab', { name: '用户与账号' }))

    const carol = (await screen.findByText('carol')).closest('tr')!
    expect(within(carol).getByRole('button', { name: '停用账号' })).toBeDisabled()
    expect(within(carol).getByText(/配置来源的管理员/)).toBeInTheDocument()

    const alice = (await screen.findByText('alice')).closest('tr')!
    expect(within(alice).getByRole('button', { name: '停用账号' })).toBeDisabled()
    expect(within(alice).getByText(/当前登录账号/)).toBeInTheDocument()

    const bob = (await screen.findByText('bob')).closest('tr')!
    expect(within(bob).getByRole('button', { name: '停用账号' })).toBeEnabled()
  })

  it('explains a protected_user rejection instead of a generic permission error', async () => {
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const path = new URL(url, 'http://localhost').pathname
      if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
      if (path === '/api/v1/admin/users' && method === 'GET') return jsonResponse({ users, next_cursor: '' })
      if (path === '/api/v1/admin/users/8' && method === 'PATCH') {
        return jsonResponse({ error: { code: 'protected_user', message: 'platform administrator cannot be changed' } }, 403)
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<SystemPage />)
    await user.click(screen.getByRole('tab', { name: '用户与账号' }))
    const bob = (await screen.findByText('bob')).closest('tr')!
    await user.click(within(bob).getByRole('button', { name: '停用账号' }))
    const dialog = await screen.findByRole('dialog', { name: '停用账号？' })
    await user.click(within(dialog).getByRole('button', { name: '停用' }))
    expect(await within(dialog).findByText(/受配置保护/)).toBeInTheDocument()
    expect(within(dialog).queryByText('没有访问权限')).not.toBeInTheDocument()
  })

  it('adds a user-scoped project grant', async () => {
    const writes: Array<{ url: string; method?: string; body?: unknown }> = []
    mock(writes)
    const user = userEvent.setup()
    await openUser(user, 'bob')
    expect(screen.getByLabelText('角色')).toHaveTextContent('manage')
    await user.type(screen.getByLabelText('项目 ID'), '42')
    await chooseOption(user, screen.getByLabelText('角色'), 'manage')
    await user.click(screen.getByRole('button', { name: '添加授权' }))
    await waitFor(() => expect(writes.some((write) => write.method === 'PUT' && write.url.includes('/admin/projects/42/grants/8'))).toBe(true))
    expect(writes.find((write) => write.method === 'PUT')?.body).toEqual({ role: 'manage' })
  })

  it('confirms before deleting a user project grant', async () => {
    const writes: Array<{ url: string; method?: string; body?: unknown }> = []
    mock(writes, [{ project_id: 42, user_id: 8, role: 'manage' }])
    const user = userEvent.setup()
    await openUser(user, 'bob')

    await user.click(await screen.findByRole('button', { name: '删除' }))
    const dialog = await screen.findByRole('dialog', { name: '删除项目授权？' })
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('dialog', { name: '删除项目授权？' })).not.toBeInTheDocument()
    expect(writes.some((write) => write.method === 'DELETE')).toBe(false)

    await user.click(screen.getByRole('button', { name: '删除' }))
    const confirm = await screen.findByRole('dialog', { name: '删除项目授权？' })
    await user.click(within(confirm).getByRole('button', { name: '删除' }))
    await waitFor(() => expect(writes.some(
      (write) => write.method === 'DELETE' && write.url.includes('/admin/projects/42/grants/8'),
    )).toBe(true))
  })

  it('keeps the grant confirmation keyboard reachable inside the drawer', async () => {
    mock([], [{ project_id: 42, user_id: 8, role: 'manage' }])
    const user = userEvent.setup()
    await openUser(user, 'bob')
    await user.click(await screen.findByRole('button', { name: '删除' }))
    const dialog = await screen.findByRole('dialog', { name: '删除项目授权？' })
    const confirm = within(dialog).getByRole('button', { name: '删除' })
    for (let index = 0; index < 6 && document.activeElement !== confirm; index += 1) {
      await user.tab()
    }
    expect(confirm).toHaveFocus()
  })

  it('stacks modal overlays above drawers', () => {
    const tokens = readFileSync(resolve(process.cwd(), 'src/styles/tokens.css'), 'utf8')
    const z = (name: string) => Number(tokens.match(new RegExp(`--${name}:\\s*(\\d+)`))?.[1])
    expect(z('z-modal')).toBeGreaterThan(z('z-drawer'))
    const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')
    expect(css).toMatch(/\.modalOverlay\s*\{[^}]*z-index:\s*var\(--z-modal\)/)
  })
})
