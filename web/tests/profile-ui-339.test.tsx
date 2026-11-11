import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const base = { protocol: 'openai-chat', base_url: 'https://example.com', has_api_key: true, revision: 1, referenced_by: [] }
const baseParams = {
  max_tokens: 4096, temperature: 0.2, reasoning: { effort: 'high' },
  thinking: { budget_tokens: 1024, type: 'disabled' },
}
const models = [
  { ...base, id: 'config:base', display_name: 'base', source: 'config', read_only: true, model: 'm', params: baseParams, available: true, vote_identity: 'example.com/m' },
  { ...base, id: 'user:one', display_name: 'one', source: 'user', read_only: false, model: 'm2', params: { max_tokens: 2048 }, available: true, vote_identity: 'example.com/m2' },
  { ...base, id: 'user:bad', display_name: 'bad', source: 'user', read_only: false, model: 'm3', params: {}, available: false, unavailable_reason: 'credential', vote_identity: 'example.com/m3' },
]

const node = (name: string, model: string, source: string, extra: Record<string, unknown> = {}) => ({
  node: name, model, source, configured: 'config:base', configured_source: 'config',
  effective_params: { max_tokens: 4096 }, vote_identity: model === 'config:base' ? 'example.com/m' : 'example.com/m2', ...extra,
})

const defaultParams = {
  max_tokens: 1000, temperature: 0.5, reasoning: { effort: 'max' },
  thinking: { budget_tokens: 2048, type: 'enabled' },
}

const detail = {
  profile: 'default',
  overrides: {
    default: { model: 'config:base', params: defaultParams },
    challenge: { model: 'user:one', params: { max_tokens: 2048 } },
  } as Record<string, unknown>,
  revision: 3,
  effective: { provider: 'base' },
  nodes: [
    node('default', 'config:base', 'override', { param_source: 'override', effective_params: defaultParams }),
    node('explore', 'config:base', 'derived', { configured_source: 'derived', param_source: 'override_default', effective_params: defaultParams }),
    node('lanes.code_review', 'config:base', 'config_default', { configured_source: 'config_default', param_source: 'override_default', effective_params: defaultParams }),
    node('lanes.review', 'user:one', 'override_default', {
      configured_source: 'config_default', param_source: 'override_default',
      effective_params: { max_tokens: 1000, temperature: 0.5, reasoning: { effort: 'max' }, thinking: { budget_tokens: 2048 } },
    }),
    node('lanes.reasoner', 'config:base', 'config', { param_source: 'override_default', effective_params: defaultParams }),
    node('lanes.validate', 'config:base', 'config', { param_source: 'override_default', effective_params: defaultParams }),
    node('challenge', 'user:one', 'override', { configured_source: 'config', param_source: 'override', effective_params: { max_tokens: 2048, temperature: 0.5, reasoning: { effort: 'max' }, thinking: { budget_tokens: 2048 } } }),
  ],
}

function hint(value: string, source: string, overridden = false): string {
  return `${overridden ? '覆盖中 · ' : ''}继承值：${value || '未设置'} · 来源：${source}`
}

function modelHint(label: string, source: string, overridden = false): string {
  return `${overridden ? '覆盖中 · ' : ''}继承值：${label} · 来源：${source}`
}

function thinkingObject(budget: string, type: string, source: string): string {
  return `继承 thinking：budget_tokens ${budget || '未设置'} · type ${type || '未设置'} · 来源：${source}`
}

function thinkingWhole(overridden = false): string {
  return `${overridden ? '覆盖中 · ' : ''}thinking 由本层整体覆盖；未填写的子字段不继承`
}

function mock(handler?: (url: string, init?: RequestInit) => Response | undefined, source: typeof detail = detail) {
  return installFetch((url, init) => {
    const custom = handler?.(url, init)
    if (custom) return custom
    if (url.startsWith('/api/v1/profiles/default')) return jsonResponse(source)
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default', config: source.effective }] })
    return jsonResponse({})
  })
}

const isPut = (url: string, init?: RequestInit) => url === '/api/v1/profiles/default/overrides' && init?.method === 'PUT'

const PROFILE_LABELS = ['全局默认', '探索', 'code-review', 'review', 'reasoner', 'validate', 'challenge']

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

function putBody(fetchMock: ReturnType<typeof mock>) {
  const call = fetchMock.mock.calls.find(([url, init]) => isPut(String(url), init))
  return JSON.parse(String(call?.[1]?.body))
}

