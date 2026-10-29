import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient } from '@tanstack/react-query'
import { WorkspacePanel } from '../src/features/project/WorkspacePanel'
import { contextDoc } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const current = contextDoc({ id: 'd2', content: '# Current', base_sha: '1111111111111111', created_at: '2026-10-20T00:00:00Z' })
const refreshed = { ...current, id: 'd3', content: '# New current', base_sha: 'bbbbbbbbbbbbbbbb' }

function ProjectSwitchHarness({ initial }: { initial: number }) {
  const [id, setId] = useState(initial)
  return (
    <>
      <button onClick={() => setId(id === 47 ? 48 : 47)}>切换项目</button>
      <WorkspacePanel key={id} projectId={id} canManage />
    </>
  )
}

describe('workspace auto refresh', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('polls while refreshing and reloads context docs when the workspace settles', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let workspaceCalls = 0
    let currentCalls = 0
    let settled = false
    installFetch((url) => {
      if (url.endsWith('/workspace')) {
        workspaceCalls += 1
        return jsonResponse({ state: settled ? 'ready' : 'refreshing', context_sha: settled ? 'bbbbbbbbbbbbbbbb' : 'aaaaaaaaaaaaaaaa' })
      }
      if (url.includes('/context-docs/current')) {
        currentCalls += 1
        return jsonResponse(settled ? refreshed : current)
      }
      if (url.includes('/context-docs')) return jsonResponse({ documents: [current] })
      return jsonResponse({})
    })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    expect(await screen.findByText('# Current')).toBeInTheDocument()
    expect(screen.getByText('刷新中')).toBeInTheDocument()
    expect(workspaceCalls).toBe(1)

    settled = true
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })

    await waitFor(() => expect(workspaceCalls).toBeGreaterThanOrEqual(2))
    await waitFor(() => expect(screen.getByText('# New current')).toBeInTheDocument())
    expect(currentCalls).toBeGreaterThanOrEqual(2)
    expect(screen.getByText('就绪')).toBeInTheDocument()
  })

  it('stops polling and reloads context docs when the workspace fails', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let workspaceCalls = 0
    let currentCalls = 0
    let settled = false
    installFetch((url) => {
      if (url.endsWith('/workspace')) {
        workspaceCalls += 1
        return jsonResponse({ state: settled ? 'error' : 'refreshing', last_error: settled ? '工作区操作失败' : undefined })
      }
      if (url.includes('/context-docs/current')) {
        currentCalls += 1
        return jsonResponse(current)
      }
      if (url.includes('/context-docs')) return jsonResponse({ documents: [current] })
      return jsonResponse({})
    })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    expect(await screen.findByText('刷新中')).toBeInTheDocument()

    settled = true
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    await waitFor(() => expect(screen.getByText('异常')).toBeInTheDocument())
    expect(currentCalls).toBeGreaterThanOrEqual(2)

    const callsAtFailure = workspaceCalls
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000)
    })
    expect(workspaceCalls).toBe(callsAtFailure)
  })

  it('keeps polling after a job is submitted even if the first refresh still returns ready', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let workspaceCalls = 0
    let refreshCalls = 0
    let historyCalls = 0
    let settled = false
    installFetch((url) => {
      if (url.endsWith('/workspace/refresh')) {
        refreshCalls += 1
        return jsonResponse({ job_id: 'j1' }, 202)
      }
      if (url.endsWith('/workspace')) {
        workspaceCalls += 1
        if (workspaceCalls <= 2) return jsonResponse({ state: 'ready', last_refresh_at: 't1', updated_at: 'u1' })
        if (workspaceCalls === 3) return jsonResponse({ state: 'refreshing', updated_at: 'u2' })
        settled = true
        return jsonResponse({ state: 'ready', last_refresh_at: 't2', updated_at: 'u3' })
      }
      if (url.includes('/context-docs/current')) return jsonResponse(settled ? refreshed : current)
      if (url.includes('/context-docs')) {
        historyCalls += 1
        return jsonResponse({ documents: [current] })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    expect(await screen.findByText('# Current')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '刷新工作区' }))
    await waitFor(() => expect(refreshCalls).toBe(1))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    await waitFor(() => expect(screen.getByText('刷新中')).toBeInTheDocument())

    const historyBeforeSettle = historyCalls
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    await waitFor(() => expect(screen.getByText('# New current')).toBeInTheDocument())
    await waitFor(() => expect(historyCalls).toBeGreaterThan(historyBeforeSettle))

    const callsAtReady = workspaceCalls
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000)
    })
    expect(workspaceCalls).toBe(callsAtReady)
  })

  it('reloads docs when the job finishes without an observed refreshing state', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let workspaceCalls = 0
    let refreshCalls = 0
    let refreshAt = 't1'
    let docVersion = 'old'
    installFetch((url) => {
      if (url.endsWith('/workspace/refresh')) {
        refreshCalls += 1
        return jsonResponse({ job_id: 'j2' }, 202)
      }
      if (url.endsWith('/workspace')) {
        workspaceCalls += 1
        return jsonResponse({ state: 'ready', last_refresh_at: refreshAt })
      }
      if (url.includes('/context-docs/current')) return jsonResponse(docVersion === 'old' ? current : refreshed)
      if (url.includes('/context-docs')) {
        return jsonResponse({ documents: [docVersion === 'old' ? current : refreshed] })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    renderWithProviders(<WorkspacePanel projectId={44} canManage />)

    expect(await screen.findByText('# Current')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '刷新工作区' }))
    await waitFor(() => expect(refreshCalls).toBe(1))

    // The worker finishes between polls: the state stays "ready", only the
    // refresh timestamp and the documents change.
    refreshAt = 't2'
    docVersion = 'new'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    await waitFor(() => expect(screen.getByText('# New current')).toBeInTheDocument())

    const callsAtReady = workspaceCalls
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000)
    })
    expect(workspaceCalls).toBe(callsAtReady)
  })

  it('resumes tracking after a remount while the queued job has not started', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let refreshCalls = 0
    let refreshAt = 't1'
    let docVersion = 'old'
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: 30_000 } },
    })
    installFetch((url) => {
      if (url.endsWith('/workspace/refresh')) {
        refreshCalls += 1
        return jsonResponse({ job_id: 'j3' }, 202)
      }
      if (url.endsWith('/workspace')) return jsonResponse({ state: 'ready', last_refresh_at: refreshAt })
      if (url.includes('/context-docs/current')) return jsonResponse(docVersion === 'old' ? current : refreshed)
      if (url.includes('/context-docs')) {
        return jsonResponse({ documents: [docVersion === 'old' ? current : refreshed] })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    const first = renderWithProviders(<WorkspacePanel projectId={45} canManage />, { queryClient })
    expect(await screen.findByText('# Current')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '刷新工作区' }))
    await waitFor(() => expect(refreshCalls).toBe(1))

    first.unmount()
    renderWithProviders(<WorkspacePanel projectId={45} canManage />, { queryClient })
    // Still queued: the cached workspace and the document are both old.
    expect(await screen.findByText('# Current')).toBeInTheDocument()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    refreshAt = 't2'
    docVersion = 'new'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    await waitFor(() => expect(screen.getByText('# New current')).toBeInTheDocument())
  })

  it('does not treat the previous error as the new job finishing', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let workspaceCalls = 0
    let refreshCalls = 0
    let phase: 'old' | 'running' | 'done' = 'old'
    installFetch((url) => {
      if (url.endsWith('/workspace/refresh')) {
        refreshCalls += 1
        return jsonResponse({ job_id: 'j4' }, 202)
      }
      if (url.endsWith('/workspace')) {
        workspaceCalls += 1
        if (workspaceCalls <= 2) {
          return jsonResponse({ state: 'error', last_refresh_at: 't1', updated_at: 'u1', last_error: '旧错误' })
        }
        if (workspaceCalls === 3) return jsonResponse({ state: 'refreshing', updated_at: 'u2' })
        return jsonResponse({ state: 'ready', last_refresh_at: 't2', updated_at: 'u3' })
      }
      if (url.includes('/context-docs/current')) return jsonResponse(phase === 'done' ? refreshed : current)
      if (url.includes('/context-docs')) {
        return jsonResponse({ documents: [phase === 'done' ? refreshed : current] })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    renderWithProviders(<WorkspacePanel projectId={46} canManage />)

    expect(await screen.findByText('异常')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '刷新工作区' }))
    await waitFor(() => expect(refreshCalls).toBe(1))

    // The POST returns while the cached state is still the previous error.
    phase = 'running'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    expect(workspaceCalls).toBeGreaterThanOrEqual(3)

    phase = 'done'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    await waitFor(() => expect(screen.getByText('# New current')).toBeInTheDocument())
  })

  it('resumes tracking after switching to another project and back', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let refreshCalls = 0
    let workspace48Calls = 0
    let refreshAt = 't1'
    let docVersion = 'old'
    installFetch((url) => {
      const path = new URL(url, 'http://x').pathname
      if (path === '/api/v1/projects/47/workspace/refresh') {
        refreshCalls += 1
        return jsonResponse({ job_id: 'j5' }, 202)
      }
      if (path === '/api/v1/projects/47/workspace') {
        return jsonResponse({ state: 'ready', last_refresh_at: refreshAt, updated_at: refreshAt })
      }
      if (path === '/api/v1/projects/47/context-docs/current') {
        return jsonResponse(docVersion === 'old' ? current : refreshed)
      }
      if (path === '/api/v1/projects/47/context-docs') {
        return jsonResponse({ documents: [docVersion === 'old' ? current : refreshed] })
      }
      if (path === '/api/v1/projects/48/workspace') {
        workspace48Calls += 1
        return jsonResponse({ state: 'ready' })
      }
      if (path === '/api/v1/projects/48/context-docs/current') return jsonResponse(current)
      if (path === '/api/v1/projects/48/context-docs') return jsonResponse({ documents: [current] })
      return jsonResponse({})
    })
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: 30_000 } },
    })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    renderWithProviders(<ProjectSwitchHarness initial={47} />, { queryClient })

    expect(await screen.findByText('# Current')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '刷新工作区' }))
    await waitFor(() => expect(refreshCalls).toBe(1))

    await user.click(screen.getByRole('button', { name: '切换项目' }))
    await waitFor(() => expect(workspace48Calls).toBeGreaterThan(0))
    await user.click(screen.getByRole('button', { name: '切换项目' }))
    await screen.findByText('# Current')

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    refreshAt = 't2'
    docVersion = 'new'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    await waitFor(() => expect(screen.getByText('# New current')).toBeInTheDocument())
  })

  it('stops tracking and reloads docs when the tracking window expires', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let workspaceCalls = 0
    let currentCalls = 0
    let historyCalls = 0
    installFetch((url) => {
      if (url.endsWith('/workspace/refresh')) return jsonResponse({ job_id: 'j6' }, 202)
      if (url.endsWith('/workspace')) {
        workspaceCalls += 1
        return jsonResponse({ state: 'ready', last_refresh_at: 't1', updated_at: 'u1' })
      }
      if (url.includes('/context-docs/current')) {
        currentCalls += 1
        return jsonResponse(current)
      }
      if (url.includes('/context-docs')) {
        historyCalls += 1
        return jsonResponse({ documents: [current] })
      }
      return jsonResponse({})
    })
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: 30_000 } },
    })
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    const first = renderWithProviders(<WorkspacePanel projectId={49} canManage />, { queryClient })
    expect(await screen.findByText('# Current')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '刷新工作区' }))
    await waitFor(() => expect(workspaceCalls).toBeGreaterThanOrEqual(2))

    const currentBeforeExpiry = currentCalls
    const historyBeforeExpiry = historyCalls
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5 * 60 * 1000 + 1000)
    })
    await waitFor(() => expect(currentCalls).toBeGreaterThan(currentBeforeExpiry))
    await waitFor(() => expect(historyCalls).toBeGreaterThan(historyBeforeExpiry))

    const callsAfterExpiry = workspaceCalls
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000)
    })
    expect(workspaceCalls).toBe(callsAfterExpiry)

    first.unmount()
    renderWithProviders(<WorkspacePanel projectId={49} canManage />, { queryClient })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000)
    })
    expect(workspaceCalls).toBe(callsAfterExpiry)
  })

  it('reloads the current doc when the first request is still in flight', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let workspaceCalls = 0
    let currentCalls = 0
    let resolveFirst: ((value: Response) => void) | undefined
    installFetch((url) => {
      if (url.endsWith('/workspace')) {
        workspaceCalls += 1
        return jsonResponse({ state: workspaceCalls === 1 ? 'refreshing' : 'ready' })
      }
      if (url.includes('/context-docs/current')) {
        currentCalls += 1
        if (currentCalls === 1) return new Promise<Response>((resolve) => { resolveFirst = resolve })
        return jsonResponse(refreshed)
      }
      if (url.includes('/context-docs')) return jsonResponse({ documents: [current] })
      return jsonResponse({})
    })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    await waitFor(() => expect(currentCalls).toBe(1))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2500)
    })
    await waitFor(() => expect(screen.getByText('# New current')).toBeInTheDocument())

    await act(async () => { resolveFirst?.(jsonResponse(current)) })
    expect(screen.getByText('# New current')).toBeInTheDocument()
  })

  it('reloads docs when the panel remounts after the workspace settled', async () => {
    let currentCalls = 0
    installFetch((url) => {
      if (url.endsWith('/workspace')) return jsonResponse({ state: 'refreshing' })
      if (url.includes('/context-docs/current')) {
        currentCalls += 1
        return jsonResponse(currentCalls === 1 ? current : refreshed)
      }
      if (url.includes('/context-docs')) return jsonResponse({ documents: [current] })
      return jsonResponse({})
    })
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: 30_000 } },
    })
    const first = renderWithProviders(<WorkspacePanel projectId={42} canManage />, { queryClient })
    expect(await screen.findByText('# Current')).toBeInTheDocument()

    first.unmount()
    queryClient.setQueryData(['project', 42, 'workspace'], { state: 'ready' })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />, { queryClient })

    await waitFor(() => expect(screen.getByText('# New current')).toBeInTheDocument())
  })

  it('does not poll when the workspace is already in a terminal state', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let workspaceCalls = 0
    installFetch((url) => {
      if (url.endsWith('/workspace')) {
        workspaceCalls += 1
        return jsonResponse({ state: 'ready' })
      }
      if (url.includes('/context-docs/current')) return jsonResponse(current)
      if (url.includes('/context-docs')) return jsonResponse({ documents: [current] })
      return jsonResponse({})
    })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    expect(await screen.findByText('# Current')).toBeInTheDocument()
    expect(screen.getByText('就绪')).toBeInTheDocument()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(7000)
    })
    expect(workspaceCalls).toBe(1)
  })
})
