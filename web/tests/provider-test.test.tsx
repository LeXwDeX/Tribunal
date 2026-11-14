import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('model draft test', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('does not treat a response without ok:boolean as success', async () => {
    const user = userEvent.setup()
    installFetch((url) => {
      if (url === '/api/v1/models:test') return jsonResponse({ message: '模型可用' })
      if (url.startsWith('/api/v1/models')) {
        return jsonResponse({
          models: [{
            id: 'user:1', display_name: 'claude', source: 'user', read_only: false,
            protocol: 'anthropic', base_url: 'https://api.anthropic.com', model: 'test',
            params: { max_tokens: 4096 }, has_api_key: true, available: true, revision: 1,
            referenced_by: [], vote_identity: 'api.anthropic.com/test',
          }],
        })
      }
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    await user.click(await screen.findByRole('button', { name: '保存前连通性测试' }))
    expect(await screen.findByText(/测试失败/)).toBeInTheDocument()
    expect(screen.queryByText(/^测试成功/)).not.toBeInTheDocument()
  })
})
