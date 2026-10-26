import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { OverviewPage } from '../src/pages/OverviewPage'
import { ReviewsPage } from '../src/pages/ReviewsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

// listItem mirrors one GET /api/v1/reviews item as the server returns it:
// workflow fields, duration_ms, nodes and findings_summary; no mr_title,
// usage, access or publications.
function listItem(overrides: Record<string, unknown> = {}) {
  return {
    id: 'wf-list-1',
    kind: 'review',
    project_id: 42,
    mr_iid: 318,
    head_sha: 'head-1',
    base_sha: 'base-1',
    fingerprint: 'fp-1',
    profile: 'deep',
    trigger: 'mr_update',
    status: 'running',
    tokens_in: 0,
    tokens_out: 0,
    cost_micros: 0,
    created_at: new Date().toISOString(),
    nodes: [
      { id: 'plan', kind: 'plan', status: 'completed', provider: 'claude', attempt: 1, duration_ms: 8120 },
      { id: 'review.std.p1', kind: 'review.standards', status: 'completed', depends_on: ['plan'], provider: 'claude', attempt: 1 },
      { id: 'verify.f7', kind: 'verify', status: 'running', depends_on: ['review.std.p1'], provider: 'gpt', attempt: 1 },
    ],
    findings_summary: { published: 2, marked: 1, dropped: 1, by_severity: { P0: 1, P1: 0, P2: 3, info: 0 } },
    ...overrides,
  }
}

function routeList(reviews: unknown[]) {
  return installFetch((url) => {
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/reviews') return jsonResponse({ reviews, next_cursor: '' })
    if (parsed.pathname === '/api/v1/projects') {
      return jsonResponse({ projects: [{ id: 42, path_with_namespace: 'group/backend' }], next_cursor: '' })
    }
    if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 1, cost_micros: 0, trend: [] })
    return jsonResponse({})
  })
}

describe('review list summary fields', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows P0/P1/P2 counts from findings_summary in the review list', async () => {
    routeList([listItem({ status: 'completed', verdict: 'BLOCKING' })])
    renderWithProviders(<ReviewsPage />, { route: '/reviews' })

    const link = await screen.findByRole('link', { name: /!318/ })
    const row = link.closest('tr')!
    expect(within(row).getByRole('img', { name: 'P0 1' })).toBeInTheDocument()
    expect(within(row).getByRole('img', { name: 'P1 0' })).toBeInTheDocument()
    expect(within(row).getByRole('img', { name: 'P2 3' })).toBeInTheDocument()
  })

  it('draws the overview stage track from list nodes', async () => {
    routeList([listItem()])
    renderWithProviders(<OverviewPage />, { route: '/' })

    const track = await screen.findByRole('list', { name: '阶段：挑战' })
    const current = within(track).getAllByRole('listitem').find((item) => item.getAttribute('aria-current') === 'step')
    expect(current).toHaveTextContent('挑战')
  })

  it('omits the stage track when a list item has no nodes yet', async () => {
    routeList([listItem({ status: 'running', nodes: [] })])
    renderWithProviders(<OverviewPage />, { route: '/' })

    // The list API has no mr_title, so the item falls back to !iid in both the
    // running and recent panels.
    await screen.findAllByRole('link', { name: '!318' })
    expect(screen.queryByRole('list', { name: /^阶段：/ })).not.toBeInTheDocument()
  })
})
