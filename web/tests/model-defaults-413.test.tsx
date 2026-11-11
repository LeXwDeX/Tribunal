import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const userModel = {
  id: 'user:inherits', display_name: 'inherits', source: 'user', read_only: false,
  protocol: 'anthropic', base_url: 'https://example.test/v1', model: 'claude-x',
  params: { max_tokens: 0, max_concurrency: 1, strict_role_alternation: false },
  effective_params: { max_tokens: 8192, temperature: 0.3, reasoning: { effort: 'high' }, max_concurrency: 1, strict_role_alternation: true },
  param_sources: {
    max_tokens: 'default', temperature: 'default', reasoning: 'default', max_concurrency: 'model', strict_role_alternation: 'default',
  },
  has_api_key: true, available: true, revision: 2, referenced_by: [], vote_identity: 'example.test/claude-x',
}

const configModel = {
  id: 'config:base', display_name: 'base', source: 'config', read_only: true,
  protocol: 'openai-chat', base_url: 'https://example.test/v1', model: 'm',
  params: { max_tokens: 1000 },
  effective_params: { max_tokens: 1000 },
  param_sources: {},
  has_api_key: true, available: true, revision: 0, referenced_by: [], vote_identity: 'example.test/m',
}

const storedDefaults = {
  params: {
    max_tokens: 8192, context_window: 128000, temperature: 0.3,
    reasoning: { effort: 'high' }, strict_role_alternation: true,
  },
  revision: 3,
  updated_by: 1,
  updated_at: '2026-11-11T10:00:00Z',
}

function routes(overrides: {
  defaults?: unknown
  put?: Response | ((init?: RequestInit) => Response)
  remove?: Response
} = {}) {
  return (url: string, init?: RequestInit) => {
    if (url === '/api/v1/model-defaults' && init?.method === 'PUT') {
      if (typeof overrides.put === 'function') return overrides.put(init)
      if (overrides.put) return overrides.put
      const body = JSON.parse(String(init.body)) as { params: unknown; revision: number }
      return jsonResponse({ params: body.params, revision: body.revision + 1 })
    }
    if (url.startsWith('/api/v1/model-defaults') && init?.method === 'DELETE') {
      return overrides.remove ?? jsonResponse({ params: {}, revision: 4 })
    }
    if (url.startsWith('/api/v1/model-defaults')) return jsonResponse(overrides.defaults ?? storedDefaults)
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models: [userModel, configModel] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
    if (url.startsWith('/api/v1/model-presets')) return jsonResponse({ presets: [] })
    return jsonResponse({})
  }
}

async function openPage() {
  const user = userEvent.setup()
  renderWithProviders(<ModelsPage />, { route: '/models' })
  await screen.findByLabelText('max_tokens')
  return user
}

async function chooseOption(user: ReturnType<typeof userEvent.setup>, trigger: HTMLElement, name: string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name }))
}

