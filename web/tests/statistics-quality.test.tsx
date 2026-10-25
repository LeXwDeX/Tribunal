import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { Project, ProviderQualityRow } from '../src/api/types'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function qualityRow(overrides: Partial<ProviderQualityRow> = {}): ProviderQualityRow {
  return {
    provider: 'anthropic',
    proposed: 10,
    reviewed: 5,
    valid: 4,
    invalid: 1,
    accuracy: 0.8,
    overturned: 2,
    overturned_ratio: 0.2,
    refuted: 0,
    refuted_reviewed: 0,
    false_refuted: 0,
    false_kill_rate: null,
    ...overrides,
  }
}

function routeStats(stats: Record<string, unknown>, projects: Project[] = []) {
  installFetch((url) => {
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects })
    if (url.startsWith('/api/v1/stats')) return jsonResponse(stats)
    return jsonResponse({})
  })
}

async function qualityPanel() {
  const heading = await screen.findByRole('heading', { name: '人工质量指标' })
  const panel = heading.closest('section')
  if (!panel) throw new Error('quality panel not found')
  return within(panel)
}

function rowFor(table: HTMLElement, provider: string) {
  const row = within(table).getByText(provider).closest('tr')
  if (!row) throw new Error(`row not found: ${provider}`)
  return row
}

