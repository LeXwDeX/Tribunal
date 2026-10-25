import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { project, review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('project detail review pagination', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('retries a failed review page with its cursor and keeps the first page', async () => {
    const calls: Array<string | null> = []
    let nextPageAttempts = 0
    installFetch((url) => {
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname === '/api/v1/projects/42') return jsonResponse(project({ id: 42 }))
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({ runs: 9, blocking: 1 })
      if (parsed.pathname === '/api/v1/reviews') {
        const cursor = parsed.searchParams.get('cursor')
        calls.push(cursor)
        if (cursor === 'p2') {
          nextPageAttempts += 1
          if (nextPageAttempts === 1) return jsonResponse({ error: { message: 'next page failed' } }, 500)
          return jsonResponse({ reviews: [review({ id: 'review-2', mr_iid: 319, mr_title: 'fix: 第二页' })] })
        }
        return jsonResponse({ reviews: [review()], next_cursor: 'p2' })
      }
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(
      <Routes>
        <Route path="/projects/:id/:tab" element={<ProjectDetailPage />} />
      </Routes>,
      { route: '/projects/42/overview' },
    )

    expect(await screen.findByText('运行次数')).toBeInTheDocument()
    expect(screen.getByText(/包含评审与 onboarding/)).toBeInTheDocument()
    expect(screen.queryByText('评审次数')).not.toBeInTheDocument()
    expect(await screen.findByRole('link', { name: /!318/ })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '加载更多评审' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('加载失败')
    expect(screen.getByRole('link', { name: /!318/ })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '重试' }))
    await screen.findByRole('link', { name: /!319/ }, { timeout: 1500 })
    expect(screen.getByRole('link', { name: /!318/ })).toBeInTheDocument()
    expect(calls).toEqual([null, 'p2', 'p2'])
  })
})
