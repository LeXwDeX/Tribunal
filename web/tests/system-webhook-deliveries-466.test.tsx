import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function routes(deliveries: Record<string, unknown>) {
  installFetch((url) => {
    const parsed = new URL(url, 'http://localhost')
    if (parsed.pathname === '/api/v1/webhook-deliveries') return jsonResponse(deliveries)
    if (parsed.pathname === '/api/v1/projects') {
      return jsonResponse({ projects: [{ id: 42, path_with_namespace: 'group/backend', access: 'read' }] })
    }
    if (parsed.pathname === '/api/v1/api-tokens') return jsonResponse({ tokens: [] })
    return jsonResponse({ api_key_configured: false })
  })
}

describe('webhook delivery view (#466)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('groups deliveries by MR and exposes reasons without actions', async () => {
    routes({
      deliveries: [{
        project_id: 42,
        mr_iid: 7,
        deliveries: 4,
        enqueued: 0,
        ignored: 3,
        rejected: 1,
        rejected_invalid_secret: 1,
        last_received_at: new Date(Date.now() - 120_000).toISOString(),
        reasons: { unsupported_action: 2, branch_filtered: 1 },
      }],
      next_cursor: '',
    })
    const user = userEvent.setup()
    renderWithProviders(<SystemPage />)
    await user.click(await screen.findByRole('tab', { name: 'Webhook 投递' }))
    const title = await screen.findByRole('heading', { name: 'Webhook 投递' })
    const panel = title.closest('section')!
    const row = within(panel).getByText('group/backend').closest('tr')!
    expect(within(row).getByText('!7')).toBeInTheDocument()
    expect(within(row).getByText('unsupported_action: 2')).toBeInTheDocument()
    expect(within(row).getByText('branch_filtered: 1')).toBeInTheDocument()
    const cells = within(row).getAllByRole('cell')
    expect(cells[3]).toHaveTextContent('4')
    expect(cells[5]).toHaveTextContent('3')
    expect(cells[6]).toHaveTextContent('1')
    expect(cells[7]).toHaveTextContent('1')
    expect(within(panel).queryByRole('button')).not.toBeInTheDocument()
  })

  it('shows a non-MR delivery bucket instead of inventing an MR', async () => {
    routes({
      deliveries: [{
        project_id: 42,
        mr_iid: 0,
        deliveries: 1,
        enqueued: 0,
        ignored: 1,
        rejected: 0,
        rejected_invalid_secret: 0,
        last_received_at: new Date(Date.now() - 120_000).toISOString(),
        reasons: { unsupported_action: 1 },
      }],
      next_cursor: '',
    })
    const user = userEvent.setup()
    renderWithProviders(<SystemPage />)
    await user.click(await screen.findByRole('tab', { name: 'Webhook 投递' }))
    const title = await screen.findByRole('heading', { name: 'Webhook 投递' })
    const panel = title.closest('section')!
    expect(within(panel).getByText('未知 MR')).toBeInTheDocument()
  })

  it('keeps the empty state when no delivery was recorded', async () => {
    routes({ deliveries: [], next_cursor: '' })
    const user = userEvent.setup()
    renderWithProviders(<SystemPage />)
    await user.click(await screen.findByRole('tab', { name: 'Webhook 投递' }))
    expect(await screen.findByText('没有 Webhook 投递')).toBeInTheDocument()
  })
})