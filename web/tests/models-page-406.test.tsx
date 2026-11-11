import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const v4Preset = {
  id: 'deepseek-v4-pro-max',
  display_name: 'DeepSeek V4 Pro (maximum)',
  protocol: 'openai-chat',
  base_url: 'https://api.deepseek.com',
  model: 'deepseek-v4-pro',
  params: {
    max_tokens: 393216,
    context_window: 655360,
    reasoning: { effort: 'max' },
    forced_tool_choice: 'unsupported',
    strict_role_alternation: false,
    max_concurrency: 500,
    rpm: 0,
    preserve_reasoning_content: true,
  },
  source_url: 'https://api-docs.deepseek.com/quick_start/pricing',
  verified_at: '2026-11-11',
}

const flashPreset = {
  ...v4Preset,
  id: 'deepseek-flash-max',
  display_name: 'DeepSeek Flash (maximum)',
  model: 'deepseek-flash',
  params: { ...v4Preset.params, max_concurrency: 2500 },
}

const storedModel = {
  id: 'user:aaaaaaaa-1111', display_name: 'stored', source: 'user', read_only: false,
  protocol: 'openai-chat', base_url: 'https://example.com/v1', model: 'm',
  params: {
    max_tokens: 8192, context_window: 128000, temperature: 0.7,
    reasoning: { effort: 'high' }, thinking: { budget_tokens: 2048, type: 'enabled' },
    forced_tool_choice: 'supported', strict_role_alternation: true, max_concurrency: 4,
    rpm: 30, preserve_reasoning_content: true,
    price: { input_per_mtok: 3, output_per_mtok: 15 },
  },
  has_api_key: true, available: true, revision: 7, referenced_by: [], vote_identity: 'example.com/m',
}

function routes(overrides: {
  models?: unknown[]
  presets?: unknown[]
  presetsResponse?: Response | Promise<Response>
  test?: Response
  create?: Response
} = {}) {
  return (url: string, init?: RequestInit) => {
    if (url === '/api/v1/model-presets') {
      return overrides.presetsResponse ?? jsonResponse({ presets: overrides.presets ?? [v4Preset, flashPreset] })
    }
    if (url === '/api/v1/models:test') return overrides.test ?? jsonResponse({ ok: true, model: 'm' })
    if (url === '/api/v1/models' && init?.method === 'POST') {
      return overrides.create ?? jsonResponse({ id: 'user:new', effective: 'new_workflows' })
    }
    if (url.startsWith('/api/v1/models') && init?.method === 'PATCH') {
      return jsonResponse(overrides.models?.[0] ?? storedModel)
    }
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models: overrides.models ?? [storedModel] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
    return jsonResponse({})
  }
}

async function chooseOption(user: ReturnType<typeof userEvent.setup>, trigger: HTMLElement, name: string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name }))
}

async function openCreate(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: '新增模型' }))
}

async function fillConnection(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('显示名'), 'deepseek')
  await user.type(screen.getByLabelText('base_url'), 'https://api.deepseek.com')
  await user.type(screen.getByLabelText('上游模型'), 'deepseek-v4-pro')
}

function postedBody(fetchMock: ReturnType<typeof installFetch>, method: string): Record<string, unknown> {
  const call = fetchMock.mock.calls.find(([url, init]) => String(url).startsWith('/api/v1/models') && init?.method === method)
  return JSON.parse(String(call?.[1]?.body))
}

