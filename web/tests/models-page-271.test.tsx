import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function renderModels(profileBudget: Record<string, unknown>) {
  installFetch((url) => {
    if (url.startsWith('/api/v1/providers')) {
      return jsonResponse({
        providers: [{
          name: 'primary', protocol: 'openai-chat', base_url: 'https://llm.example.com/v1', model: 'test-model',
          max_concurrency: 4, rpm: 0, stats_24h: { calls: 3, errors: 0, error_rate: 0, p50_latency_ms: 900 },
        }],
      })
    }
    if (url.startsWith('/api/v1/profiles')) {
      return jsonResponse({ profiles: [{ name: 'default', config: { budget: profileBudget } }] })
    }
    return jsonResponse({})
  })
  renderWithProviders(<ModelsPage />, { route: '/models' })
}

describe('models page display (#271)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the profile deadline, sent as Go duration nanoseconds, as a readable duration', async () => {
    renderModels({ max_nodes: 30, deadline: 1_200_000_000_000, max_tokens: 1_500_000 })
    const budget = await screen.findByText(/^预算 /)
    expect(budget).toHaveTextContent('deadline: 20m')
    expect(budget).not.toHaveTextContent('1200000000000')
    expect(budget).toHaveTextContent('max_nodes: 30')
    expect(budget).toHaveTextContent('max_tokens: 1500000')
  })

  it('keeps a non-numeric deadline as sent', async () => {
    renderModels({ deadline: '20m' })
    expect(await screen.findByText(/^预算 /)).toHaveTextContent('deadline: 20m')
  })

  it('does not render an API key environment row the providers API never returns', async () => {
    renderModels({ max_nodes: 30 })
    expect(await screen.findByText('primary')).toBeInTheDocument()
    expect(screen.queryByText(/API key 环境变量/)).not.toBeInTheDocument()
  })
})
