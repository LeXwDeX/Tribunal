import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
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
})
