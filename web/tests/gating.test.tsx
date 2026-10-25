import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

describe('system page permissions', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('lets a viewer create and revoke personal tokens', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/api-tokens')) return jsonResponse({ tokens: [] })
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />, { me: viewerMe })
    await waitFor(() => expect(screen.getByRole('button', { name: '创建 token' })).toBeInTheDocument())
    expect(screen.queryByText(/只读账号可查看 token 列表/)).not.toBeInTheDocument()
  })

  it('hides platform settings and access administration from a viewer', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/api-tokens')) return jsonResponse({ tokens: [] })
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />, { me: viewerMe })
    await waitFor(() => expect(screen.getByText('我的 API token')).toBeInTheDocument())
    expect(screen.queryByRole('tab', { name: '平台配置（只读）' })).not.toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: '用户与账号' })).not.toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: '项目授权' })).not.toBeInTheDocument()
  })

  it('shows platform settings and access administration to an admin', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/api-tokens')) return jsonResponse({ tokens: [] })
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />)
    await waitFor(() => expect(screen.getByRole('tab', { name: '平台配置（只读）' })).toBeInTheDocument())
    expect(screen.getByRole('tab', { name: '用户与账号' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: '项目授权' })).toBeInTheDocument()
  })
})
