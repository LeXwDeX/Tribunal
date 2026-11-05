import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const base = { protocol: 'openai-chat', base_url: 'https://example.com', params: {}, has_api_key: true, revision: 1, referenced_by: [] }
const models = [
  { ...base, id: 'config:base', display_name: 'base', source: 'config', read_only: true, model: 'm', available: true, vote_identity: 'example.com/m' },
  { ...base, id: 'user:one', display_name: 'one', source: 'user', read_only: false, model: 'm2', available: true, vote_identity: 'example.com/m2' },
  { ...base, id: 'user:bad', display_name: 'bad', source: 'user', read_only: false, model: 'm3', available: false, unavailable_reason: 'credential', vote_identity: 'example.com/m3' },
]

const node = (name: string, model: string, source: string, extra: Record<string, unknown> = {}) => ({
  node: name, model, source, configured: 'config:base', configured_source: 'config',
  effective_params: { max_tokens: 4096 }, vote_identity: model === 'config:base' ? 'example.com/m' : 'example.com/m2', ...extra,
})

const detail = {
  profile: 'default', overrides: { challenge: { model: 'user:one', params: { max_tokens: 2048 } } }, revision: 3,
  effective: { provider: 'base' },
  nodes: [
    node('default', 'config:base', 'config'),
    node('explore', 'config:base', 'derived'),
    node('lanes.code_review', 'config:base', 'config_default'),
    node('lanes.review', 'user:one', 'override_default', { param_source: 'override_default', effective_params: { max_tokens: 1000, temperature: 0.5, reasoning: { effort: 'max' }, thinking: { budget_tokens: 2048 } } }),
    node('lanes.reasoner', 'config:base', 'config'),
    node('lanes.validate', 'config:base', 'config'),
    node('challenge', 'user:one', 'override', { param_source: 'override' }),
  ],
}

function mock(handler?: (url: string, init?: RequestInit) => Response | undefined) {
  return installFetch((url, init) => {
    const custom = handler?.(url, init)
    if (custom) return custom
    if (url.startsWith('/api/v1/profiles/default')) return jsonResponse(detail)
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default', config: detail.effective }] })
    return jsonResponse({})
  })
}

const isPut = (url: string, init?: RequestInit) => url === '/api/v1/profiles/default/overrides' && init?.method === 'PUT'

async function openEditor() {
  const user = userEvent.setup()
  renderWithProviders(<ModelsPage />, { route: '/models' })
  await user.click(await screen.findByRole('button', { name: '编辑档位' }))
  return user
}