describe('model form capability fields (#406)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('creates a model with blank max_tokens and no concurrency default', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes({ models: [] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)

    expect(screen.getByLabelText('max_tokens')).toHaveValue(null)
    expect(screen.getByLabelText('max_concurrency')).toHaveValue(null)
    expect(screen.getByText(/模型默认（不发送；DeepSeek 默认 8K\/64K\/128K，不等于最大）/)).toBeInTheDocument()

    await fillConnection(user)
    await user.click(screen.getByRole('button', { name: '创建' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/models' && init?.method === 'POST')).toBe(true))
    const body = postedBody(fetchMock, 'POST')
    expect(body.params).toMatchObject({ strict_role_alternation: false, preserve_reasoning_content: false })
    expect((body.params as Record<string, unknown>).max_tokens).toBeUndefined()
    expect((body.params as Record<string, unknown>).max_concurrency).toBeUndefined()
    expect((body.params as Record<string, unknown>).price).toBeUndefined()
  })

  it('rejects a blank max_tokens for anthropic before submitting', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes({ models: [] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    await fillConnection(user)
    await chooseOption(user, screen.getByLabelText('协议'), 'anthropic')

    await user.click(screen.getByRole('button', { name: '创建' }))
    expect(await screen.findByText('anthropic 协议要求 max_tokens 大于 0，不能留空。')).toBeInTheDocument()
    expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/models' && init?.method === 'POST')).toBe(false)
  })

  it('fills every field from an API preset without saving it', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes({ models: [] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)

    expect(await screen.findByRole('button', { name: 'DeepSeek V4 Pro 最大能力' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'DeepSeek V4 Pro 最大能力' }))
    expect(screen.getByLabelText('上游模型')).toHaveValue('deepseek-v4-pro')
    expect(screen.getByLabelText('max_concurrency')).toHaveValue(500)
    await user.click(screen.getByRole('button', { name: 'DeepSeek Flash 最大能力' }))

    expect(screen.getByLabelText('显示名')).toHaveValue('DeepSeek Flash (maximum)')
    expect(screen.getByLabelText('base_url')).toHaveValue('https://api.deepseek.com')
    expect(screen.getByLabelText('上游模型')).toHaveValue('deepseek-flash')
    expect(screen.getByLabelText('max_tokens')).toHaveValue(393216)
    expect(screen.getByLabelText('context_window')).toHaveValue(655360)
    expect(screen.getByLabelText('max_concurrency')).toHaveValue(2500)
    expect(screen.getByLabelText('rpm')).toHaveValue(0)
    expect(screen.getByLabelText('reasoning.effort')).toHaveTextContent('max')
    expect(screen.getByLabelText('forced_tool_choice')).toHaveTextContent('unsupported')
    expect(screen.getByLabelText('strict_role_alternation')).not.toBeChecked()
    expect(screen.getByLabelText('preserve_reasoning_content')).toBeChecked()
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false)

    await user.click(screen.getByRole('button', { name: '创建' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/models' && init?.method === 'POST')).toBe(true))
    expect(postedBody(fetchMock, 'POST').params).toMatchObject({
      max_tokens: 393216,
      context_window: 655360,
      reasoning: { effort: 'max' },
      forced_tool_choice: 'unsupported',
      strict_role_alternation: false,
      max_concurrency: 2500,
      rpm: 0,
      preserve_reasoning_content: true,
    })
  })

  it('echoes every stored capability field when editing and never submits price', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes({ models: [storedModel] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))

    expect(screen.getByLabelText('max_tokens')).toHaveValue(8192)
    expect(screen.getByLabelText('context_window')).toHaveValue(128000)
    expect(screen.getByLabelText('max_concurrency')).toHaveValue(4)
    expect(screen.getByLabelText('rpm')).toHaveValue(30)
    expect(screen.getByLabelText('temperature')).toHaveValue(0.7)
    expect(screen.getByLabelText('reasoning.effort')).toHaveTextContent('high')
    expect(screen.getByLabelText('thinking.type')).toHaveTextContent('enabled')
    expect(screen.getByLabelText('thinking.budget_tokens')).toHaveValue(2048)
    expect(screen.getByLabelText('forced_tool_choice')).toHaveTextContent('supported')
    expect(screen.getByLabelText('strict_role_alternation')).toBeChecked()
    expect(screen.getByLabelText('preserve_reasoning_content')).toBeChecked()

    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PATCH')).toBe(true))
    const params = postedBody(fetchMock, 'PATCH').params as Record<string, unknown>
    expect(params).toMatchObject({
      max_tokens: 8192, context_window: 128000, max_concurrency: 4, rpm: 30, temperature: 0.7,
      reasoning: { effort: 'high' }, thinking: { budget_tokens: 2048, type: 'enabled' },
      forced_tool_choice: 'supported', strict_role_alternation: true, preserve_reasoning_content: true,
    })
    expect(params.price).toBeUndefined()
  })

  it('warns when max_concurrency is explicitly zero', async () => {
    const user = userEvent.setup()
    installFetch(routes({ models: [] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    fireEvent.change(screen.getByLabelText('max_concurrency'), { target: { value: '0' } })
    expect(await screen.findByText(/不创建本地并发信号量，不限制本地并发/)).toBeInTheDocument()
  })

  it('shows API field errors under the matching controls', async () => {
    const user = userEvent.setup()
    installFetch(routes({
      models: [],
      create: jsonResponse({
        error: {
          code: 'validation_failed',
          message: 'invalid model',
          details: [
            { field: 'params.max_tokens', message: 'params.max_tokens must not be negative' },
            { field: 'api_key', message: 'api_key is required when the endpoint changes' },
            { field: 'params.strict_role_alternation', message: 'must be a boolean' },
          ],
        },
      }, 400),
    }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    await fillConnection(user)
    await user.click(screen.getByRole('button', { name: '创建' }))

    const tokens = await screen.findByLabelText('max_tokens')
    expect(await screen.findByText('params.max_tokens must not be negative')).toBeInTheDocument()
    expect(tokens).toHaveAttribute('aria-invalid', 'true')
    expect(tokens).toHaveAttribute('aria-describedby', expect.stringContaining('-error'))
    expect(screen.getByText('api_key is required when the endpoint changes')).toBeInTheDocument()
    const keyMode = screen.getByLabelText('API key 状态')
    expect(keyMode).toHaveAttribute('aria-invalid', 'true')
    expect(keyMode).toHaveAttribute('aria-describedby', expect.stringContaining('-error'))
    const strict = screen.getByLabelText('strict_role_alternation')
    expect(strict).toHaveAttribute('aria-invalid', 'true')
    expect(strict).toHaveAttribute('aria-describedby', expect.stringContaining('-error'))
    expect(screen.getByText('must be a boolean')).toBeInTheDocument()
  })

  it('surfaces save failures that carry no field details', async () => {
    const user = userEvent.setup()
    installFetch(routes({
      models: [],
      create: jsonResponse({ error: { code: 'provider_test_failed', message: 'provider test failed' } }, 422),
    }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    await fillConnection(user)
    await user.click(screen.getByRole('button', { name: '创建' }))
    expect(await screen.findByText('provider test failed')).toBeInTheDocument()
  })

  it('surfaces draft test failures that carry no field details', async () => {
    const user = userEvent.setup()
    installFetch(routes({
      models: [],
      test: jsonResponse({ error: { code: 'provider_test_failed', message: 'provider test failed' } }, 422),
    }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    await fillConnection(user)
    await user.click(screen.getByRole('button', { name: '保存前测试' }))
    expect(await screen.findByText('provider test failed')).toBeInTheDocument()
  })

  it('rejects an anthropic thinking budget below the minimum', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes({ models: [] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    await fillConnection(user)
    await chooseOption(user, screen.getByLabelText('协议'), 'anthropic')
    await user.type(screen.getByLabelText('max_tokens'), '4096')
    fireEvent.change(screen.getByLabelText('thinking.budget_tokens'), { target: { value: '512' } })
    await user.click(screen.getByRole('button', { name: '创建' }))
    expect(await screen.findByText('anthropic 的 thinking.budget_tokens 必须不小于 1024。')).toBeInTheDocument()
    expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/models' && init?.method === 'POST')).toBe(false)

    fireEvent.change(screen.getByLabelText('thinking.budget_tokens'), { target: { value: '1024' } })
    await user.click(screen.getByRole('button', { name: '创建' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/models' && init?.method === 'POST')).toBe(true))
  })

  it('does not offer preset fill when editing an existing model', async () => {
    const user = userEvent.setup()
    installFetch(routes({ models: [storedModel] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    expect(await screen.findByRole('button', { name: '保存' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /最大能力/ })).not.toBeInTheDocument()
  })

  it('shows a loading state while presets load', async () => {
    const user = userEvent.setup()
    installFetch(routes({ models: [], presetsResponse: new Promise<Response>(() => {}) }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    expect(await screen.findByText('预设加载中…')).toBeInTheDocument()
  })

  it('shows a retryable error when presets fail to load', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes({
      models: [],
      presetsResponse: jsonResponse({ error: { code: 'unavailable', message: 'unavailable' } }, 503),
    }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    expect(await screen.findByText('预设加载失败，仍可手动填写。')).toBeInTheDocument()
    const before = fetchMock.mock.calls.filter(([url]) => url === '/api/v1/model-presets').length
    await user.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => url === '/api/v1/model-presets').length).toBe(before + 1))
  })

  it('shows the YAML path on read-only config model cards and no price summary', async () => {
    installFetch(routes({
      models: [{
        ...storedModel, id: 'config:primary', display_name: 'primary', source: 'config', read_only: true,
      }],
    }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    expect(await screen.findByText('primary')).toBeInTheDocument()
    expect(screen.getByText('providers.primary')).toBeInTheDocument()
    expect(screen.queryByText(/价格（每百万 token）/)).not.toBeInTheDocument()
    expect(screen.queryByText(/input_per_mtok/)).not.toBeInTheDocument()
  })

  it('keeps the grouped form grid responsive and the form usable in dark theme at 390 and 1440 widths', async () => {
    const css = readFileSync(resolve(process.cwd(), 'src/pages/insights.module.css'), 'utf8')
    expect(css).toMatch(/\.modelFormGrid\s*\{[^}]*grid-template-columns:\s*repeat\(2, minmax\(0, 1fr\)\)/)
    expect(css).toMatch(/@media \(max-width: 640px\)\s*\{[\s\S]*?\.modelFormGrid\s*\{[^}]*grid-template-columns:\s*minmax\(0, 1fr\)/)

    const user = userEvent.setup()
    installFetch(routes({ models: [] }))
    document.documentElement.dataset.theme = 'dark'
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await openCreate(user)
    for (const width of [390, 1440]) {
      Object.defineProperty(window, 'innerWidth', { configurable: true, value: width })
      expect(screen.getByLabelText('max_tokens')).toBeInTheDocument()
      expect(screen.getByLabelText('context_window')).toBeInTheDocument()
      expect(screen.getByLabelText('preserve_reasoning_content')).toBeInTheDocument()
    }
    delete document.documentElement.dataset.theme
  })
})
