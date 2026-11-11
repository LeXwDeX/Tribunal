import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const configModel = {
  id: 'config:primary', display_name: 'primary', source: 'config', read_only: true,
  protocol: 'openai-chat', base_url: 'https://api.example.test/v1', model: 'deepseek-chat',
  params: { max_tokens: 8192, context_window: 128000, temperature: 0.3 },
  effective_params: { max_tokens: 8192, context_window: 128000, temperature: 0.3 },
  param_sources: {},
  has_api_key: true, available: true, revision: 0, referenced_by: [],
  vote_identity: 'api.example.test/deepseek-chat',
}

function configPayload(overrides: {
  baseRevision?: string
  overlayRevision?: string
  restartRequired?: boolean
} = {}) {
  return {
    config: {
      providers: [{
        name: 'primary', protocol: 'openai-chat', base_url: 'https://api.example.test/v1',
        model: 'deepseek-chat', api_key_configured: true,
        max_tokens: 8192, context_window: 128000, temperature: 0.3,
        max_concurrency: 4, rpm: 60, forced_tool_choice: 'supported',
        strict_role_alternation: true,
        reasoning: { effort: 'high' }, thinking: { type: 'enabled', budget_tokens: 2048 },
        preserve_reasoning_content: true,
      }],
    },
    origins: {
      'providers.primary.protocol': 'base',
      'providers.primary.base_url': 'base',
      'providers.primary.model': 'overlay',
      'providers.primary.max_tokens': 'base',
      'providers.primary.temperature': 'overlay',
      'providers.primary.reasoning.effort': 'base',
      'providers.primary.thinking.type': 'base',
      'providers.primary.thinking.budget_tokens': 'base',
    },
    warnings: [],
    base_revision: overrides.baseRevision ?? 'base-rev-1',
    overlay_revision: overrides.overlayRevision ?? 'overlay-rev-1',
    overlay_present: true,
    applied: false,
    restart_required: overrides.restartRequired ?? true,
    last_error: '',
    last_error_at: null,
  }
}

const nodeOverride = {
  node: 'lanes.review', model: 'config:primary', source: 'override',
  configured: 'config:primary', configured_source: 'config',
  override: { model: 'config:primary', params: { max_tokens: 1000 } },
  params: { max_tokens: 1000 }, param_source: 'override',
  effective_params: { max_tokens: 1000 },
  vote_identity: 'api.example.test/deepseek-chat',
}

interface RouteOptions {
  overlay?: (body: Record<string, unknown>) => Response
  nodes?: unknown[]
  models?: unknown[]
}

function routes(options: RouteOptions = {}) {
  const state = {
    provider: configPayload().config.providers[0],
    overlayRevision: 'overlay-rev-1',
    patchBody: undefined as Record<string, unknown> | undefined,
    configReads: 0,
    modelReads: 0,
  }
  const fetchImpl = (url: string, init?: RequestInit) => {
    if (url === '/api/v1/config/overlay' && init?.method === 'PATCH') {
      const body = JSON.parse(String(init.body)) as Record<string, unknown>
      state.patchBody = body
      if (options.overlay) return options.overlay(body)
      return jsonResponse({
        dry_run: false, applied: true, changed: true,
        base_revision: body.base_revision,
        previous_overlay_revision: body.overlay_revision,
        overlay_revision: 'overlay-rev-2',
        restart_required: true,
        notice: 'Saved to the overlay file. Restart the service to apply it; restarting may cancel in-flight reviews.',
      })
    }
    if (url === '/api/v1/config') {
      state.configReads += 1
      return jsonResponse({
        ...configPayload({ overlayRevision: state.overlayRevision }),
        config: { providers: [state.provider] },
      })
    }
    if (url === '/api/v1/config/status') {
      return jsonResponse({
        applied: false, restart_required: true,
        base_revision: 'base-rev-1', overlay_revision: state.overlayRevision,
        overlay_present: true, warnings: [], last_error: '', last_error_at: null, updated_at: null,
      })
    }
    if (url === '/api/v1/profiles/default') {
      return jsonResponse({
        profile: 'default', overrides: options.nodes ? nodeOverride.override : {},
        revision: 1, effective: {}, nodes: options.nodes ?? [],
      })
    }
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
    if (url === '/api/v1/model-defaults') return jsonResponse({ params: {}, revision: 0 })
    if (url === '/api/v1/model-presets') return jsonResponse({ presets: [] })
    if (url.startsWith('/api/v1/models')) {
      state.modelReads += 1
      return jsonResponse({ models: options.models ?? [configModel] })
    }
    return jsonResponse({})
  }
  const fetchMock = installFetch(fetchImpl)
  return { fetchMock, state }
}