describe('profile node sources, params and same-model hint (#339)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the global default card above the six configurable nodes', async () => {
    mock()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const globalCard = await screen.findByRole('region', { name: '全局默认' })
    expect(screen.getByRole('heading', { name: '全局默认', level: 3 })).toBeInTheDocument()
    expect(within(globalCard).getByText('base')).toBeInTheDocument()
    expect(within(globalCard).getByText(/生效参数 max_tokens 1000 · temperature 0.5/)).toBeInTheDocument()
    expect(within(globalCard).getByText(/参数来源 覆盖/)).toBeInTheDocument()
    expect(within(globalCard).getByText('全局默认覆盖逐字段作用于下方节点；节点未覆盖字段继承全局默认，仍未设置的继承自身模型')).toBeInTheDocument()
    const list = screen.getByRole('list', { name: '节点生效模型' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(6)
    expect(globalCard.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('shows all five source labels with model name, short id and effective params', async () => {
    mock()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const list = await screen.findByRole('list', { name: '节点生效模型' })
    for (const label of ['覆盖', '全局默认', '配置', '配置默认', '派生']) {
      expect(within(list).getAllByText(label).length).toBeGreaterThan(0)
    }
    expect(within(list).getAllByText('one').length).toBeGreaterThan(0)
    expect(within(list).getAllByText(/max_tokens 1000 · temperature 0.5 · reasoning.effort max · thinking.budget_tokens 2048/).length).toBeGreaterThan(0)
    expect(within(list).getAllByText(/配置值 base（配置）/).length).toBeGreaterThan(0)
    expect(within(list).getAllByText(/参数来源 全局默认/).length).toBeGreaterThan(0)
    expect(within(list).getAllByText(/参数来源 覆盖/).length).toBeGreaterThan(0)
  })

  it('shows the same-model hint for nodes sharing a vote identity', async () => {
    mock()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const notes = await screen.findAllByRole('note')
    expect(notes.some((item) => item.textContent?.includes('这些节点会被计为同一模型：全局默认、探索'))).toBe(true)
    expect(notes.some((item) => item.textContent?.includes('review、challenge'))).toBe(true)
  })

  it('shows inherited values and their sources below the node inputs', async () => {
    mock()
    const user = await openEditor()
    const global = screen.getByRole('group', { name: '全局默认' })
    expect(within(global).getByText(hint('4096', '模型', true))).toBeInTheDocument()
    expect(within(global).getByText(hint('0.2', '模型', true))).toBeInTheDocument()
    expect(within(global).getByText(hint('high', '模型', true))).toBeInTheDocument()
    expect(within(global).getAllByText(thinkingWhole(true))).toHaveLength(2)
    expect(within(global).getByText(modelHint('base（config:base）', '配置', true))).toBeInTheDocument()

    const review = screen.getByRole('group', { name: 'review' })
    expect(within(review).getByText(hint('1000', '全局默认'))).toBeInTheDocument()
    expect(within(review).getByText(hint('0.5', '全局默认'))).toBeInTheDocument()
    expect(within(review).getByText(hint('max', '全局默认'))).toBeInTheDocument()
    expect(within(review).getAllByText(thinkingObject('2048', 'enabled', '全局默认'))).toHaveLength(2)
    expect(within(review).getByText(modelHint('base（config:base）', '全局默认'))).toBeInTheDocument()

    const challenge = screen.getByRole('group', { name: 'challenge' })
    expect(within(challenge).getByText(hint('1000', '全局默认', true))).toBeInTheDocument()
    expect(within(challenge).getByText(modelHint('base（config:base）', '全局默认', true))).toBeInTheDocument()
    expect(within(challenge).getAllByText(thinkingObject('2048', 'enabled', '全局默认'))).toHaveLength(2)

    // Editing the global default updates what the nodes inherit, before saving.
    const globalMax = screen.getByLabelText('全局默认 max_tokens')
    await user.clear(globalMax)
    await user.type(globalMax, '2500')
    expect(within(review).getByText(hint('2500', '全局默认'))).toBeInTheDocument()
  })

  it('saves model and params overrides with the current revision and only the overridden fields', async () => {
    const fetchMock = mock((url, init) => (isPut(url, init) ? jsonResponse(detail) : undefined))
    const user = await openEditor()
    await chooseOption(user, screen.getByLabelText('validate 模型'), /^one（user:one）/)
    await user.type(screen.getByLabelText('validate max_tokens'), '1500')
    await user.type(screen.getByLabelText('validate temperature'), '0.3')
    await chooseOption(user, screen.getByLabelText('validate reasoning.effort'), 'max')
    await user.type(screen.getByLabelText('validate thinking.budget_tokens'), '2048')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(true))
    const body = putBody(fetchMock)
    expect(body).toEqual({
      revision: 3,
      overrides: {
        default: { model: 'config:base', params: defaultParams },
        challenge: { model: 'user:one', params: { max_tokens: 2048 } },
        lanes: { validate: { model: 'user:one', params: { max_tokens: 1500, temperature: 0.3, reasoning: { effort: 'max' }, thinking: { budget_tokens: 2048 } } } },
      },
    })
    // Inherited values are presentation only; nodes without overrides are never submitted.
    expect(body.overrides.explore).toBeUndefined()
    expect(body.overrides.lanes.review).toBeUndefined()
  })

  it('edits the global default card and submits it as the default slot', async () => {
    const fetchMock = mock((url, init) => (isPut(url, init) ? jsonResponse(detail) : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '清除challenge覆盖' }))
    await chooseOption(user, screen.getByLabelText('全局默认 模型'), /^one（user:one）/)
    const max = screen.getByLabelText('全局默认 max_tokens')
    await user.clear(max)
    await user.type(max, '2000')
    const temperature = screen.getByLabelText('全局默认 temperature')
    await user.clear(temperature)
    await user.type(temperature, '0.7')
    await chooseOption(user, screen.getByLabelText('全局默认 reasoning.effort'), 'low')
    const budget = screen.getByLabelText('全局默认 thinking.budget_tokens')
    await user.clear(budget)
    await user.type(budget, '1500')
    await chooseOption(user, screen.getByLabelText('全局默认 thinking.type'), 'disabled')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(true))
    expect(putBody(fetchMock)).toEqual({
      revision: 3,
      overrides: {
        default: {
          model: 'user:one',
          params: {
            max_tokens: 2000, temperature: 0.7, reasoning: { effort: 'low' },
            thinking: { budget_tokens: 1500, type: 'disabled' },
          },
        },
      },
    })
  })

  it('submits only a partially overridden global default and falls back to the model elsewhere', async () => {
    const partial = {
      ...detail,
      overrides: { default: { params: { max_tokens: 1000 } } },
      nodes: detail.nodes.map((item) => (item.node === 'default'
        ? { ...item, param_source: 'override', effective_params: { max_tokens: 1000 } }
        : item)),
    }
    const fetchMock = mock((url, init) => (isPut(url, init) ? jsonResponse(partial) : undefined), partial)
    const user = await openEditor()

    const global = screen.getByRole('group', { name: '全局默认' })
    expect(within(global).getByText(hint('4096', '模型', true))).toBeInTheDocument()
    expect(within(global).getByText(hint('0.2', '模型'))).toBeInTheDocument()
    expect(within(global).getByText(hint('high', '模型'))).toBeInTheDocument()
    expect(within(global).getAllByText(thinkingObject('1024', 'disabled', '模型'))).toHaveLength(2)
    expect(screen.getByText('全局默认逐字段作用于 6 个节点；节点输入下方显示继承值与来源，保存只提交实际覆盖项。')).toBeInTheDocument()

    const review = screen.getByRole('group', { name: 'review' })
    expect(within(review).getByText(hint('1000', '全局默认'))).toBeInTheDocument()
    expect(within(review).getByText(hint('0.2', '模型'))).toBeInTheDocument()
    expect(within(review).getByText(hint('high', '模型'))).toBeInTheDocument()
    expect(within(review).getAllByText(thinkingObject('1024', 'disabled', '模型'))).toHaveLength(2)

    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(true))
    expect(putBody(fetchMock)).toEqual({
      revision: 3,
      overrides: { default: { params: { max_tokens: 1000 } } },
    })
  })

  it('does not claim a thinking subfield is inherited when the node overrides the other one', async () => {
    const source = {
      ...detail,
      overrides: {
        default: { params: { thinking: { budget_tokens: 2048, type: 'enabled' } } },
        challenge: { params: { thinking: { budget_tokens: 1024 } } },
      },
    }
    mock(undefined, source)
    await openEditor()
    const challenge = screen.getByRole('group', { name: 'challenge' })
    expect(within(challenge).getByText(thinkingWhole(true))).toBeInTheDocument()
    expect(within(challenge).getByText(thinkingWhole(false))).toBeInTheDocument()
    expect(within(challenge).queryByText(/继承 thinking/)).not.toBeInTheDocument()
    expect(within(challenge).queryByText(/继承值：enabled/)).not.toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: '全局默认' })).getAllByText(thinkingWhole(true))).toHaveLength(2)
  })

  it('does not claim the missing thinking subfield when the node only sets type', async () => {
    const source = {
      ...detail,
      overrides: {
        default: { params: { thinking: { budget_tokens: 2048 } } },
        challenge: { params: { thinking: { type: 'enabled' } } },
      },
    }
    mock(undefined, source)
    await openEditor()
    const challenge = screen.getByRole('group', { name: 'challenge' })
    expect(within(challenge).getByText(thinkingWhole(false))).toBeInTheDocument()
    expect(within(challenge).getByText(thinkingWhole(true))).toBeInTheDocument()
    expect(within(challenge).queryByText(/继承 thinking/)).not.toBeInTheDocument()
    expect(within(challenge).queryByText(/继承值：2048/)).not.toBeInTheDocument()

    const global = screen.getByRole('group', { name: '全局默认' })
    expect(within(global).getByText(thinkingWhole(true))).toBeInTheDocument()
    expect(within(global).getByText(thinkingWhole(false))).toBeInTheDocument()
    expect(within(global).queryByText(/继承值：disabled/)).not.toBeInTheDocument()
  })

  it('treats a partially overridden global default thinking as a whole object', async () => {
    const source = {
      ...detail,
      overrides: { default: { params: { thinking: { budget_tokens: 2048 } } } },
    }
    mock(undefined, source)
    await openEditor()
    const global = screen.getByRole('group', { name: '全局默认' })
    expect(within(global).getByText(thinkingWhole(true))).toBeInTheDocument()
    expect(within(global).getByText(thinkingWhole(false))).toBeInTheDocument()
    expect(within(global).queryByText(/继承 thinking/)).not.toBeInTheDocument()
    expect(within(global).queryByText(/继承值：disabled/)).not.toBeInTheDocument()

    const review = screen.getByRole('group', { name: 'review' })
    expect(within(review).getAllByText(thinkingObject('2048', '', '全局默认'))).toHaveLength(2)
  })

  it('shows unknown instead of stale effective params when the model catalog is empty', async () => {
    const partial = {
      ...detail,
      overrides: { default: { params: { max_tokens: 1000 } } },
      nodes: detail.nodes.map((item) => (item.node === 'lanes.review'
        ? { ...item, effective_params: { max_tokens: 1000, temperature: 0.9 } }
        : item)),
    }
    installFetch((url, init) => {
      if (isPut(url, init)) return jsonResponse(partial)
      if (url.startsWith('/api/v1/profiles/default')) return jsonResponse(partial)
      if (url.startsWith('/api/v1/models')) return jsonResponse({ models: [] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default', config: partial.effective }] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑档位' }))

    const review = screen.getByRole('group', { name: 'review' })
    expect(within(review).getAllByText('继承值：未知').length).toBeGreaterThan(0)
    expect(within(review).queryByText(/继承值：0\.9/)).not.toBeInTheDocument()
    const global = screen.getByRole('group', { name: '全局默认' })
    expect(within(global).getAllByText('继承值：未知').length).toBeGreaterThan(0)
  })

  it('does not present a node override as the inherited model when configuration has no source', async () => {
    const source = {
      ...detail,
      overrides: {
        default: { params: defaultParams },
        challenge: { model: 'user:one' },
      },
      nodes: detail.nodes.map((item) => (item.node === 'challenge'
        ? { ...item, configured: '', configured_source: 'derived' }
        : item)),
    }
    mock(undefined, source)
    await openEditor()
    const challenge = screen.getByRole('group', { name: 'challenge' })
    expect(within(challenge).getByText('覆盖中 · 继承值：未知（无配置来源）')).toBeInTheDocument()
    expect(within(challenge).queryByText(/继承值：one/)).not.toBeInTheDocument()
  })

  it('clears one node by saving without it', async () => {
    const fetchMock = mock((url, init) => (isPut(url, init) ? jsonResponse(detail) : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '清除challenge覆盖' }))
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(true))
    expect(putBody(fetchMock)).toEqual({
      revision: 3,
      overrides: { default: { model: 'config:base', params: defaultParams } },
    })
  })

  it('clears the global default and restores inheritance for the six nodes', async () => {
    const fetchMock = mock((url, init) => (isPut(url, init) ? jsonResponse(detail) : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '清除全局默认覆盖' }))
    const review = screen.getByRole('group', { name: 'review' })
    expect(within(review).getByText(hint('4096', '模型'))).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(true))
    expect(putBody(fetchMock)).toEqual({
      revision: 3,
      overrides: { challenge: { model: 'user:one', params: { max_tokens: 2048 } } },
    })
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
    expect(within(screen.getByRole('group', { name: '全局默认' })).getByText('must be at least 1024 for anthropic models')).toBeInTheDocument()
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
    await user.type(screen.getByLabelText('review temperature'), '2.5')
    await user.type(screen.getByLabelText('review max_tokens'), '-5')
    await user.type(screen.getByLabelText('review thinking.budget_tokens'), '1.5')
    await user.type(screen.getByLabelText('validate temperature'), '3')
    await user.type(screen.getByLabelText('reasoner temperature'), '2.6')
    const globalMax = screen.getByLabelText('全局默认 max_tokens')
    await user.clear(globalMax)
    await user.type(globalMax, '-1')
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(fetchMock.mock.calls.some(([url, init]) => isPut(String(url), init))).toBe(false)
    const review = screen.getByRole('group', { name: 'review' })
    expect(within(review).getByText('temperature 必须在 0 到 2 之间。')).toBeInTheDocument()
    expect(within(review).getByText('max_tokens 必须是正整数。')).toBeInTheDocument()
    expect(within(review).getByText('thinking.budget_tokens 必须是正整数。')).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: 'validate' })).getByText('temperature 必须在 0 到 2 之间。')).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: 'reasoner' })).getByText('temperature 必须在 0 到 2 之间。')).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: '全局默认' })).getByText('max_tokens 必须是正整数。')).toBeInTheDocument()
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
    const describedText = (control: HTMLElement) => (control.getAttribute('aria-describedby') ?? '')
      .split(/\s+/).filter(Boolean).map((id) => document.getElementById(id)?.textContent).filter(Boolean).join(' ')
    expect(describedText(max)).toContain('first；second')
    expect(describedText(max)).toContain('继承值：1000 · 来源：全局默认')
    const validateTemp = screen.getByLabelText('validate temperature')
    expect(describedText(validateTemp)).toContain('params broken')
    await user.clear(max)
    await user.type(max, '1')
    expect(within(screen.getByRole('group', { name: '探索' })).queryByText('first；second')).not.toBeInTheDocument()
    expect(describedText(max)).not.toContain('first；second')
    await user.clear(validateTemp)
    await user.type(validateTemp, '1')
    expect(screen.queryByText('params broken')).not.toBeInTheDocument()
  })

  it('shows the stale notice after the fieldsets, scrolls it into view and focuses the reload action', async () => {
    const scroll = vi.fn()
    const original = Element.prototype.scrollIntoView
    Element.prototype.scrollIntoView = scroll
    try {
      mock((url, init) => (isPut(url, init)
        ? jsonResponse({ error: { code: 'stale_revision', message: 'revision does not match' } }, 409)
        : undefined))
      const user = await openEditor()
      await user.click(screen.getByRole('button', { name: '保存' }))
      const alert = await screen.findByText(/请刷新并重新加载/)
      const lastGroup = screen.getByRole('group', { name: 'challenge' })
      expect(lastGroup.compareDocumentPosition(alert) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
      const reload = screen.getByRole('button', { name: '刷新并重新加载' })
      await waitFor(() => expect(reload).toHaveFocus())
      expect(scroll).toHaveBeenCalledWith({ block: 'nearest' })
    } finally {
      Element.prototype.scrollIntoView = original
    }
  })

  it('focuses an unmapped server error below the fieldsets', async () => {
    mock((url, init) => (isPut(url, init)
      ? jsonResponse({ error: { code: 'validation_failed', message: 'invalid overrides', details: [
        { field: 'overrides.unknown.model', message: 'nope' },
      ] } }, 400)
      : undefined))
    const user = await openEditor()
    await user.click(screen.getByRole('button', { name: '保存' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('overrides.unknown.model: nope')
    await waitFor(() => expect(alert).toHaveFocus())
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
    expect(screen.getByLabelText('challenge max_tokens')).toHaveValue(777)
    expect(screen.getByLabelText('review max_tokens')).toHaveValue(null)
    expect(screen.getByLabelText('全局默认 max_tokens')).toHaveValue(null)
    expect(screen.getByRole('button', { name: '保存' })).toBeEnabled()
  })

  it('does not print the raw vote identity and shows unavailable when effective params are absent', async () => {
    mock((url) => (url === '/api/v1/profiles/default'
      ? jsonResponse({ ...detail, nodes: detail.nodes.map((item) => (item.node === 'default' ? { ...item, effective_params: undefined } : item)) })
      : undefined))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const globalCard = await screen.findByRole('region', { name: '全局默认' })
    expect(within(globalCard).getByText(/生效参数 不可用/)).toBeInTheDocument()
    expect(within(globalCard).queryByText(/参数来源/)).not.toBeInTheDocument()
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
