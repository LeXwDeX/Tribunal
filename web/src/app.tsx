import { useCallback, useEffect, useState } from 'react'
import { QueryClient, useQuery } from '@tanstack/react-query'
import { Route, Routes } from 'react-router-dom'
import { fetchAuthStatus, fetchMe } from './api/api'
import { ApiError, LOGOUT_EVENT, UNAUTHORIZED_EVENT, setCsrfToken } from './api/client'
import { AuthScreen, BootLoading, authErrorMessage } from './components/AuthScreen'
import { AppShell } from './components/AppShell'
import { ToastProvider, useToast } from './components/ui'
import { OverviewPage } from './pages/OverviewPage'
import { ReviewsPage } from './pages/ReviewsPage'
import { ReviewDetailPage } from './pages/ReviewDetailPage'
import { ProjectsPage } from './pages/ProjectsPage'
import { ProjectDetailPage } from './pages/ProjectDetailPage'
import { ExtensionsPage } from './pages/ExtensionsPage'
import { ModelsPage } from './pages/ModelsPage'
import { StatisticsPage } from './pages/StatisticsPage'
import { SystemPage } from './pages/SystemPage'
import { NotFoundPage } from './pages/NotFoundPage'

export function createQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        refetchOnWindowFocus: false,
        staleTime: 30_000,
      },
    },
  })
}

function AuthErrorToast({ message, onShown }: { message: string; onShown: () => void }) {
  const { push } = useToast()
  useEffect(() => {
    // A zero-delay timer survives StrictMode's simulated unmount: the first
    // timer is cleared, the second pushes once after the remount.
    const timer = window.setTimeout(() => {
      push('error', message)
      onShown()
    }, 0)
    return () => window.clearTimeout(timer)
  }, [message, onShown, push])
  return null
}

export function App() {
  const meQuery = useQuery({ queryKey: ['me'], queryFn: fetchMe })
  const authStatusQuery = useQuery({ queryKey: ['auth-status'], queryFn: fetchAuthStatus, staleTime: Infinity })
  const [sessionEnded, setSessionEnded] = useState<'expired' | 'logout' | null>(null)
  const [authError, setAuthError] = useState(() => authErrorMessage(window.location.search))
  const clearAuthError = useCallback(() => setAuthError(undefined), [])

  useEffect(() => {
    const params = new URLSearchParams(window.location.search)
    if (!params.has('auth_error')) return
    params.delete('auth_error')
    const search = params.toString()
    window.history.replaceState(null, '', `${window.location.pathname}${search ? `?${search}` : ''}${window.location.hash}`)
  }, [])

  useEffect(() => {
    if (meQuery.data?.csrf_token) setCsrfToken(meQuery.data.csrf_token)
  }, [meQuery.data])

  useEffect(() => {
    const onUnauthorized = () => setSessionEnded('expired')
    const onLogout = () => setSessionEnded('logout')
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
    window.addEventListener(LOGOUT_EVENT, onLogout)
    return () => {
      window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
      window.removeEventListener(LOGOUT_EVENT, onLogout)
    }
  }, [])

  if (meQuery.isPending || authStatusQuery.isPending) return <BootLoading />

  const meError = meQuery.error
  const unauthorizedError = meError instanceof ApiError && meError.status === 401
  const sessionExpired = meError instanceof ApiError && meError.status === 401 && meError.code === 'session_expired'
  if (sessionEnded || unauthorizedError || !meQuery.data) {
    const notice = sessionEnded === 'logout'
      ? '已退出登录。'
      : sessionEnded === 'expired' || sessionExpired
        ? '会话已过期，请重新登录。'
        : undefined
    return (
      <ToastProvider>
        <AuthScreen
          notice={authError ?? notice}
          error={!unauthorizedError && !sessionEnded ? meQuery.error : undefined}
          onRetry={() => { void meQuery.refetch() }}
          oauthEnabled={authStatusQuery.data?.oauth_enabled}
        />
      </ToastProvider>
    )
  }

  return (
    <ToastProvider>
      {authError ? <AuthErrorToast message={authError} onShown={clearAuthError} /> : null}
      <Routes>
        <Route element={<AppShell me={meQuery.data} />}>
          <Route index element={<OverviewPage />} />
          <Route path="reviews" element={<ReviewsPage />} />
          <Route path="reviews/:id" element={<ReviewDetailPage />} />
          <Route path="projects" element={<ProjectsPage />} />
          <Route path="projects/:id/:tab?" element={<ProjectDetailPage />} />
          <Route path="extensions/:tab?" element={<ExtensionsPage />} />
          <Route path="models" element={<ModelsPage />} />
          <Route path="statistics" element={<StatisticsPage />} />
          <Route path="system" element={<SystemPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Routes>
    </ToastProvider>
  )
}