function patchedChanges(state: ReturnType<typeof routes>['state']): Array<Record<string, unknown>> {
  return (state.patchBody?.changes ?? []) as Array<Record<string, unknown>>
}

function changeFor(state: ReturnType<typeof routes>['state'], path: string[]): Record<string, unknown> | undefined {
  return patchedChanges(state).find((change) => JSON.stringify(change.path) === JSON.stringify(path))
}

async function openConfigEditor(user: ReturnType<typeof userEvent.setup>) {
  renderWithProviders(<ModelsPage />, { route: '/models' })
  const button = await screen.findByRole('button', { name: '编辑配置' })
  await waitFor(() => expect(button).toBeEnabled())
  await user.click(button)
  return screen.getByRole('dialog')
}

async function chooseOption(user: ReturnType<typeof userEvent.setup>, trigger: HTMLElement, name: string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name }))
}

describe('config model overlay editor (#412)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows config model values with base/overlay/0028 sources and an edit action', async () => {
    const { fetchMock } = routes({ nodes: [nodeOverride] })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    expect(await screen.findByRole('button', { name: '编辑配置' })).toBeInTheDocument()
    expect(screen.getAllByText('primary').length).toBeGreaterThan(0)
    expect(fetchMock.mock.calls.some(([url]) => url === '/api/v1/config')).toBe(true)

    expect(screen.getAllByText('基础配置').length).toBeGreaterThan(0)
    expect(screen.getAllByText('overlay').length).toBeGreaterThan(0)
    const nodes = screen.getByText(/0028 节点覆盖/)
    expect(nodes).toHaveTextContent('review')
    expect(nodes).toHaveTextContent('max_tokens 1000')
    expect(screen.getByRole('button', { name: '在评审档位中清除' })).toBeInTheDocument()
    expect(screen.getByText('API key 已配置')).toBeInTheDocument()
  })

  it('saves edited fields to the overlay with both revisions and asks for a restart', async () => {
    const user = userEvent.setup()
    const { fetchMock, state } = routes()
    const dialog = await openConfigEditor(user)
    const form = within(dialog)

    expect(form.getByLabelText('协议')).toHaveTextContent('openai-chat')
    expect(form.getByLabelText('上游模型')).toHaveValue('deepseek-chat')
    expect(form.getByLabelText('max_tokens')).toHaveValue(8192)
    expect(form.getAllByText('来源：基础配置', { exact: false }).length).toBeGreaterThan(0)
    const configReadsBefore = state.configReads
    const modelReadsBefore = state.modelReads

    await user.clear(form.getByLabelText('max_tokens'))
    await user.type(form.getByLabelText('max_tokens'), '4096')
    await user.clear(form.getByLabelText('temperature'))
    await user.type(form.getByLabelText('temperature'), '0.5')
    await chooseOption(user, form.getByLabelText('reasoning.effort'), 'max')
    await chooseOption(user, form.getByLabelText('thinking.type'), 'disabled')
    await user.click(form.getByLabelText('strict_role_alternation'))
    await user.click(form.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(state.patchBody).toBeDefined())
    expect(state.patchBody?.base_revision).toBe('base-rev-1')
    expect(state.patchBody?.overlay_revision).toBe('overlay-rev-1')
    expect(changeFor(state, ['providers', 'primary', 'max_tokens'])).toMatchObject({ value: 4096 })
    expect(changeFor(state, ['providers', 'primary', 'temperature'])).toMatchObject({ value: 0.5 })
    expect(changeFor(state, ['providers', 'primary', 'reasoning', 'effort'])).toMatchObject({ value: 'max' })
    expect(changeFor(state, ['providers', 'primary', 'thinking', 'type'])).toMatchObject({ value: 'disabled' })
    expect(changeFor(state, ['providers', 'primary', 'strict_role_alternation'])).toMatchObject({ value: false })

    expect(await screen.findByText(/已保存到 overlay，待重启生效/)).toBeInTheDocument()
    expect(screen.getByText(/重启服务可能取消在途评审/)).toBeInTheDocument()
    await waitFor(() => expect(state.configReads).toBeGreaterThan(configReadsBefore))
    await waitFor(() => expect(state.modelReads).toBeGreaterThan(modelReadsBefore))
    expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/config' && init?.method !== 'PATCH')).toBe(true)
  })

  it('validates values against the provider allowlist before writing', async () => {
    const user = userEvent.setup()
    const { state } = routes()
    const dialog = await openConfigEditor(user)
    const form = within(dialog)

    await chooseOption(user, form.getByLabelText('协议'), 'anthropic')
    await user.clear(form.getByLabelText('max_tokens'))
    fireEvent.change(form.getByLabelText('thinking.budget_tokens'), { target: { value: '512' } })
    await user.click(form.getByRole('button', { name: '保存' }))
    expect(await form.findByText('anthropic 协议要求 max_tokens 大于 0，不能留空。')).toBeInTheDocument()
    expect(state.patchBody).toBeUndefined()

    fireEvent.change(form.getByLabelText('max_tokens'), { target: { value: '4096' } })
    await user.click(form.getByRole('button', { name: '保存' }))
    expect(await form.findByText('anthropic 的 thinking.budget_tokens 必须不小于 1024。')).toBeInTheDocument()
    expect(state.patchBody).toBeUndefined()

    fireEvent.change(form.getByLabelText('thinking.budget_tokens'), { target: { value: '1024' } })
    fireEvent.change(form.getByLabelText('temperature'), { target: { value: '3' } })
    await user.click(form.getByRole('button', { name: '保存' }))
    expect(await form.findByText('temperature 必须在 0 到 2 之间。')).toBeInTheDocument()
    expect(state.patchBody).toBeUndefined()
  })

  it('reports a stale overlay revision and reloads the file snapshot', async () => {
    const user = userEvent.setup()
    const { fetchMock } = routes({
      overlay: () => jsonResponse({
        error: {
          code: 'stale_config', message: 'the overlay changed; reload and retry',
          details: [{ field: 'changes', message: 'overlay_revision mismatch' }],
        },
      }, 409),
    })
    const dialog = await openConfigEditor(user)
    const before = fetchMock.mock.calls.filter(([url]) => url === '/api/v1/config').length
    await user.clear(within(dialog).getByLabelText('max_tokens'))
    await user.type(within(dialog).getByLabelText('max_tokens'), '2048')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('配置已被修改，请重新加载后再保存。')
    expect(screen.getByRole('button', { name: '重新加载' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '重新加载' }))
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => url === '/api/v1/config').length).toBeGreaterThan(before))
  })

  it('does not write when nothing changed', async () => {
    const user = userEvent.setup()
    const { state } = routes()
    const dialog = await openConfigEditor(user)
    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    expect(await within(dialog).findByText('没有需要保存的更改。')).toBeInTheDocument()
    expect(state.patchBody).toBeUndefined()
  })

  it('resets the whole model to base after confirmation', async () => {
    const user = userEvent.setup()
    const { state } = routes()
    const dialog = await openConfigEditor(user)
    await user.click(within(dialog).getByRole('button', { name: '重置为 base' }))
    const confirm = await screen.findByRole('dialog', { name: /重置配置模型/ })
    await user.click(within(confirm).getByRole('button', { name: '重置为 base' }))

    await waitFor(() => expect(state.patchBody).toBeDefined())
    expect(changeFor(state, ['providers', 'primary'])).toMatchObject({ delete: true })
    expect(state.patchBody?.changes).toHaveLength(1)
    expect(await screen.findByText(/已保存到 overlay，待重启生效/)).toBeInTheDocument()
    expect(screen.queryByRole('dialog', { name: /编辑配置模型/ })).not.toBeInTheDocument()
  })

  it('lists provider_in_use referrers when resetting the model to base and never touches the database', async () => {
    const user = userEvent.setup()
    const { state } = routes({
      overlay: () => jsonResponse({
        error: {
          code: 'provider_in_use', message: 'the provider is still referenced',
          details: [
            { field: 'changes', message: 'overrides.lanes.review.model refers to providers.primary' },
            { field: 'changes', message: 'profiles.default.lanes.review.provider refers to providers.primary' },
          ],
        },
      }, 409),
    })
    const dialog = await openConfigEditor(user)
    await user.click(within(dialog).getByRole('button', { name: '重置为 base' }))
    const confirm = await screen.findByRole('dialog', { name: /重置配置模型/ })
    await user.click(within(confirm).getByRole('button', { name: '重置为 base' }))

    expect(await screen.findByText(/该 provider 仍被引用/)).toBeInTheDocument()
    expect(screen.getByText('overrides.lanes.review.model refers to providers.primary')).toBeInTheDocument()
    expect(screen.getByText('profiles.default.lanes.review.provider refers to providers.primary')).toBeInTheDocument()
    expect(screen.getByText(/不会自动修改数据库或档位/)).toBeInTheDocument()
    expect(changeFor(state, ['providers', 'primary'])).toMatchObject({ delete: true })
    expect(state.patchBody?.changes).toHaveLength(1)
    expect(screen.getByRole('dialog', { name: /编辑配置模型/ })).toBeInTheDocument()
  })

  it('keeps credentials and api_key_env out of the config editor', async () => {
    const user = userEvent.setup()
    routes()
    const dialog = await openConfigEditor(user)
    const form = within(dialog)
    expect(form.queryByLabelText('api_key_env')).not.toBeInTheDocument()
    expect(form.queryByLabelText('api_key')).not.toBeInTheDocument()
    expect(form.getByText('API key 已配置（只读）')).toBeInTheDocument()
    expect(form.queryByDisplayValue(/sk-/)).not.toBeInTheDocument()
  })

  it('opens the profile editor to clear node overrides', async () => {
    const user = userEvent.setup()
    routes({ nodes: [nodeOverride] })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '在评审档位中清除' }))
    expect(await screen.findByRole('dialog', { name: '编辑评审档位' })).toBeInTheDocument()
  })

  it('hides the config editor from viewers', async () => {
    routes()
    renderWithProviders(<ModelsPage />, { route: '/models', me: viewerMe })
    expect(await screen.findByText('primary')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '编辑配置' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '在评审档位中清除' })).not.toBeInTheDocument()
  })

  it('renders the config editor at 390 and 1440 widths in both themes', async () => {
    const user = userEvent.setup()
    routes()
    const dialog = await openConfigEditor(user)
    for (const width of [390, 1440]) {
      Object.defineProperty(window, 'innerWidth', { configurable: true, value: width })
      for (const theme of ['light', 'dark']) {
        document.documentElement.dataset.theme = theme
        fireEvent(window, new Event('resize'))
        expect(within(dialog).getByLabelText('max_tokens')).toBeInTheDocument()
        expect(within(dialog).getByRole('button', { name: '保存' })).toBeInTheDocument()
        expect(within(dialog).getByRole('button', { name: '重置为 base' })).toBeInTheDocument()
      }
    }
    document.documentElement.dataset.theme = 'light'
  })
})
