import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { ModelsPage } from '../src/pages/ModelsPage'
import { deriveReviewStage } from '../src/lib/reviewStage'
import { stageForKind } from '../src/features/review/reviewView'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function renderProfiles(profiles: Array<{ name: string; config: Record<string, unknown> }>) {
  installFetch((url) => {
    if (url.startsWith('/api/v1/providers')) return jsonResponse({ providers: [] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles })
    return jsonResponse({})
  })
  renderWithProviders(<ModelsPage />, { route: '/models' })
}

async function stepTexts(): Promise<string[]> {
  const steps = await screen.findByRole('list', { name: 'default 拓扑步骤' })
  return within(steps).getAllByRole('listitem').map((li) => li.textContent ?? '')
}

describe('single review profile in the console (#253)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('maps explore to planning and validate to the proposer lane', () => {
    expect(stageForKind('explore')).toBe('规划')
    expect(stageForKind('validate')).toBe('提出')
    expect(stageForKind('review')).toBe('提出')
    expect(deriveReviewStage([{ id: 'explore', kind: 'explore', status: 'completed' }])).toBe(1)
    expect(deriveReviewStage([{ id: 'validate', kind: 'validate', status: 'completed' }])).toBe(2)
  })

  it('shows the fixed topology with each node provider', async () => {
    renderProfiles([{
      name: 'default',
      config: {
        provider: 'primary',
        explore: { provider: 'primary' },
        lanes: { code_review: { provider: 'primary' }, review: { provider: 'secondary' }, reasoner: { provider: '' }, validate: { provider: 'primary' } },
        challenge: { provider: 'secondary' },
      },
    }])
    const texts = await stepTexts()
    expect(texts.map((text) => text.replace(/^\d+ · /, '').split('（')[0])).toEqual([
      '探索explore', '提出code-review', '提出review', '提出reasoner', '提出validate', '挑战verify', '裁决adjudicate代码', '汇报synthesize代码',
    ])
    expect(texts[2]).toContain('secondary')
    expect(texts[3]).toContain('primary') // blank lane provider falls back to the profile provider
    expect(texts[5]).toContain('secondary')
    expect(texts[7]).toContain('代码')
  })

  it('lists only the review profile and names onboarding apart', async () => {
    renderProfiles([
      { name: 'default', config: { provider: 'primary' } },
      { name: 'onboarding', config: { agent: { provider: 'primary' } } },
      { name: 'deep', config: { provider: 'primary' } },
    ])
    await stepTexts()
    expect(screen.queryByRole('list', { name: 'onboarding 拓扑步骤' })).not.toBeInTheDocument()
    expect(screen.queryByRole('list', { name: 'deep 拓扑步骤' })).not.toBeInTheDocument()
    expect(screen.getByText(/项目接入（onboarding，不是评审档位）使用 provider：primary/)).toBeInTheDocument()
  })
})
