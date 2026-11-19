import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function routeStats(stats: Record<string, unknown>) {
  installFetch((url) => {
    const path = new URL(url, 'http://localhost').pathname
    if (path.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    if (path === '/api/v1/stats/reviews') return jsonResponse({})
    if (path === '/api/v1/stats') return jsonResponse(stats)
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

function headers(table: HTMLElement): string[] {
  return within(table).getAllByRole('columnheader').map((header) => header.textContent ?? '')
}

const day = (value: string, runs: number) => ({
  day: value,
  runs,
  duration_samples: 1,
  avg_duration_ms: 900,
  p95_duration_ms: 1200,
})

// 10 errors in 7,280 calls is the 0.0013736… rate the console used to print raw.
const modelRow = {
  provider: 'anthropic',
  model: 'claude-sonnet',
  calls: 7280,
  errors: 10,
  error_rate: 0.0013736263736263737,
  input_tokens: 1234567,
  output_tokens: 89012,
  avg_latency_ms: 6815.031593406594,
  p50_latency_ms: 412.4,
  errors_by_kind: { context_length_exceeded: 4, rate_limited: 1200 },
}

describe('statistics by-model table', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('uses Chinese headers and readable numbers, never raw field names or raw floats', async () => {
    routeStats({ by_model: [modelRow] })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('按模型')
    const table = panel.getByRole('table', { name: '按模型统计明细' })
    expect(headers(table)).toEqual([
      '模型 / 提供方', '调用数', '错误 / 错误率', '输入 / 输出 token', '平均 / P50 延迟', '错误类别',
    ])
    for (const raw of ['provider', 'error_rate', 'input_tokens', 'avg_latency_ms', 'p50_latency_ms', 'errors_by_kind']) {
      expect(within(table).queryByRole('columnheader', { name: raw })).not.toBeInTheDocument()
    }

    const row = rowFor(table, 'anthropic')
    expect(row.getByText('claude-sonnet')).toBeInTheDocument()
    expect(row.getByText('7,280')).toBeInTheDocument()
    expect(row.getByText('10')).toBeInTheDocument()
    expect(row.getByText('0.14%')).toBeInTheDocument()
    expect(row.getByText('1,234,567')).toBeInTheDocument()
    expect(row.getByText('89,012')).toBeInTheDocument()
    expect(row.getByText('6.8s')).toBeInTheDocument()
    expect(row.getByText('412ms')).toBeInTheDocument()
    expect(table.textContent).not.toMatch(/\d\.\d{4,}/)
  })

  it('right-aligns numeric columns on both header and cell and keeps text columns left', async () => {
    routeStats({ by_model: [modelRow] })
    renderWithProviders(<StatisticsPage />)

    const table = (await panelByTitle('按模型')).getByRole('table', { name: '按模型统计明细' })
    for (const name of ['调用数', '错误 / 错误率', '输入 / 输出 token', '平均 / P50 延迟']) {
      expect(within(table).getByRole('columnheader', { name }).className).toMatch(/cellNum/)
    }
    for (const name of ['模型 / 提供方', '错误类别']) {
      expect(within(table).getByRole('columnheader', { name }).className).not.toMatch(/cellNum/)
    }
    const row = rowFor(table, 'anthropic')
    for (const text of ['7,280', '10', '0.14%', '1,234,567', '89,012', '6.8s', '412ms']) {
      expect(row.getByText(text).closest('td')!.className).toMatch(/cellNum/)
    }
    expect(row.getByText('claude-sonnet').className).toMatch(/stackedMono/)
  })

  it('shows every errors_by_kind badge in full inside a wrapping list', async () => {
    routeStats({ by_model: [modelRow] })
    renderWithProviders(<StatisticsPage />)

    const table = (await panelByTitle('按模型')).getByRole('table', { name: '按模型统计明细' })
    const row = rowFor(table, 'anthropic')
    const context = row.getByText('context_length_exceeded: 4')
    const limited = row.getByText('rate_limited: 1,200')
    expect(context.className).toMatch(/badgeWarn/)
    expect(limited.className).toMatch(/badgeWarn/)
    // The badges share one wrapping container instead of one clipped line.
    expect(context.parentElement).toBe(limited.parentElement)
    expect(context.parentElement?.className).toMatch(/kindList/)
  })

  it('shows muted text, not a badge, when a model has no error categories', async () => {
    routeStats({ by_model: [{ ...modelRow, errors: 0, error_rate: 0, errors_by_kind: {} }] })
    renderWithProviders(<StatisticsPage />)

    const table = (await panelByTitle('按模型')).getByRole('table', { name: '按模型统计明细' })
    const row = rowFor(table, 'anthropic')
    expect(row.getByText('无错误类别').className).toMatch(/cellMuted/)
    expect(row.getByText('0%')).toBeInTheDocument()
  })

  it('formats missing rates and latencies as a dash instead of zero', async () => {
    routeStats({ by_model: [{ ...modelRow, error_rate: null, avg_latency_ms: null, p50_latency_ms: undefined }] })
    renderWithProviders(<StatisticsPage />)

    const row = rowFor((await panelByTitle('按模型')).getByRole('table', { name: '按模型统计明细' }), 'anthropic')
    expect(row.getAllByText('—')).toHaveLength(3)
  })

  it('pages more than 20 models client-side with a pager below the table', async () => {
    const user = userEvent.setup()
    routeStats({
      by_model: Array.from({ length: 25 }, (_, index) => ({
        ...modelRow,
        model: `model-${String(index + 1).padStart(2, '0')}`,
        errors_by_kind: {},
      })),
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('按模型')
    const table = panel.getByRole('table', { name: '按模型统计明细' })
    expect(within(table).getAllByRole('row')).toHaveLength(21)
    expect(within(table).getByText('model-20')).toBeInTheDocument()
    expect(within(table).queryByText('model-21')).not.toBeInTheDocument()
    expect(panel.getByText('第 1 / 2 页 · 共 25 条')).toBeInTheDocument()

    await user.click(panel.getByRole('button', { name: '下一页' }))
    expect(within(table).getAllByRole('row')).toHaveLength(6)
    expect(within(table).getByText('model-25')).toBeInTheDocument()
    expect(within(table).queryByText('model-20')).not.toBeInTheDocument()
    expect(panel.getByText('第 2 / 2 页 · 共 25 条')).toBeInTheDocument()
  })

  it('does not render a pager for a short list', async () => {
    routeStats({ by_model: [modelRow] })
    renderWithProviders(<StatisticsPage />)

    await panelByTitle('按模型')
    expect(screen.queryByRole('navigation', { name: '表格分页' })).not.toBeInTheDocument()
  })

  it('renders the legacy keyed by_model object with Chinese headers and formatted values', async () => {
    routeStats({
      by_model: {
        'anthropic/claude-sonnet': {
          provider: 'anthropic',
          model: 'claude-sonnet',
          calls: 5,
          error_rate: 0.0013736263736263737,
          avg_latency_ms: 6815.031593406594,
          p50_latency_ms: 412,
          score: 0.123456789,
        },
      },
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('按模型')
    const table = panel.getByRole('table', { name: '按模型统计明细' })
    expect(headers(table)).toEqual(['名称', '提供方', '模型', '调用数', '错误率', '平均延迟', 'P50 延迟', 'score'])
    const row = rowFor(table, 'anthropic/claude-sonnet')
    expect(row.getByText('0.14%')).toBeInTheDocument()
    expect(row.getByText('6.8s')).toBeInTheDocument()
    expect(row.getByText('412ms')).toBeInTheDocument()
    expect(row.getByText('0.123')).toBeInTheDocument()
    expect(row.getByText('5').className).toMatch(/cellNum/)
    expect(within(table).getByRole('columnheader', { name: '调用数' }).className).toMatch(/cellNum/)
    expect(within(table).getByRole('columnheader', { name: '名称' }).className).not.toMatch(/cellNum/)
    expect(table.textContent).not.toMatch(/\d\.\d{4,}/)
  })

  it('shows the empty state instead of an empty table when nothing was returned', async () => {
    routeStats({ by_model: [] })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('按模型')
    expect(panel.getByText('没有按模型统计')).toBeInTheDocument()
    expect(panel.queryByRole('table')).not.toBeInTheDocument()
  })
})

describe('statistics trend actions', () => {
  afterEach(() => vi.unstubAllGlobals())

  const stats = {
    runs: 4,
    by_project: [{
      project_id: 42, runs: 3, blocking: 1, published: 4, duration_samples: 1, avg_duration_ms: 900, p95_duration_ms: 1200, trend: [day('2026-10-01', 3)],
    }],
    by_provider: [{
      provider: 'anthropic', runs: 5, duration_samples: 1, avg_duration_ms: 900, p95_duration_ms: 1200, trend: [day('2026-10-01', 5)],
    }],
    by_block: [{
      block: 'propose', runs: 4, duration_samples: 1, avg_duration_ms: 900, p95_duration_ms: 1200, trend: [day('2026-10-01', 4)],
    }],
  }

  it.each([
    ['项目统计', '项目 #42', '查看 项目 #42 每日趋势'],
    ['Provider 统计', 'anthropic', '查看 anthropic 每日趋势'],
    ['Block 统计', 'propose', '查看 propose 每日趋势'],
  ])('puts the 每日趋势 button of %s in a right-aligned actions column as a compact table action', async (title, key, name) => {
    routeStats(stats)
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle(title)
    const table = panel.getByRole('table')
    const header = within(table).getByRole('columnheader', { name: '操作' })
    expect(header.className).toMatch(/cellActions/)
    expect(within(table).getAllByRole('columnheader').at(-1)).toBe(header)

    const button = panel.getByRole('button', { name })
    expect(button).toHaveTextContent('每日趋势')
    expect(button.className).toMatch(/smallButton/)
    expect(button.closest('td')?.className).toMatch(/cellActions/)
    expect(rowFor(table, key).getByRole('button', { name })).toBe(button)

    await user.click(button)
    expect(button).toHaveTextContent('收起')
    expect(button).toHaveAttribute('aria-expanded', 'true')
    expect(panel.getByRole('table', { name: `${key} 每日趋势` })).toBeInTheDocument()
  })

  it('explains an unavailable project trend through the badge reason, not extra text', async () => {
    routeStats({ runs: 3, by_project: [{ project_id: 7, runs: 3, blocking: 1, published: 2 }] })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('项目统计')
    const row = rowFor(panel.getByRole('table'), '项目 #7')
    expect(row.queryByRole('button')).not.toBeInTheDocument()
    const badge = row.getAllByText('不可用').find((node) => node.className.includes('badge'))
    if (!badge) throw new Error('trend badge missing')
    const anchor = badge.closest('[tabindex]')
    expect(anchor).toHaveAccessibleDescription('该行没有每日趋势数据；旧版服务端可能不支持。')
    expect(anchor?.closest('td')?.className).toMatch(/cellActions/)
  })

  it('pages a daily trend longer than 20 UTC days', async () => {
    const user = userEvent.setup()
    routeStats({
      runs: 25,
      trend: Array.from({ length: 25 }, (_, index) => day(`2026-10-${String(index + 1).padStart(2, '0')}`, index + 1)),
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('每日运行趋势')
    const table = panel.getByRole('table', { name: '每日运行趋势' })
    expect(within(table).getAllByRole('row')).toHaveLength(21)
    expect(within(table).getByText('2026-10-20')).toBeInTheDocument()
    expect(within(table).queryByText('2026-10-21')).not.toBeInTheDocument()

    await user.click(panel.getByRole('button', { name: '下一页' }))
    expect(within(table).getAllByRole('row')).toHaveLength(6)
    expect(within(table).getByText('2026-10-25')).toBeInTheDocument()
    expect(panel.getByText('第 2 / 2 页 · 共 25 条')).toBeInTheDocument()
  })
})

describe('statistics table labels and alignment', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('gives every table a Chinese accessible name', async () => {
    routeStats({
      runs: 1,
      trend: [day('2026-10-01', 1)],
      by_project: [{ project_id: 1, runs: 1, blocking: 0, published: 0, duration_samples: 1, avg_duration_ms: 900, p95_duration_ms: 1200, trend: [] }],
      by_provider: [{ provider: 'anthropic', runs: 1, duration_samples: 1, avg_duration_ms: 900, p95_duration_ms: 1200, trend: [] }],
      by_block: [{ block: 'propose', runs: 1, duration_samples: 1, avg_duration_ms: 900, p95_duration_ms: 1200, trend: [] }],
      by_model: [modelRow],
      quality_by_provider: [{
        provider: 'anthropic',
        proposed: 4, reviewed: 2, valid: 1, invalid: 1, accuracy: 0.5, overturned: 1, overturned_ratio: 0.25,
        refuted: 2, refuted_reviewed: 2, false_refuted: 1, false_kill_rate: 0.5,
      }],
      feedback: {
        total: 1,
        by_signal: { thumbsup: 1 },
        by_provider: [{ provider: 'anthropic', role: 'proposer', total: 1, by_signal: { thumbsup: 1 } }],
      },
    })
    renderWithProviders(<StatisticsPage />)

    await panelByTitle('按模型')
    const names = screen.getAllByRole('table').map((table) => table.getAttribute('aria-label'))
    expect(names).toEqual([
      '每日运行趋势',
      '按提出方统计的人工质量指标',
      '按挑战方统计的人工质量指标',
      '按 Provider 与角色归因的反馈计数',
      '按项目统计明细',
      '按 Provider 统计明细',
      '按 Block 统计明细',
      '按模型统计明细',
    ])
    for (const name of names) expect(name).toMatch(/[一-鿿]/)
  })

  it('aligns the provider attribution counts right and groups thousands', async () => {
    routeStats({
      feedback: {
        total: 1234,
        by_signal: { thumbsup: 1234 },
        by_provider: [{ provider: 'anthropic', role: 'proposer', total: 1234, by_signal: { thumbsup: 1234 } }],
      },
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('反馈信号')
    const table = panel.getByRole('table', { name: '按 Provider 与角色归因的反馈计数' })
    expect(headers(table)).toEqual(['提供方', '角色', '计数', '信号'])
    expect(within(table).getByRole('columnheader', { name: '计数' }).className).toMatch(/cellNum/)
    const row = rowFor(table, 'anthropic')
    expect(row.getByText('1,234').className).toMatch(/cellNum/)
    expect(row.getByText('thumbsup: 1,234')).toBeInTheDocument()
    // One badge for the role, nothing stacked under it.
    expect(row.getByText('提出方').className).toMatch(/badge/)
  })

  it('uses 提供方 headers and percentages with two decimals below one percent in the quality tables', async () => {
    routeStats({
      quality_by_provider: [{
        provider: 'anthropic',
        proposed: 7280, reviewed: 7280, valid: 7270, invalid: 10, accuracy: 7270 / 7280, overturned: 10, overturned_ratio: 10 / 7280,
        refuted: 0, refuted_reviewed: 0, false_refuted: 0, false_kill_rate: null,
      }],
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('人工质量指标')
    const proposer = panel.getByRole('table', { name: '按提出方统计的人工质量指标' })
    expect(headers(proposer)).toEqual(['提供方', '提出问题', '已复核', '有效', '无效', '准确率', '被推翻', '推翻率'])
    for (const name of ['提出问题', '已复核', '有效', '无效', '准确率', '被推翻', '推翻率']) {
      expect(within(proposer).getByRole('columnheader', { name }).className).toMatch(/cellNum/)
    }
    const row = rowFor(proposer, 'anthropic')
    expect(row.getByText('7,270 / 7,280')).toBeInTheDocument()
    expect(row.getByText('99.9%')).toBeInTheDocument()
    expect(row.getByText('0.14%')).toBeInTheDocument()
    expect(row.getAllByText('7,280').length).toBeGreaterThan(0)
    expect(proposer.textContent).not.toMatch(/\d\.\d{4,}/)
  })

  it('shows the per-role empty text instead of an empty table', async () => {
    routeStats({
      quality_by_provider: [{
        provider: 'anthropic',
        proposed: 3, reviewed: 0, valid: 0, invalid: 0, accuracy: null, overturned: 0, overturned_ratio: 0,
        refuted: 0, refuted_reviewed: 0, false_refuted: 0, false_kill_rate: null,
      }],
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await panelByTitle('人工质量指标')
    expect(panel.getByText('没有挑战方样本。')).toBeInTheDocument()
    expect(panel.queryByRole('table', { name: '按挑战方统计的人工质量指标' })).not.toBeInTheDocument()
    expect(panel.getByRole('table', { name: '按提出方统计的人工质量指标' })).toBeInTheDocument()
  })
})
