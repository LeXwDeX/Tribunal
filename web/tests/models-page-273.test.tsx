import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function renderProfile(config: Record<string, unknown>) {
  installFetch((url) => {
    if (url.startsWith('/api/v1/models')) return jsonResponse({ models: [] })
    if (url.startsWith('/api/v1/providers')) return jsonResponse({ providers: [] })
    if (url === '/api/v1/profiles/default') return jsonResponse({ profile: 'default', overrides: {}, revision: 0, effective: config, nodes: [] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default', config }] })
    return jsonResponse({})
  })
  renderWithProviders(<ModelsPage />, { route: '/models' })
}

async function stepWho(stage: string): Promise<string> {
  const steps = await screen.findByRole('list', { name: 'default 拓扑步骤' })
  const item = within(steps).getAllByRole('listitem').find((li) => li.textContent?.includes(`· ${stage}`))
  if (!item) throw new Error(`step ${stage} not rendered`)
  return item.textContent ?? ''
}

describe('models page blank providers (#273)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the challenge provider without a fallback suffix', async () => {
    renderProfile({ challenge: { provider: 'primary', fallback: '', only: null } })
    expect(await screen.findByText(/还没有模型/)).toBeInTheDocument()
    expect(screen.queryByText(/重启或热加载/)).not.toBeInTheDocument()
    const text = await stepWho('挑战')
    expect(text).toContain('primary')
    expect(text).not.toContain('fallback')
  })

  it('treats blank providers as unconfigured when the profile has no provider', async () => {
    renderProfile({ provider: '  ', challenge: { provider: '' }, lanes: { review: { provider: ' ' } } })
    expect(await stepWho('挑战')).toContain('未配置 provider')
    expect(await stepWho('探索')).toContain('未配置 provider')
  })

  it('falls back from a blank node provider to the profile provider', async () => {
    renderProfile({ provider: 'primary', explore: { provider: '' } })
    expect(await stepWho('探索')).toContain('primary')
    expect(await stepWho('汇报')).toContain('代码')
  })
})
