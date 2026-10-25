import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { OverviewPage } from '../src/pages/OverviewPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('overview run count label', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('labels runs as all workflows, including onboarding', async () => {
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 12, blocking: 3 })
      if (parsed.pathname === '/api/v1/reviews') return jsonResponse({ reviews: [] })
      if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
      if (parsed.pathname === '/api/v1/providers') return jsonResponse({ providers: [] })
      return jsonResponse({})
    })
    renderWithProviders(<OverviewPage />, { route: '/' })

    const label = await screen.findByText('运行次数')
    const metric = label.closest('div')
    if (!metric) throw new Error('runs metric not found')
    expect(within(metric).getByText('12')).toBeInTheDocument()
    expect(within(metric).getByText(/包含评审与 onboarding/)).toBeInTheDocument()
    expect(screen.queryByText('评审次数')).not.toBeInTheDocument()
  })
})
