import { afterEach, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import { OverviewPage } from '../src/pages/OverviewPage'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

afterEach(() => vi.unstubAllGlobals())

it('uses model statistics for the administrator health alerts', async () => {
  const requests: string[] = []
  installFetch((raw) => {
    const path = new URL(raw, 'http://localhost').pathname
    requests.push(path)
    if (path === '/api/v1/models') return jsonResponse({ models: [{ id: 'user:abc', display_name: 'Example', stats_24h: { errors: 2, error_rate: 0.5 } }] })
    if (path === '/api/v1/projects') return jsonResponse({ projects: [] })
    if (path === '/api/v1/reviews') return jsonResponse({ reviews: [] })
    return jsonResponse({ runs: 1 })
  })
  renderWithProviders(<OverviewPage />)
  expect(await screen.findByText(/Example 近 24 小时有 2 次错误/)).toBeInTheDocument()
  expect(requests).toContain('/api/v1/models')
  expect(requests).not.toContain('/api/v1/providers')
})

it('does not request model health for a viewer', async () => {
  const requests: string[] = []
  installFetch((raw) => {
    const path = new URL(raw, 'http://localhost').pathname
    requests.push(path)
    if (path === '/api/v1/projects') return jsonResponse({ projects: [] })
    if (path === '/api/v1/reviews') return jsonResponse({ reviews: [] })
    return jsonResponse({ runs: 1 })
  })
  renderWithProviders(<OverviewPage />, { me: viewerMe })
  await screen.findByText('无异常')
  expect(requests).not.toContain('/api/v1/models')
})
