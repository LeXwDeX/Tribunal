import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import { OverviewPage } from '../src/pages/OverviewPage'
import { REVIEW_STAGES, deriveReviewStage } from '../src/lib/reviewStage'
import type { GraphNode } from '../src/api/types'
import { review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

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
    expect(deriveReviewStage([node('reasoner', 'reasoner', 'completed')])).toBe(2)
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

function routeOverview(stats: unknown, running: unknown[] = [], failed: unknown[] = []) {
  return installFetch((url) => {
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/stats') return jsonResponse(stats)
    if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
    if (parsed.pathname === '/api/v1/providers') return jsonResponse({ providers: [] })
    if (parsed.pathname === '/api/v1/reviews') {
      return jsonResponse({ reviews: parsed.searchParams.get('status') === 'running' ? running : parsed.searchParams.get('status') === 'failed' ? failed : [] })
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

  it('explains cost pricing and places the alert coverage beside the title', async () => {
    routeOverview({ runs: 1, cost_micros: 0 })
    renderWithProviders(<OverviewPage />)
    expect(await screen.findByText(/需在 provider 配置 price 才统计成本/)).toBeInTheDocument()
    const heading = screen.getByRole('heading', { name: '告警' })
    const header = heading.parentElement
    if (!header) throw new Error('alert header missing')
    expect(within(header).getByText('检查范围：token 过期、工作区异常、停用项目、provider 错误、评审执行失败和发布失败')).toBeInTheDocument()
    expect(await screen.findByText('无异常')).toBeInTheDocument()
  })

  it('keeps the coverage hint with alerts and omits provider checks for viewers', async () => {
    routeOverview({ runs: 1 }, [], [review({ id: 'failure', mr_title: 'failed run', status: 'failed' })])
    renderWithProviders(<OverviewPage />, { me: viewerMe })
    expect(await screen.findByText(/failed run.*执行失败/)).toBeInTheDocument()
    const header = screen.getByRole('heading', { name: '告警' }).parentElement
    if (!header) throw new Error('alert header missing')
    expect(within(header).getByText('检查范围：token 过期、工作区异常、停用项目、评审执行失败和发布失败')).toBeInTheDocument()
    expect(screen.queryByText('无异常')).not.toBeInTheDocument()
  })

  it.each([[false, false], [true, false], [false, true], [true, true]])('orders sections with alerts=%s and running=%s', async (hasAlert, hasRunning) => {
    routeOverview({ runs: 1, cost_micros: 0 }, hasRunning ? [review({ id: 'running', mr_title: 'active review' })] : [], hasAlert ? [review({ id: 'failure', mr_title: 'failed run', status: 'failed' })] : [])
    renderWithProviders(<OverviewPage />)
    const stats = await screen.findByRole('region', { name: '近 7 天指标' })
    const alerts = screen.getByRole('heading', { name: '告警' })
    const running = screen.getByRole('heading', { name: '运行中' })
    const recent = screen.getByRole('heading', { name: '最近评审' })
    expect(stats.compareDocumentPosition(alerts) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(alerts.compareDocumentPosition(running) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(running.compareDocumentPosition(recent) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    if (hasAlert) expect(await screen.findByText(/failed run.*执行失败/)).toBeInTheDocument()
    else expect(await screen.findByText('无异常')).toBeInTheDocument()
    if (hasRunning) {
      expect(await screen.findByText('active review')).toBeInTheDocument()
      expect(screen.queryByText('当前没有运行中的评审')).not.toBeInTheDocument()
    } else expect(await screen.findByText('当前没有运行中的评审')).toBeInTheDocument()
  })

  it('shows running reviews without the empty state and explains the cost limit', async () => {
    routeOverview({ runs: 1, cost_micros: 0 }, [review({ id: 'running', mr_title: 'active review' })])
    renderWithProviders(<OverviewPage />)
    expect(await screen.findByText('active review')).toBeInTheDocument()
    expect(screen.queryByText('当前没有运行中的评审')).not.toBeInTheDocument()
    expect(await screen.findByText(/无法区分未配置价格/)).toHaveAttribute('title', '接口无法区分未配置价格和实际零成本')
    expect(screen.getByText('BLOCKING 工作流数 / 所有工作流数')).toBeInTheDocument()
    expect(screen.getByText('裁决为 publish 或 mark 的问题数，不代表实际发布成功')).toBeInTheDocument()
    expect(screen.getByText('drop 问题数 / 有裁决的问题数，不是模型准确率')).toBeInTheDocument()
  })

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
    expect(within(strip).getByText('发布类问题')).toBeInTheDocument()
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
