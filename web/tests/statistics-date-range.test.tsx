import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StatisticsPage, dateRange } from '../src/pages/StatisticsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function routeStats(calls: string[]) {
  installFetch((url) => {
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/projects') return jsonResponse({ projects: [] })
    if (parsed.pathname === '/api/v1/stats') {
      calls.push(url)
      return jsonResponse({ runs: 1 })
    }
    return jsonResponse({})
  })
}

describe('dateRange', () => {
  it('ends the range at the next local midnight, not the last millisecond', () => {
    expect(dateRange({ fromDate: '2026-10-02', toDate: '2026-10-08' })).toEqual({
      from: new Date(2026, 9, 2).toISOString(),
      to: new Date(2026, 9, 9).toISOString(),
    })
  })

  it('uses calendar days so the range stays one full day across DST', () => {
    expect(dateRange({ fromDate: '2026-11-01', toDate: '2026-11-01' })).toEqual({
      from: new Date(2026, 10, 1).toISOString(),
      to: new Date(2026, 10, 2).toISOString(),
    })
  })

  it('rejects impossible dates instead of rolling them over', () => {
    expect(dateRange({ fromDate: '2026-02-30', toDate: '2026-03-01' })).toBeNull()
    expect(dateRange({ fromDate: '2026-10-02', toDate: '2026-13-01' })).toBeNull()
  })

  it('rejects an inverted range and extreme values without throwing', () => {
    expect(dateRange({ fromDate: '2026-10-08', toDate: '2026-10-02' })).toBeNull()
    expect(dateRange({ fromDate: '9999-99-99', toDate: '9999-99-99' })).toBeNull()
    expect(dateRange({ fromDate: '9999-12-31', toDate: '9999-12-31' })).toBeNull()
  })
})

describe('statistics date filters', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('requests stats with the inclusive start and the next local midnight', async () => {
    const calls: string[] = []
    routeStats(calls)
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)

    fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: '2026-10-02' } })
    fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '2026-10-08' } })
    await user.click(screen.getByRole('button', { name: '应用' }))

    const from = new Date(2026, 9, 2).toISOString()
    const to = new Date(2026, 9, 9).toISOString()
    await waitFor(() => {
      const match = calls
        .map((url) => new URL(url, 'http://localhost'))
        .find((parsed) => parsed.searchParams.get('from') === from)
      expect(match).toBeDefined()
      expect(match?.searchParams.get('to')).toBe(to)
    })
  })

  it('rejects a range whose next local midnight leaves the supported year', async () => {
    const calls: string[] = []
    routeStats(calls)
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)
    await waitFor(() => expect(calls.length).toBe(1))

    fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: '9999-12-31' } })
    fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '9999-12-31' } })
    await user.click(screen.getByRole('button', { name: '应用' }))

    expect(screen.getByRole('alert')).toBeInTheDocument()
    expect(calls).toHaveLength(1)
    expect(screen.getByText('运行次数')).toBeInTheDocument()
  })

  it('does not submit an impossible date entered through the DOM', async () => {
    const calls: string[] = []
    routeStats(calls)
    const user = userEvent.setup()
    renderWithProviders(<StatisticsPage />)
    await waitFor(() => expect(calls.length).toBe(1))

    fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: '9999-99-99' } })
    fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '9999-99-99' } })
    await user.click(screen.getByRole('button', { name: '应用' }))

    expect(screen.getByRole('alert')).toBeInTheDocument()
    expect(calls).toHaveLength(1)
    expect(screen.getByText('运行次数')).toBeInTheDocument()
  })
})
