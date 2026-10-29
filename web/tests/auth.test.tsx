import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { App } from '../src/app'
import { setCsrfToken } from '../src/api/client'
import { adminMe, createTestQueryClient, installFetch, jsonResponse } from './test-utils'

describe('authentication boundary', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    setCsrfToken(null)
    window.history.replaceState(null, '', '/')
  })

  it('shows a GitLab login action on 401 and never invents a user', async () => {
    installFetch(() => jsonResponse({ error: { code: 'unauthenticated', message: 'login required' } }, 401))
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <MemoryRouter initialEntries={['/']}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    )
    const login = await screen.findByRole('link', { name: '通过 GitLab 登录' })
    expect(login).toHaveAttribute('href', '/auth/login')
    expect(screen.queryByText('会话已过期，请重新登录。')).not.toBeInTheDocument()
    expect(screen.queryByText('Alice')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '退出登录' })).not.toBeInTheDocument()
  })

  it('shows the expiry notice only when the server reports an expired session', async () => {
    installFetch(() => jsonResponse({ error: { code: 'session_expired', message: 'session expired' } }, 401))
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <MemoryRouter initialEntries={['/']}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    )
    expect(await screen.findByRole('link', { name: '通过 GitLab 登录' })).toBeInTheDocument()
    expect(screen.getAllByText('会话已过期，请重新登录。')).toHaveLength(1)
  })

  it('hides the login action when the server reports OAuth disabled', async () => {
    installFetch((url) => {
      if (url.endsWith('/auth/status')) return jsonResponse({ oauth_enabled: false })
      return jsonResponse({ error: { code: 'unauthenticated', message: 'login required' } }, 401)
    })
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <MemoryRouter initialEntries={['/']}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    )
    expect(await screen.findByText(/平台尚未启用 GitLab OAuth 登录/)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '通过 GitLab 登录' })).not.toBeInTheDocument()
  })

  it('shows a readable message for a failed browser login', async () => {
    window.history.replaceState(null, '', '/?auth_error=oauth_state')
    installFetch((url) => {
      if (url.endsWith('/auth/status')) return jsonResponse({ oauth_enabled: true })
      return jsonResponse({ error: { code: 'unauthenticated', message: 'login required' } }, 401)
    })
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <MemoryRouter initialEntries={['/']}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    )
    expect(await screen.findByText('登录状态已失效，请重试。')).toBeInTheDocument()
    expect(window.location.search).toBe('')
  })

  it('falls back for an unknown or inherited auth_error value', async () => {
    window.history.replaceState(null, '', '/?auth_error=__proto__')
    installFetch((url) => {
      if (url.endsWith('/auth/status')) return jsonResponse({ oauth_enabled: true })
      return jsonResponse({ error: { code: 'unauthenticated', message: 'login required' } }, 401)
    })
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <MemoryRouter initialEntries={['/']}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    )
    expect(await screen.findByText('登录未完成，请重试。')).toBeInTheDocument()
  })

  it('shows the OAuth failure after returning to an existing session', async () => {
    window.history.replaceState(null, '', '/?auth_error=oauth_denied')
    installFetch((url) => {
      if (url.endsWith('/api/v1/me')) return jsonResponse(adminMe)
      if (url.endsWith('/auth/status')) return jsonResponse({ oauth_enabled: true })
      return jsonResponse({ reviews: [] })
    })
    render(
      <StrictMode>
        <QueryClientProvider client={createTestQueryClient()}>
          <MemoryRouter initialEntries={['/']}>
            <App />
          </MemoryRouter>
        </QueryClientProvider>
      </StrictMode>,
    )
    expect(await screen.findAllByText('GitLab 登录被取消或拒绝。')).toHaveLength(1)
    expect(window.location.search).toBe('')
  })
})
