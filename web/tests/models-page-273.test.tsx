import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function renderProfile(config: Record<string, unknown>) {
  installFetch((url) => {
    if (url.startsWith('/api/v1/providers')) return jsonResponse({ providers: [] })
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

describe('models page challenge step (#273)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('omits the fallback suffix when fallback is empty', async () => {
    renderProfile({ challenge: { provider: 'primary', fallback: '', only: null } })
    const text = await stepWho('挑战')
    expect(text).toContain('primary')
    expect(text).not.toContain('fallback')
  })

  it('omits the fallback suffix when fallback is only whitespace', async () => {
    renderProfile({ challenge: { provider: 'primary', fallback: '  ' } })
    expect(await stepWho('挑战')).not.toContain('fallback')
  })

  it('shows an unconfigured provider instead of an orphan fallback suffix', async () => {
    renderProfile({ challenge: { provider: '', fallback: '', only: null } })
    const text = await stepWho('挑战')
    expect(text).toContain('未配置 provider')
    expect(text).not.toContain('fallback')
  })

  it('keeps a configured fallback', async () => {
    renderProfile({ challenge: { provider: 'primary', fallback: 'secondary' } })
    expect(await stepWho('挑战')).toContain('primary（fallback secondary）')
  })

  it('treats empty providers on other steps as unconfigured', async () => {
    renderProfile({ propose: [{ block: 'correctness', provider: '' }], plan: { mode: 'code', provider: '' } })
    expect(await stepWho('提出')).toContain('未配置 provider')
    expect(await stepWho('规划')).toContain('代码')
  })
})
