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

const PROFILE_LABELS = ['默认', '探索', 'code-review', 'review', 'reasoner', 'validate', 'challenge']

async function chooseOption(user: ReturnType<typeof userEvent.setup>, trigger: HTMLElement, name: RegExp | string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name }))
}

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
    await chooseOption(user, screen.getByLabelText('validate 模型'), /^one（user:one）/)
    await user.type(screen.getByLabelText('validate max_tokens'), '1500')
    await user.type(screen.getByLabelText('validate temperature'), '0.3')
    await chooseOption(user, screen.getByLabelText('validate reasoning.effort'), 'max')
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
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'DELETE')).toBe(false)
    await user.click(within(screen.getByRole('dialog', { name: '恢复为配置' })).getByRole('button', { name: '恢复为配置' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => String(url) === '/api/v1/profiles/default/overrides?revision=3' && init?.method === 'DELETE')).toBe(true))
  })

  it('disables unavailable models and shows the reason', async () => {
    mock()
    const user = await openEditor()
    for (const label of PROFILE_LABELS) {
      await user.click(screen.getByRole('combobox', { name: `${label} 模型` }))
      const options = await screen.findAllByRole('option', { name: /bad.*不可用：credential/ })
      expect(options).toHaveLength(1)
      expect(options[0]).toHaveAttribute('aria-disabled', 'true')
      await user.keyboard('{Escape}')
    }
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

  it('cancelling the restore confirmation sends nothing', async () => {
    const fetchMock = mock()
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '恢复为配置' }))
    await user.click(within(screen.getByRole('dialog', { name: '恢复为配置' })).getByRole('button', { name: '取消' }))
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'DELETE')).toBe(false)
  })

  it('disables restore when no overrides are stored', async () => {
    mock((url) => (url === '/api/v1/profiles/default' ? jsonResponse({ ...detail, overrides: {} }) : undefined))
    await openEditor()
    expect(screen.getByRole('button', { name: '恢复为配置' })).toBeDisabled()
  })

  it('validates inputs on the client without sending a request', async () => {
    const fetchMock = mock()
    const user = await openEditor()
    await user.type(screen.getByLabelText('review temperature'), '0x1')
    await user.type(screen.getByLabelText('review max_tokens'), '-5')
    await user.type(screen.getByLabelText('review thinking.budget_tokens'), '1.5')
    await user.type(screen.getByLabelText('validate temperature'), ' ')
    await user.type(screen.getByLabelText('reasoner temperature'), '2.5')
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(false)
    const review = screen.getByRole('group', { name: 'review' })
    expect(within(review).getByText('temperature 必须在 0 到 2 之间。')).toBeInTheDocument()
    expect(within(review).getByText('max_tokens 必须是正整数。')).toBeInTheDocument()
    expect(within(review).getByText('thinking.budget_tokens 必须是正整数。')).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: 'validate' })).getByText('temperature 必须在 0 到 2 之间。')).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: 'reasoner' })).getByText('temperature 必须在 0 到 2 之间。')).toBeInTheDocument()
  })

  it('links errors with aria-describedby, joins messages, focuses the first invalid input and clears on edit', async () => {
    mock((url, init) => (isPut(url, init)
      ? jsonResponse({ error: { code: 'validation_failed', message: 'invalid overrides', details: [
        { field: 'overrides.explore.params.max_tokens', message: 'first' },
        { field: 'overrides.explore.params.max_tokens', message: 'second' },
        { field: 'overrides.lanes.validate.params', message: 'params broken' },
      ] } }, 400)
      : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '保存' }))
    const max = screen.getByLabelText('探索 max_tokens')
    expect(await within(screen.getByRole('group', { name: '探索' })).findByText('first；second')).toBeInTheDocument()
    await waitFor(() => expect(max).toHaveFocus())
    const describedBy = max.getAttribute('aria-describedby') ?? ''
    expect(document.getElementById(describedBy)?.textContent).toBe('first；second')
    const validateTemp = screen.getByLabelText('validate temperature')
    expect(document.getElementById(validateTemp.getAttribute('aria-describedby') ?? '')?.textContent).toBe('params broken')
    await user.type(max, '1')
    expect(within(screen.getByRole('group', { name: '探索' })).queryByText('first；second')).not.toBeInTheDocument()
    expect(max).not.toHaveAttribute('aria-describedby')
    await user.type(validateTemp, '1')
    expect(screen.queryByText('params broken')).not.toBeInTheDocument()
  })

  it('shows the stale notice at the top of the drawer body, before the node fieldsets', async () => {
    mock((url, init) => (isPut(url, init)
      ? jsonResponse({ error: { code: 'stale_revision', message: 'revision does not match' } }, 409)
      : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '保存' }))
    const alert = await screen.findByText(/请刷新并重新加载/)
    const firstGroup = screen.getByRole('group', { name: '默认' })
    expect(alert.compareDocumentPosition(firstGroup) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('keeps the stale notice when the reload fails and resets the form after a successful refetch', async () => {
    let revision = 3
    let failReads = false
    mock((url, init) => {
      if (isPut(url, init)) return jsonResponse({ error: { code: 'stale_revision', message: 'revision does not match' } }, 409)
      if (url === '/api/v1/profiles/default' && (!init?.method || init.method === 'GET')) {
        if (failReads) return jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500)
        return jsonResponse({ ...detail, revision, overrides: { challenge: { model: 'user:one', params: { max_tokens: 777 } } } })
      }
      return undefined
    })
    const user = await openEditor()
    await user.type(screen.getByLabelText('review max_tokens'), '123')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await screen.findByText(/请刷新并重新加载/)
    failReads = true
    await user.click(screen.getByRole('button', { name: '刷新并重新加载' }))
    expect(await screen.findByText('重新加载失败，请稍后重试。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '刷新并重新加载' })).toBeInTheDocument()
    failReads = false
    revision = 4
    await user.click(screen.getByRole('button', { name: '刷新并重新加载' }))
    await waitFor(() => expect(screen.queryByText(/请刷新并重新加载/)).not.toBeInTheDocument())
    expect(screen.getByLabelText('challenge max_tokens')).toHaveValue('777')
    expect(screen.getByLabelText('review max_tokens')).toHaveValue('')
    expect(screen.getByRole('button', { name: '保存' })).toBeEnabled()
  })

  it('does not print the raw vote identity and shows unavailable when effective params are absent', async () => {
    mock((url) => (url === '/api/v1/profiles/default'
      ? jsonResponse({ ...detail, nodes: detail.nodes.map((item, index) => (index === 0 ? { ...item, effective_params: undefined } : item)) })
      : undefined))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const list = await screen.findByRole('list', { name: '节点生效模型' })
    expect(within(list).getByText(/生效参数 不可用/)).toBeInTheDocument()
    expect(screen.queryByText(/example\.com\/m/)).not.toBeInTheDocument()
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
