import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
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

// A node whose model still comes from the configuration but whose parameters
// are overridden in 0028. Its `source` stays "config"; only param_source marks
// the override.
const paramOnlyOverride = {
  node: 'lanes.review', model: 'config:primary', source: 'config',
  configured: 'config:primary', configured_source: 'config',
  override: { params: { max_tokens: 700 } },
  params: { max_tokens: 700 }, param_source: 'override',
  effective_params: { max_tokens: 700 },
  vote_identity: 'api.example.test/deepseek-chat',
}

const defaultParamOverride = {
  node: 'default', model: 'config:primary', source: 'config',
  configured: 'config:primary', configured_source: 'config',
  override: { params: { temperature: 0.2 } },
  params: { temperature: 0.2 }, param_source: 'override',
  effective_params: { temperature: 0.2 },
  vote_identity: 'api.example.test/deepseek-chat',
}

// This node inherits the default-level parameter override; it has no own
// override and must not be listed a second time.
const inheritedParamOverride = {
  node: 'lanes.code_review', model: 'config:primary', source: 'config',
  configured: 'config:primary', configured_source: 'config',
  params: { temperature: 0.2 }, param_source: 'override_default',
  effective_params: { temperature: 0.2 },
  vote_identity: 'api.example.test/deepseek-chat',
}

interface RouteOptions {
  overlay?: (body: Record<string, unknown>) => Response
  // probe answers POST /api/v1/models/{id}/probe; the default is success.
  probe?: () => Response
  nodes?: unknown[]
  models?: unknown[]
  provider?: Record<string, unknown>
  // watcherRevision models another administrator publishing a higher revision
  // while the console waits for the watcher; the write response still names
  // the revision saved by this request.
  watcherRevision?: string
  // holdSnapshot keeps the snapshot at its pre-save revision so the test can
  // publish it later or exercise the timeout fallback.
  holdSnapshot?: boolean
}

