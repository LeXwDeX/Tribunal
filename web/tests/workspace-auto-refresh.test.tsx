import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import { WorkspacePanel } from '../src/features/project/WorkspacePanel'
import { contextDoc } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const current = contextDoc({ id: 'd2', content: '# Current', base_sha: '1111111111111111', created_at: '2026-10-20T00:00:00Z' })
const refreshed = { ...current, id: 'd3', content: '# New current', base_sha: 'bbbbbbbbbbbbbbbb' }

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
