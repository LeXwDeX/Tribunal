import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { ModelsPage } from '../src/pages/ModelsPage'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const day = (value: string, runs: number) => ({
  day: value,
  runs,
  cost_micros: 0,
  duration_samples: 0,
  avg_duration_ms: null,
  p95_duration_ms: null,
})

function routeStats(stats: Record<string, unknown>) {
  installFetch((url) => {
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    if (url.startsWith('/api/v1/stats')) return jsonResponse(stats)
    return jsonResponse({})
  })
}

describe('daily runs chart', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('draws one titled bar per returned UTC day without filling gaps', async () => {
    routeStats({ trend: [day('2026-10-01', 3), day('2026-10-03', 1)] })
    renderWithProviders(<StatisticsPage />)

    const chart = await screen.findByRole('img', { name: /每日运行次数柱状图/ })
    const bars = chart.querySelectorAll('rect')
    expect(bars).toHaveLength(2)
    const titles = Array.from(bars).map((bar) => bar.querySelector('title')?.textContent)
    expect(titles).toEqual(['2026-10-01 · 3 次', '2026-10-03 · 1 次'])
    expect(chart.textContent).not.toContain('2026-10-02')
    const [tall, short] = Array.from(bars).map((bar) => Number(bar.getAttribute('height')))
    expect(tall).toBeGreaterThan(short)
    expect(short).toBeGreaterThan(0)

    // The table stays below the chart as the accessible data source.
    const table = screen.getByRole('table', { name: '每日运行趋势' })
    expect(chart.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it.each([
    ['missing', {}],
    ['empty', { trend: [] }],
    ['invalid', { trend: [{ ...day('2026-10-01', 3), runs: -1 }] }],
  ])('draws no chart when the trend is %s', async (_label, stats) => {
    routeStats({ runs: 0, ...stats })
    renderWithProviders(<StatisticsPage />)

    await screen.findByRole('heading', { name: '每日运行趋势' })
    expect(screen.queryByRole('img', { name: /每日运行次数柱状图/ })).not.toBeInTheDocument()
  })
})

describe('quality ratio', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows a progress bar for samples and 无样本 without a bar for a zero denominator', async () => {
    routeStats({
      quality_by_provider: [{
        provider: 'anthropic',
        proposed: 10,
        reviewed: 0,
        valid: 0,
        invalid: 0,
        accuracy: null,
        overturned: 2,
        overturned_ratio: 0.2,
        refuted: 0,
        refuted_reviewed: 0,
        false_refuted: 0,
        false_kill_rate: null,
      }],
    })
    renderWithProviders(<StatisticsPage />)

    const table = await screen.findByRole('table', { name: '按提出方统计的人工质量指标' })
    const row = within(table).getByText('anthropic').closest('tr') as HTMLElement
    const cells = within(row).getAllByRole('cell')
    const accuracy = cells[5]
    const overturned = cells[7]

    expect(within(accuracy).getByText('无样本')).toBeInTheDocument()
    expect(accuracy.querySelector('[class*="ratioFill"]')).toBeNull()

    expect(within(overturned).getByText('20.0%')).toBeInTheDocument()
    expect(within(overturned).getByText('2 / 10')).toBeInTheDocument()
    const fill = overturned.querySelector('[class*="ratioFill"]') as HTMLElement
    expect(fill).not.toBeNull()
    expect(fill.style.width).toBe('20%')
  })
})

describe('models layout', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows only the 24h stats the API returns and puts each profile budget in its panel aside', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/providers')) {
        return jsonResponse({ providers: [{ name: 'claude', protocol: 'anthropic', stats_24h: { calls: 1284, p50_latency_ms: 2100 } }] })
      }
      if (url.startsWith('/api/v1/profiles')) {
        return jsonResponse({ profiles: [{ name: 'standard', config: { budget: { max_cost_usd: 3 }, propose: [{ block: 'code-review', provider: 'claude' }] } }] })
      }
      return jsonResponse({})
    })
    renderWithProviders(<ModelsPage />, { route: '/models' })

    const stats = await screen.findByRole('list', { name: 'claude 近 24 小时' })
    const items = within(stats).getAllByRole('listitem').map((item) => item.textContent)
    expect(items).toEqual(['24h 调用1,284', '错误率不可用', 'P50 延迟2s'])

    const heading = await screen.findByRole('heading', { name: /standard/ })
    const panel = heading.closest('section') as HTMLElement
    expect(within(panel).getByText('预算 max_cost_usd: 3')).toBeInTheDocument()
    expect(within(panel).getByRole('list', { name: 'standard 拓扑步骤' })).toBeInTheDocument()
  })

  it('keeps the permission error for non-admins', async () => {
    installFetch(() => jsonResponse({ error: 'forbidden' }, 403))
    renderWithProviders(<ModelsPage />, { route: '/models' })

    expect(await screen.findAllByText('没有访问权限')).toHaveLength(2)
  })
})

describe('system layout', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('renders token revoke as a small danger button', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/api-tokens')) return jsonResponse({ tokens: [{ id: 1, name: 'ci', created_at: '2026-10-01T00:00:00Z' }] })
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />, { route: '/system' })

    const revoke = await screen.findByRole('button', { name: '撤销' })
    expect(revoke.className).toMatch(/danger/)
    expect(revoke.className).toMatch(/smallButton/)
  })
})
