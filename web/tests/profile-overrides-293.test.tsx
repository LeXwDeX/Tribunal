import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const models = [
  { id: 'config:base', display_name: 'base', source: 'config', read_only: true, protocol: 'openai-chat', base_url: 'https://example.com', model: 'm', params: {}, has_api_key: true, available: true, revision: 0, referenced_by: [], vote_identity: 'example.com/m' },
  { id: 'user:one', display_name: 'one', source: 'user', read_only: false, protocol: 'openai-chat', base_url: 'https://example.com', model: 'm', params: {}, has_api_key: true, available: true, revision: 1, referenced_by: [], vote_identity: 'example.com/m' },
  { id: 'user:bad', display_name: 'bad', source: 'user', read_only: false, protocol: 'openai-chat', base_url: 'https://example.com', model: 'm', params: {}, has_api_key: true, available: false, unavailable_reason: 'credential', revision: 1, referenced_by: [], vote_identity: 'example.com/m' },
]

const detail = {
  profile: 'default', overrides: { challenge: { model: 'user:one' } } as Record<string, unknown>, revision: 3,
  effective: { provider: 'base', challenge: { provider: 'base' } },
  nodes: [
    { node: 'default', model: 'base', source: 'config' },
    { node: 'explore', model: 'base', source: 'derived' },
    { node: 'lanes.code_review', model: 'base', source: 'config_default' },
    { node: 'challenge', model: 'one', source: 'override' },
  ],
}

function mock(profile = detail) {
  return installFetch((url, init) => {
    if (url === '/api/v1/profiles/default' && init?.method === 'PUT') return jsonResponse(profile, 200)
    if (url.startsWith('/api/v1/profiles/default') && init?.method === 'DELETE') return jsonResponse(profile, 200)
    if (url === '/api/v1/profiles/default') return jsonResponse(profile)
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default', config: profile.effective }, { name: 'onboarding', config: {} }] })
    return jsonResponse({})
  })
}

const PROFILE_LABELS = ['默认', '探索', 'code-review', 'review', 'reasoner', 'validate', 'challenge']

async function chooseOption(user: ReturnType<typeof userEvent.setup>, trigger: HTMLElement, name: RegExp | string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name }))
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
    for (const label of PROFILE_LABELS) {
      await user.click(screen.getByRole('combobox', { name: `${label} 模型` }))
      const options = await screen.findAllByRole('option', { name: /bad.*不可用：credential/ })
      expect(options).toHaveLength(1)
      expect(options[0]).toHaveAttribute('aria-disabled', 'true')
      await user.keyboard('{Escape}')
    }
  })

  it('round-trips a stored thinking.type when saving another node', async () => {
    const profile = {
      ...detail,
      overrides: { explore: { params: { thinking: { type: 'disabled' } } } },
    }
    const fetchMock = mock(profile)
    const user = userEvent.setup()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑档位' }))
    expect(screen.getByLabelText('探索 thinking.type')).toHaveTextContent('disabled')
    await chooseOption(user, screen.getByLabelText('review 模型'), /^one（user:one）/)
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/profiles/default/overrides' && init?.method === 'PUT')).toBe(true))
    const call = fetchMock.mock.calls.find(([url, init]) => url === '/api/v1/profiles/default/overrides' && init?.method === 'PUT')
    const body = JSON.parse(String(call?.[1]?.body))
    expect(body.overrides.explore.params.thinking.type).toBe('disabled')
    expect(body.overrides.lanes.review.model).toBe('user:one')
  })

  it('saves a lane override and resets to configuration', async () => {
    const fetchMock = mock()
    const user = userEvent.setup()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑档位' }))
    await chooseOption(user, screen.getByLabelText('review 模型'), /^one（user:one）/)
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === '/api/v1/profiles/default/overrides' && init?.method === 'PUT')).toBe(true))
    const call = fetchMock.mock.calls.find(([url, init]) => url === '/api/v1/profiles/default/overrides' && init?.method === 'PUT')
    const body = JSON.parse(String(call?.[1]?.body))
    expect(body.revision).toBe(3)
    expect(body.overrides.lanes.review.model).toBe('user:one')

    await user.click(screen.getByRole('button', { name: '编辑档位' }))
    await user.click(screen.getByRole('button', { name: '恢复为配置' }))
    await user.click(within(screen.getByRole('dialog', { name: '恢复为配置' })).getByRole('button', { name: '恢复为配置' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url.startsWith('/api/v1/profiles/default/overrides') && init?.method === 'DELETE')).toBe(true))
  })
})
