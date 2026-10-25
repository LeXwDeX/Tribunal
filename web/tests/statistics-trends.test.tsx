import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const trendDay = {
  day: '2026-10-01',
  runs: 3,
  cost_micros: 1_500_000,
  duration_samples: 2,
  avg_duration_ms: 950,
  p95_duration_ms: 1800,
}

const anotherTrendDay = {
  day: '2026-10-03',
  runs: 1,
  cost_micros: 0,
  duration_samples: 0,
  avg_duration_ms: null,
  p95_duration_ms: null,
}

const providerTrendDay = {
  day: '2026-10-01',
  runs: 5,
  cost_micros: 2_000_000,
  duration_samples: 4,
  avg_duration_ms: 100,
  p95_duration_ms: 200,
}

const blockTrendDay = {
  day: '2026-10-01',
  runs: 4,
  cost_micros: 3_000_000,
  duration_samples: 2,
  avg_duration_ms: 300,
  p95_duration_ms: 400,
}

function routeStats(stats: Record<string, unknown>) {
  installFetch((url) => {
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    if (url.startsWith('/api/v1/stats')) return jsonResponse(stats)
    return jsonResponse({})
  })
}

async function panelByTitle(title: string) {
  const heading = await screen.findByRole('heading', { name: title })
  const panel = heading.closest('section')
  if (!panel) throw new Error(`panel not found: ${title}`)
  return within(panel)
}

function rowFor(table: HTMLElement, text: string) {
  const row = within(table).getByText(text).closest('tr')
  if (!row) throw new Error(`row not found: ${text}`)
  return within(row)
}

function topMetric(label: string): HTMLElement {
  const labelNode = screen.getAllByText(label)
    .find((node) => typeof node.className === 'string' && node.className.includes('metricLabel'))
  const metric = labelNode?.closest('div')
  if (!metric) throw new Error(`top metric not found: ${label}`)
  return metric
}

const fullStats = {
  runs: 4,
  duration_samples: 2,
  avg_duration_ms: 950,
  p95_duration_ms: 1800,
  cost_micros: 1_500_000,
  trend: [trendDay, anotherTrendDay],
  by_project: [{
    project_id: 42,
    runs: 3,
    blocking: 1,
    published: 4,
    cost_micros: 1_000_000,
    duration_samples: 2,
    avg_duration_ms: 950,
    p95_duration_ms: 1800,
    trend: [trendDay],
  }],
  by_provider: [{
    provider: 'anthropic',
    runs: 5,
    cost_micros: 2_000_000,
    duration_samples: 4,
    avg_duration_ms: 100,
    p95_duration_ms: 200,
    trend: [providerTrendDay],
  }],
  by_block: [{
    block: 'propose',
    runs: 4,
    cost_micros: 3_000_000,
    duration_samples: 2,
    avg_duration_ms: 300,
    p95_duration_ms: 400,
    trend: [blockTrendDay],
  }],
}

