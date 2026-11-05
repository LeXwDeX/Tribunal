import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const models = [
  { id: 'config:base', display_name: 'base', source: 'config', read_only: true, protocol: 'openai-chat', base_url: 'https://example.com', model: 'm', params: {}, has_api_key: true, available: true, revision: 0, referenced_by: [], vote_identity: 'example.com/m' },
  { id: 'user:one', display_name: 'one', source: 'user', read_only: false, protocol: 'openai-chat', base_url: 'https://example.com', model: 'm', params: {}, has_api_key: true, available: true, revision: 1, referenced_by: [], vote_identity: 'example.com/m' },
  { id: 'user:bad', display_name: 'bad', source: 'user', read_only: false, protocol: 'openai-chat', base_url: 'https://example.com', model: 'm', params: {}, has_api_key: true, available: false, unavailable_reason: 'credential', revision: 1, referenced_by: [], vote_identity: 'example.com/m' },
]

const detail = {
  profile: 'default', overrides: {}, revision: 3,
  effective: { provider: 'base', challenge: { provider: 'base' } },
  nodes: [
    { node: 'default', model: 'base', source: 'config' },
    { node: 'explore', model: 'base', source: 'derived' },
    { node: 'lanes.code_review', model: 'base', source: 'config_default' },
    { node: 'challenge', model: 'one', source: 'override' },
  ],
}

function mock() {
  return installFetch((url, init) => {
    if (url === '/api/v1/profiles/default' && init?.method === 'PUT') return jsonResponse(detail, 200)
    if (url.startsWith('/api/v1/profiles/default') && init?.method === 'DELETE') return jsonResponse(detail, 200)
    if (url === '/api/v1/profiles/default') return jsonResponse(detail)
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default', config: detail.effective }, { name: 'onboarding', config: {} }] })
    return jsonResponse({})
  })
}

describe('profile overrides UI (#293)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows node sources and disables unavailable models', async () => {
    mock()
    const user = userEvent.setup()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    expect(await screen.findByText('覆盖')).toBeInTheDocument()
    expect(screen.getByText('配置默认')).toBeInTheDocument()
    expect(screen.getByText('派生')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '编辑档位' }))
    expect(screen.getAllByRole('option', { name: /bad/ }).every((option) => option.hasAttribute('disabled'))).toBe(true)
  })

  it('saves a lane override and resets to configuration', async () => {
    const fetchMock = mock()
    const user = userEvent.setup()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑档位' }))
    await user.selectOptions(screen.getByLabelText('review'), 'user:one')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/profiles/default/overrides' && init?.method === 'PUT')).toBe(true))
    const call = fetchMock.mock.calls.find(([url, init]) => url === '/api/v1/profiles/default/overrides' && init?.method === 'PUT')
    const body = JSON.parse(String(call?.[1]?.body))
    expect(body.revision).toBe(3)
    expect(body.overrides.lanes.review.model).toBe('user:one')

    await user.click(screen.getByRole('button', { name: '编辑档位' }))
    await user.click(screen.getByRole('button', { name: '恢复为配置' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url.startsWith('/api/v1/profiles/default/overrides') && init?.method === 'DELETE')).toBe(true))
  })
})
