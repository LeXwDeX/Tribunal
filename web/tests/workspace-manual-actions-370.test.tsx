import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { WorkspacePanel } from '../src/features/project/WorkspacePanel'
import { contextDoc } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const current = contextDoc({ id: 'd1', content: '# Current', base_sha: '1111111111111111' })

function routes(workspace: Record<string, unknown>) {
  return (url: string) => {
    if (url.endsWith('/workspace')) return jsonResponse({ project_id: 42, ...workspace })
    if (url.includes('/context-docs/current')) return jsonResponse(current)
    if (url.includes('/context-docs')) return jsonResponse({ documents: [current] })
    return jsonResponse({})
  }
}

describe('workspace manual actions', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('exposes the manual toggle as a disclosure with a visible indicator', async () => {
    installFetch(routes({ state: 'ready' }))
    const user = userEvent.setup()
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    const manual = await screen.findByRole('button', { name: '手动操作' })
    expect(manual).toHaveAttribute('aria-expanded', 'false')
    expect(manual.querySelector('svg')).not.toBeNull()
    expect(screen.queryByRole('button', { name: '重建索引' })).not.toBeInTheDocument()

    await user.click(manual)
    expect(manual).toHaveAttribute('aria-expanded', 'true')
    const controls = manual.getAttribute('aria-controls')
    expect(controls).toBeTruthy()
    const region = document.getElementById(controls ?? '')
    expect(region).not.toBeNull()
    expect(region).toHaveTextContent('重建索引')
    expect(region).toHaveTextContent('重新生成上下文')

    await user.click(manual)
    expect(manual).toHaveAttribute('aria-expanded', 'false')
    expect(document.getElementById(controls ?? '')).toBeNull()
  })

  it('associates the fee hint only with regenerate', async () => {
    installFetch(routes({ state: 'ready' }))
    const user = userEvent.setup()
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    await screen.findByRole('button', { name: '手动操作' })
    await user.click(screen.getByRole('button', { name: '手动操作' }))

    expect(screen.getByRole('button', { name: '重新生成上下文' })).toHaveAccessibleDescription(/可能产生费用/)
    expect(screen.getByRole('button', { name: '重建索引' })).not.toHaveAccessibleDescription(/可能产生费用/)
  })

  it('keeps the ignore-policy description on reindex and off regenerate', async () => {
    installFetch(routes({
      state: 'ready',
      index_status: 'degraded',
      last_error: 'index: blocked by ignore policy',
    }))
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    const message = await screen.findByText(/已按 ignore 策略跳过索引/)
    const reindex = screen.getByRole('button', { name: '重建索引' })
    const regenerate = screen.getByRole('button', { name: '重新生成上下文' })
    expect(message).toHaveAttribute('role', 'status')
    expect(reindex).toHaveAccessibleDescription(/已按 ignore 策略跳过索引/)
    expect(regenerate).not.toHaveAccessibleDescription(/已按 ignore 策略跳过索引/)
    expect(regenerate).toHaveAccessibleDescription(/可能产生费用/)
  })

  it('associates the locked-document warning only with regenerate', async () => {
    installFetch((url) => {
      if (url.includes('/context-docs/current')) {
        const kind = new URL(url, 'http://x').searchParams.get('kind')
        return jsonResponse({ ...current, locked: kind === 'agents' })
      }
      return routes({ state: 'ready' })(url)
    })
    const user = userEvent.setup()
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    await screen.findByRole('button', { name: '手动操作' })
    await user.click(screen.getByRole('button', { name: '手动操作' }))
    expect(await screen.findByText(/执行时若 AGENTS 仍锁定/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新生成上下文' })).toHaveAccessibleDescription(/执行时若 AGENTS 仍锁定/)
    expect(screen.getByRole('button', { name: '重建索引' })).not.toHaveAccessibleDescription(/执行时若 AGENTS 仍锁定/)
  })
})