import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SettingsPanel } from '../src/features/project/SettingsPanel'
import type { Project } from '../src/api/types'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const accessToken = 'glpat-new-secret'

function validation(overrides: Record<string, unknown> = {}) {
  return {
    project_id: 42,
    path_with_namespace: 'group/backend',
    web_url: 'https://gitlab.example.com/group/backend',
    default_branch: 'main',
    username: 'review-bot',
    access_level: 30,
    token_scopes: ['api', 'read_repository'],
    token_expires_at: '2027-01-02T00:00:00Z',
    git_head: '0123456789abcdef0123456789abcdef01234567',
    ...overrides,
  }
}

function baseRoutes(validate: () => Response, onRequest?: (url: string, init?: RequestInit) => void) {
  return (url: string, init?: RequestInit) => {
    onRequest?.(url, init)
    if (url === '/api/v1/projects/validate' && init?.method === 'POST') return validate()
    if (url === '/api/v1/projects/42/token' && init?.method === 'PUT') return new Response(null, { status: 204 })
    if (url.startsWith('/api/v1/projects/42')) return jsonResponse(project())
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project()] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
    return jsonResponse({})
  }
}

async function openKeyDialog() {
  const user = userEvent.setup()
  renderWithProviders(<SettingsPanel project={project()} />)
  await user.click(await screen.findByRole('button', { name: '更新访问 Key' }))
  return user
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((resolvePromise) => { resolve = resolvePromise })
  return { promise, resolve }
}

function renderSwitchablePanel(initial: Project) {
  let switchProject: (next: Project) => void = () => {}
  function Harness() {
    const [current, setCurrent] = useState(initial)
    switchProject = setCurrent
    return <SettingsPanel project={current} />
  }
  const view = renderWithProviders(<Harness />)
  return { ...view, switchTo: (next: Project) => act(() => switchProject(next)) }
}

