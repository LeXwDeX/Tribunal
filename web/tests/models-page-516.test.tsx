import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const userModel = {
  id: 'user:aaaaaaaa-1111', display_name: 'same', source: 'user', read_only: false,
  protocol: 'openai-chat', base_url: 'https://example.com/v1', model: 'm',
  params: { max_tokens: 100 }, has_api_key: true, available: true, revision: 1,
  referenced_by: [], vote_identity: 'example.com/m',
}

function routes() {
  return (url: string, init?: RequestInit) => {
    if (url === '/api/v1/models:test') return jsonResponse({ ok: true, model: 'm' })
    if (url.startsWith('/api/v1/models') && init?.method === 'PATCH') {
      return jsonResponse({ ...userModel, revision: 2 })
    }
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models: [userModel] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
    return jsonResponse({})
  }
}

describe('user model updates (#516)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('omits the client-only id from the update body while the draft test keeps it', async () => {
    const user = userEvent.setup()
    const fetchMock = installFetch(routes())
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.click(screen.getByRole('button', { name: '保存前连通性测试' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => url === '/api/v1/models:test')).toBe(true))
    const draft = fetchMock.mock.calls.find(([url]) => url === '/api/v1/models:test')
    expect(JSON.parse(String(draft?.[1]?.body)).id).toBe('user:aaaaaaaa-1111')

    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PATCH')).toBe(true))
    const patch = fetchMock.mock.calls.find(([, init]) => init?.method === 'PATCH')
    const body = JSON.parse(String(patch?.[1]?.body))
    expect(body.id).toBeUndefined()
    expect(body.revision).toBe(1)
    expect(body.display_name).toBe('same')
    expect(body.params.max_tokens).toBe(100)
  })
})