function routes(options: RouteOptions = {}) {
  const state = {
    provider: { ...configPayload().config.providers[0], ...options.provider } as Record<string, unknown> | undefined,
    origins: { ...configPayload().origins } as Record<string, string>,
    overlayRevision: 'overlay-rev-1',
    configFails: false,
    patchBody: undefined as Record<string, unknown> | undefined,
    configReads: 0,
    modelReads: 0,
  }
  // applyChanges simulates the file watcher: after a successful PATCH the next
  // GET /api/v1/config returns the new file-effective values and origins.
  const applyChanges = (body: Record<string, unknown>) => {
    const changes = (body.changes ?? []) as Array<{ path: string[]; value?: unknown; delete?: boolean }>
    for (const change of changes) {
      const [, name, ...rest] = change.path
      if (!state.provider || name !== state.provider.name || !rest.length) {
        if (change.delete && change.path.length === 2 && state.provider?.name === name) state.provider = undefined
        continue
      }
      let current = state.provider
      for (const key of rest.slice(0, -1)) {
        if (!current[key] || typeof current[key] !== 'object') current[key] = {}
        current = current[key] as Record<string, unknown>
      }
      const leaf = rest[rest.length - 1]
      if (change.delete) delete current[leaf]
      else current[leaf] = change.value
      state.origins[`providers.${[name, ...rest].join('.')}`] = change.delete ? 'base' : 'overlay'
    }
    state.overlayRevision = 'overlay-rev-2'
  }
  const fetchImpl = (url: string, init?: RequestInit) => {
    if (url === '/api/v1/config/overlay' && init?.method === 'PATCH') {
      const body = JSON.parse(String(init.body)) as Record<string, unknown>
      state.patchBody = body
      if (options.overlay) return options.overlay(body)
      applyChanges(body)
      if (options.holdSnapshot) state.overlayRevision = String(body.overlay_revision)
      else if (options.watcherRevision) state.overlayRevision = options.watcherRevision
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
      if (state.configFails) {
        return jsonResponse({ error: { code: 'internal', message: 'snapshot unavailable' } }, 500)
      }
      return jsonResponse({
        ...configPayload({ overlayRevision: state.overlayRevision }),
        config: { providers: state.provider ? [state.provider] : [] },
        origins: state.origins,
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
    if (url.endsWith('/probe') && init?.method === 'POST') {
      if (options.probe) return options.probe()
      return jsonResponse({ ok: true, model: 'deepseek-chat', latency_ms: 120, probe: 'tool_call', tool_call: true })
    }
    if (url.endsWith('/test') && init?.method === 'POST') {
      return jsonResponse({ ok: true, model: 'deepseek-chat', latency_ms: 40 })
    }
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
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

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

  it('shows parameter-only 0028 overrides and lists the default level separately', async () => {
    routes({ nodes: [defaultParamOverride, paramOnlyOverride, inheritedParamOverride] })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await screen.findByRole('button', { name: '编辑配置' })
    const nodes = screen.getByText(/0028 节点覆盖/)
    expect(nodes).toHaveTextContent('default')
    expect(nodes).toHaveTextContent('temperature 0.2')
    expect(nodes).toHaveTextContent('review')
    expect(nodes).toHaveTextContent('max_tokens 700')
    // The inherited default-level parameters are not repeated on every node.
    expect(nodes).not.toHaveTextContent('code-review')
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
    // Only changed leaves are submitted: unchanged connection fields and
    // parameters stay out of the patch.
    expect(patchedChanges(state)).toHaveLength(5)
    expect(changeFor(state, ['providers', 'primary', 'model'])).toBeUndefined()
    expect(changeFor(state, ['providers', 'primary', 'base_url'])).toBeUndefined()
    expect(changeFor(state, ['providers', 'primary', 'thinking', 'budget_tokens'])).toBeUndefined()

    expect(await screen.findByText(/已保存到 overlay，待重启生效/)).toBeInTheDocument()
    expect(screen.getByText(/重启服务可能取消在途评审/)).toBeInTheDocument()
    // The refetched file-effective values reach the card, not just the
    // running-catalog copy; the edited field now reports the overlay origin.
    const paramsGroup = await screen.findByRole('group', { name: 'primary 参数' })
    await waitFor(() => expect(within(paramsGroup).getByText('4096')).toBeInTheDocument())
    const maxTokensEntry = within(paramsGroup).getByText('max_tokens').closest('span')
    expect(maxTokensEntry).not.toBeNull()
    expect(within(maxTokensEntry as HTMLElement).getByText('overlay')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('button', { name: '编辑配置' })).toBeEnabled())
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

  it('explains a missing overlay directory with actionable guidance', async () => {
    const user = userEvent.setup()
    routes({
      overlay: () => jsonResponse({
        error: {
          code: 'config_directory_create_failed',
          message: 'cannot create the configuration overlay directory',
          details: [{ field: 'changes', message: 'the overlay directory could not be created' }],
        },
      }, 500),
    })
    const dialog = await openConfigEditor(user)
    await user.clear(within(dialog).getByLabelText('max_tokens'))
    await user.type(within(dialog).getByLabelText('max_tokens'), '2048')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('无法创建 overlay 目录')
    expect(screen.getByRole('alert')).toHaveTextContent('数据卷')
  })

  it('separates a connectivity test from a real tool-call probe', async () => {
    const user = userEvent.setup()
    routes()
    const dialog = await openConfigEditor(user)
    await user.click(within(dialog).getByRole('button', { name: '工具调用测试' }))
    expect(await within(dialog).findByText(/工具调用测试成功/)).toHaveTextContent('submit_result')
    await user.click(within(dialog).getByRole('button', { name: '连通性测试' }))
    expect(await within(dialog).findByText(/连通性测试成功/)).toBeInTheDocument()
    expect(within(dialog).getByText(/测试针对已保存的模型/)).toBeInTheDocument()
  })

  it('explains a model that cannot return the submit_result tool call', async () => {
    const user = userEvent.setup()
    routes({
      probe: () => jsonResponse({
        error: { code: 'tool_call_unsupported', message: 'model answered without the submit_result tool call' },
      }, 422),
    })
    const dialog = await openConfigEditor(user)
    await user.click(within(dialog).getByRole('button', { name: '工具调用测试' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('没有返回 submit_result 工具调用')
  })

  it('does not write when nothing changed', async () => {
    const user = userEvent.setup()
    const { state } = routes()
    const dialog = await openConfigEditor(user)
    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    expect(await within(dialog).findByText('没有需要保存的更改。')).toBeInTheDocument()
    expect(state.patchBody).toBeUndefined()
  })

  it('allows an empty base_url and only validates a non-empty value', async () => {
    const user = userEvent.setup()
    const { state } = routes({ provider: { base_url: '' } })
    const dialog = await openConfigEditor(user)
    const form = within(dialog)
    expect(form.getByLabelText('base_url')).toHaveValue('')
    expect(form.getByText(/留空沿用协议适配器默认端点/)).toBeInTheDocument()

    fireEvent.change(form.getByLabelText('base_url'), { target: { value: 'ftp://api.example.test/v1' } })
    await user.click(form.getByRole('button', { name: '保存' }))
    expect(await form.findByText('base_url 必须是绝对 http(s) URL，不含凭据、查询或片段。')).toBeInTheDocument()
    expect(state.patchBody).toBeUndefined()

    fireEvent.change(form.getByLabelText('base_url'), { target: { value: '' } })
    await user.clear(form.getByLabelText('max_tokens'))
    await user.type(form.getByLabelText('max_tokens'), '4096')
    await user.click(form.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(state.patchBody).toBeDefined())
    expect(patchedChanges(state)).toHaveLength(1)
    expect(changeFor(state, ['providers', 'primary', 'max_tokens'])).toMatchObject({ value: 4096 })
    expect(changeFor(state, ['providers', 'primary', 'base_url'])).toBeUndefined()
  })

  it('offers effort none with the allow switch and saves both leaves', async () => {
    const user = userEvent.setup()
    const { state } = routes()
    const dialog = await openConfigEditor(user)
    const form = within(dialog)
    await chooseOption(user, form.getByLabelText('reasoning.effort'), 'none（需开启 allow_reasoning_effort_none）')
    await user.click(form.getByLabelText('allow_reasoning_effort_none'))
    await user.click(form.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(state.patchBody).toBeDefined())
    expect(changeFor(state, ['providers', 'primary', 'reasoning', 'effort'])).toMatchObject({ value: 'none' })
    expect(changeFor(state, ['providers', 'primary', 'allow_reasoning_effort_none'])).toMatchObject({ value: true })
  })

  it('shows explicit zero max_concurrency and rpm on the card and in the editor', async () => {
    const user = userEvent.setup()
    routes({ provider: { max_concurrency: 0, rpm: 0 } })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const button = await screen.findByRole('button', { name: '编辑配置' })
    await waitFor(() => expect(button).toBeEnabled())
    const params = screen.getByRole('group', { name: 'primary 参数' })
    expect(within(params).getByText('max_concurrency')).toBeInTheDocument()
    expect(within(params).getByText('0（不限制本地并发）')).toBeInTheDocument()
    expect(within(params).getByText('rpm')).toBeInTheDocument()
    expect(within(params).getByText('0（不限制）')).toBeInTheDocument()

    await user.click(button)
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByLabelText('max_concurrency')).toHaveValue(0)
    expect(within(dialog).getByLabelText('rpm')).toHaveValue(0)
  })

  it('maps server invalid_config details under the matching field', async () => {
    const user = userEvent.setup()
    const { state } = routes({
      overlay: () => jsonResponse({
        error: {
          code: 'invalid_config', message: 'candidate configuration is invalid',
          details: [{ field: 'changes', message: 'providers.primary.model must not be empty' }],
        },
      }, 400),
    })
    const dialog = await openConfigEditor(user)
    const form = within(dialog)
    await user.clear(form.getByLabelText('max_tokens'))
    await user.type(form.getByLabelText('max_tokens'), '4096')
    await user.click(form.getByRole('button', { name: '保存' }))

    expect(await form.findByText('providers.primary.model must not be empty')).toBeInTheDocument()
    expect(form.getByLabelText('上游模型')).toHaveAttribute('aria-invalid', 'true')
    expect(state.patchBody).toBeDefined()
  })

  it('reloads the draft and says so when the config changed externally', async () => {
    const user = userEvent.setup()
    const { state } = routes()
    const { queryClient } = renderWithProviders(<ModelsPage />, { route: '/models' })
    const button = await screen.findByRole('button', { name: '编辑配置' })
    await waitFor(() => expect(button).toBeEnabled())
    await user.click(button)
    const dialog = screen.getByRole('dialog')
    const form = within(dialog)
    await user.clear(form.getByLabelText('max_tokens'))
    await user.type(form.getByLabelText('max_tokens'), '1234')

    state.provider = { ...(state.provider ?? {}), max_tokens: 2048 }
    state.overlayRevision = 'overlay-rev-9'
    await queryClient.refetchQueries({ queryKey: ['config'] })

    expect(await form.findByText('配置已被他人修改，已重新加载。')).toBeInTheDocument()
    expect(form.getByLabelText('max_tokens')).toHaveValue(2048)
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

  it('marks a reset overlay-only provider as removed from the config until restart', async () => {
    const user = userEvent.setup()
    routes()
    const dialog = await openConfigEditor(user)
    await user.click(within(dialog).getByRole('button', { name: '重置为 base' }))
    const confirm = await screen.findByRole('dialog', { name: /重置配置模型/ })
    await user.click(within(confirm).getByRole('button', { name: '重置为 base' }))

    // The runtime catalog still lists the model until the next restart, but
    // the file snapshot no longer defines the provider.
    expect(await screen.findByText('已从配置移除，重启后消失。')).toBeInTheDocument()
    expect(screen.getAllByText('primary').length).toBeGreaterThan(0)
    expect(screen.queryByText('配置文件快照不可用，暂不能在控制台编辑。')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '编辑配置' })).not.toBeInTheDocument()
  })

  it('keeps the apply state while the snapshot is still at the pre-save revision', async () => {
    const user = userEvent.setup()
    const { state } = routes({ holdSnapshot: true })
    const dialog = await openConfigEditor(user)
    await user.clear(within(dialog).getByLabelText('max_tokens'))
    await user.type(within(dialog).getByLabelText('max_tokens'), '4096')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))

    expect(await screen.findByText('配置快照应用中，稍后可再次编辑。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '编辑配置' })).toBeDisabled()
    expect(screen.getByText(/正在应用配置快照/)).toBeInTheDocument()

    state.overlayRevision = 'overlay-rev-2'
    await waitFor(() => expect(screen.getByRole('button', { name: '编辑配置' })).toBeEnabled(), { timeout: 3000 })
    expect(await screen.findByText(/已保存到 overlay，待重启生效/)).toBeInTheDocument()
  })

  it('ends the apply state when another writer publishes a higher revision', async () => {
    const user = userEvent.setup()
    routes({ watcherRevision: 'overlay-rev-3' })
    const dialog = await openConfigEditor(user)
    await user.clear(within(dialog).getByLabelText('max_tokens'))
    await user.type(within(dialog).getByLabelText('max_tokens'), '4096')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))

    // The write response names overlay-rev-2, but the snapshot jumps straight
    // to overlay-rev-3; any newer revision ends the apply state (#441).
    expect(await screen.findByText(/已保存到 overlay，待重启生效/)).toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('button', { name: '编辑配置' })).toBeEnabled())
    expect(screen.queryByText('配置快照应用中，稍后可再次编辑。')).not.toBeInTheDocument()
  })

  it('falls back after the watcher timeout and re-enables editing', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    routes({ holdSnapshot: true })
    const dialog = await openConfigEditor(user)
    await user.clear(within(dialog).getByLabelText('max_tokens'))
    await user.type(within(dialog).getByLabelText('max_tokens'), '4096')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))
    expect(await screen.findByText('配置快照应用中，稍后可再次编辑。')).toBeInTheDocument()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(16_000)
    })
    expect(screen.getByText(/配置快照未在预期时间内更新，已恢复编辑/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '编辑配置' })).toBeEnabled()
  })

  it('falls back when the snapshot refresh fails and re-enables editing', async () => {
    const user = userEvent.setup()
    const { state } = routes({ holdSnapshot: true })
    const dialog = await openConfigEditor(user)
    await user.clear(within(dialog).getByLabelText('max_tokens'))
    await user.type(within(dialog).getByLabelText('max_tokens'), '4096')
    state.configFails = true
    await user.click(within(dialog).getByRole('button', { name: '保存' }))

    expect(await screen.findByText(/配置快照刷新失败，已恢复编辑/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '编辑配置' })).toBeEnabled()
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

  it('hides the config editor from viewers and explains the read-only source', async () => {
    routes()
    renderWithProviders(<ModelsPage />, { route: '/models', me: viewerMe })
    expect(await screen.findByText('primary')).toBeInTheDocument()
    expect(screen.getByText('只读：仅平台管理员可通过配置文件（overlay）编辑。')).toBeInTheDocument()
    expect(screen.queryByText('配置文件快照不可用，暂不能在控制台编辑。')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '编辑配置' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '在评审档位中清除' })).not.toBeInTheDocument()
  })

  // jsdom has no layout engine, so this only proves the dialog renders at both
  // widths and themes plus the responsive grid contract in the stylesheet.
  // Pixel layout at 1440/390 is checked manually per AGENTS.md.
  it('renders the config editor at 390 and 1440 widths in both themes (render-only)', async () => {
    const css = readFileSync(resolve(process.cwd(), 'src/pages/insights.module.css'), 'utf8')
    expect(css).toMatch(/\.modelFormGrid\s*\{[^}]*grid-template-columns:\s*repeat\(2, minmax\(0, 1fr\)\)/)
    expect(css).toMatch(/@media \(max-width: 640px\)\s*\{[\s\S]*?\.modelFormGrid\s*\{[^}]*grid-template-columns:\s*minmax\(0, 1fr\)/)

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
