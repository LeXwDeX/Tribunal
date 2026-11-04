import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SettingsPanel } from '../src/features/project/SettingsPanel'
import { describeWorkspaceError } from '../src/lib/diagnostics'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function webhookPanel() {
  const heading = screen.getByRole('heading', { name: 'GitLab Webhook' })
  const section = heading.closest('section')
  if (!section) throw new Error('webhook panel is not a section')
  return within(section)
}

function routes(initial = project(), webhook?: () => Response, validate?: () => Response) {
  return (url: string, init?: RequestInit) => {
    if (url.endsWith('/webhook') && webhook) return webhook()
    if (url === '/api/v1/projects/validate' && validate) return validate()
    if (url.startsWith('/api/v1/projects/42')) return jsonResponse(initial)
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [initial] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
    return jsonResponse({})
  }
}

describe('workspace and token diagnostics (#279)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('maps fixed workspace failures to actionable hints and keeps the code', () => {
    expect(describeWorkspaceError('gitlab: project token rejected (401)')).toMatch(/Key 无效、已过期或已撤销.*gitlab: project token rejected \(401\)/)
    expect(describeWorkspaceError('gitlab: access denied (403)')).toMatch(/Reporter/)
    expect(describeWorkspaceError('git: branch not found')).toMatch(/分支不存在/)
    expect(describeWorkspaceError('git: network error')).toMatch(/网络/)
    expect(describeWorkspaceError('onboarding: model provider rejected credentials')).toMatch(/模型服务拒绝了凭据/)
    expect(describeWorkspaceError('something new')).toBe('something new')
  })

  it('names the Maintainer requirement when enabling the webhook is forbidden', async () => {
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined })
    installFetch(routes(disabled, () => jsonResponse({ error: {
      code: 'gitlab_rejected', message: 'GitLab rejected the request (status 403)',
      details: [{ field: 'access_level', message: 'managing project webhooks requires the Maintainer role' }],
    } }, 422)))
    renderWithProviders(<SettingsPanel project={disabled} />)
    await userEvent.setup().click(webhookPanel().getByRole('button', { name: '启用 GitLab Activity' }))
    const alert = await webhookPanel().findByRole('alert')
    expect(alert).toHaveTextContent('需要 Maintainer 角色')
    expect(alert).toHaveTextContent('403')
  })

  it('names an invalid token when enabling the webhook is unauthorized', async () => {
    const disabled = project({ webhook_state: 'disabled', hook_id: undefined })
    installFetch(routes(disabled, () => jsonResponse({ error: {
      code: 'gitlab_rejected', message: 'GitLab rejected the request (status 401)',
      details: [{ field: 'token', message: 'the project token is invalid, expired or revoked; replace it' }],
    } }, 422)))
    renderWithProviders(<SettingsPanel project={disabled} />)
    await userEvent.setup().click(webhookPanel().getByRole('button', { name: '启用 GitLab Activity' }))
    const alert = await webhookPanel().findByRole('alert')
    expect(alert).toHaveTextContent('Key 无效、已过期或已撤销')
  })

  it('explains a stored webhook failure caused by a missing Maintainer role', async () => {
    const failed = project({ webhook_state: 'failed', hook_id: undefined, webhook_error: 'GitLab webhook list failed (status 403): Maintainer role required' })
    installFetch(routes(failed))
    renderWithProviders(<SettingsPanel project={failed} />)
    expect(await webhookPanel().findByText(/需要 Maintainer 角色/)).toBeInTheDocument()
  })

  it('warns during key validation that a Developer key cannot enable the webhook', async () => {
    installFetch(routes(project(), undefined, () => jsonResponse({
      project_id: 42, path_with_namespace: 'group/backend', web_url: 'https://gitlab.example.com/group/backend',
      default_branch: 'main', username: 'review-bot', access_level: 30, token_scopes: ['api', 'read_repository'],
      git_head: '0123456789abcdef0123456789abcdef01234567',
      warnings: [{ code: 'webhook_requires_maintainer', message: 'the token role is below Maintainer' }],
    })))
    renderWithProviders(<SettingsPanel project={project()} />)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: '更新访问 Key' }))
    await user.type(screen.getByLabelText('新的访问 Key'), 'glpat-new-secret')
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    expect(screen.getByText(/低于 Maintainer/)).toBeInTheDocument()
  })
})
