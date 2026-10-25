import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SettingsPanel } from '../src/features/project/SettingsPanel'
import type { Project, ProjectWebhookState } from '../src/api/types'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const webhookResponse = (overrides: Partial<ProjectWebhookState> = {}): ProjectWebhookState => ({
  project: project(),
  hook_id: 99,
  state: 'enabled',
  events: ['push_events', 'merge_requests_events', 'note_events'],
  last_error: '',
  managed_by: 'crc',
  ...overrides,
})

function routes(
  initial: Project,
  calls: Array<{ url: string; method: string }>,
  response: () => Response = () => jsonResponse(webhookResponse()),
) {
  return (url: string, init?: RequestInit) => {
    const method = init?.method || 'GET'
    if (url.endsWith('/webhook') || url.endsWith('/webhook/rebuild')) {
      calls.push({ url, method })
      return response()
    }
    if (url.startsWith('/api/v1/projects/42')) return jsonResponse(initial)
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [initial] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
    return jsonResponse({})
  }
}

describe('project webhook settings', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the disabled state and enables GitLab Activity explicitly', async () => {
    const calls: Array<{ url: string; method: string }> = []
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined })
    installFetch(routes(disabled, calls))
    renderWithProviders(<SettingsPanel project={disabled} />)

    expect(await screen.findByText('未启用')).toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '启用 GitLab Activity' }))
    await waitFor(() => expect(calls).toEqual([
      { url: '/api/v1/projects/42/webhook', method: 'POST' },
    ]))
    expect(await screen.findByText(/GitLab webhook 已启用/)).toBeInTheDocument()
  })

  it('shows state, events and the sanitized last error for an enabled hook', async () => {
    const enabled = project({
      hook_id: 99,
      webhook_state: 'enabled',
      webhook_events: ['push_events', 'merge_requests_events', 'note_events'],
      webhook_error: 'GitLab webhook update failed (status 403)',
      webhook_managed_by: 'crc',
      webhook_updated_at: '2026-10-25T02:00:00Z',
    })
    installFetch(routes(enabled, []))
    renderWithProviders(<SettingsPanel project={enabled} />)

    expect(await screen.findByText('已启用')).toBeInTheDocument()
    expect(screen.getByText(/hook #99/)).toBeInTheDocument()
    expect(screen.getByText(/Push · Merge request · Note/)).toBeInTheDocument()
    expect(screen.getByText('GitLab webhook update failed (status 403)')).toBeInTheDocument()
    expect(screen.getByText(/托管方：本服务/)).toBeInTheDocument()
  })

  it('rebuilds only after confirmation and calls the rebuild endpoint', async () => {
    const calls: Array<{ url: string; method: string }> = []
    const enabled = project({ hook_id: 99, webhook_state: 'enabled', webhook_managed_by: 'crc' })
    installFetch(routes(enabled, calls))
    renderWithProviders(<SettingsPanel project={enabled} />)

    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: '重建 webhook' }))
    const dialog = screen.getByRole('dialog', { name: '重建 GitLab webhook？' })
    await user.click(within(dialog).getByRole('button', { name: '重建 webhook' }))
    await waitFor(() => expect(calls).toEqual([
      { url: '/api/v1/projects/42/webhook/rebuild', method: 'POST' },
    ]))
  })

  it('disables only after confirmation and calls DELETE', async () => {
    const calls: Array<{ url: string; method: string }> = []
    const enabled = project({ hook_id: 99, webhook_state: 'enabled', webhook_managed_by: 'crc' })
    installFetch(routes(enabled, calls, () => jsonResponse(webhookResponse({
      hook_id: 0, state: 'disabled', last_error: '',
    }))))
    renderWithProviders(<SettingsPanel project={enabled} />)

    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: '禁用 webhook' }))
    const dialog = screen.getByRole('dialog', { name: '禁用 GitLab webhook？' })
    await user.click(within(dialog).getByRole('button', { name: '禁用 webhook' }))
    await waitFor(() => expect(calls).toEqual([
      { url: '/api/v1/projects/42/webhook', method: 'DELETE' },
    ]))
    expect(await screen.findByText(/GitLab webhook 已禁用/)).toBeInTheDocument()
  })

  it('keeps read-only accounts on the status view without write controls', async () => {
    const enabled = project({
      access: 'read', hook_id: 99, webhook_state: 'enabled', webhook_managed_by: 'crc',
    })
    installFetch(routes(enabled, []))
    renderWithProviders(<SettingsPanel project={enabled} />)

    expect(await screen.findByText('已启用')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重建 webhook' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '禁用 webhook' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '启用 GitLab Activity' })).not.toBeInTheDocument()
  })
})
