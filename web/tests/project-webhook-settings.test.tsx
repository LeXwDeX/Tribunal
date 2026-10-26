import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SettingsPanel } from '../src/features/project/SettingsPanel'
import type { Project, ProjectWebhookState } from '../src/api/types'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function webhookPanel() {
  const heading = screen.getByRole('heading', { name: 'GitLab Webhook' })
  const section = heading.closest('section')
  if (!section) throw new Error('webhook panel is not a section')
  return within(section)
}

function deferred() {
  let resolve: (value: Response) => void = () => {}
  const promise = new Promise<Response>((done) => { resolve = done })
  return { promise, resolve }
}

const failure = (status: number, message: string) => jsonResponse({ error: { code: 'gitlab_rejected', message } }, status)


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

  it('puts webhook controls in their own panel, separate from the access key', async () => {
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined, token_scopes: ['api'] })
    installFetch(routes(disabled, []))
    renderWithProviders(<SettingsPanel project={disabled} />)

    const panel = webhookPanel()
    expect(panel.getByText('未启用')).toBeInTheDocument()
    expect(panel.getByRole('button', { name: '启用 GitLab Activity' })).toBeInTheDocument()
    expect(panel.queryByRole('button', { name: '更新访问 Key' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '更新访问 Key' })).toBeInTheDocument()
  })

  it('shows progress, then echoes the server hook id and events after enabling', async () => {
    const calls: Array<{ url: string; method: string }> = []
    const gate = deferred()
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined })
    installFetch(routes(disabled, calls, () => gate.promise as unknown as Response))
    renderWithProviders(<SettingsPanel project={disabled} />)
    const user = userEvent.setup()

    const enable = webhookPanel().getByRole('button', { name: '启用 GitLab Activity' })
    await user.click(enable)
    expect(await webhookPanel().findByText('正在启用 Webhook…')).toBeInTheDocument()
    expect(enable).toBeDisabled()
    await user.click(enable)
    gate.resolve(jsonResponse(webhookResponse({ hook_id: 321, events: ['merge_requests_events', 'note_events'] })))

    expect(await webhookPanel().findByText('Webhook 已启用')).toBeInTheDocument()
    const panel = webhookPanel()
    expect(panel.getByText('已启用')).toBeInTheDocument()
    expect(panel.getByText(/hook #321/)).toBeInTheDocument()
    expect(panel.getByText(/Merge request · Note/)).toBeInTheDocument()
    expect(panel.getByText(/托管方：本服务/)).toBeInTheDocument()
    expect(calls).toEqual([{ url: '/api/v1/projects/42/webhook', method: 'POST' }])
  })

  it('keeps a failed enable retryable and clears the error once it succeeds', async () => {
    const calls: Array<{ url: string; method: string }> = []
    let attempts = 0
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined })
    installFetch(routes(disabled, calls, () => {
      attempts += 1
      return attempts === 1
        ? failure(502, 'GitLab webhook create failed (status 403)')
        : jsonResponse(webhookResponse({ hook_id: 77 }))
    }))
    renderWithProviders(<SettingsPanel project={disabled} />)
    const user = userEvent.setup()

    await user.click(webhookPanel().getByRole('button', { name: '启用 GitLab Activity' }))
    const alert = await webhookPanel().findByRole('alert')
    expect(alert).toHaveTextContent('启用 Webhook 失败')
    expect(alert).toHaveTextContent('GitLab webhook create failed (status 403)')

    await user.click(webhookPanel().getByRole('button', { name: '启用 GitLab Activity' }))
    expect(await webhookPanel().findByText('Webhook 已启用')).toBeInTheDocument()
    expect(webhookPanel().queryByRole('alert')).not.toBeInTheDocument()
    expect(webhookPanel().getByText(/hook #77/)).toBeInTheDocument()
    expect(calls).toHaveLength(2)
  })

  it('echoes the sanitized server error from a failed webhook state', async () => {
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined })
    installFetch(routes(disabled, [], () => jsonResponse(webhookResponse({
      hook_id: 0, state: 'failed', events: [], last_error: 'GitLab webhook list failed (status 403)',
    }))))
    renderWithProviders(<SettingsPanel project={disabled} />)
    const user = userEvent.setup()

    await user.click(webhookPanel().getByRole('button', { name: '启用 GitLab Activity' }))
    expect(await webhookPanel().findByText('配置失败')).toBeInTheDocument()
    expect(webhookPanel().getByText('GitLab webhook list failed (status 403)')).toBeInTheDocument()
    expect(webhookPanel().getByRole('button', { name: '重试启用 GitLab Activity' })).toBeInTheDocument()
  })

  it('clears a failed rebuild when its dialog closes and does not leak it into the panel', async () => {
    const calls: Array<{ url: string; method: string }> = []
    const enabled = project({ hook_id: 99, webhook_state: 'enabled', webhook_managed_by: 'crc' })
    installFetch(routes(enabled, calls, () => failure(502, 'GitLab webhook update failed (status 500)')))
    renderWithProviders(<SettingsPanel project={enabled} />)
    const user = userEvent.setup()

    await user.click(webhookPanel().getByRole('button', { name: '重建 webhook' }))
    let dialog = screen.getByRole('dialog', { name: '重建 GitLab webhook？' })
    await user.click(within(dialog).getByRole('button', { name: '重建 webhook' }))
    expect(await within(dialog).findByText(/GitLab webhook update failed \(status 500\)/)).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: '取消' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(webhookPanel().queryByRole('alert')).not.toBeInTheDocument()
    expect(webhookPanel().getByText('已启用')).toBeInTheDocument()
    expect(webhookPanel().getByText(/hook #99/)).toBeInTheDocument()

    await user.click(webhookPanel().getByRole('button', { name: '重建 webhook' }))
    dialog = screen.getByRole('dialog', { name: '重建 GitLab webhook？' })
    expect(within(dialog).queryByRole('alert')).not.toBeInTheDocument()
    expect(calls).toEqual([{ url: '/api/v1/projects/42/webhook/rebuild', method: 'POST' }])
  })

  it('shows the server state after disabling even before the project refetches', async () => {
    const enabled = project({ hook_id: 99, webhook_state: 'enabled', webhook_managed_by: 'crc', webhook_events: ['push_events'] })
    installFetch(routes(enabled, [], () => jsonResponse(webhookResponse({
      hook_id: 0, state: 'disabled', events: [], last_error: '', managed_by: '',
    }))))
    renderWithProviders(<SettingsPanel project={enabled} />)
    const user = userEvent.setup()

    await user.click(webhookPanel().getByRole('button', { name: '禁用 webhook' }))
    const dialog = screen.getByRole('dialog', { name: '禁用 GitLab webhook？' })
    await user.click(within(dialog).getByRole('button', { name: '禁用 webhook' }))
    expect(await webhookPanel().findByText('Webhook 已禁用')).toBeInTheDocument()
    expect(webhookPanel().getByText('未启用')).toBeInTheDocument()
    expect(webhookPanel().queryByText(/hook #99/)).not.toBeInTheDocument()
    expect(webhookPanel().getByRole('button', { name: '启用 GitLab Activity' })).toBeInTheDocument()
  })

  it('does not overwrite unsaved project settings or save them when a webhook action completes', async () => {
    const calls: Array<{ url: string; method: string }> = []
    const patches: string[] = []
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined, settings: { target_branches: ['main'] } })
    const base = routes(disabled, calls)
    installFetch((url, init) => {
      if (init?.method === 'PATCH') patches.push(url)
      return base(url, init)
    })
    renderWithProviders(<SettingsPanel project={disabled} />)
    const user = userEvent.setup()

    const branches = screen.getByLabelText('目标分支')
    await user.clear(branches)
    await user.type(branches, 'release/*')
    await user.click(webhookPanel().getByRole('button', { name: '启用 GitLab Activity' }))
    expect(await webhookPanel().findByText('Webhook 已启用')).toBeInTheDocument()

    expect(screen.getByLabelText('目标分支')).toHaveValue('release/*')
    expect(patches).toEqual([])
    expect(calls).toEqual([{ url: '/api/v1/projects/42/webhook', method: 'POST' }])
  })

  it('never touches the webhook when only project settings are saved', async () => {
    const calls: Array<{ url: string; method: string }> = []
    const patches: Array<Record<string, unknown>> = []
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined })
    const base = routes(disabled, calls)
    installFetch((url, init) => {
      if (init?.method === 'PATCH') {
        patches.push(JSON.parse(String(init.body)))
        return jsonResponse(disabled)
      }
      return base(url, init)
    })
    renderWithProviders(<SettingsPanel project={disabled} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: '保存设置' }))
    await waitFor(() => expect(patches).toHaveLength(1))
    expect(Object.keys(patches[0]).sort()).toEqual(['profile', 'settings'])
    expect(calls).toEqual([])
  })
})