describe('statistics daily trends', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows an overall sparse UTC trend with cost, durations and sample counts', async () => {
    routeStats(fullStats)
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('每日运行趋势')
    const table = panel.getByRole('table', { name: '每日运行趋势' })
    expect(within(table).getByRole('columnheader', { name: '日期 (UTC)' })).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: '工作流次数' })).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: '耗时样本' })).toBeInTheDocument()

    const first = rowFor(table, '2026-10-01')
    expect(first.getByText('3')).toBeInTheDocument()
    expect(first.getByText('2')).toBeInTheDocument()
    expect(first.getByText('$1.5000')).toBeInTheDocument()
    expect(first.getByText('950ms')).toBeInTheDocument()
    expect(first.getByText('2s')).toBeInTheDocument()

    const second = rowFor(table, '2026-10-03')
    expect(second.getByText('$0.0000')).toBeInTheDocument()
    expect(second.getAllByText('不可用')).toHaveLength(2)
    expect(second.getByText('0')).toBeInTheDocument()

    expect(within(table).queryByText('2026-10-02')).not.toBeInTheDocument()
    expect(panel.getByText(/只返回有执行的日期，不补零/)).toBeInTheDocument()
    expect(panel.getByText(/用时为空时表示没有有效的开始与结束时间配对/)).toBeInTheDocument()

    expect(screen.getByText(/样本 2 · P95 2s/)).toBeInTheDocument()
  })

  it('labels project workflows separately from provider and block started nodes', async () => {
    routeStats(fullStats)
    renderWithProviders(<StatisticsPage />)

    const projectPanel = await panelByTitle('项目统计')
    const projectTable = projectPanel.getByRole('table')
    expect(within(projectTable).getByRole('columnheader', { name: '工作流次数' })).toBeInTheDocument()
    expect(within(projectTable).getByRole('columnheader', { name: 'BLOCKING' })).toBeInTheDocument()
    expect(within(projectTable).getByRole('columnheader', { name: '已发布' })).toBeInTheDocument()
    const projectRow = rowFor(projectTable, '项目 #42')
    expect(projectRow.getByText('3')).toBeInTheDocument()
    expect(projectRow.getByText('1')).toBeInTheDocument()
    expect(projectRow.getByText('4')).toBeInTheDocument()
    expect(projectRow.getByText('$1.0000')).toBeInTheDocument()
    expect(projectRow.getByText('2')).toBeInTheDocument()

    const providerPanel = await panelByTitle('Provider 统计')
    const providerTable = providerPanel.getByRole('table')
    expect(within(providerTable).getByRole('columnheader', { name: '已启动节点数' })).toBeInTheDocument()
    const providerRow = rowFor(providerTable, 'anthropic')
    expect(providerRow.getByText('5')).toBeInTheDocument()
    expect(providerRow.getByText('$2.0000')).toBeInTheDocument()
    expect(providerRow.getByText('4')).toBeInTheDocument()

    const blockPanel = await panelByTitle('Block 统计')
    const blockTable = blockPanel.getByRole('table')
    const blockRow = rowFor(blockTable, 'propose')
    expect(blockRow.getByText('4')).toBeInTheDocument()
    expect(blockRow.getByText('2')).toBeInTheDocument()
    expect(blockRow.getByText('$3.0000')).toBeInTheDocument()

    expect(screen.getByText(/只要节点有开始时间（started_at）就计入/)).toBeInTheDocument()
    expect(screen.getByText(/运行中、已完成和失败的节点都会计入/)).toBeInTheDocument()
    expect(screen.getByText(/不等于 LLM 调用次数或重试尝试次数/)).toBeInTheDocument()
    expect(screen.getByText(/没有 provider 的节点只计入 block/)).toBeInTheDocument()
    expect(screen.getByText(/最新一次尝试，不是重试尝试历史/)).toBeInTheDocument()
  })

  it('expands and collapses a dimension trend with the keyboard', async () => {
    routeStats(fullStats)
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('Provider 统计')
    const button = panel.getByRole('button', { name: '查看 anthropic 每日趋势' })
    expect(panel.queryByRole('group', { name: 'anthropic 每日趋势' })).not.toBeInTheDocument()

    button.focus()
    await user.keyboard('{Enter}')
    expect(button).toHaveAttribute('aria-expanded', 'true')

    const detail = panel.getByRole('group', { name: 'anthropic 每日趋势' })
    expect(within(detail).getByText('2026-10-01')).toBeInTheDocument()
    expect(within(detail).getByRole('table', { name: 'anthropic 每日趋势' })).toBeInTheDocument()

    await user.keyboard(' ')
    expect(button).toHaveAttribute('aria-expanded', 'false')
    expect(panel.queryByRole('group', { name: 'anthropic 每日趋势' })).not.toBeInTheDocument()
  })

  it('uses dimension-specific count labels in nested daily trends', async () => {
    routeStats(fullStats)
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)

    const projectPanel = await panelByTitle('项目统计')
    await user.click(projectPanel.getByRole('button', { name: '查看 项目 #42 每日趋势' }))
    const projectTrend = projectPanel.getByRole('table', { name: '项目 #42 每日趋势' })
    expect(within(projectTrend).getByRole('columnheader', { name: '工作流次数' })).toBeInTheDocument()
    expect(within(projectTrend).queryByRole('columnheader', { name: '运行次数' })).not.toBeInTheDocument()

    const providerPanel = await panelByTitle('Provider 统计')
    await user.click(providerPanel.getByRole('button', { name: '查看 anthropic 每日趋势' }))
    const providerTrend = providerPanel.getByRole('table', { name: 'anthropic 每日趋势' })
    expect(within(providerTrend).getByRole('columnheader', { name: '已启动节点数' })).toBeInTheDocument()
    expect(within(providerTrend).queryByRole('columnheader', { name: '运行次数' })).not.toBeInTheDocument()

    const blockPanel = await panelByTitle('Block 统计')
    await user.click(blockPanel.getByRole('button', { name: '查看 propose 每日趋势' }))
    const blockTrend = blockPanel.getByRole('table', { name: 'propose 每日趋势' })
    expect(within(blockTrend).getByRole('columnheader', { name: '已启动节点数' })).toBeInTheDocument()
    expect(within(blockTrend).queryByRole('columnheader', { name: '运行次数' })).not.toBeInTheDocument()
  })

  it.each([
    ['zero samples', { duration_samples: 0 }, /样本 0 · P95 不可用/],
    ['negative samples', { duration_samples: -1 }, /样本数不可用 · P95 不可用/],
    ['samples above runs', { duration_samples: 4 }, /样本数不可用 · P95 不可用/],
    ['non-numeric samples', { duration_samples: '2' }, /样本数不可用 · P95 不可用/],
    ['negative durations', { duration_samples: 2 }, /样本 2 · P95 不可用/],
  ])('hides misleading top durations for %s', async (_label, partial, note) => {
    routeStats({
      runs: 3,
      cost_micros: 100,
      avg_duration_ms: 2000,
      p95_duration_ms: 4000,
      ...partial,
      ...(_label === 'negative durations' ? { avg_duration_ms: -100, p95_duration_ms: -200 } : {}),
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText(note)).toBeInTheDocument()
    const average = within(topMetric('平均用时'))
    expect(average.getByText('不可用')).toBeInTheDocument()
    expect(average.queryByText('2s')).not.toBeInTheDocument()
    expect(average.queryByText('4s')).not.toBeInTheDocument()

    const runs = within(topMetric('运行次数'))
    expect(runs.getByText('3')).toBeInTheDocument()
    const cost = within(topMetric('成本'))
    expect(cost.getByText('$0.0001')).toBeInTheDocument()
  })

  it('keeps valid legacy average and P95 when duration samples are absent', async () => {
    routeStats({
      runs: 3,
      cost_micros: 100,
      avg_duration_ms: 2000,
      p95_duration_ms: 4000,
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText(/样本数不可用 · P95 4s/)).toBeInTheDocument()
    const average = within(topMetric('平均用时'))
    expect(average.getByText('2s')).toBeInTheDocument()
    expect(average.queryByText('不可用')).not.toBeInTheDocument()
    expect(within(topMetric('运行次数')).getByText('3')).toBeInTheDocument()
    expect(within(topMetric('成本')).getByText('$0.0001')).toBeInTheDocument()
  })

  it('renders zero cost and null durations without inventing zero durations', async () => {
    routeStats({
      runs: 1,
      by_provider: [{
        provider: 'local',
        runs: 1,
        cost_micros: 0,
        duration_samples: 0,
        avg_duration_ms: null,
        p95_duration_ms: null,
        trend: [],
      }],
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('Provider 统计')
    const row = rowFor(panel.getByRole('table'), 'local')
    expect(row.getByText('$0.0000')).toBeInTheDocument()
    expect(row.getAllByText('不可用')).toHaveLength(2)
    expect(row.getByText('0')).toBeInTheDocument()
  })

  it('keeps old project rows and marks missing new fields unavailable, not zero', async () => {
    routeStats({
      runs: 3,
      blocking: 1,
      published: 2,
      cost_micros: 100,
      by_project: [{
        project_id: 7,
        runs: 3,
        blocking: 1,
        published: 2,
        cost_micros: 100,
      }],
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('项目统计')
    const row = rowFor(panel.getByRole('table'), '项目 #7')
    expect(row.getByText('3')).toBeInTheDocument()
    expect(row.getByText('1')).toBeInTheDocument()
    expect(row.getByText('2')).toBeInTheDocument()
    expect(row.getByText('$0.0001')).toBeInTheDocument()
    expect(row.getAllByText('不可用').length).toBeGreaterThanOrEqual(3)
    expect(row.queryByText('0')).not.toBeInTheDocument()

    expect(screen.getByText('每日趋势不可用')).toBeInTheDocument()
    expect(screen.getByText('Provider 统计不可用')).toBeInTheDocument()
    expect(screen.getByText('Block 统计不可用')).toBeInTheDocument()
    expect(screen.getByText(/样本数不可用/)).toBeInTheDocument()
  })

  it('fails the whole overall trend closed on a malformed row', async () => {
    routeStats({
      trend: [{ ...trendDay, runs: -1 }],
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText('每日趋势数据格式无效')).toBeInTheDocument()
    expect(screen.queryByText('2026-10-01')).not.toBeInTheDocument()
  })

  it('fails a dimension closed instead of silently dropping one malformed row', async () => {
    routeStats({
      by_provider: [
        fullStats.by_provider[0],
        {
          provider: 'openai',
          runs: '5',
          cost_micros: 0,
          duration_samples: 0,
          avg_duration_ms: null,
          p95_duration_ms: null,
          trend: [],
        },
      ],
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText('Provider 统计数据格式无效')).toBeInTheDocument()
    expect(screen.queryByText('anthropic')).not.toBeInTheDocument()
    expect(screen.queryByText('openai')).not.toBeInTheDocument()
  })

  it('fails the project dimension closed on a negative workflow count', async () => {
    routeStats({
      by_project: [
        fullStats.by_project[0],
        {
          project_id: 43,
          runs: -1,
          blocking: 0,
          published: 0,
          cost_micros: 0,
          duration_samples: 0,
          avg_duration_ms: null,
          p95_duration_ms: null,
          trend: [],
        },
      ],
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText('项目统计数据格式无效')).toBeInTheDocument()
    expect(screen.queryByText('项目 #42')).not.toBeInTheDocument()
    expect(screen.queryByText('项目 #43')).not.toBeInTheDocument()
  })

  it('fails the block dimension closed on a fractional node count', async () => {
    routeStats({
      by_block: [{
        block: 'propose',
        runs: 1.5,
        cost_micros: 0,
        duration_samples: 0,
        avg_duration_ms: null,
        p95_duration_ms: null,
        trend: [],
      }],
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText('Block 统计数据格式无效')).toBeInTheDocument()
    expect(screen.queryByText('propose')).not.toBeInTheDocument()
  })

  it('fails closed when an old project row has only part of the new fields', async () => {
    routeStats({
      by_project: [{
        project_id: 42,
        runs: 3,
        blocking: 1,
        published: 4,
        cost_micros: 100,
        duration_samples: 2,
      }],
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText('项目统计数据格式无效')).toBeInTheDocument()
    expect(screen.queryByText('项目 #42')).not.toBeInTheDocument()
  })

  it('keys expanded rows to identity when a refetch reorders the list', async () => {
    const reordered = {
      ...fullStats,
      by_project: [
        { project_id: 43, runs: 1, blocking: 0, published: 0, cost_micros: 0, duration_samples: 0, avg_duration_ms: null, p95_duration_ms: null, trend: [] },
        fullStats.by_project[0],
      ],
      by_provider: [
        { provider: 'openai', runs: 1, cost_micros: 0, duration_samples: 0, avg_duration_ms: null, p95_duration_ms: null, trend: [] },
        fullStats.by_provider[0],
      ],
      by_block: [
        { block: 'challenge', runs: 1, cost_micros: 0, duration_samples: 0, avg_duration_ms: null, p95_duration_ms: null, trend: [] },
        fullStats.by_block[0],
      ],
    }
    let statsCalls = 0
    installFetch((url) => {
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
      if (url.startsWith('/api/v1/stats')) {
        statsCalls += 1
        return jsonResponse(statsCalls === 1 ? fullStats : reordered)
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    const { queryClient } = renderWithProviders(<StatisticsPage />)

    const projectPanel = await panelByTitle('项目统计')
    const providerPanel = await panelByTitle('Provider 统计')
    const blockPanel = await panelByTitle('Block 统计')
    await user.click(projectPanel.getByRole('button', { name: '查看 项目 #42 每日趋势' }))
    await user.click(providerPanel.getByRole('button', { name: '查看 anthropic 每日趋势' }))
    await user.click(blockPanel.getByRole('button', { name: '查看 propose 每日趋势' }))

    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ['stats'] })
    })
    await waitFor(() => expect(statsCalls).toBe(2))
    await projectPanel.findByText('项目 #43')
    await providerPanel.findByText('openai')
    await blockPanel.findByText('challenge')

    expect(projectPanel.getByRole('button', { name: '查看 项目 #43 每日趋势' })).toHaveAttribute('aria-expanded', 'false')
    expect(projectPanel.queryByRole('group', { name: '项目 #43 每日趋势' })).not.toBeInTheDocument()
    expect(projectPanel.getByRole('button', { name: '收起 项目 #42 每日趋势' })).toHaveAttribute('aria-expanded', 'true')
    expect(projectPanel.getByRole('group', { name: '项目 #42 每日趋势' })).toBeInTheDocument()

    expect(providerPanel.getByRole('button', { name: '查看 openai 每日趋势' })).toHaveAttribute('aria-expanded', 'false')
    expect(providerPanel.queryByRole('group', { name: 'openai 每日趋势' })).not.toBeInTheDocument()
    expect(providerPanel.getByRole('button', { name: '收起 anthropic 每日趋势' })).toHaveAttribute('aria-expanded', 'true')
    expect(providerPanel.getByRole('group', { name: 'anthropic 每日趋势' })).toBeInTheDocument()

    expect(blockPanel.getByRole('button', { name: '查看 challenge 每日趋势' })).toHaveAttribute('aria-expanded', 'false')
    expect(blockPanel.queryByRole('group', { name: 'challenge 每日趋势' })).not.toBeInTheDocument()
    expect(blockPanel.getByRole('button', { name: '收起 propose 每日趋势' })).toHaveAttribute('aria-expanded', 'true')
    expect(blockPanel.getByRole('group', { name: 'propose 每日趋势' })).toBeInTheDocument()
  })

  it('fails closed when duration samples exceed the run count', async () => {
    routeStats({
      runs: 1,
      duration_samples: 2,
      trend: [{ ...trendDay, runs: 1, duration_samples: 2 }],
      by_provider: [{ ...fullStats.by_provider[0], runs: 2, duration_samples: 3 }],
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText('每日趋势数据格式无效')).toBeInTheDocument()
    expect(screen.getByText('Provider 统计数据格式无效')).toBeInTheDocument()
    expect(screen.getByText(/样本数不可用/)).toBeInTheDocument()
  })

  it('distinguishes known empty arrays from unavailable fields', async () => {
    routeStats({
      runs: 0,
      duration_samples: 0,
      trend: [],
      by_project: [],
      by_provider: [],
      by_block: [],
    })
    renderWithProviders(<StatisticsPage />)

    expect(await screen.findByText('没有每日趋势')).toBeInTheDocument()
    expect(screen.getByText('没有项目统计')).toBeInTheDocument()
    expect(screen.getByText('没有 provider 统计')).toBeInTheDocument()
    expect(screen.getByText('没有 block 统计')).toBeInTheDocument()
    expect(screen.queryByText('每日趋势不可用')).not.toBeInTheDocument()
    expect(screen.queryByText('Provider 统计不可用')).not.toBeInTheDocument()
    expect(screen.getByText(/样本 0/)).toBeInTheDocument()
  })

  it('marks the trend unavailable when an older response omits it even with metrics', async () => {
    routeStats({ runs: 1 })
    renderWithProviders(<StatisticsPage />)

    await waitFor(() => expect(screen.getByText('每日趋势不可用')).toBeInTheDocument())
    expect(screen.getByText(/旧版服务端可能不返回每日趋势/)).toBeInTheDocument()
  })
})
