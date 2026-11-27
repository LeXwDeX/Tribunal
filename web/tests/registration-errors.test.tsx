import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ApiError } from '../src/api/client'
import { describeRegistrationError, describeRegistrationField } from '../src/lib/diagnostics'
import { ProjectsPage } from '../src/pages/ProjectsPage'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const repositoryUrl = 'https://gitlab.example.com/group/backend'

function apiError(status: number, code: string, message: string, details?: Array<{ field: string; message: string }>, requestId?: string) {
  return new ApiError(status, code, message, requestId, details)
}

const sharedBotNotFound = () => apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 404)', [
  { field: 'shared_bot_access', message: 'the shared bot cannot see this project; check the project address and add the shared bot to the project with Reporter or higher (Guest, Minimal Access or a pending invitation is not enough)' },
], 'req-1')

describe('registration error copy (#653)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('names the shared bot membership when the bot cannot see the project', () => {
    const text = describeRegistrationError(sharedBotNotFound())
    expect(text).toMatch(/平台 bot/)
    expect(text).toMatch(/Reporter/)
    expect(text).toMatch(/Guest/)
    expect(text).not.toMatch(/shared bot|GitLab rejected/)
  })

  it('explains a shared bot below Reporter', () => {
    const text = describeRegistrationError(apiError(400, 'validation_failed', 'shared bot access level is insufficient', [
      { field: 'shared_bot_access', message: 'the shared bot needs Reporter or higher on this project to read code and publish review notes' },
    ]))
    expect(text).toMatch(/平台 bot.*Reporter/)
  })

  it('separates an invalid and a forbidden shared bot credential', () => {
    const invalid = describeRegistrationError(apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 401)', [
      { field: 'shared_bot_credential', message: 'the shared bot credential is invalid, expired or revoked; update the credential referenced by gitlab.bot_token_env and restart the service' },
    ]))
    expect(invalid).toMatch(/无效、已过期或已撤销/)
    const forbidden = describeRegistrationError(apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 403)', [
      { field: 'shared_bot_credential', message: 'the shared bot credential is not permitted; check its scopes and the shared bot\'s project role' },
    ]))
    expect(forbidden).toMatch(/scope/)
    expect(forbidden).toMatch(/角色/)
    const scope = describeRegistrationError(apiError(400, 'validation_failed', 'shared bot credential scopes are insufficient', [
      { field: 'shared_bot_credential', message: 'read_repository scope is required' },
    ]))
    expect(scope).toMatch(/缺少 read_repository scope/)
  })

  it('asks for Maintainer on the provisioning credential when the webhook is forbidden', () => {
    const text = describeRegistrationError(apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 403)', [
      { field: 'provisioning_credential', message: 'managing project webhooks requires the provisioning credential to hold Maintainer on this project; grant the role or have a Maintainer manage the webhook in GitLab' },
    ]))
    expect(text).toMatch(/Webhook/)
    expect(text).toMatch(/Maintainer/)
    expect(text).not.toMatch(/provisioning_credential/)
  })

  it('explains a missing provisioning credential', () => {
    const text = describeRegistrationError(apiError(400, 'validation_failed', 'provisioning credential is required for automatic webhook creation', [
      { field: 'provisioning_credential', message: 'configure gitlab.provisioning_token_env with a token that can manage project webhooks, then restart the service' },
    ]))
    expect(text).toMatch(/未配置/)
  })

  it('tells a registrant below Reporter to request the role', () => {
    const text = describeRegistrationError(apiError(400, 'validation_failed', 'access level is insufficient', [
      { field: 'access_level', message: 'Reporter or higher is required to register this project' },
    ]))
    expect(text).toMatch(/Reporter/)
    expect(text).not.toMatch(/required to register/)
  })

  it('gives a non-administrator an actionable unified denial', () => {
    const error = apiError(400, 'validation_failed', 'project registration is not permitted', [
      { field: 'project', message: 'the project cannot be registered by this account' },
    ])
    const text = describeRegistrationError(error)
    expect(text).toMatch(/Reporter/)
    expect(text).toMatch(/平台管理员/)
    expect(describeRegistrationField(error, { field: 'project', message: 'the project cannot be registered by this account' })).toMatch(/无法登记/)
  })

  it('explains a bare GitLab 404 and keeps the request ID for unknown failures', () => {
    expect(describeRegistrationError(apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 404)'))).toMatch(/找不到该项目/)
    const unknown = describeRegistrationError(apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 418)', undefined, 'req-9'))
    expect(unknown).toMatch(/418/)
    expect(unknown).toMatch(/req-9/)
  })

  it('uses the real server strings for unconfigured and under-scoped platform credentials', () => {
    expect(describeRegistrationError(apiError(400, 'validation_failed', 'shared bot credential is not configured', [
      { field: 'shared_bot_credential', message: 'configure gitlab.bot_token_env and restart the service' },
    ]))).toMatch(/未配置/)
    expect(describeRegistrationError(apiError(400, 'validation_failed', 'provisioning credential scopes are insufficient', [
      { field: 'provisioning_credential', message: 'api scope is required to manage webhooks' },
    ]))).toMatch(/缺少 api scope/)
  })

  it('keeps an unmapped detail next to the mapped hints', () => {
    const text = describeRegistrationError(apiError(400, 'validation_failed', 'invalid', [
      { field: 'access_level', message: 'Reporter or higher is required to register this project' },
      { field: 'something_new', message: 'raw detail' },
    ]))
    expect(text).toMatch(/Reporter/)
    expect(text).toMatch(/raw detail/)
  })

  it('words the unified denial without assuming a shared bot', () => {
    const text = describeRegistrationError(apiError(400, 'validation_failed', 'project registration is not permitted', [
      { field: 'project', message: 'the project cannot be registered by this account' },
    ]))
    expect(text).toMatch(/访问 Key/)
  })

  it('points a webhook 404 at the provisioning account, not the project address', () => {
    const text = describeRegistrationError(apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 404)', [
      { field: 'provisioning_credential', message: 'the provisioning credential cannot see this project; add its account to the project with Maintainer or have a Maintainer manage the webhook in GitLab' },
    ]))
    expect(text).toMatch(/Webhook 凭据/)
    expect(text).toMatch(/Maintainer/)
    expect(text).not.toMatch(/仓库地址/)
  })

  it('leaves an unknown shared bot credential message untranslated', () => {
    const error = apiError(400, 'validation_failed', 'x', [{ field: 'shared_bot_credential', message: 'something new' }])
    expect(describeRegistrationField(error, { field: 'shared_bot_credential', message: 'something new' })).toBeNull()
    expect(describeRegistrationField(error, { field: 'shared_bot_credential', message: 'must be active and not revoked' })).toMatch(/未激活或已撤销/)
  })

  it('maps the remaining token and rate-limit cases', () => {
    const error = apiError(400, 'validation_failed', 'x')
    expect(describeRegistrationField(error, { field: 'token.scopes', message: 'api scope is required' })).toMatch(/缺少 api scope/)
    expect(describeRegistrationField(error, { field: 'token', message: 'must not be expired' })).toMatch(/已过期/)
    expect(describeRegistrationField(error, { field: 'token', message: 'must not be empty' })).toMatch(/请填写访问 Key/)
    expect(describeRegistrationError(apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 429)'))).toMatch(/限流/)
    expect(describeRegistrationError(apiError(422, 'gitlab_rejected', 'GitLab rejected the request (status 403)', [
      { field: 'access_level', message: 'managing project webhooks requires the Maintainer role; use a Maintainer token or have a Maintainer manage the webhook in GitLab' },
    ]))).toMatch(/Maintainer/)
  })

  it('leaves unrelated errors to the default rendering', () => {
    expect(describeRegistrationError(new Error('boom'))).toBeNull()
    expect(describeRegistrationError(apiError(403, 'forbidden', 'forbidden'))).toBeNull()
  })

  it('shows the shared bot hint instead of the raw upstream text in the dialog', async () => {
    installFetch((url, init) => {
      if (url === '/api/v1/projects' && init?.method === 'POST') {
        return jsonResponse({ error: {
          code: 'gitlab_rejected', message: 'GitLab rejected the request (status 404)', request_id: 'req-1',
          details: [{ field: 'shared_bot_access', message: 'the shared bot cannot see this project; check the project address and add the shared bot to the project with Reporter or higher (Guest, Minimal Access or a pending invitation is not enough)' }],
        } }, 422)
      }
      if (url === '/api/v1/projects/validate' && init?.method === 'POST') {
        return jsonResponse({ error: {
          code: 'gitlab_rejected', message: 'GitLab rejected the request (status 404)',
          details: [{ field: 'shared_bot_access', message: 'the shared bot cannot see this project' }],
        } }, 422)
      }
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<ProjectsPage />, { route: '/projects', me: { ...viewerMe, shared_bot_credential: true } })
    const registerButtons = await screen.findAllByRole('button', { name: '注册项目' })
    await user.click(registerButtons[0])
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)

    await user.click(screen.getByRole('button', { name: '校验连接' }))
    expect(await screen.findByText('校验失败')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '保存并初始化' }))
    const alert = (await screen.findByText('保存并初始化失败')).closest('[role="alert"]')
    expect(screen.queryByText('校验失败')).not.toBeInTheDocument()
    expect(alert).not.toBeNull()
    expect(alert).toHaveTextContent(/平台 bot.*Reporter/)
    expect(alert).not.toHaveTextContent(/GitLab rejected|shared_bot_access/)
  })
})
