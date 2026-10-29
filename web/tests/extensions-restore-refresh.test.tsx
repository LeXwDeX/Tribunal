import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, onlineManager } from '@tanstack/react-query'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { listBindings } from '../src/api/api'
import { setCsrfToken } from '../src/api/client'
import type { BlockBinding, Skill } from '../src/api/types'
import { binding, skill } from './fixtures'
import { createTestQueryClient, installFetch, jsonResponse, renderWithProviders } from './test-utils'

type Grant = { mcp_tools: string[]; skills: string[] }
type RestoreResult = {
  changes: Array<{
    block: string
    status: 'change' | 'unchanged' | 'skipped'
    reason: string
    add: Grant
    remove: Grant
    ownership_changed: boolean
  }>
  preview_token: string
  restart_required: boolean
}
type Recorded = {
  url: string
  method: string
  body?: { dry_run?: boolean; preview_token?: string; skills?: string[]; mcp_tools?: string[] }
}

const restorePreview: RestoreResult = {
  changes: [{
    block: 'review',
    status: 'change',
    reason: '',
    add: { mcp_tools: [], skills: [] },
    remove: { mcp_tools: [], skills: ['builtin-skill'] },
    ownership_changed: false,
  }],
  preview_token: 'restore-token',
  restart_required: false,
}

function setupRestore({
  deferRefresh, deferRefreshFailure, failRefresh, deferPost, uncertainPost, responseShape, conflict, paged,
}: {
  deferRefresh?: boolean
  deferRefreshFailure?: boolean
  failRefresh?: boolean
  deferPost?: boolean
  uncertainPost?: 'server-error' | 'network' | 'timeout' | 'abort' | 'decode'
  responseShape?: 'html' | 'no-content' | 'null'
  conflict?: boolean
  paged?: boolean
}) {
  let current: BlockBinding[] = [
    binding({ scope: 'platform', block: 'review', skills: ['builtin-skill', 'custom-skill'] }),
  ]
  const writes: Recorded[] = []
  const restorePosts: Array<Record<string, unknown>> = []
  const skills: Skill[] = [
    skill({ id: 'k-builtin', name: 'builtin-skill', builtin: true }),
    skill({ id: 'k-custom', name: 'custom-skill' }),
    skill({ id: 'k-another', name: 'another-skill' }),
  ]
  let releaseRefresh: (() => void) | undefined
  let releasePost: (() => void) | undefined
  let pendingRefresh = false
  let failAfterRestore = false
  let reads = 0

  const handler = (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    const body = init?.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined
    if (url.startsWith('/api/v1/block-bindings/restore-defaults')) {
      if (body?.dry_run) return jsonResponse(restorePreview)
      restorePosts.push(body || {})
      if (conflict) {
        // Another admin replaced the bindings after this preview was rendered.
        current = [binding({ scope: 'platform', block: 'review', skills: ['v2-skill'] })]
        if (deferRefresh) pendingRefresh = true
        if (failRefresh) failAfterRestore = true
        return jsonResponse({ error: { code: 'conflict', message: 'stale preview' } }, 409)
      }
      const apply = () => {
        // The server applies the restore when the POST resolves, but the
        // following read is delayed so the UI must not expose the stale matrix.
        current = current.map((item) => (
          item.block === 'review' ? { ...item, skills: ['custom-skill'] } : item
        ))
        if (deferRefresh) pendingRefresh = true
        if (failRefresh) failAfterRestore = true
      }
      if (deferPost) {
        return new Promise<Response>((resolve) => {
          let released = false
          releasePost = () => {
            if (released) return
            released = true
            apply()
            resolve(jsonResponse(restorePreview))
          }
        })
      }
      apply()
      if (uncertainPost === 'server-error') {
        return jsonResponse({ error: { code: 'internal', message: 'restore response lost' } }, 500)
      }
      if (uncertainPost === 'network') throw new TypeError('network down')
      if (uncertainPost === 'timeout') {
        return jsonResponse({ error: { code: 'timeout', message: 'request timeout' } }, 408)
      }
      if (uncertainPost === 'abort') throw new DOMException('aborted', 'AbortError')
      if (uncertainPost === 'decode') {
        return new Response('{not-json', { status: 200, headers: { 'content-type': 'application/json' } })
      }
      if (responseShape === 'html') {
        return new Response('<html>proxy error</html>', { status: 200, headers: { 'content-type': 'text/html' } })
      }
      if (responseShape === 'no-content') return new Response(null, { status: 204 })
      if (responseShape === 'null') return jsonResponse(null)
      return jsonResponse(restorePreview)
    }
    if (url.startsWith('/api/v1/block-bindings')) {
      if (method === 'GET') {
        reads += 1
        if (paged) {
          const cursor = new URL(url, 'http://localhost').searchParams.get('cursor')
          if (cursor === 'page-2') {
            return jsonResponse({ bindings: [binding({ scope: 'platform', block: 'reasoner', skills: ['reasoner-skill'] })] })
          }
          return jsonResponse({
            bindings: current.filter((item) => item.block === 'review'),
            next_cursor: 'page-2',
          })
        }
        if (pendingRefresh) {
          pendingRefresh = false
          return new Promise<Response>((resolve) => {
            releaseRefresh = () => resolve(deferRefreshFailure
              ? jsonResponse({ error: { code: 'internal', message: 'refresh failed' } }, 500)
              : jsonResponse({ bindings: current }))
          })
        }
        if (failAfterRestore) {
          failAfterRestore = false
          return jsonResponse({ error: { code: 'internal', message: 'refresh failed' } }, 500)
        }
        return jsonResponse({ bindings: current })
      }
      if (method === 'PUT' && body) {
        writes.push({ url, method, body })
        const block = decodeURIComponent(url.split('/').pop() || '')
        const next = {
          ...(current.find((item) => item.block === block) || binding({ block })),
          ...body,
        } as BlockBinding
        current = current.map((item) => (item.block === block ? next : item))
        return jsonResponse(next)
      }
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills })
    return jsonResponse({})
  }

  return {
    handler,
    writes,
    restorePosts,
    reads: () => reads,
    releasePost: () => releasePost?.(),
    releaseRefresh: () => releaseRefresh?.(),
  }
}

