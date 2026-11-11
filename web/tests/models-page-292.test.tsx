import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const userModel = {
  id: 'user:aaaaaaaa-1111', display_name: 'same', source: 'user', read_only: false,
  protocol: 'openai-chat', base_url: 'https://example.com/v1', model: 'm',
  params: { max_tokens: 100 }, has_api_key: true, available: true, revision: 1,
  referenced_by: [], vote_identity: 'example.com/m',
}

function routes(overrides: Record<string, unknown> = {}) {
  return (url: string, init?: RequestInit) => {
    if (url === '/api/v1/models:test') return jsonResponse(overrides.test ?? { ok: true, model: 'm' })
    if (url.startsWith('/api/v1/models') && init?.method === 'PATCH') return jsonResponse(overrides.patch ?? userModel)
    if (url.startsWith('/api/v1/models') && init?.method === 'DELETE') return jsonResponse(overrides.delete ?? {})
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models: overrides.models ?? [userModel] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
    return jsonResponse({})
  }
}

async function chooseOption(user: ReturnType<typeof userEvent.setup>, trigger: HTMLElement, name: string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name }))
}

describe('models page management (#292)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('keeps configured models read-only and shows unavailable reasons', async () => {
    installFetch(routes({
      models: [
        { ...userModel, id: 'config:primary', display_name: 'primary', source: 'config', read_only: true },
        { ...userModel, id: 'user:bad', display_name: 'bad', available: false, unavailable_reason: 'credential' },
      ],
    }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    expect(await screen.findByText('primary')).toBeInTheDocument()
    expect(screen.queryAllByRole('button', { name: '编辑' })).toHaveLength(1)
    expect(screen.getByText(/不可用原因：credential/)).toBeInTheDocument()
  })

  it('separates same-name models with short ids', async () => {
    installFetch(routes({
      models: [
        userModel,
        { ...userModel, id: 'user:bbbbbbbb-2222', display_name: 'same', revision: 2 },
      ],
    }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    expect(await screen.findByText(/user:aaa/)).toBeInTheDocument()
    expect(screen.getByText(/user:bbb/)).toBeInTheDocument()
  })

  it('omits the key when it stays unchanged and sends it when replaced', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes())
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.click(screen.getByRole('button', { name: '保存前测试' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => url === '/api/v1/models:test')).toBe(true))
    let body = JSON.parse(String(fetchMock.mock.calls.find(([url]) => url === '/api/v1/models:test')?.[1]?.body))
    expect(body.api_key).toBeUndefined()

    await chooseOption(user, screen.getByLabelText('API key 状态'), '替换')
    await user.type(screen.getByLabelText('API key', { selector: 'input' }), 'secret-canary')
    await user.click(screen.getByRole('button', { name: '保存前测试' }))
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => url === '/api/v1/models:test')).toHaveLength(2))
    const calls = fetchMock.mock.calls.filter(([url]) => url === '/api/v1/models:test')
    body = JSON.parse(String(calls[calls.length - 1][1]?.body))
    expect(body.api_key).toBe('secret-canary')
    expect(body.id).toBe('user:aaaaaaaa-1111')
  })

  it('shows revision conflicts and referenced deletes', async () => {
    const user = userEvent.setup()
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/models') && init?.method === 'PATCH') {
        return jsonResponse({ error: { code: 'stale_revision', message: 'revision does not match' } }, 409)
      }
      if (url.startsWith('/api/v1/models') && init?.method === 'DELETE') {
        return jsonResponse({ error: { code: 'referenced', message: 'model is referenced', details: [{ field: 'references', message: 'default/lanes.review' }] } }, 409)
      }
      if (url.startsWith('/api/v1/models')) return jsonResponse({ models: [userModel] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText('已被他人修改，请重新加载。')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '取消' }))
    await user.click(screen.getByRole('button', { name: '删除' }))
    await user.click(screen.getAllByRole('button', { name: '删除' }).at(-1)!)
    expect(await screen.findByText(/default\/lanes.review/)).toBeInTheDocument()
  })

  it('explains egress denials', async () => {
    const user = userEvent.setup()
    installFetch((url) => {
      if (url === '/api/v1/models:test') return jsonResponse({ error: { code: 'egress_denied', message: 'denied' } }, 422)
      if (url.startsWith('/api/v1/models')) return jsonResponse({ models: [userModel] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.click(screen.getByRole('button', { name: '保存前测试' }))
    expect(await screen.findByText(/出站地址被拒绝/)).toBeInTheDocument()
  })

  it('sends max_concurrency and preserves stored params when editing', async () => {
    const user = userEvent.setup()
    const stored = {
      ...userModel,
      params: {
        max_tokens: 100, max_concurrency: 3, rpm: 60, context_window: 8000, forced_tool_choice: 'auto',
        strict_role_alternation: true, thinking: { budget_tokens: 500 }, price: { input_per_mtok: 1, output_per_mtok: 2 },
        reasoning: { effort: 'low' },
      },
    }
    const fetchMock = installFetch(routes({ models: [stored] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    expect(screen.getByLabelText('max_concurrency')).toHaveValue(3)
    await chooseOption(user, screen.getByLabelText('reasoning.effort'), 'high')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PATCH')).toBe(true))
    const patch = fetchMock.mock.calls.find(([, init]) => init?.method === 'PATCH')
    const body = JSON.parse(String(patch?.[1]?.body))
    const { price: _price, ...expected } = stored.params
    expect(body.params).toEqual({ ...expected, reasoning: { effort: 'high' }, preserve_reasoning_content: false })
  })

  it('keeps the new-model defaults and accepts explicit zero concurrency', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes({ models: [] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '新增模型' }))
    expect(screen.getByLabelText('max_concurrency')).toHaveValue(null)
    await user.type(screen.getByLabelText('max_tokens'), '256')
    await user.click(screen.getByRole('button', { name: '保存前测试' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => url === '/api/v1/models:test')).toBe(true))
    let testCall = fetchMock.mock.calls.find(([url]) => url === '/api/v1/models:test')
    expect(JSON.parse(String(testCall?.[1]?.body)).params).toEqual({
      max_tokens: 256, strict_role_alternation: false, preserve_reasoning_content: false,
    })

    await user.clear(screen.getByLabelText('max_concurrency'))
    await user.type(screen.getByLabelText('max_concurrency'), '0')
    await user.click(screen.getByRole('button', { name: '保存前测试' }))
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => url === '/api/v1/models:test')).toHaveLength(2))
    const calls = fetchMock.mock.calls.filter(([url]) => url === '/api/v1/models:test')
    testCall = calls[calls.length - 1]
    expect(JSON.parse(String(testCall?.[1]?.body)).params).toEqual({
      max_tokens: 256, max_concurrency: 0, strict_role_alternation: false, preserve_reasoning_content: false,
    })

    fireEvent.change(screen.getByLabelText('max_concurrency'), { target: { value: '-1' } })
    await user.click(screen.getByRole('button', { name: '保存前测试' }))
    expect(await screen.findByText('max_concurrency 必须是不小于 0 的整数。')).toBeInTheDocument()
    expect(fetchMock.mock.calls.filter(([url]) => url === '/api/v1/models:test')).toHaveLength(2)
  })

  it('preserves a stored zero max_concurrency when editing', async () => {
    const user = userEvent.setup()
    const stored = { ...userModel, params: { max_tokens: 100, max_concurrency: 0 } }
    const fetchMock = installFetch(routes({ models: [stored] }))
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    expect(screen.getByLabelText('max_concurrency')).toHaveValue(0)
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PATCH')).toBe(true))
    const patch = fetchMock.mock.calls.find(([, init]) => init?.method === 'PATCH')
    expect(JSON.parse(String(patch?.[1]?.body)).params.max_concurrency).toBe(0)
  })
})
