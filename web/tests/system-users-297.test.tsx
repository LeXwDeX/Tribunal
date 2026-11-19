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
  projects: Array<{ id: number; path_with_namespace: string }> = [],
) {
  return installFetch((url, init) => {
    const method = (init?.method || 'GET').toUpperCase()
    const path = new URL(url, 'http://localhost').pathname
    if (method !== 'GET') writes.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
    if (path === '/api/v1/admin/users') return jsonResponse({ users, next_cursor: '' })
    if (path === '/api/v1/admin/users/8/grants') return jsonResponse({ grants, next_cursor: '' })
    if (path === '/api/v1/admin/projects') return jsonResponse({ projects, next_cursor: '' })
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

  it('reuses the refetched revision after a successful role change (#539)', async () => {
    const writes: Array<{ url: string; method?: string; body?: Record<string, unknown> }> = []
    let revision = 2
    let role = 'viewer'
    let globalRead = false
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const path = new URL(url, 'http://localhost').pathname
      if (method !== 'GET') writes.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
      if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
      if (path === '/api/v1/admin/users' && method === 'GET') {
        return jsonResponse({
          users: [users[0], { ...users[1], revision, role, global_read: globalRead }, users[2]],
          next_cursor: '',
        })
      }
      if (path === '/api/v1/admin/users/8/grants') return jsonResponse({ grants: [], next_cursor: '' })
      if (path === '/api/v1/admin/users/8' && method === 'PATCH') {
        const body = JSON.parse(String(init?.body)) as { revision?: number; role?: string; global_read?: boolean }
        if (body.revision !== revision) {
          return jsonResponse({ error: { code: 'stale_revision', message: 'user revision is stale' } }, 409)
        }
        revision += 1
        if (body.role) role = body.role
        if (typeof body.global_read === 'boolean') globalRead = body.global_read
        return jsonResponse({ id: 8, role, disabled: false, global_read: globalRead })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    await openUser(user, 'bob')
    await user.click(screen.getByRole('button', { name: '提升为管理员' }))
    await user.click(screen.getByRole('button', { name: '确认' }))
    await waitFor(() => expect(writes.some((write) => write.method === 'PATCH' && write.body?.role === 'admin')).toBe(true))
    // The drawer renders from the refetched list, so the new role is visible
    // without closing and reopening it.
    expect(await screen.findByRole('button', { name: '降级为普通用户' })).toBeEnabled()
    await user.click(screen.getByRole('button', { name: '开启全局模式' }))
    await user.click(screen.getByRole('button', { name: '确认' }))
    await waitFor(() => expect(writes.some((write) => write.method === 'PATCH' && write.body?.global_read === true)).toBe(true))
    const globalWrite = writes.find((write) => write.method === 'PATCH' && write.body?.global_read === true)
    expect(globalWrite?.body).toMatchObject({ revision: 3 })
    expect(screen.queryByText(/用户信息已被其他管理员更新|stale/)).not.toBeInTheDocument()
  })

  it('shows a read-only role as a badge with the reason instead of a disabled button', async () => {
    mock([])
    const user = userEvent.setup()
    await openUser(user, 'alice')
    let drawer = screen.getByRole('dialog', { name: 'Alice (@alice)' })
    let role = within(drawer).getByRole('region', { name: '平台角色' })
    expect(within(role).getByText('管理员')).toBeInTheDocument()
    expect(within(role).getByText('当前登录账号，不能降级自己。')).toBeInTheDocument()
    expect(within(role).queryByRole('button')).not.toBeInTheDocument()
    await user.click(within(drawer).getByRole('button', { name: '关闭抽屉' }))

    const row = (await screen.findByText('carol')).closest('tr')!
    await user.click(within(row).getByRole('button', { name: '管理' }))
    drawer = screen.getByRole('dialog', { name: 'Carol (@carol)' })
    role = within(drawer).getByRole('region', { name: '平台角色' })
    expect(within(role).getByText('管理员')).toBeInTheDocument()
    expect(within(role).getByText('来源：配置文件')).toBeInTheDocument()
    expect(within(role).getByText('由配置文件提供，修改需改配置并重启。')).toBeInTheDocument()
    expect(within(role).queryByRole('button')).not.toBeInTheDocument()
  })

  it('shows full project access instead of grants for an administrator', async () => {
    const writes: Array<{ url: string; method?: string; body?: unknown }> = []
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const path = new URL(url, 'http://localhost').pathname
      if (method !== 'GET') writes.push({ url, method })
      if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
      if (path === '/api/v1/admin/users') return jsonResponse({ users: [users[0], users[1], { ...users[2], grant_count: 3 }], next_cursor: '' })
      if (path.endsWith('/grants')) throw new Error('administrator grants must not be fetched')
      return jsonResponse({})
    })
    const user = userEvent.setup()
    await openUser(user, 'carol')
    const drawer = screen.getByRole('dialog', { name: 'Carol (@carol)' })
    const access = within(drawer).getByRole('region', { name: '项目访问' })
    expect(within(access).getByText('全部项目')).toBeInTheDocument()
    expect(within(access).getByText(/无需单独授权/)).toBeInTheDocument()
    expect(within(access).getByText(/保留的 3 条项目授权不生效，降级为普通用户后恢复生效/)).toBeInTheDocument()
    expect(within(drawer).queryByRole('region', { name: '项目授权' })).not.toBeInTheDocument()
    expect(within(drawer).queryByLabelText('项目 ID')).not.toBeInTheDocument()
    expect(within(drawer).queryByRole('button', { name: '添加授权' })).not.toBeInTheDocument()
  })

  it('shows an add-grant failure next to the form and clears it on edit', async () => {
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const path = new URL(url, 'http://localhost').pathname
      if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
      if (path === '/api/v1/admin/users') return jsonResponse({ users, next_cursor: '' })
      if (path === '/api/v1/admin/users/8/grants') return jsonResponse({ grants: [], next_cursor: '' })
      if (method === 'PUT') return jsonResponse({ error: { code: 'not_found', message: 'not found' } }, 404)
      return jsonResponse({})
    })
    const user = userEvent.setup()
    await openUser(user, 'bob')
    await user.type(screen.getByLabelText('项目 ID'), '404{Enter}')
    expect(await screen.findByText('添加授权失败')).toBeInTheDocument()
    await user.type(screen.getByLabelText('项目 ID'), '1')
    expect(screen.queryByText('添加授权失败')).not.toBeInTheDocument()
  })

  it('keeps the delete confirmation open with the error when deleting fails', async () => {
    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const path = new URL(url, 'http://localhost').pathname
      if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
      if (path === '/api/v1/admin/users') return jsonResponse({ users, next_cursor: '' })
      if (path === '/api/v1/admin/users/8/grants') return jsonResponse({ grants: [{ project_id: 42, user_id: 8, role: 'read' }], next_cursor: '' })
      if (method === 'DELETE') return jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500)
      return jsonResponse({})
    })
    const user = userEvent.setup()
    await openUser(user, 'bob')
    await user.click(await screen.findByRole('button', { name: '删除 项目 #42 的授权' }))
    const dialog = await screen.findByRole('dialog', { name: '删除项目授权？' })
    await user.click(within(dialog).getByRole('button', { name: '删除' }))
    expect(await within(dialog).findByRole('alert')).toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: '删除项目授权？' })).toBeInTheDocument()
  })

  it('does not carry form state from one user to another', async () => {
    mock([])
    const user = userEvent.setup()
    await openUser(user, 'bob')
    await user.type(screen.getByLabelText('项目 ID'), '42')
    await user.click(screen.getByRole('button', { name: '关闭抽屉' }))
    const row = (await screen.findByText('bob')).closest('tr')!
    await user.click(within(row).getByRole('button', { name: '管理' }))
    expect(screen.getByLabelText('项目 ID')).toHaveValue('')
  })

  it('fits the user list at desktop widths with a stacked user cell and short actions', async () => {
    mock([])
    const user = userEvent.setup()
    renderWithProviders(<SystemPage />)
    await user.click(screen.getByRole('tab', { name: '用户与账号' }))
    const table = await screen.findByRole('table', { name: '用户与账号' })
    expect(within(table).getAllByRole('columnheader').map((cell) => cell.textContent)).toEqual([
      '用户', '角色', '全局模式', '项目授权数', '状态 / 最近登录', '操作',
    ])
    const bob = within(table).getByText('bob').closest('tr')!
    expect(within(bob).getByText('Bob')).toBeInTheDocument()
    expect(within(bob).getByRole('button', { name: '停用账号' })).toHaveTextContent(/^停用$/)
    expect(within(bob).getByRole('button', { name: '撤销凭据' })).toBeInTheDocument()
  })

  it('shows the current global mode state', async () => {
    mock([])
    const user = userEvent.setup()
    await openUser(user, 'bob')
    const global = within(screen.getByRole('dialog', { name: 'Bob (@bob)' })).getByRole('region', { name: '全局模式' })
    expect(within(global).getByText('未开启')).toBeInTheDocument()
    expect(within(global).getByRole('button', { name: '开启全局模式' })).toBeEnabled()
  })

  it('lists grants in a table with project names when the project list has them', async () => {
    mock([], [
      { project_id: 42, user_id: 8, role: 'manage' },
      { project_id: 77, user_id: 8, role: 'read' },
    ], [{ id: 42, path_with_namespace: 'group/app' }])
    const user = userEvent.setup()
    await openUser(user, 'bob')
    const table = await screen.findByRole('table', { name: '项目授权列表' })
    const rows = within(table).getAllByRole('row')
    expect(within(rows[0]).getAllByRole('columnheader').map((cell) => cell.textContent)).toEqual(['项目', '角色', '操作'])
    expect(await within(rows[1]).findByText('group/app')).toBeInTheDocument()
    expect(within(rows[1]).getByText('#42')).toBeInTheDocument()
    expect(within(rows[1]).getByText('manage')).toBeInTheDocument()
    expect(within(rows[1]).getByRole('button', { name: '删除 group/app 的授权' })).toBeInTheDocument()
    expect(within(rows[2]).getByText('项目 #77')).toBeInTheDocument()
    expect(within(rows[2]).getByText('read')).toBeInTheDocument()
    expect(within(rows[2]).getByRole('button', { name: '删除 项目 #77 的授权' })).toBeInTheDocument()
  })

  it('shows an empty state when the user has no grants', async () => {
    mock([])
    const user = userEvent.setup()
    await openUser(user, 'bob')
    expect(await screen.findByText('没有项目授权')).toBeInTheDocument()
    expect(screen.queryByRole('table', { name: '项目授权列表' })).not.toBeInTheDocument()
  })

  it('shows a retryable error when grants fail to load', async () => {
    let fail = true
    installFetch((url) => {
      const path = new URL(url, 'http://localhost').pathname
      if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
      if (path === '/api/v1/admin/users') return jsonResponse({ users, next_cursor: '' })
      if (path === '/api/v1/admin/users/8/grants') {
        if (fail) return jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500)
        return jsonResponse({ grants: [{ project_id: 42, user_id: 8, role: 'read' }], next_cursor: '' })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    await openUser(user, 'bob')
    expect(await screen.findByText('授权列表加载失败')).toBeInTheDocument()
    fail = false
    await user.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByText('项目 #42')).toBeInTheDocument()
  })

  it('keeps the add button disabled until a project ID is entered', async () => {
    mock([])
    const user = userEvent.setup()
    await openUser(user, 'bob')
    const add = screen.getByRole('button', { name: '添加授权' })
    expect(add).toBeDisabled()
    await user.type(screen.getByLabelText('项目 ID'), '42')
    expect(add).toBeEnabled()
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
    const carolDisable = within(carol).getByRole('button', { name: '停用账号' })
    // aria-disabled keeps the control focusable so keyboard users can open the reason.
    expect(carolDisable).toHaveAttribute('aria-disabled', 'true')
    expect(carolDisable).not.toBeDisabled()
    carolDisable.focus()
    expect(carolDisable).toHaveFocus()
    await user.click(carolDisable)
    expect(screen.queryByRole('dialog', { name: '停用账号？' })).not.toBeInTheDocument()
    expect(carolDisable).toHaveAccessibleDescription(/配置来源的管理员/)
    // The reason is not repeated as row text.
    expect(within(carol).queryByText(/配置来源的管理员/)).not.toBeInTheDocument()

    const alice = (await screen.findByText('alice')).closest('tr')!
    const aliceDisable = within(alice).getByRole('button', { name: '停用账号' })
    expect(aliceDisable).toHaveAttribute('aria-disabled', 'true')
    expect(aliceDisable).toHaveAccessibleDescription(/当前登录账号/)

    const bob = (await screen.findByText('bob')).closest('tr')!
    expect(within(bob).getByRole('button', { name: '停用账号' })).not.toHaveAttribute('aria-disabled')
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

    await user.click(await screen.findByRole('button', { name: '删除 项目 #42 的授权' }))
    const dialog = await screen.findByRole('dialog', { name: '删除项目授权？' })
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('dialog', { name: '删除项目授权？' })).not.toBeInTheDocument()
    expect(writes.some((write) => write.method === 'DELETE')).toBe(false)

    await user.click(screen.getByRole('button', { name: '删除 项目 #42 的授权' }))
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
    await user.click(await screen.findByRole('button', { name: '删除 项目 #42 的授权' }))
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
