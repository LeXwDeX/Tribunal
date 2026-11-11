import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ProjectsPage } from '../src/pages/ProjectsPage'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

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

  it('offers registration to a signed-in viewer', async () => {
    installFetch(baseRoutes())
    renderWithProviders(<ProjectsPage />, { route: '/projects', me: viewerMe })
    expect(await screen.findByRole('button', { name: '注册项目' })).toBeInTheDocument()
  })

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

  it('saves registration without any webhook field and ignores a returned secret', async () => {
    const registerBodies: Array<Record<string, unknown>> = []
    const webhookCalls: string[] = []
    installFetch((url, init) => {
      if (url.includes('/webhook')) webhookCalls.push(url)
      if (url === '/api/v1/projects' && init?.method === 'POST') {
        registerBodies.push(JSON.parse(String(init.body)))
        return jsonResponse({ project: project(), job_id: 'job-1', webhook_secret: 'whsec_once_only' }, 201)
      }
      return baseRoutes()(url, init)
    })
    const user = await openRegisterDialog()
    expect(screen.queryByLabelText(/自动创建 GitLab webhook/)).not.toBeInTheDocument()
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
    expect(dialog).toHaveTextContent('group/backend')
    expect(screen.getByText(/加密保存访问 Key/)).toBeInTheDocument()
    expect(screen.getByText(/会调用模型并消耗 token/)).toBeInTheDocument()
    expect(screen.queryByText(/费用/)).not.toBeInTheDocument()
    expect(dialog).toHaveTextContent('Webhook 需在项目设置中单独配置')
    expect(registerBodies).toHaveLength(1)
    expect(registerBodies[0]).toEqual({
      project: repositoryUrl,
      token: accessToken,
      profile: 'default',
      settings: { skip_draft: true },
    })
    expect(registerBodies[0]).not.toHaveProperty('auto_webhook')
    expect(screen.queryByLabelText('Webhook secret')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain('whsec_once_only')
    expect(webhookCalls).toEqual([])
    await user.click(screen.getByRole('button', { name: '完成' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    const registerButtons = await screen.findAllByRole('button', { name: '注册项目' })
    await user.click(registerButtons[0])
    expect(screen.getByLabelText('仓库地址或项目 ID')).toHaveValue('')
    expect(screen.getByLabelText('访问 Key')).toHaveValue('')
    expect(screen.queryByText('校验通过，尚未保存')).not.toBeInTheDocument()
    expect(screen.queryByText('项目已保存，初始化已开始')).not.toBeInTheDocument()
    expect(webhookCalls).toEqual([])
  })

  it('sends the single review profile, edited branches and draft settings without webhook fields', async () => {
    const registerBodies: Array<Record<string, unknown>> = []
    installFetch((url, init) => {
      if (url === '/api/v1/projects' && init?.method === 'POST') {
        registerBodies.push(JSON.parse(String(init.body)))
        return jsonResponse({ project: project(), job_id: 'job-2' }, 201)
      }
      return baseRoutes()(url, init)
    })
    const user = await openRegisterDialog()
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)
    await user.type(screen.getByLabelText('访问 Key'), accessToken)
    expect(screen.queryByLabelText('评审档位')).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('目标分支'), 'main, release/*')
    await user.click(screen.getByLabelText('跳过 Draft MR'))
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    await user.click(screen.getByRole('button', { name: '保存并初始化' }))
    await screen.findByRole('dialog', { name: '项目已保存，初始化已开始' })
    expect(registerBodies).toEqual([{
      project: repositoryUrl,
      token: accessToken,
      profile: 'default',
      settings: { skip_draft: false, target_branches: ['main', 'release/*'] },
    }])
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

describe('already registered conflict (#277)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('tells the registrant to ask an administrator for access', async () => {
    installFetch(baseRoutes({
      register: () => jsonResponse({ error: {
        code: 'conflict',
        message: 'project is already registered; ask a platform administrator to assign it to you',
      } }, 409),
    }))
    const user = await openRegisterDialog()
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)
    await user.type(screen.getByLabelText('访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    await user.click(screen.getByRole('button', { name: '保存并初始化' }))

    expect(await screen.findByText(/该项目已注册。若项目列表中看不到它/)).toBeInTheDocument()
    expect(screen.getByText(/请联系管理员把该项目分配给你/)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '前往系统页授权' })).not.toBeInTheDocument()
    expect(screen.queryByText('保存并初始化失败')).not.toBeInTheDocument()
    expect(screen.queryByText(/ask a platform administrator/)).not.toBeInTheDocument()
  })

  it('shows no grant hint for other conflicts', async () => {
    installFetch(baseRoutes({
      register: () => jsonResponse({ error: {
        code: 'conflict',
        message: 'local project data remains; manual cleanup is required before registration',
      } }, 409),
    }))
    const user = await openRegisterDialog()
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)
    await user.type(screen.getByLabelText('访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    await user.click(screen.getByRole('button', { name: '保存并初始化' }))

    expect(await screen.findByText('保存并初始化失败')).toBeInTheDocument()
    expect(screen.queryByText(/请联系管理员把该项目分配给你/)).not.toBeInTheDocument()
  })
})

describe('project validation result layout (#251)', () => {
  afterEach(() => vi.unstubAllGlobals())

  // jsdom does not compute layout, so these assertions pin the CSS contract that
  // prevents the regression; the rendered widths were checked in a browser.
  it('lays out long identity and many scopes as label/value rows with a bounded label column', async () => {
    const longIdentity = 'project_201_bot_7f3a9c2e1b4d6f8a0c2e4b6d8f0a1c3e5b7d9f1a'
    const scopes = ['api', 'read_api', 'read_repository', 'write_repository', 'read_registry', 'write_registry', 'read_user', 'ai_features']
    installFetch(baseRoutes({ validate: () => jsonResponse(validation({ username: longIdentity, token_scopes: scopes })) }))
    const user = await openRegisterDialog()
    await user.type(screen.getByLabelText('仓库地址或项目 ID'), repositoryUrl)
    await user.type(screen.getByLabelText('访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))

    const panel = (await screen.findByText('校验通过，尚未保存')).closest('[role="status"]') as HTMLElement
    const identity = within(panel).getByText(longIdentity)
    const grid = identity.closest('dl') as HTMLElement
    const columns = getComputedStyle(grid).gridTemplateColumns
    expect(columns, `label column must be capped, got "${columns}"`).toMatch(/^fit-content\(.+\)\s+minmax\(0(px)?,\s*1fr\)$/)

    for (const label of ['项目', '当前身份', '所需 scope', '仓库 HEAD']) {
      const term = within(grid).getByText(label, { selector: 'dt' })
      const row = term.parentElement as HTMLElement
      // Each dt/dd wrapper must not become a grid cell of its own.
      expect(getComputedStyle(row).display, `${label} row wrapper`).toBe('contents')
    }

    const value = identity.closest('dd') as HTMLElement
    expect(getComputedStyle(value).minWidth).toBe('0')
    expect(getComputedStyle(value).overflowWrap).toBe('anywhere')
    for (const scope of scopes) expect(within(grid).getByText(scope)).toBeInTheDocument()
  })
})
