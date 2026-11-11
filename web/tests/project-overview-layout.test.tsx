import { afterEach, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { project, review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

afterEach(() => vi.unstubAllGlobals())

it('places both project cards before the full-width recent reviews panel', async () => {
  installFetch((url) => {
    const path = new URL(url, 'http://localhost').pathname
    if (path === '/api/v1/projects/42') return jsonResponse(project())
    if (path === '/api/v1/stats') return jsonResponse({ runs: 9 })
    if (path === '/api/v1/reviews') return jsonResponse({ reviews: Array.from({ length: 20 }, (_, i) => review({ id: `r${i}`, mr_iid: 100 + i, mr_title: `Review ${i}` })) })
    return jsonResponse({})
  })
  renderWithProviders(<Routes><Route path="/projects/:id/:tab" element={<ProjectDetailPage />} /></Routes>, { route: '/projects/42/overview' })

  const basic = (await screen.findByRole('heading', { name: '基本信息' })).closest('section')!
  const workspace = screen.getByRole('heading', { name: '工作区摘要' }).closest('section')!
  const reviews = screen.getByRole('heading', { name: '最近评审' }).closest('section')!
  expect(basic.parentElement).toBe(workspace.parentElement)
  expect(basic.parentElement?.nextElementSibling).toBe(reviews)
  expect(within(reviews).getByRole('link', { name: '查看全部' })).toHaveAttribute('href', '/reviews?project_id=42')
  expect(await within(reviews).findAllByRole('row')).toHaveLength(21)

  const stats = screen.getByRole('region', { name: '本项目近 7 天' })
  expect(within(stats).getByText('运行次数')).toBeInTheDocument()
  expect(within(stats).getByText('耗时 平均 / P95')).toBeInTheDocument()
  expect(within(stats).queryByText('成本')).not.toBeInTheDocument()
})