describe('project access key update', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('validates the key against the current project before storing it', async () => {
    const calls: Array<{ url: string; method: string; body?: unknown }> = []
    installFetch(baseRoutes(() => jsonResponse(validation()), (url, init) => {
      calls.push({
        url,
        method: init?.method || 'GET',
        body: init?.body ? JSON.parse(String(init.body)) : undefined,
      })
    }))
    const user = await openKeyDialog()
    expect(screen.getByRole('dialog', { name: '更新访问 Key' })).toBeInTheDocument()
    await user.type(screen.getByLabelText('新的访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    expect(await screen.findByText('校验通过，尚未保存')).toBeInTheDocument()
    expect(calls.find((call) => call.url === '/api/v1/projects/validate')?.body).toEqual({ project: '42', token: accessToken })
    expect(calls.some((call) => call.url === '/api/v1/projects/42/token' && call.method === 'PUT')).toBe(false)

    await user.click(screen.getByRole('button', { name: '保存访问 Key' }))
    expect(await screen.findByText(/仅更新平台保存的访问 Key/)).toBeInTheDocument()
    expect(screen.getByText(/未在 GitLab 创建或撤销 Token/)).toBeInTheDocument()
    expect(screen.getByText(/不会自动触发评审或初始化/)).toBeInTheDocument()
    const put = calls.find((call) => call.url === '/api/v1/projects/42/token' && call.method === 'PUT')
    expect(put?.body).toEqual({ token: accessToken })
    expect(calls.findIndex((call) => call.url === '/api/v1/projects/validate')).toBeLessThan(
      calls.findIndex((call) => call.url === '/api/v1/projects/42/token' && call.method === 'PUT'),
    )
    expect(calls.filter((call) => call.url === '/api/v1/projects/validate')).toHaveLength(1)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('keeps the key update retryable when storing fails', async () => {
    let putAttempts = 0
    installFetch((url, init) => {
      if (url === '/api/v1/projects/42/token' && init?.method === 'PUT') {
        putAttempts += 1
        if (putAttempts === 1) {
          return jsonResponse({ error: { code: 'internal', message: '保存失败' } }, 500)
        }
        return new Response(null, { status: 204 })
      }
      return baseRoutes(() => jsonResponse(validation()))(url, init)
    })
    const user = await openKeyDialog()
    await user.type(screen.getByLabelText('新的访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    await user.click(screen.getByRole('button', { name: '保存访问 Key' }))

    expect(await screen.findByText('保存失败')).toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: '更新访问 Key' })).toBeInTheDocument()
    expect(screen.queryByText(/仅更新平台保存的访问 Key/)).not.toBeInTheDocument()
    expect(screen.getByLabelText('新的访问 Key')).toHaveValue(accessToken)
    const save = screen.getByRole('button', { name: '保存访问 Key' })
    expect(save).toBeEnabled()
    await user.click(save)
    expect(await screen.findByText(/仅更新平台保存的访问 Key/)).toBeInTheDocument()
    expect(putAttempts).toBe(2)
  })

  it('does not store the key when validation returns a different project and allows a retry', async () => {
    let attempts = 0
    const puts: Array<unknown> = []
    installFetch((url, init) => {
      if (url === '/api/v1/projects/42/token' && init?.method === 'PUT') {
        puts.push(JSON.parse(String(init.body)))
      }
      if (url === '/api/v1/projects/validate' && init?.method === 'POST') {
        attempts += 1
        return jsonResponse(validation(attempts === 1 ? { project_id: 99 } : {}))
      }
      return baseRoutes(() => jsonResponse(validation()))(url, init)
    })
    const user = await openKeyDialog()
    await user.type(screen.getByLabelText('新的访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    expect(await screen.findByText(/校验返回的项目与当前项目不一致，未更新访问 Key/)).toBeInTheDocument()
    expect(screen.queryByText('校验通过，尚未保存')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '保存访问 Key' })).toBeDisabled()
    expect(puts).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: '校验连接' }))
    expect(await screen.findByText('校验通过，尚未保存')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '保存访问 Key' }))
    await screen.findByText(/仅更新平台保存的访问 Key/)
    expect(attempts).toBe(2)
    expect(puts).toEqual([{ token: accessToken }])
  })

  it('resets key state on project switch and ignores a late response from the old project', async () => {
    const oldValidation = deferred<Response>()
    const newValidation = deferred<Response>()
    const validationProjects: string[] = []
    const puts: string[] = []
    installFetch((url, init) => {
      if (url === '/api/v1/projects/validate' && init?.method === 'POST') {
        const body = JSON.parse(String(init.body)) as { project: string }
        validationProjects.push(body.project)
        return body.project === '42' ? oldValidation.promise : newValidation.promise
      }
      if (url === '/api/v1/projects/43/token' && init?.method === 'PUT') {
        puts.push(url)
        return new Response(null, { status: 204 })
      }
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
      return jsonResponse({})
    })
    const { switchTo } = renderSwitchablePanel(project())
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: '更新访问 Key' }))
    await user.type(screen.getByLabelText('新的访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))

    switchTo(project({ id: 43, path_with_namespace: 'group/other' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    await user.click(screen.getByRole('button', { name: '更新访问 Key' }))
    const validate = screen.getByRole('button', { name: '校验连接' })
    expect(validate).toBeEnabled()
    await user.type(screen.getByLabelText('新的访问 Key'), accessToken)
    await user.click(validate)
    await waitFor(() => expect(validationProjects).toEqual(['42', '43']))

    await act(async () => { oldValidation.resolve(jsonResponse(validation())) })
    expect(screen.queryByText('校验通过，尚未保存')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '保存访问 Key' })).toBeDisabled()

    await act(async () => {
      newValidation.resolve(jsonResponse(validation({ project_id: 43, path_with_namespace: 'group/other' })))
    })
    expect(await screen.findByText('校验通过，尚未保存')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '保存访问 Key' }))
    expect(await screen.findByText(/仅更新平台保存的访问 Key/)).toBeInTheDocument()
    expect(puts).toEqual(['/api/v1/projects/43/token'])
  })

  it('keeps a newer save pending when an older project save settles', async () => {
    const oldSave = deferred<Response>()
    const newSave = deferred<Response>()
    installFetch((url, init) => {
      if (url === '/api/v1/projects/validate' && init?.method === 'POST') {
        const body = JSON.parse(String(init.body)) as { project: string }
        return jsonResponse(validation({
          project_id: Number(body.project),
          path_with_namespace: body.project === '43' ? 'group/other' : 'group/backend',
        }))
      }
      if (url === '/api/v1/projects/42/token' && init?.method === 'PUT') return oldSave.promise
      if (url === '/api/v1/projects/43/token' && init?.method === 'PUT') return newSave.promise
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
      return jsonResponse({})
    })
    const { switchTo } = renderSwitchablePanel(project())
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: '更新访问 Key' }))
    await user.type(screen.getByLabelText('新的访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    await user.click(screen.getByRole('button', { name: '保存访问 Key' }))
    await waitFor(() => expect(screen.getByRole('button', { name: '保存访问 Key' })).toBeDisabled())

    switchTo(project({ id: 43, path_with_namespace: 'group/other' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    const newToken = 'glpat-other-secret'
    await user.click(screen.getByRole('button', { name: '更新访问 Key' }))
    expect(screen.getByRole('button', { name: '校验连接' })).toBeEnabled()
    await user.type(screen.getByLabelText('新的访问 Key'), newToken)
    await user.click(screen.getByRole('button', { name: '校验连接' }))
    await screen.findByText('校验通过，尚未保存')
    await user.click(screen.getByRole('button', { name: '保存访问 Key' }))
    await waitFor(() => expect(screen.getByRole('button', { name: '保存访问 Key' })).toBeDisabled())

    await act(async () => { oldSave.resolve(new Response(null, { status: 204 })) })
    expect(screen.getByRole('dialog', { name: '更新访问 Key' })).toBeInTheDocument()
    expect(screen.getByLabelText('新的访问 Key')).toHaveValue(newToken)
    expect(screen.getByRole('button', { name: '保存访问 Key' })).toBeDisabled()
    expect(screen.queryByText(/仅更新平台保存的访问 Key/)).not.toBeInTheDocument()

    await act(async () => { newSave.resolve(new Response(null, { status: 204 })) })
    expect(await screen.findByText(/仅更新平台保存的访问 Key/)).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('lets a non-admin manage user store the key without the admin preflight or global profiles', async () => {
    const calls: Array<{ url: string; method: string; body?: unknown }> = []
    installFetch((url, init) => {
      calls.push({
        url,
        method: init?.method || 'GET',
        body: init?.body ? JSON.parse(String(init.body)) : undefined,
      })
      if (url === '/api/v1/projects/42/token' && init?.method === 'PUT') return new Response(null, { status: 204 })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
      if (url.startsWith('/api/v1/projects')) return jsonResponse(project())
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<SettingsPanel project={project({ access: 'manage' })} />, { me: viewerMe })
    await user.click(await screen.findByRole('button', { name: '更新访问 Key' }))
    expect(screen.queryByRole('button', { name: '校验连接' })).not.toBeInTheDocument()
    expect(screen.getAllByText(/保存时由服务端校验/).length).toBeGreaterThan(0)
    await user.type(screen.getByLabelText('新的访问 Key'), accessToken)
    await user.click(screen.getByRole('button', { name: '保存访问 Key' }))
    await waitFor(() => expect(calls.some((call) => call.url === '/api/v1/projects/42/token' && call.method === 'PUT')).toBe(true))
    expect(calls.find((call) => call.url === '/api/v1/projects/42/token')?.body).toEqual({ token: accessToken })
    expect(calls.some((call) => call.url === '/api/v1/projects/validate')).toBe(false)
    expect(calls.some((call) => call.url.startsWith('/api/v1/profiles'))).toBe(false)
  })

  it('does not offer the key update to a viewer', async () => {
    const fetchMock = installFetch(baseRoutes(() => jsonResponse(validation())))
    renderWithProviders(<SettingsPanel project={project({ access: 'read' })} />, { me: viewerMe })
    expect(await screen.findByText('项目设置（只读）')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '更新访问 Key' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '轮换 token' })).not.toBeInTheDocument()
    expect(fetchMock.mock.calls.some(([input]) => String(input).startsWith('/api/v1/profiles'))).toBe(false)
  })
})