describe('statistics provider quality', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows role-specific rates with numerators and denominators, and no fake zero for empty samples', async () => {
    routeStats({
      quality_by_provider: [
        qualityRow(),
        qualityRow({
          provider: 'openai',
          proposed: 0,
          reviewed: 0,
          valid: 0,
          invalid: 0,
          accuracy: null,
          overturned: 0,
          overturned_ratio: null,
          refuted: 4,
          refuted_reviewed: 2,
          false_refuted: 1,
          false_kill_rate: 0.5,
        }),
        qualityRow({
          provider: 'local',
          proposed: 3,
          reviewed: 0,
          valid: 0,
          invalid: 0,
          accuracy: null,
          overturned: 0,
          overturned_ratio: 0,
          refuted: 0,
          refuted_reviewed: 0,
          false_refuted: 0,
          false_kill_rate: null,
        }),
      ],
    })
    renderWithProviders(<StatisticsPage />)

    const panel = await qualityPanel()
    const proposer = panel.getByRole('table', { name: '按提出方统计的人工质量指标' })
    expect(within(proposer).queryByText('openai')).not.toBeInTheDocument()
    const anthropic = rowFor(proposer, 'anthropic')
    expect(within(anthropic).getByText('80.0%')).toBeInTheDocument()
    expect(within(anthropic).getByText('4 / 5')).toBeInTheDocument()
    expect(within(anthropic).getByText('20.0%')).toBeInTheDocument()
    expect(within(anthropic).getByText('2 / 10')).toBeInTheDocument()
    expect(within(anthropic).getByText('2')).toBeInTheDocument()
    expect(within(anthropic).getByText('4')).toBeInTheDocument()
    expect(within(anthropic).getByText('1')).toBeInTheDocument()

    const local = rowFor(proposer, 'local')
    expect(within(local).getByText('无样本')).toBeInTheDocument()
    expect(within(local).getByText('0 / 0')).toBeInTheDocument()
    expect(within(local).getByText('0.0%')).toBeInTheDocument()
    expect(within(local).getByText('0 / 3')).toBeInTheDocument()

    const challenger = panel.getByRole('table', { name: '按挑战方统计的人工质量指标' })
    expect(within(challenger).queryByText('anthropic')).not.toBeInTheDocument()
    const openai = rowFor(challenger, 'openai')
    expect(within(openai).getByText('4')).toBeInTheDocument()
    expect(within(openai).getByText('2')).toBeInTheDocument()
    expect(within(openai).getByText('1')).toBeInTheDocument()
    expect(within(openai).getByText('50.0%')).toBeInTheDocument()
    expect(within(openai).getByText('1 / 2')).toBeInTheDocument()

    expect(panel.getByText(/未标注和未确定不计入/)).toBeInTheDocument()
    expect(panel.getByText(/没有样本时显示“无样本”/)).toBeInTheDocument()
    expect(panel.getByText(/不会改变发布结果或评审结论/)).toBeInTheDocument()
  })

  it('marks a missing field as unavailable, not a zero result', async () => {
    routeStats({ runs: 1 })
    renderWithProviders(<StatisticsPage />)

    const panel = await qualityPanel()
    expect(panel.getByText('人工质量指标不可用')).toBeInTheDocument()
    expect(panel.getByText(/旧版服务端/)).toBeInTheDocument()
    expect(panel.queryByRole('table')).not.toBeInTheDocument()
  })

  it('shows a known empty array as an empty result, not unavailable', async () => {
    routeStats({ quality_by_provider: [] })
    renderWithProviders(<StatisticsPage />)

    const panel = await qualityPanel()
    expect(panel.getByText('没有人工质量样本')).toBeInTheDocument()
    expect(panel.getByText(/不是不可用/)).toBeInTheDocument()
    expect(panel.queryByText('人工质量指标不可用')).not.toBeInTheDocument()
    expect(panel.queryByRole('table')).not.toBeInTheDocument()
  })

  it.each([
    ['negative count', [qualityRow({ proposed: -1 })]],
    ['fractional count', [qualityRow({ reviewed: 1.5, valid: 1, invalid: 0.5 })]],
    ['accuracy above one', [qualityRow({ accuracy: 1.2 })]],
    ['accuracy null with reviewed samples', [qualityRow({ accuracy: null })]],
    ['overturned ratio null with proposed samples', [qualityRow({ overturned_ratio: null })]],
    ['false kill rate numeric without samples', [qualityRow({ false_kill_rate: 0 })]],
    ['reviewed mismatch', [qualityRow({ valid: 3 })]],
    ['reviewed exceeds proposed', [qualityRow({ proposed: 2 })]],
    ['duplicate provider', [qualityRow(), qualityRow()]],
    ['non-array', { provider: 'anthropic' }],
    ['empty provider', [qualityRow({ provider: '' })]],
    ['false refuted exceeds reviewed', [qualityRow({ refuted: 2, refuted_reviewed: 1, false_refuted: 2, false_kill_rate: 0.5 })]],
  ])('fails closed on malformed provider quality: %s', async (_label, value) => {
    routeStats({ quality_by_provider: value })
    renderWithProviders(<StatisticsPage />)

    const panel = await qualityPanel()
    expect(panel.getByText('人工质量指标数据格式无效')).toBeInTheDocument()
    expect(panel.queryByRole('table')).not.toBeInTheDocument()
    expect(panel.queryByText('anthropic')).not.toBeInTheDocument()
    expect(panel.queryByText('openai')).not.toBeInTheDocument()
  })

  it('preserves the applied project and date filters when rendering quality data', async () => {
    const urls: string[] = []
    installFetch((url) => {
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project()] })
      if (url.startsWith('/api/v1/stats')) {
        urls.push(url)
        return jsonResponse({ quality_by_provider: [qualityRow()] })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)
    await qualityPanel()

    await user.selectOptions(screen.getByRole('combobox', { name: '项目' }), '42')
    await user.click(screen.getByRole('button', { name: '应用' }))

    await waitFor(() => {
      const last = new URL(urls[urls.length - 1], 'http://localhost')
      expect(last.searchParams.get('project_id')).toBe('42')
    })
    const last = new URL(urls[urls.length - 1], 'http://localhost')
    expect(last.searchParams.get('from')).toBeTruthy()
    expect(last.searchParams.get('to')).toBeTruthy()
    const panel = await qualityPanel()
    expect(panel.getByText('anthropic')).toBeInTheDocument()
  })
})
