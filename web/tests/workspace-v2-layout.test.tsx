import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { WorkspacePanel } from '../src/features/project/WorkspacePanel'
import { contextDoc } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const current = contextDoc({ id: 'd2', content: '# Current', base_sha: '1111111111111111', created_at: '2026-10-20T00:00:00Z' })
const older = contextDoc({ id: 'd1', content: '# Older', base_sha: '2222222222222222', created_at: '2026-10-10T00:00:00Z' })

function routes(kinds: string[]) {
  return (url: string) => {
    if (url.endsWith('/workspace')) {
      return jsonResponse({
        project_id: 42, state: 'ready', base_sha: 'abcdef0123456789', index_sha: '9999999999999999', context_sha: '8888888888888888',
        index_status: 'stale', last_refresh_at: '2026-10-25T00:00:00Z',
      })
    }
    if (url.includes('/context-docs/current')) {
      kinds.push(new URL(url, 'http://x').searchParams.get('kind') || '')
      return jsonResponse(current)
    }
    if (url.includes('/context-docs')) return jsonResponse({ documents: [current, older] })
    return jsonResponse({})
  }
}

describe('workspace v2 layout', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('summarizes the workspace in four status tiles and keeps the extra hashes', async () => {
    installFetch(routes([]))
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    const tiles = await screen.findByRole('group', { name: '工作区状态' })
    expect(await within(tiles).findByText('abcdef0')).toBeInTheDocument()
    expect(within(tiles).getByText('stale')).toBeInTheDocument()
    expect(within(tiles).getByText('工作区')).toBeInTheDocument()
    expect(within(tiles).getByText('最近刷新')).toBeInTheDocument()
    expect(screen.getByText('9999999')).toBeInTheDocument()
    expect(screen.getByText('8888888')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新生成上下文' })).toBeInTheDocument()
  })

  it('switches document kind and opens an older version read-only from the version list', async () => {
    const kinds: string[] = []
    installFetch(routes(kinds))
    const user = userEvent.setup()
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    expect(await screen.findByText('# Current')).toBeInTheDocument()
    const versions = screen.getByRole('navigation', { name: '上下文版本历史' })
    await user.click(within(versions).getByRole('button', { name: /2222222/ }))
    expect(await screen.findByText('# Older')).toBeInTheDocument()
    expect(screen.getByText('历史版本（只读）')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '编辑' })).not.toBeInTheDocument()

    const kindGroup = screen.getByRole('group', { name: '上下文文档类型' })
    await user.click(within(kindGroup).getByRole('button', { name: 'CLAUDE' }))
    expect(within(kindGroup).getByRole('button', { name: 'CLAUDE' })).toHaveAttribute('aria-pressed', 'true')
    expect(kinds).toContain('claude')
  })

  it('uses the refreshed current document after selecting it, while keeping historical records distinct', async () => {
    const user = userEvent.setup()
    let serverDoc = { ...current, locked: false }
    const historical = { ...older, locked: true }
    const lockBodies: unknown[] = []
    installFetch((url, init) => {
      if (url.includes('/context-docs/current/lock')) {
        const body = JSON.parse(String(init?.body)) as { locked: boolean }
        lockBodies.push(body)
        serverDoc = { ...serverDoc, locked: body.locked, content: body.locked ? '# Refreshed current' : '# Current unlocked' }
        return new Response(null, { status: 204 })
      }
      if (url.includes('/context-docs/current')) return jsonResponse(serverDoc)
      // The list can contain an older copy of the current document.
      if (url.includes('/context-docs')) return jsonResponse({ documents: [current, historical] })
      if (url.endsWith('/workspace')) return jsonResponse({ state: 'ready' })
      return jsonResponse({})
    })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    await screen.findByText('# Current')
    const versions = screen.getByRole('navigation', { name: '上下文版本历史' })
    await user.click(within(versions).getByRole('button', { name: /1111111/ }))
    const lock = screen.getByRole('switch', { name: '锁定当前上下文文档' })
    expect(lock).toHaveAttribute('aria-checked', 'false')
    await user.click(lock)
    await waitFor(() => expect(lock).toHaveAttribute('aria-checked', 'true'))
    expect(screen.getByText('已锁定')).toBeInTheDocument()
    expect(screen.getByText('# Refreshed current')).toBeInTheDocument()
    await waitFor(() => expect(lock).toBeEnabled())
    await user.click(lock)
    await waitFor(() => expect(lock).toHaveAttribute('aria-checked', 'false'))
    expect(lockBodies).toEqual([{ locked: true }, { locked: false }])
    expect(screen.getByText('未锁定')).toBeInTheDocument()
    expect(screen.getByText('# Current unlocked')).toBeInTheDocument()

    await user.click(within(versions).getByRole('button', { name: /2222222/ }))
    expect(screen.getByText('# Older')).toBeInTheDocument()
    expect(screen.getByText('历史版本（只读）')).toBeInTheDocument()
    expect(screen.getByText('已锁定')).toBeInTheDocument()
    expect(screen.queryByRole('switch', { name: '锁定当前上下文文档' })).not.toBeInTheDocument()
    await user.click(within(versions).getByRole('button', { name: /1111111/ }))
    expect(screen.getByText('# Current unlocked')).toBeInTheDocument()
    expect(screen.getByText('未锁定')).toBeInTheDocument()
  })

  it.each([false, true])('inherits the server lock when saving a stale document (selected=%s)', async (selectCurrent) => {
    const user = userEvent.setup()
    let serverDoc = current
    let savedBody: Record<string, unknown> | undefined
    const lockBodies: unknown[] = []
    installFetch((url, init) => {
      if (url.includes('/context-docs/current/lock')) {
        const body = JSON.parse(String(init?.body)) as { locked: boolean }
        lockBodies.push(body)
        serverDoc = { ...serverDoc, locked: body.locked }
        return new Response(null, { status: 204 })
      }
      if (url.includes('/context-docs/current') && init?.method === 'PUT') {
        savedBody = JSON.parse(String(init.body)) as Record<string, unknown>
        serverDoc = contextDoc({
          ...serverDoc,
          id: 'd3',
          content: String(savedBody.content),
          locked: 'locked' in savedBody ? Boolean(savedBody.locked) : serverDoc.locked,
        })
        return jsonResponse(serverDoc)
      }
      if (url.includes('/context-docs/current')) return jsonResponse(serverDoc)
      if (url.includes('/context-docs')) return jsonResponse({ documents: [serverDoc, older] })
      if (url.endsWith('/workspace')) return jsonResponse({ state: 'ready' })
      return jsonResponse({})
    })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    await screen.findByText('# Current')
    if (selectCurrent) {
      await user.click(within(screen.getByRole('navigation', { name: '上下文版本历史' })).getByRole('button', { name: /1111111/ }))
    }
    await user.click(screen.getByRole('button', { name: '编辑' }))
    await user.clear(screen.getByRole('textbox', { name: '编辑上下文文档' }))
    await user.type(screen.getByRole('textbox', { name: '编辑上下文文档' }), 'New content')

    // Another administrator locks the document after this page loaded.
    serverDoc = { ...serverDoc, locked: true }
    await user.click(screen.getByRole('button', { name: '保存为新版本' }))

    await waitFor(() => expect(savedBody).toEqual({ kind: 'agents', content: 'New content' }))
    expect(serverDoc.locked).toBe(true)
    await waitFor(() => expect(screen.getByRole('switch', { name: '锁定当前上下文文档' })).toHaveAttribute('aria-checked', 'true'))

    await user.click(screen.getByRole('switch', { name: '锁定当前上下文文档' }))
    await waitFor(() => expect(lockBodies).toEqual([{ locked: false }]))
    await waitFor(() => expect(screen.getByRole('switch', { name: '锁定当前上下文文档' })).toHaveAttribute('aria-checked', 'false'))
    await user.click(screen.getByRole('switch', { name: '锁定当前上下文文档' }))
    await waitFor(() => expect(lockBodies).toEqual([{ locked: false }, { locked: true }]))
  })
})
