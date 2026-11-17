import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const models = [{
  id: 'config:primary', display_name: 'primary', source: 'config', read_only: true,
  protocol: 'openai-chat', base_url: 'https://api.example.test/v1', model: 'm',
  params: { max_tokens: 8192 }, effective_params: { max_tokens: 8192 }, param_sources: {},
  has_api_key: true, available: true, revision: 0, referenced_by: [],
  vote_identity: 'api.example.test/m',
}]

const provider = {
  name: 'primary', protocol: 'openai-chat', base_url: 'https://api.example.test/v1', model: 'm',
  api_key_configured: true, max_tokens: 8192, max_concurrency: 4, reasoning: { effort: 'high' },
}

function mockModelsPage() {
  installFetch((url) => {
    if (url === '/api/v1/config') {
      return jsonResponse({
        config: { providers: [provider] },
        origins: { 'providers.primary.reasoning.effort': 'base' },
        warnings: [],
        base_revision: 'base-rev-1',
        overlay_revision: 'overlay-rev-1',
        overlay_present: true,
        applied: false,
        restart_required: true,
        last_error: '',
        last_error_at: null,
      })
    }
    if (url === '/api/v1/config/status') {
      return jsonResponse({
        applied: false, restart_required: true, base_revision: 'base-rev-1',
        overlay_revision: 'overlay-rev-1', overlay_present: true, warnings: [],
        last_error: '', last_error_at: null, updated_at: null,
      })
    }
    if (url === '/api/v1/profiles/default') {
      return jsonResponse({ profile: 'default', overrides: {}, revision: 0, effective: {}, nodes: [] })
    }
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models })
    if (url === '/api/v1/model-defaults') return jsonResponse({ params: {}, revision: 0 })
    if (url === '/api/v1/model-presets') return jsonResponse({ presets: [] })
    return jsonResponse({})
  })
}

async function chooseOption(user: ReturnType<typeof userEvent.setup>, trigger: HTMLElement, name: string) {
  await user.click(trigger)
  await user.click(await screen.findByRole('option', { name }))
}

describe('xhigh reasoning effort (#491)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('offers xhigh in the review profile editor', async () => {
    mockModelsPage()
    const user = userEvent.setup()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑档位' }))
    const dialog = await screen.findByRole('dialog', { name: '编辑评审档位' })
    const effort = within(dialog).getByLabelText('全局默认 reasoning.effort')
    await chooseOption(user, effort, 'xhigh')
    expect(within(dialog).getByLabelText('全局默认 reasoning.effort')).toHaveTextContent('xhigh')
  })

  it('offers xhigh in the config model editor', async () => {
    mockModelsPage()
    const user = userEvent.setup()
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const edit = await screen.findByRole('button', { name: '编辑配置' })
    await waitFor(() => expect(edit).toBeEnabled())
    await user.click(edit)
    const dialog = screen.getByRole('dialog', { name: /编辑配置模型/ })
    const effort = within(dialog).getByLabelText('reasoning.effort')
    await chooseOption(user, effort, 'xhigh')
    expect(within(dialog).getByLabelText('reasoning.effort')).toHaveTextContent('xhigh')
  })
})