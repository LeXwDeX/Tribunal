import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { StatisticsPage } from '../src/pages/StatisticsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function routeStats(stats: Record<string, unknown>) {
  installFetch((url) => {
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/stats/reviews') return jsonResponse({})
    if (parsed.pathname === '/api/v1/stats') return jsonResponse(stats)
    if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
    return jsonResponse({})
  })
}

describe('statistics degradation and capacity (#466)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows feedback collector health and keeps a stored error visible', async () => {
    routeStats({
      feedback_collection: {
        last_collected_at: new Date(Date.now() - 3_600_000).toISOString(),
        last_error_class: 'upstream',
        last_error_at: new Date(Date.now() - 600_000).toISOString(),
      },
    })
    renderWithProviders(<StatisticsPage />)
    const title = await screen.findByRole('heading', { name: '反馈采集健康' })
    const panel = title.closest('section')!
    expect(within(panel).getByText('最后成功采集')).toBeInTheDocument()
    expect(within(panel).getByText('最近错误类别')).toBeInTheDocument()
    const alert = within(panel).getByRole('alert')
    expect(alert).toHaveTextContent('upstream')
    expect(alert).toHaveTextContent('反馈采集最近失败')
  })

  it('states a healthy collector without an alert', async () => {
    routeStats({
      feedback_collection: { last_collected_at: new Date(Date.now() - 3_600_000).toISOString() },
    })
    renderWithProviders(<StatisticsPage />)
    const title = await screen.findByRole('heading', { name: '反馈采集健康' })
    const panel = title.closest('section')!
    expect(within(panel).queryByRole('alert')).not.toBeInTheDocument()
    expect(within(panel).getByRole('status')).toHaveTextContent('当前没有记录的采集错误')
  })

  it('warns when the workspace capacity is over the configured limit', async () => {
    routeStats({
      workspace_capacity: {
        measured_at: new Date(Date.now() - 900_000).toISOString(),
        total_bytes: 3 * 1024 * 1024 * 1024,
        limit_bytes: 2 * 1024 * 1024 * 1024,
        over_limit: true,
      },
    })
    renderWithProviders(<StatisticsPage />)
    const title = await screen.findByRole('heading', { name: '工作区容量' })
    const panel = title.closest('section')!
    expect(within(panel).getByText('已用')).toBeInTheDocument()
    expect(within(panel).getByText('3.0 GiB')).toBeInTheDocument()
    expect(within(panel).getByText('2.0 GiB')).toBeInTheDocument()
    expect(within(panel).getByRole('alert')).toHaveTextContent('工作区容量已超过配置上限')
  })

  it('shows per-model error categories from errors_by_kind', async () => {
    routeStats({
      by_model: [{
        provider: 'deepseek', model: 'deepseek-chat', calls: 12, errors: 3,
        error_rate: 0.25, input_tokens: 1200, output_tokens: 400, avg_latency_ms: 1500,
        errors_by_kind: { transient: 2, context_too_long: 1 },
      }],
    })
    renderWithProviders(<StatisticsPage />)
    const title = await screen.findByRole('heading', { name: '按模型' })
    const panel = title.closest('section')!
    expect(within(panel).getByText('deepseek')).toBeInTheDocument()
    expect(within(panel).getByText('deepseek-chat')).toBeInTheDocument()
    expect(within(panel).getByText('transient: 2')).toBeInTheDocument()
    expect(within(panel).getByText('context_too_long: 1')).toBeInTheDocument()
  })
})