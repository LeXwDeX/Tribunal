import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function routeStats(stats: Record<string, unknown>) {
  installFetch((url) => {
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    if (url.startsWith('/api/v1/stats')) return jsonResponse(stats)
    return jsonResponse({})
  })
}

async function feedbackPanel() {
  const heading = await screen.findByText('反馈信号')
  const panel = heading.closest('section')
  if (!panel) throw new Error('feedback panel not found')
  return within(panel)
}

describe('statistics feedback', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the actual signal counts without claiming model accuracy', async () => {
    routeStats({
      feedback: { total: 3, by_signal: { thumbsup: 2, thumbsdown: 1 } },
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('3')).toBeInTheDocument()
    expect(panel.getByText('thumbsup: 2').className).toMatch(/badgeOk/)
    expect(panel.getByText('thumbsdown: 1').className).toMatch(/badgeWarn/)
    expect(panel.getByText(/不代表模型准确率/)).toBeInTheDocument()
    expect(screen.getByText('规则丢弃比例')).toBeInTheDocument()
    expect(screen.getByText('运行次数')).toBeInTheDocument()
    expect(screen.getByText(/包含评审与 onboarding/)).toBeInTheDocument()
    expect(screen.queryByText('评审次数')).not.toBeInTheDocument()
  })

  it('renders known signals in a stable order, then unknown labels alphabetically', async () => {
    routeStats({
      feedback: {
        total: 16,
        by_signal: {
          zeta: 1,
          resolved_unchanged: 4,
          thumbsdown: 2,
          alpha: 1,
          resolved_changed: 3,
          thumbsup: 5,
        },
      },
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    const labels = panel.getAllByText(/^[a-z_]+: \d+$/).map((node) => node.textContent)
    expect(labels).toEqual([
      'thumbsup: 5',
      'thumbsdown: 2',
      'resolved_changed: 3',
      'resolved_unchanged: 4',
      'alpha: 1',
      'zeta: 1',
    ])
    expect(panel.getByText('resolved_changed: 3').className).toMatch(/badgeNeutral/)
    expect(panel.getByText('alpha: 1').className).toMatch(/badgeNeutral/)
  })

  it('shows a known empty range as zero, not unavailable', async () => {
    routeStats({ feedback: { total: 0, by_signal: {} } })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('反馈总数')).toBeInTheDocument()
    expect(panel.getByText('0')).toBeInTheDocument()
    expect(panel.getByText(/反馈总数为 0/)).toBeInTheDocument()
  })

  it('shows missing feedback from an older response as unavailable', async () => {
    routeStats({ runs: 1 })
    renderWithProviders(<StatisticsPage />)

    await waitFor(() => expect(screen.getByText('反馈信号')).toBeInTheDocument())
    const panel = await feedbackPanel()
    expect(panel.getByText('反馈统计不可用')).toBeInTheDocument()
    expect(panel.getByText(/旧版服务端/)).toBeInTheDocument()
    expect(panel.queryByText('反馈总数')).not.toBeInTheDocument()
  })

  it('fails closed on negative or non-numeric signal counts without showing a total', async () => {
    routeStats({ feedback: { total: 3, by_signal: { thumbsup: 2, thumbsdown: -1 } } })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('反馈统计不可用')).toBeInTheDocument()
    expect(panel.getByText(/格式无效/)).toBeInTheDocument()
    expect(panel.queryByText('反馈总数')).not.toBeInTheDocument()
    expect(panel.queryByText('thumbsup: 2')).not.toBeInTheDocument()
    expect(panel.queryByText('thumbsdown: -1')).not.toBeInTheDocument()
  })

  it('fails closed on string signal counts', async () => {
    routeStats({ feedback: { total: 2, by_signal: { thumbsup: '2' } } })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('反馈统计不可用')).toBeInTheDocument()
    expect(panel.getByText(/格式无效/)).toBeInTheDocument()
    expect(panel.queryByText('反馈总数')).not.toBeInTheDocument()
    expect(panel.queryByText('thumbsup: 2')).not.toBeInTheDocument()
  })

  it('fails closed when feedback exists but the signal map is null', async () => {
    routeStats({ feedback: { total: 1, by_signal: null } })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('反馈统计不可用')).toBeInTheDocument()
    expect(panel.getByText(/格式无效/)).toBeInTheDocument()
    expect(panel.queryByText('反馈总数')).not.toBeInTheDocument()
  })

  it('shows provider and role attribution as counts, not an accuracy formula', async () => {
    routeStats({
      feedback: {
        total: 3,
        by_signal: { thumbsup: 2, thumbsdown: 1 },
        by_provider: [
          { provider: 'anthropic', role: 'proposer', total: 2, by_signal: { thumbsup: 2 } },
          { provider: 'openai', role: 'challenger', total: 1, by_signal: { thumbsdown: 1 } },
        ],
      },
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    const table = panel.getByRole('table')
    const rows = within(table).getAllByRole('row')
    expect(rows).toHaveLength(3)
    expect(within(table).getByText('anthropic')).toBeInTheDocument()
    expect(within(table).getByText('openai')).toBeInTheDocument()
    expect(within(table).getByText('提出方')).toBeInTheDocument()
    expect(within(table).getByText('挑战方')).toBeInTheDocument()
    expect(within(table).getByText('thumbsup: 2')).toBeInTheDocument()
    expect(within(table).getByText('thumbsdown: 1')).toBeInTheDocument()
    expect(within(table).queryByText(/%/)).not.toBeInTheDocument()
    expect(panel.getByText(/同一信号可能同时计入提出方和挑战方/)).toBeInTheDocument()
    expect(panel.getByText(/不能把两个角色的计数相加当作反馈总数/)).toBeInTheDocument()
    expect(panel.getByText(/summary、未关联或关联不一致的反馈不计入/)).toBeInTheDocument()
    expect(panel.getByText(/不代表.*正确率/)).toBeInTheDocument()
  })

  it('shows a known empty by_provider array without hiding the raw totals', async () => {
    routeStats({ feedback: { total: 2, by_signal: { thumbsup: 2 }, by_provider: [] } })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('没有可归因的反馈')).toBeInTheDocument()
    expect(panel.getByText('反馈总数')).toBeInTheDocument()
    expect(panel.getByText('2')).toBeInTheDocument()
    expect(panel.queryByText('反馈统计不可用')).not.toBeInTheDocument()
  })

  it('marks provider attribution unavailable when an older response omits it', async () => {
    routeStats({ feedback: { total: 1, by_signal: { thumbsup: 1 } } })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('按 provider 归因不可用')).toBeInTheDocument()
    expect(panel.getByText(/旧版服务端/)).toBeInTheDocument()
    expect(panel.getByText('反馈总数')).toBeInTheDocument()
    expect(panel.queryByText('反馈统计不可用')).not.toBeInTheDocument()
  })

  it.each([
    ['negative total', [{ provider: 'anthropic', role: 'proposer', total: -1, by_signal: { thumbsup: 1 } }]],
    ['fractional total', [{ provider: 'anthropic', role: 'proposer', total: 1.5, by_signal: { thumbsup: 1 } }]],
    ['unknown role', [{ provider: 'anthropic', role: 'summary', total: 1, by_signal: { thumbsup: 1 } }]],
    ['empty provider', [{ provider: '', role: 'proposer', total: 1, by_signal: { thumbsup: 1 } }]],
    ['negative signal', [{ provider: 'anthropic', role: 'proposer', total: 1, by_signal: { thumbsup: -1 } }]],
    ['string signal', [{ provider: 'anthropic', role: 'proposer', total: 1, by_signal: { thumbsup: '1' } }]],
    ['non-array provider list', { provider: 'anthropic' }],
  ])('fails closed on malformed provider attribution: %s', async (_label, byProvider) => {
    routeStats({
      feedback: {
        total: 1,
        by_signal: { thumbsup: 1 },
        by_provider: byProvider,
      },
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('反馈统计不可用')).toBeInTheDocument()
    expect(panel.getByText(/格式无效/)).toBeInTheDocument()
    expect(panel.queryByText('反馈总数')).not.toBeInTheDocument()
    expect(panel.queryByRole('table')).not.toBeInTheDocument()
  })

  it('does not silently drop one malformed provider row beside a valid row', async () => {
    routeStats({
      feedback: {
        total: 2,
        by_signal: { thumbsup: 2 },
        by_provider: [
          { provider: 'anthropic', role: 'proposer', total: 1, by_signal: { thumbsup: 1 } },
          { provider: 'openai', role: 'challenger', total: -1, by_signal: { thumbsdown: 1 } },
        ],
      },
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await feedbackPanel()
    expect(panel.getByText('反馈统计不可用')).toBeInTheDocument()
    expect(panel.queryByText('anthropic')).not.toBeInTheDocument()
    expect(panel.queryByText('反馈总数')).not.toBeInTheDocument()
    expect(panel.queryByRole('table')).not.toBeInTheDocument()
  })
})