describe('platform model defaults (#413)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the saved defaults and the inheritance source on model cards', async () => {
    installFetch(routes())
    await openPage()
    expect(await screen.findByLabelText('max_tokens')).toHaveValue(8192)
    expect(screen.getByLabelText('context_window')).toHaveValue(128000)
    expect(screen.getByLabelText('temperature')).toHaveValue(0.3)
    expect(screen.getByText('revision 3')).toBeInTheDocument()
    expect(screen.getByText(/只对 user: 模型生效/)).toBeInTheDocument()
    expect(screen.getByText(/config: 模型由配置文件负责/)).toBeInTheDocument()
    // The user card lists the inherited values; the config card never does.
    const inherited = screen.getByRole('group', { name: 'inherits 全局默认参数' })
    expect(within(inherited).getByText('max_tokens')).toBeInTheDocument()
    expect(within(inherited).getByText('8192')).toBeInTheDocument()
    expect(within(inherited).getByText('true')).toBeInTheDocument()
    expect(screen.queryByRole('group', { name: 'base 全局默认参数' })).not.toBeInTheDocument()
    // Explicit zero/false values overridden by the defaults are not shown as
    // active settings next to the inherited effective values.
    expect(screen.queryByText('0（模型默认，不发送）')).not.toBeInTheDocument()
    expect(screen.queryByText('false（关闭）')).not.toBeInTheDocument()
  })

  it('saves a full replacement with the current revision', async () => {
    const fetchMock = installFetch(routes())
    const user = await openPage()
    const tokens = screen.getByLabelText('max_tokens')
    await user.clear(tokens)
    await user.type(tokens, '4096')
    await user.click(screen.getByRole('button', { name: '保存全局默认' }))
    await waitFor(() => expect(screen.getByText(/已保存（revision 4）/)).toBeInTheDocument())
    const call = fetchMock.mock.calls.find(([url, init]) => url === '/api/v1/model-defaults' && init?.method === 'PUT')
    const body = JSON.parse(String(call?.[1]?.body)) as { params: Record<string, unknown>; revision: number }
    expect(body.revision).toBe(3)
    expect(body.params).toMatchObject({
      max_tokens: 4096, context_window: 128000, temperature: 0.3,
      reasoning: { effort: 'high' }, strict_role_alternation: true,
    })
    expect(body.params.price).toBeUndefined()
    expect(body.params.max_concurrency).toBeUndefined()
  })

  it('resets the defaults after confirmation', async () => {
    const fetchMock = installFetch(routes())
    const user = await openPage()
    await user.click(screen.getByRole('button', { name: '重置' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('重置全局默认参数')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: '重置' }))
    await waitFor(() => expect(screen.getByText(/已重置全部默认值（revision 4）/)).toBeInTheDocument())
    const call = fetchMock.mock.calls.find(([url, init]) => String(url).startsWith('/api/v1/model-defaults') && init?.method === 'DELETE')
    expect(String(call?.[0])).toBe('/api/v1/model-defaults?revision=3')
  })

  it('reports a stale revision and offers a reload', async () => {
    const fetchMock = installFetch(routes({
      put: jsonResponse({ error: { code: 'stale_revision', message: 'revision does not match', request_id: 'r1' } }, 409),
    }))
    const user = await openPage()
    await user.click(screen.getByRole('button', { name: '保存全局默认' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('已被他人修改，请重新加载后再保存。')
    await user.click(screen.getByRole('button', { name: '重新加载' }))
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url, init]) => String(url).startsWith('/api/v1/model-defaults') && init?.method === 'GET').length).toBeGreaterThan(1))
    await waitFor(() => expect(screen.queryByText('已被他人修改，请重新加载后再保存。')).not.toBeInTheDocument())
  })

  it('offers a reload when a reset hits a stale revision', async () => {
    const fetchMock = installFetch(routes({
      remove: jsonResponse({ error: { code: 'stale_revision', message: 'revision does not match', request_id: 'r1' } }, 409),
    }))
    const user = await openPage()
    await user.click(screen.getByRole('button', { name: '重置' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: '重置' }))
    expect(await screen.findByText('已被他人修改，请重新加载后再重置。')).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '重新加载' }))
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url, init]) => String(url).startsWith('/api/v1/model-defaults') && init?.method === 'GET').length).toBeGreaterThan(1))
  })

  it('shows boolean field errors below the matching checkbox', async () => {
    installFetch(routes({
      put: jsonResponse({ error: { code: 'validation_failed', message: 'invalid model defaults', request_id: 'r1', details: [{ field: 'params.strict_role_alternation', message: 'strict_role_alternation has an invalid value' }] } }, 400),
    }))
    const user = await openPage()
    await user.click(screen.getByRole('button', { name: '保存全局默认' }))
    expect(await screen.findByText('strict_role_alternation has an invalid value')).toBeInTheDocument()
  })

  it('keeps an unsaved draft while a drawer is open', async () => {
    installFetch(routes())
    const user = await openPage()
    const tokens = screen.getByLabelText('max_tokens')
    await user.clear(tokens)
    await user.type(tokens, '4096')
    await user.click(screen.getByRole('button', { name: '新增模型' }))
    expect(screen.queryByRole('region', { name: '全局默认参数' })).not.toBeInTheDocument()
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(await screen.findByLabelText('max_tokens')).toHaveValue(4096)
  })

  it('hides the panel when the defaults endpoint is forbidden', async () => {
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/model-defaults')) {
        return jsonResponse({ error: { code: 'forbidden', message: 'forbidden', request_id: 'r1' } }, 403)
      }
      return routes()(url, init)
    })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await screen.findByText('inherits')
    expect(screen.queryByRole('region', { name: '全局默认参数' })).not.toBeInTheDocument()
  })

  it('disables reset when the saved defaults contain no values', async () => {
    installFetch(routes({ defaults: { params: {}, revision: 2 } }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    expect(await screen.findByRole('button', { name: '重置' })).toBeDisabled()
  })

  it('renders the panel at 390 and 1440 widths in both themes', async () => {
    installFetch(routes())
    await openPage()
    for (const width of [390, 1440]) {
      Object.defineProperty(window, 'innerWidth', { configurable: true, value: width })
      for (const theme of ['light', 'dark']) {
        document.documentElement.dataset.theme = theme
        fireEvent(window, new Event('resize'))
        expect(screen.getByLabelText('max_tokens')).toBeInTheDocument()
        expect(screen.getByRole('button', { name: '保存全局默认' })).toBeInTheDocument()
      }
    }
    document.documentElement.dataset.theme = 'light'
  })

  it('maps field-level validation errors below the control', async () => {
    installFetch(routes({
      put: jsonResponse({ error: { code: 'validation_failed', message: 'invalid model defaults', request_id: 'r1', details: [{ field: 'params.max_tokens', message: 'max_tokens must not be negative' }] } }, 400),
    }))
    const user = await openPage()
    const tokens = screen.getByLabelText('max_tokens')
    await user.clear(tokens)
    await user.type(tokens, '10')
    await user.click(screen.getByRole('button', { name: '保存全局默认' }))
    expect(await screen.findByText('max_tokens must not be negative')).toBeInTheDocument()
  })

  it('lets an anthropic model inherit the global max_tokens from the editor', async () => {
    const fetchMock = installFetch(routes())
    const user = userEvent.setup()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await screen.findByLabelText('max_tokens')
    await user.click(screen.getByRole('button', { name: '新增模型' }))
    await user.type(screen.getByLabelText('显示名'), 'claude')
    await user.type(screen.getByLabelText('base_url'), 'https://example.test/v1')
    await user.type(screen.getByLabelText('上游模型'), 'claude-x')
    await chooseOption(user, screen.getByLabelText('协议'), 'anthropic')
    await user.click(screen.getByRole('button', { name: '创建' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/models' && init?.method === 'POST')).toBe(true))
    const call = fetchMock.mock.calls.find(([url, init]) => url === '/api/v1/models' && init?.method === 'POST')
    const body = JSON.parse(String(call?.[1]?.body)) as { params: Record<string, unknown> }
    expect(body.params.max_tokens).toBeUndefined()
    expect(screen.queryByText('anthropic 协议要求 max_tokens 大于 0，不能留空。')).not.toBeInTheDocument()
  })

  it('does not render the defaults panel for viewers', async () => {
    installFetch(routes())
    renderWithProviders(<ModelsPage />, { route: '/models', me: { id: 2, username: 'bob', name: 'Bob', role: 'viewer', csrf_token: 'csrf-viewer' } })
    await screen.findByText('inherits')
    expect(screen.queryByRole('region', { name: '全局默认参数' })).not.toBeInTheDocument()
  })
})
