import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { AppShell } from '../src/components/AppShell'
import { ErrorBoundary } from '../src/components/ErrorBoundary'
import { ToastProvider } from '../src/components/ui'
import { adminMe, createTestQueryClient, installFetch, jsonResponse } from './test-utils'
import { App } from '../src/app'

// The injectable crashing route: the page-level boundary in AppShell guards
// the outlet, so the shell itself must throw to exercise the app-level
// boundary that App owns. The mock delegates every other route unchanged.
vi.mock('../src/components/AppShell', async () => {
  const actual = await vi.importActual<typeof import('../src/components/AppShell')>('../src/components/AppShell')
  const router = await vi.importActual<typeof import('react-router-dom')>('react-router-dom')
  return {
    AppShell: (props: Parameters<typeof actual.AppShell>[0]) => {
      const location = router.useLocation()
      if (location.pathname === '/models') throw new Error('render failed at /srv/internal/path')
      return <actual.AppShell {...props} />
    },
  }
})
vi.mock('../src/pages/OverviewPage', () => ({
  OverviewPage: () => <p>总览内容</p>,
}))

function BrokenPage(): ReactNode {
  throw new Error('render failed at /srv/internal/path')
}

function renderShell(route = '/broken') {
  installFetch(() => jsonResponse({ reviews: [] }))
  return render(
    <QueryClientProvider client={createTestQueryClient()}>
      <MemoryRouter initialEntries={[route]}>
        <ToastProvider>
          <Routes>
            <Route element={<AppShell me={adminMe} />}>
              <Route index element={<p>总览内容</p>} />
              <Route path="broken" element={<BrokenPage />} />
            </Route>
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('console error boundary', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('replaces a crashing page with an error panel while navigation stays usable, and recovers on route change', async () => {
    const user = userEvent.setup()
    // React reports caught render errors through console.error; keep test output clean.
    vi.spyOn(console, 'error').mockImplementation(() => {})
    renderShell()

    expect(screen.getByRole('heading', { name: '页面遇到错误' })).toBeInTheDocument()
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('重新加载当前页面')
    // The raw error text (which may carry an internal path) never reaches the UI.
    expect(alert).not.toHaveTextContent('/srv/internal/path')
    expect(screen.getByRole('navigation', { name: '主导航' })).toBeInTheDocument()

    await user.click(screen.getByRole('link', { name: '总览' }))

    expect(await screen.findByText('总览内容')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '页面遇到错误' })).not.toBeInTheDocument()
  })

  it('offers a reload action and a home link in the app-level fallback', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <MemoryRouter initialEntries={['/system']}>
          <ToastProvider>
            <ErrorBoundary scope="app">
              <BrokenPage />
            </ErrorBoundary>
          </ToastProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    )

    expect(screen.getByRole('heading', { name: '控制台遇到错误' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '返回总览' })).toHaveAttribute('href', '/')
    expect(screen.getByRole('button', { name: '重新加载' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).not.toHaveTextContent('/srv/internal/path')
  })

  it('installs the app-level fallback in the real App and resets it on navigation', async () => {
    const user = userEvent.setup()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    installFetch((url) => {
      if (url.endsWith('/auth/status')) return jsonResponse({ oauth_enabled: true })
      if (url.endsWith('/api/v1/me')) return jsonResponse(adminMe)
      return jsonResponse({})
    })
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <MemoryRouter initialEntries={['/models']}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    )

    expect(await screen.findByRole('heading', { name: '控制台遇到错误' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).not.toHaveTextContent('/srv/internal/path')

    await user.click(screen.getByRole('link', { name: '返回总览' }))

    expect(await screen.findByText('总览内容')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '控制台遇到错误' })).not.toBeInTheDocument()
  })
})