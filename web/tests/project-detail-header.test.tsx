import { afterEach, describe, expect, it, vi } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { screen, within } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('project detail header layout', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('keeps the status badge with the title instead of the action buttons', async () => {
    installFetch((url) => {
      const path = new URL(url, 'http://localhost').pathname
      if (path === '/api/v1/projects/42') return jsonResponse(project({ id: 42 }))
      if (path === '/api/v1/stats') return jsonResponse({ runs: 0, blocking: 0 })
      if (path === '/api/v1/reviews') return jsonResponse({ reviews: [] })
      return jsonResponse({})
    })
    renderWithProviders(
      <Routes>
        <Route path="/projects/:id/:tab" element={<ProjectDetailPage />} />
      </Routes>,
      { route: '/projects/42/overview' },
    )

    const heading = await screen.findByRole('heading', { level: 1, name: /group\/backend/ })
    const titleGroup = heading.parentElement!
    const header = heading.closest('header')!
    expect(titleGroup).toHaveTextContent('已启用')
    expect(titleGroup).not.toHaveTextContent('在 GitLab 中打开')
    expect(within(header).getByRole('link', { name: '在 GitLab 中打开' })).toHaveAttribute(
      'href', 'https://gitlab.example.com/group/backend',
    )
  })

  it('keeps horizontal tab scrolling without a vertical scrollbar', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')
    const tabsRule = css.match(/\.tabs\s*\{[^}]*\}/)?.[0] ?? ''
    expect(tabsRule).toContain('overflow-x: auto')
    expect(tabsRule).toContain('overflow-y: hidden')
  })
})
