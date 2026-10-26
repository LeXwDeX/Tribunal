import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import { OverviewPage } from '../src/pages/OverviewPage'
import { REVIEW_STAGES, deriveReviewStage } from '../src/lib/reviewStage'
import type { GraphNode } from '../src/api/types'
import { review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function node(id: string, kind: string, status: string): GraphNode {
  return { id, kind, status }
}

describe('deriveReviewStage', () => {
  it('lists the five lanes in pipeline order', () => {
    expect(REVIEW_STAGES).toEqual(['规划', '提出', '挑战', '裁决', '发布'])
  })

  it('advances past the furthest completed block', () => {
    expect(deriveReviewStage([
      node('plan', 'plan', 'completed'),
      node('review.a', 'code-review', 'completed'),
      node('verify.f1', 'verify', 'running'),
    ])).toBe(2)
    expect(deriveReviewStage([node('reasoner', 'reasoner', 'completed')])).toBe(1)
    expect(deriveReviewStage([node('review.gate', 'review.gate', 'completed')])).toBe(2)
    expect(deriveReviewStage([node('verify.f1', 'verify', 'completed')])).toBe(3)
  })

  it('reaches publication once synthesis completes and never overflows', () => {
    expect(deriveReviewStage([node('synthesize', 'synthesize', 'completed')])).toBe(4)
    expect(deriveReviewStage([node('publish', 'publish', 'completed')])).toBe(4)
  })

  it('falls back to the running block when nothing mappable completed', () => {
    expect(deriveReviewStage([node('plan', 'plan', 'running')])).toBe(0)
    expect(deriveReviewStage([node('gather', 'gather', 'completed'), node('review.a', 'code-review', 'running')])).toBe(1)
  })

  it('returns null when no node maps to a lane', () => {
    expect(deriveReviewStage(undefined)).toBeNull()
    expect(deriveReviewStage([])).toBeNull()
    expect(deriveReviewStage([node('x', 'agent', 'completed'), node('y', 'onboarding', 'running')])).toBeNull()
    expect(deriveReviewStage([node('review.a', 'code-review', 'pending')])).toBeNull()
  })
})

function routeOverview(stats: unknown, running: unknown[] = []) {
  return installFetch((url) => {
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/stats') return jsonResponse(stats)
    if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
    if (parsed.pathname === '/api/v1/providers') return jsonResponse({ providers: [] })
    if (parsed.pathname === '/api/v1/reviews') {
      return jsonResponse({ reviews: parsed.searchParams.get('status') === 'running' ? running : [] })
    }
    return jsonResponse({})
  })
}

function trendDay(day: string, runs: number, cost: number) {
  return { day, runs, cost_micros: cost, duration_samples: 0, avg_duration_ms: null, p95_duration_ms: null }
}

async function metric(label: string) {
  const element = (await screen.findByText(label)).closest('div')
  if (!element) throw new Error(`${label} metric missing`)
  return element
}

describe('overview v2 layout', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('draws sparklines only on runs and cost when the trend has at least two points', async () => {
    routeOverview({
      runs: 5,
      cost_micros: 2_000_000,
      trend: [trendDay('2026-10-20', 2, 500_000), trendDay('2026-10-21', 3, 1_500_000)],
    })
    const { container } = renderWithProviders(<OverviewPage />, { route: '/' })
    expect((await metric('运行次数')).querySelector('svg polyline')).not.toBeNull()
    expect((await metric('成本')).querySelector('svg polyline')).not.toBeNull()
    expect((await metric('BLOCKING 比例')).querySelector('svg')).toBeNull()
    expect(container.querySelectorAll('section[aria-label="近 7 天指标"] svg')).toHaveLength(2)
  })

  it('does not draw sparklines for a missing or single-point trend', async () => {
    routeOverview({ runs: 5, cost_micros: 1, trend: [trendDay('2026-10-21', 5, 1)] })
    const single = renderWithProviders(<OverviewPage />, { route: '/' })
    await metric('运行次数')
    expect(single.container.querySelectorAll('section[aria-label="近 7 天指标"] svg')).toHaveLength(0)
    single.unmount()

    routeOverview({ runs: 5 })
    const missing = renderWithProviders(<OverviewPage />, { route: '/' })
    await metric('运行次数')
    expect(missing.container.querySelectorAll('section[aria-label="近 7 天指标"] svg')).toHaveLength(0)
  })

  it('shows six metrics in the strip', async () => {
    routeOverview({ runs: 5 })
    renderWithProviders(<OverviewPage />, { route: '/' })
    const strip = await screen.findByRole('region', { name: '近 7 天指标' })
    expect(within(strip).getByText('运行次数')).toBeInTheDocument()
    expect(within(strip).getByText('BLOCKING 比例')).toBeInTheDocument()
    expect(within(strip).getByText('已发布问题')).toBeInTheDocument()
    expect(within(strip).getByText('规则丢弃比例')).toBeInTheDocument()
    expect(within(strip).getByText('成本')).toBeInTheDocument()
    expect(within(strip).getByText('耗时 平均 / P95')).toBeInTheDocument()
  })

  it('renders a stage track for derivable runs and omits it otherwise', async () => {
    routeOverview({ runs: 2 }, [
      review({
        id: 'staged',
        mr_iid: 11,
        mr_title: 'staged run',
        nodes: [node('plan', 'plan', 'completed'), node('review.a', 'code-review', 'running')],
      }),
      review({ id: 'unknown', mr_iid: 12, mr_title: 'unknown run', nodes: [node('x', 'agent', 'running')] }),
    ])
    renderWithProviders(<OverviewPage />, { route: '/' })

    const staged = (await screen.findByText('staged run')).closest('li')
    if (!staged) throw new Error('staged run missing')
    const track = within(staged).getByRole('list', { name: '阶段：提出' })
    const stages = within(track).getAllByRole('listitem')
    expect(stages.map((item) => item.textContent)).toEqual(REVIEW_STAGES)
    expect(stages[1]).toHaveAttribute('aria-current', 'step')

    const unknown = screen.getByText('unknown run').closest('li')
    if (!unknown) throw new Error('unknown run missing')
    await waitFor(() => expect(within(unknown).queryByRole('list')).not.toBeInTheDocument())
    expect(within(unknown).getByText('!12')).toBeInTheDocument()
  })
})
