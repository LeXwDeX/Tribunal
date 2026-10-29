import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
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
    installFetch(routes({
      state: 'ready',
      index_status: 'degraded',
      last_error: 'index: blocked by ignore policy',
    }))
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    const message = await screen.findByText(/已按 ignore 策略跳过索引/)
    expect(message).toHaveTextContent('这是安全配置，不是故障')
    expect(message).toHaveTextContent('修改项目 ignore 配置后重建索引')
    expect(message).toHaveAttribute('role', 'status')
    expect(screen.queryByText(/最近错误/)).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('keeps a normal workspace error as a warning alert', async () => {
    installFetch(routes({ state: 'error', last_error: '工作区操作失败' }))
    renderWithProviders(<WorkspacePanel projectId={42} canManage />)

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('最近错误：工作区操作失败')
    expect(screen.queryByText(/已按 ignore 策略跳过索引/)).not.toBeInTheDocument()
  })
})
