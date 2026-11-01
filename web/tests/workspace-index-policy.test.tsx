import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
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

describe('workspace index policy', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows an informational status when indexing is blocked by the ignore policy', async () => {
    const bodies: unknown[] = []
    const policyRoutes = routes({
      state: 'ready',
      index_status: 'degraded',
      last_error: 'index: blocked by ignore policy',
    })
    installFetch((url, init) => {
      if (url.endsWith('/workspace/refresh')) {
        bodies.push(JSON.parse(String(init?.body)))
        return jsonResponse({ job_id: 'job' })
      }
      return policyRoutes(url)
    })
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    const message = await screen.findByText(/已按 ignore 策略跳过索引/)
    const manual = screen.getByRole('button', { name: '手动操作' })
    expect(manual).toHaveAttribute('aria-expanded', 'true')
    expect(message).toHaveTextContent('这是安全配置，不是故障')
    expect(message).toHaveTextContent('修改项目 ignore 配置后重建索引')
    expect(message).toHaveAttribute('role', 'status')
    expect(screen.queryByText(/最近错误/)).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    const user = userEvent.setup()
    expect(screen.getByRole('button', { name: '重建索引' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '重建索引' })).toHaveAccessibleDescription(/已按 ignore 策略跳过索引/)
    expect(screen.getByRole('button', { name: '重新生成上下文' })).toBeEnabled()
    await user.click(screen.getByRole('button', { name: '重建索引' }))
    await waitFor(() => expect(bodies).toEqual([{ reindex: true }]))
    await user.click(manual)
    expect(manual).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText(/已按 ignore 策略跳过索引/)).not.toBeInTheDocument()
  })

  it('shows the blocked explanation to read-only users without actions', async () => {
    installFetch(routes({ state: 'ready', index_status: 'degraded', last_error: 'index: blocked by ignore policy' }))
    renderWithProviders(<WorkspacePanel projectId={42} />)
    expect(await screen.findByText(/已按 ignore 策略跳过索引/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '手动操作' })).toHaveAttribute('aria-expanded', 'true')
    expect(screen.queryByRole('button', { name: '重建索引' })).not.toBeInTheDocument()
  })

  it('keeps a normal workspace error as a warning alert', async () => {
    installFetch(routes({ state: 'error', last_error: '工作区操作失败' }))
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('最近错误：工作区操作失败')
    expect(screen.queryByText(/已按 ignore 策略跳过索引/)).not.toBeInTheDocument()
  })
})