function renderBindings() {
  return renderWithProviders(<ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />)
}

async function startRestore(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: '恢复 review 的默认绑定' }))
  const dialog = await screen.findByRole('dialog', { name: '恢复 review 的默认绑定' })
  await within(dialog).findByRole('region', { name: 'review' })
  await user.click(within(dialog).getByRole('button', { name: '确认恢复' }))
}

describe('bindings after a successful restore', () => {
  afterEach(() => {
    onlineManager.setOnline(true)
    vi.unstubAllGlobals()
    setCsrfToken(null)
  })

  it('keeps the matrix locked until the refreshed snapshot is applied', async () => {
    const state = setupRestore({ deferRefresh: true })
    installFetch(state.handler)
    setCsrfToken('csrf-restore')
    const user = userEvent.setup()
    renderBindings()

    await startRestore(user)
    await waitFor(() => expect(state.restorePosts).toHaveLength(1))

    // The restore POST is done but the refreshed GET is still pending, so the
    // stale grant matrix and its save action must not be reachable.
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeDisabled()
    expect(await screen.findByText('默认绑定已恢复，正在读取新的绑定…')).toBeInTheDocument()

    await act(async () => { state.releaseRefresh() })
    const custom = await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(custom).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill another-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(state.writes[0]).toMatchObject({
      url: '/api/v1/block-bindings/review',
      method: 'PUT',
      body: { skills: ['custom-skill', 'another-skill'] },
    })
  })

  it('keeps the stale matrix disabled after leaving and reopening the bindings tab during the refresh', async () => {
    const state = setupRestore({ deferRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()

    function Harness() {
      const [tab, setTab] = useState('bindings')
      return <ExtensionsManager scope="platform" tab={tab} onTabChange={setTab} />
    }

    renderWithProviders(<Harness />)
    await startRestore(user)
    await waitFor(() => expect(state.restorePosts).toHaveLength(1))

    await user.click(screen.getByRole('tab', { name: /MCP 服务器/ }))
    await user.click(screen.getByRole('tab', { name: '块绑定' }))

    // The restore operation survives the tab switch, so the stale matrix stays
    // hidden until the refreshed list is merged by the remounted tab.
    expect(await screen.findByText('默认绑定已恢复，正在读取新的绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()

    await act(async () => { state.releaseRefresh() })
    await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
  })


  it('keeps the old matrix locked after a full unmount and remount while the restore POST is pending', async () => {
    const state = setupRestore({ deferPost: true, deferRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    const queryClient = createTestQueryClient()

    function Harness() {
      const [mounted, setMounted] = useState(true)
      return (
        <>
          <button type="button" onClick={() => setMounted((value) => !value)}>离开/返回</button>
          {mounted
            ? <ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />
            : <div>已离开</div>}
        </>
      )
    }

    renderWithProviders(<Harness />, { queryClient })
    await startRestore(user)
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '离开/返回' }))
    expect(screen.getByText('已离开')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '离开/返回' }))

    expect(await screen.findByText('正在恢复默认授权…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()

    await act(async () => { state.releasePost() })
    await waitFor(() => expect(state.reads()).toBeGreaterThanOrEqual(2))
    expect(await screen.findByText('默认绑定已恢复，正在读取新的绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()

    await act(async () => { state.releaseRefresh() })
    await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)
  })

  it('keeps the completed-restore failure notice after a remount and only retries the read', async () => {
    const state = setupRestore({ deferPost: true, failRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    const queryClient = createTestQueryClient()

    function Harness() {
      const [mounted, setMounted] = useState(true)
      return (
        <>
          <button type="button" onClick={() => setMounted((value) => !value)}>离开/返回</button>
          {mounted
            ? <ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />
            : <div>已离开</div>}
        </>
      )
    }

    renderWithProviders(<Harness />, { queryClient })
    await startRestore(user)
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '离开/返回' }))
    await user.click(screen.getByRole('button', { name: '离开/返回' }))
    await act(async () => { state.releasePost() })

    expect(await screen.findByText('恢复已完成，但读取绑定失败')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '重试' }))
    await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)
  })


  it('does not carry a platform restore lock into another scope', async () => {
    const state = setupRestore({ deferPost: true, deferRefresh: true })
    const handler = (url: string, init?: RequestInit) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/block-bindings' && parsed.searchParams.get('scope') === 'project') {
        return jsonResponse({
          bindings: [binding({ scope: 'project', project_id: 43, block: 'review', skills: ['custom-skill'] })],
        })
      }
      return state.handler(url, init)
    }
    installFetch(handler)
    const user = userEvent.setup()

    function Harness() {
      const [scope, setScope] = useState<'platform' | 'project'>('platform')
      return (
        <>
          <button type="button" onClick={() => setScope((value) => (value === 'platform' ? 'project' : 'platform'))}>切换作用域</button>
          <ExtensionsManager
            scope={scope}
            projectId={scope === 'project' ? 43 : undefined}
            canManage
            tab="bindings"
            onTabChange={() => {}}
          />
        </>
      )
    }

    renderWithProviders(<Harness />)
    await startRestore(user)
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '切换作用域' }))
    expect(await screen.findByRole('button', { name: '保存绑定' })).toBeEnabled()
    expect(screen.getByRole('button', { name: 'review 使用 Skill custom-skill' })).toHaveAttribute('aria-pressed', 'true')

    await act(async () => { state.releasePost() })
    // The platform POST completing in its old instance must not lock project 43.
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeEnabled()

    await user.click(screen.getByRole('button', { name: '切换作用域' }))
    expect(await screen.findByText(/正在(应用新的绑定|读取新的绑定)/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()

    await act(async () => { state.releaseRefresh() })
    await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)
  })


  it('treats a 5xx restore response as uncertain and reads the server state before unlocking', async () => {
    const state = setupRestore({ uncertainPost: 'server-error', deferRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()

    function Harness() {
      const [tab, setTab] = useState('bindings')
      return <ExtensionsManager scope="platform" tab={tab} onTabChange={setTab} />
    }

    renderWithProviders(<Harness />)
    await startRestore(user)
    expect(state.restorePosts).toHaveLength(1)

    expect(await screen.findByText('恢复结果不确定，正在读取服务器绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '确认恢复' })).not.toBeInTheDocument()

    // The pending uncertainty survives a tab switch.
    await user.click(screen.getByRole('tab', { name: /MCP 服务器/ }))
    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(await screen.findByText('恢复结果不确定，正在读取服务器绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()

    await act(async () => { state.releaseRefresh() })
    const custom = await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(custom).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill another-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(state.writes[0]).toMatchObject({
      url: '/api/v1/block-bindings/review',
      method: 'PUT',
      body: { skills: ['custom-skill', 'another-skill'] },
    })
    expect(state.restorePosts).toHaveLength(1)
  })


  it.each(['timeout', 'abort', 'decode'] as const)('treats a %s restore failure as uncertain', async (mode) => {
    const state = setupRestore({ uncertainPost: mode, deferRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await startRestore(user)
    expect(await screen.findByText('恢复结果不确定，正在读取服务器绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()
    expect(state.restorePosts).toHaveLength(1)

    await act(async () => { state.releaseRefresh() })
    expect(await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)
  })

  it('treats a lost network response as uncertain and only retries the read after failure', async () => {
    const state = setupRestore({ uncertainPost: 'network', failRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await startRestore(user)
    expect(await screen.findByText('恢复结果不确定，读取绑定失败')).toBeInTheDocument()
    expect(screen.getByText(/请核对实际绑定/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '重试' }))
    const custom = await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(custom).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill another-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(state.writes[0]).toMatchObject({
      url: '/api/v1/block-bindings/review',
      method: 'PUT',
      body: { skills: ['custom-skill', 'another-skill'] },
    })
  })


  it('locks the stale preview after 409 and reads V2 before allowing edits or saves', async () => {
    const state = setupRestore({ conflict: true, deferRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()

    function Harness() {
      const [tab, setTab] = useState('bindings')
      return <ExtensionsManager scope="platform" tab={tab} onTabChange={setTab} />
    }

    renderWithProviders(<Harness />)
    await startRestore(user)
    expect(state.restorePosts).toHaveLength(1)
    const dialog = await screen.findByRole('dialog', { name: '恢复 review 的默认绑定' })
    expect(await within(dialog).findByText('预览已过期，绑定已变化。请重新预览后再确认。')).toBeInTheDocument()
    expect(within(dialog).getByText('正在读取最新绑定，完成后才能重新预览或确认。')).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '重新预览' })).toBeDisabled()
    expect(within(dialog).queryByRole('button', { name: '确认恢复' })).not.toBeInTheDocument()

    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(await screen.findByText('预览已过期，正在读取最新绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill v2-skill' })).not.toBeInTheDocument()
    expect(screen.queryByText(/恢复已完成/)).not.toBeInTheDocument()

    // The stale-preview lock also survives a tab switch.
    await user.click(screen.getByRole('tab', { name: /MCP 服务器/ }))
    await user.click(screen.getByRole('tab', { name: '块绑定' }))
    expect(await screen.findByText('预览已过期，正在读取最新绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()

    await act(async () => { state.releaseRefresh() })
    expect(await screen.findByRole('button', { name: 'review 使用 Skill v2-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: 'review 使用 Skill custom-skill' })).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill another-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(state.writes[0]).toMatchObject({
      url: '/api/v1/block-bindings/review',
      method: 'PUT',
      body: { skills: ['v2-skill', 'another-skill'] },
    })
    expect(state.restorePosts).toHaveLength(1)
  })

  it('keeps the stale-preview lock when the V2 read fails and only retries the read', async () => {
    const state = setupRestore({ conflict: true, deferRefresh: true, deferRefreshFailure: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await startRestore(user)
    const dialog = await screen.findByRole('dialog', { name: '恢复 review 的默认绑定' })
    await within(dialog).findByText('预览已过期，绑定已变化。请重新预览后再确认。')
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(await screen.findByText('预览已过期，正在读取最新绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()

    await act(async () => { state.releaseRefresh() })
    expect(await screen.findByText('预览已过期，读取绑定失败')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByText(/恢复已完成/)).not.toBeInTheDocument()
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByRole('button', { name: 'review 使用 Skill v2-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)
  })

  it('stays locked when the restore completes without an observer and the remount read is delayed', async () => {
    const state = setupRestore({ deferPost: true, deferRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    const queryClient = createTestQueryClient()

    function Harness() {
      const [mounted, setMounted] = useState(true)
      return (
        <>
          <button type="button" onClick={() => setMounted((value) => !value)}>离开/返回</button>
          {mounted
            ? <ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />
            : <div>已离开</div>}
        </>
      )
    }

    renderWithProviders(<Harness />, { queryClient })
    await startRestore(user)
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '离开/返回' }))
    await act(async () => { state.releasePost() })
    await user.click(screen.getByRole('button', { name: '离开/返回' }))

    expect(await screen.findByText(/正在(应用新的绑定|读取新的绑定)/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()

    await waitFor(() => expect(state.reads()).toBeGreaterThanOrEqual(2))
    await act(async () => { state.releaseRefresh() })
    await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)
  })


  it.each(['html', 'no-content', 'null'] as const)('treats a %s restore response as uncertain', async (shape) => {
    const state = setupRestore({ responseShape: shape, deferRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await startRestore(user)
    expect(await screen.findByText('恢复结果不确定，正在读取服务器绑定…')).toBeInTheDocument()
    expect(screen.queryByText('默认绑定已恢复。')).not.toBeInTheDocument()
    expect(screen.getByText(/请核对实际绑定/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()
    expect(state.restorePosts).toHaveLength(1)

    await act(async () => { state.releaseRefresh() })
    expect(await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)
  })


  it('keeps the matrix locked while the restore read is paused offline', async () => {
    const state = setupRestore({ uncertainPost: 'network' })
    installFetch(state.handler)
    const user = userEvent.setup()
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: 30_000 } },
    })
    queryClient.setQueryData(['block-bindings', 'platform', undefined], {
      pages: [{ bindings: [binding({ scope: 'platform', block: 'review', skills: ['builtin-skill', 'custom-skill'] })] }],
      pageParams: [undefined],
    })
    renderWithProviders(<ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />, { queryClient })

    await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    onlineManager.setOnline(false)
    await startRestore(user)

    expect(await screen.findByText('恢复结果不确定，正在读取服务器绑定…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()
    expect(queryClient.getQueryState(['block-bindings', 'platform', undefined])?.fetchStatus).toBe('paused')

    onlineManager.setOnline(true)
    expect(await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeEnabled()
    expect(state.restorePosts).toHaveLength(1)
  })

  it('does not let an old in-flight read unlock or overwrite the post-restore snapshot', async () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: 30_000 } },
    })
    const bindingsKey = ['block-bindings', 'platform', undefined] as const
    const v1 = [binding({ scope: 'platform', block: 'review', skills: ['builtin-skill', 'custom-skill'] })]
    const v2 = [binding({ scope: 'platform', block: 'review', skills: ['custom-skill'] })]
    let current = v1
    const restorePosts: Array<Record<string, unknown>> = []
    let releaseOldGet: (() => void) | undefined
    let releaseNewGet: (() => void) | undefined
    let releasePost: (() => void) | undefined
    let getCalls = 0

    const handler = (url: string, init?: RequestInit) => {
      const method = (init?.method || 'GET').toUpperCase()
      const body = init?.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined
      if (url.startsWith('/api/v1/block-bindings/restore-defaults')) {
        if (body?.dry_run) return jsonResponse(restorePreview)
        restorePosts.push(body || {})
        return new Promise<Response>((resolve) => {
          releasePost = () => {
            current = v2
            resolve(jsonResponse(restorePreview))
          }
        })
      }
      if (url.startsWith('/api/v1/block-bindings')) {
        if (method === 'GET') {
          const call = getCalls++
          if (call === 0) {
            return new Promise<Response>((resolve) => {
              releaseOldGet = () => resolve(jsonResponse({ bindings: v1 }))
            })
          }
          if (call === 1) {
            return new Promise<Response>((resolve) => {
              releaseNewGet = () => resolve(jsonResponse({ bindings: v2 }))
            })
          }
          return jsonResponse({ bindings: current })
        }
      }
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
      if (url.startsWith('/api/v1/skills')) {
        return jsonResponse({ skills: [
          skill({ id: 'k-builtin', name: 'builtin-skill', builtin: true }),
          skill({ id: 'k-custom', name: 'custom-skill' }),
        ] })
      }
      return jsonResponse({})
    }
    installFetch(handler)

    queryClient.setQueryData(bindingsKey, { pages: [{ bindings: v1 }], pageParams: [undefined] })
    const background = queryClient.fetchInfiniteQuery({
      queryKey: bindingsKey,
      queryFn: ({ pageParam }) => listBindings('platform', undefined, pageParam),
      initialPageParam: undefined as string | undefined,
      getNextPageParam: (lastPage) => lastPage?.next_cursor || undefined,
      pages: 1,
      staleTime: 0,
    })
    background.catch(() => {})

    const user = userEvent.setup()
    function Harness() {
      const [mounted, setMounted] = useState(true)
      return (
        <>
          <button type="button" onClick={() => setMounted((value) => !value)}>离开/返回</button>
          {mounted
            ? <ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />
            : <div>已离开</div>}
        </>
      )
    }

    renderWithProviders(<Harness />, { queryClient })
    await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    await waitFor(() => expect(releaseOldGet).toBeDefined())

    await startRestore(user)
    await user.click(screen.getByRole('button', { name: '离开/返回' }))
    await act(async () => { releasePost?.() })
    await act(async () => {
      releaseOldGet?.()
      await background.catch(() => {})
    })

    await user.click(screen.getByRole('button', { name: '离开/返回' }))
    // The canceled old read must not make V1 look fresh again.
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()

    await waitFor(() => expect(releaseNewGet).toBeDefined())
    await act(async () => { releaseNewGet?.() })
    expect(await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeEnabled()
    expect(restorePosts).toHaveLength(1)
  })


  it('refetches every loaded binding page after restore', async () => {
    const state = setupRestore({ paged: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '加载更多绑定' }))
    expect(await screen.findByRole('button', { name: 'reasoner 使用 Skill reasoner-skill' })).toHaveAttribute('aria-pressed', 'true')

    await startRestore(user)
    expect(await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: 'reasoner 使用 Skill reasoner-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(state.restorePosts).toHaveLength(1)
  })

  it('reports a completed restore when the refresh fails and retries only the read', async () => {
    const state = setupRestore({ failRefresh: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await startRestore(user)

    expect(await screen.findByText('恢复已完成，但读取绑定失败')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill custom-skill' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '确认恢复' })).not.toBeInTheDocument()
    expect(state.restorePosts).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '重试' }))
    const custom = await screen.findByRole('button', { name: 'review 使用 Skill custom-skill' })
    expect(custom).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill builtin-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.restorePosts).toHaveLength(1)
  })
})