describe('profile node sources, params and same-model hint (#339)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows all five source labels with model name, short id and effective params', async () => {
    mock()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const list = await screen.findByRole('list', { name: '节点生效模型' })
    for (const label of ['覆盖', '继承覆盖默认', '配置', '配置默认', '派生']) {
      expect(within(list).getAllByText(label).length).toBeGreaterThan(0)
    }
    expect(within(list).getAllByText('one').length).toBeGreaterThan(0)
    expect(within(list).getByText(/max_tokens 1000 · temperature 0.5 · reasoning.effort max · thinking.budget_tokens 2048/)).toBeInTheDocument()
    expect(within(list).getAllByText(/配置值 base（配置）/).length).toBeGreaterThan(0)
  })

  it('shows the same-model hint for nodes sharing a vote identity', async () => {
    mock()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const notes = await screen.findAllByRole('note')
    expect(notes.some((item) => item.textContent?.includes('这些节点会被计为同一模型：默认、探索'))).toBe(true)
    expect(notes.some((item) => item.textContent?.includes('review、challenge'))).toBe(true)
  })

  it('saves model and params overrides with the current revision', async () => {
    const fetchMock = mock((url, init) => (isPut(url, init) ? jsonResponse(detail) : undefined))
    const user = await openEditor()
    await user.selectOptions(screen.getByLabelText('validate 模型'), 'user:one')
    await user.type(screen.getByLabelText('validate max_tokens'), '1500')
    await user.type(screen.getByLabelText('validate temperature'), '0.3')
    await user.selectOptions(screen.getByLabelText('validate reasoning.effort'), 'max')
    await user.type(screen.getByLabelText('validate thinking.budget_tokens'), '2048')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(true))
    const call = fetchMock.mock.calls.find(([url, init]) => isPut(String(url), init))
    expect(JSON.parse(String(call?.[1]?.body))).toEqual({
      revision: 3,
      overrides: {
        challenge: { model: 'user:one', params: { max_tokens: 2048 } },
        lanes: { validate: { model: 'user:one', params: { max_tokens: 1500, temperature: 0.3, reasoning: { effort: 'max' }, thinking: { budget_tokens: 2048 } } } },
      },
    })
  })

  it('clears one node by saving without it', async () => {
    const fetchMock = mock((url, init) => (isPut(url, init) ? jsonResponse(detail) : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '清除challenge覆盖' }))
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(true))
    const call = fetchMock.mock.calls.find(([url, init]) => isPut(String(url), init))
    expect(JSON.parse(String(call?.[1]?.body))).toEqual({ revision: 3, overrides: {} })
  })

  it('restores the whole profile to configuration', async () => {
    const fetchMock = mock((url, init) => (init?.method === 'DELETE' ? jsonResponse(detail) : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '恢复为配置' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => String(url) === '/api/v1/profiles/default/overrides?revision=3' && init?.method === 'DELETE')).toBe(true))
  })

  it('disables unavailable models and shows the reason', async () => {
    mock()
    await openEditor()
    const options = screen.getAllByRole('option', { name: /bad.*不可用：credential/ })
    expect(options.length).toBe(7)
    expect(options.every((option) => option.hasAttribute('disabled'))).toBe(true)
  })

  it('maps 400 field paths next to the matching inputs', async () => {
    mock((url, init) => (isPut(url, init)
      ? jsonResponse({ error: { code: 'validation_failed', message: 'invalid overrides', details: [
        { field: 'overrides.lanes.validate.model', message: 'model is unavailable' },
        { field: 'overrides.default.params.thinking.budget_tokens', message: 'must be at least 1024 for anthropic models' },
        { field: 'overrides.explore.params', message: 'overrides.explore.params.temperature must be between 0 and 2' },
      ] } }, 400)
      : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '保存' }))
    const validate = screen.getByRole('group', { name: 'validate' })
    expect(await within(validate).findByText('model is unavailable')).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: '默认' })).getByText('must be at least 1024 for anthropic models')).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: '探索' })).getByText('overrides.explore.params.temperature must be between 0 and 2')).toBeInTheDocument()
    expect(screen.getByLabelText('validate 模型')).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByLabelText('探索 temperature')).toHaveAttribute('aria-invalid', 'true')
  })

  it('asks to refresh and reload on a stale revision', async () => {
    const fetchMock = mock((url, init) => (isPut(url, init)
      ? jsonResponse({ error: { code: 'stale_revision', message: 'revision does not match' } }, 409)
      : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText(/请刷新并重新加载/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled()
    const reads = () => fetchMock.mock.calls.filter(([url, init]) => String(url) === '/api/v1/profiles/default' && (!init?.method || init.method === 'GET')).length
    const before = reads()
    await user.click(screen.getByRole('button', { name: '刷新并重新加载' }))
    await waitFor(() => expect(reads()).toBeGreaterThan(before))
    await waitFor(() => expect(screen.queryByText(/请刷新并重新加载/)).not.toBeInTheDocument())
  })

  it('renders read-only for non-admins', async () => {
    mock()
    renderWithProviders(<ModelsPage />, { route: '/models', me: viewerMe })
    expect(await screen.findByRole('list', { name: '节点生效模型' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '编辑档位' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '恢复为配置' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('validate 模型')).not.toBeInTheDocument()
  })
})
