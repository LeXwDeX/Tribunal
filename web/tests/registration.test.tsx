import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ProjectsPage } from '../src/pages/ProjectsPage'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const repositoryUrl = 'https://gitlab.example.com/group/backend'
const accessToken = 'glpat-secret'

function validation(overrides: Record<string, unknown> = {}) {
  return {
    project_id: 42,
    path_with_namespace: 'group/backend',
    web_url: repositoryUrl,
    default_branch: 'main',
    username: 'review-bot',
    access_level: 30,
    token_scopes: ['api', 'read_repository'],
    token_expires_at: '2027-01-02T00:00:00Z',
    git_head: '0123456789abcdef0123456789abcdef01234567',
    ...overrides,
  }
}

async function openRegisterDialog() {
  const user = userEvent.setup()
  renderWithProviders(<ProjectsPage />, { route: '/projects' })
  const registerButtons = await screen.findAllByRole('button', { name: '注册项目' })
  await user.click(registerButtons[0])
  return user
}

function baseRoutes(handlers: { validate?: () => Response; register?: () => Response } = {}) {
  return (url: string, init?: RequestInit) => {
    if (url === '/api/v1/projects/validate' && init?.method === 'POST') {
      return handlers.validate ? handlers.validate() : jsonResponse(validation())
    }
    if (url === '/api/v1/projects' && init?.method === 'POST') {
      return handlers.register
        ? handlers.register()
        : jsonResponse({ project: project(), job_id: 'job-1' }, 201)
    }
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
    return jsonResponse({})
  }
}

describe('project registration', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('validates a full repository URL without registering the project', async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    let registered = false
    installFetch((url, init) => {
      if (init?.method === 'POST') requests.push({ url, init })
      if (url === '/api/v1/projects' && init?.method === 'POST') registered = true
      return baseRoutes()(url, init)
    })
    const user = await openRegisterDialog()
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)
    await user.type(screen.getByLabelText('访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))

    expect(await screen.findByText('校验通过，尚未保存')).toBeInTheDocument()
    expect(screen.getByText('Developer（30）')).toBeInTheDocument()
    expect(screen.getByText('read_repository')).toBeInTheDocument()
    expect(screen.getByText('0123456789abcdef0123456789abcdef01234567')).toBeInTheDocument()
    expect(screen.getByText(/不证明评论写入、Webhook 或完整 clone/)).toBeInTheDocument()
    expect(requests).toHaveLength(1)
    expect(requests[0].url).toBe('/api/v1/projects/validate')
    expect(JSON.parse(String(requests[0].init?.body))).toEqual({ project: repositoryUrl, token: accessToken })
    expect(registered).toBe(false)
    expect(screen.queryByText(/项目已注册|项目已保存/)).not.toBeInTheDocument()
  })

  it('saves with auto_webhook off only after validation and clears the one-time secret on close', async () => {
    const registerBodies: unknown[] = []
    installFetch((url, init) => {
      if (url === '/api/v1/projects' && init?.method === 'POST') {
        registerBodies.push(JSON.parse(String(init.body)))
        return jsonResponse({ project: project(), job_id: 'job-1', webhook_secret: 'whsec_once_only' }, 201)
      }
      return baseRoutes()(url, init)
    })
    const user = await openRegisterDialog()
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)
    await user.type(screen.getByLabelText('访问 Key'), accessToken)
    const save = screen.getByRole('button', { name: '保存并初始化' })
    expect(save).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    expect(save).toBeEnabled()
    await user.click(save)

    const dialog = await screen.findByRole('dialog', { name: '项目已保存，初始化已开始' })
    expect(dialog).toHaveTextContent('job-1')
    expect(screen.getByText(/加密保存访问 Key/)).toBeInTheDocument()
    expect(screen.getByText(/可能产生模型费用/)).toBeInTheDocument()
    expect(registerBodies).toHaveLength(1)
    expect(registerBodies[0]).toMatchObject({
      project: repositoryUrl,
      token: accessToken,
      auto_webhook: false,
      settings: { skip_draft: true },
    })
    const secret = screen.getByLabelText('Webhook secret')
    expect(secret).toHaveValue('whsec_once_only')
    expect(secret).toHaveAttribute('readonly')
    await user.click(screen.getByRole('button', { name: '完成' }))

    await waitFor(() => expect(screen.queryByDisplayValue('whsec_once_only')).not.toBeInTheDocument())
    const registerButtons = await screen.findAllByRole('button', { name: '注册项目' })
    await user.click(registerButtons[0])
    expect(screen.getByLabelText('访问 Key')).toHaveValue('')
    expect(screen.queryByText('校验通过，尚未保存')).not.toBeInTheDocument()
  })

  it('invalidates validation when the address or key changes', async () => {
    installFetch(baseRoutes())
    const user = await openRegisterDialog()
    const address = screen.getByLabelText('仓库地址或项目 ID')
    const key = screen.getByLabelText('访问 Key')
    await user.type(address, repositoryUrl)
    await user.type(key, accessToken)
    const save = screen.getByRole('button', { name: '保存并初始化' })
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    expect(save).toBeEnabled()

    await user.type(address, '/')
    expect(screen.queryByText('校验通过，尚未保存')).not.toBeInTheDocument()
    expect(save).toBeDisabled()

    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    expect(save).toBeEnabled()
    await user.type(key, 'x')
    expect(screen.queryByText('校验通过，尚未保存')).not.toBeInTheDocument()
    expect(save).toBeDisabled()
  })

  it('keeps a failed validation retryable and never saves', async () => {
    let attempts = 0
    let registered = false
    installFetch((url, init) => {
      if (url === '/api/v1/projects' && init?.method === 'POST') registered = true
      if (url === '/api/v1/projects/validate' && init?.method === 'POST') {
        attempts += 1
        if (attempts === 1) {
          return jsonResponse({
            error: { code: 'validation_error', message: '无法读取项目', details: [{ field: 'token', message: 'Key 无效' }] },
          }, 422)
        }
        return jsonResponse(validation())
      }
      return baseRoutes()(url, init)
    })
    const user = await openRegisterDialog()
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)
    await user.type(screen.getByLabelText('访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))

    expect(await screen.findByText(/无法读取项目/)).toBeInTheDocument()
    expect(screen.queryByText('校验通过，尚未保存')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '保存并初始化' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    expect(await screen.findByText('校验通过，尚未保存')).toBeInTheDocument()
    expect(attempts).toBe(2)
    expect(registered).toBe(false)
  })

  it('clears the key and validation state when closed before saving', async () => {
    installFetch(baseRoutes())
    const user = await openRegisterDialog()
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)
    await user.type(screen.getByLabelText('访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    await user.click(screen.getByRole('button', { name: '取消' }))

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain('glpat-secret')
    expect(Object.keys(window.localStorage || {})).toHaveLength(0)
    expect(Object.keys(window.sessionStorage || {})).toHaveLength(0)
    expect(window.location.href).not.toContain('glpat-secret')

    const registerButtons = await screen.findAllByRole('button', { name: '注册项目' })
    await user.click(registerButtons[0])
    expect(screen.getByLabelText('访问 Key')).toHaveValue('')
    expect(screen.queryByText('校验通过，尚未保存')).not.toBeInTheDocument()
  })
})
