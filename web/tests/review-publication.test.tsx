import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { ReviewTable } from '../src/components/ReviewTable'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { finding, review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function renderTable(reviews = [
  review({
    id: 'r-failed',
    mr_iid: 318,
    status: 'completed',
    verdict: 'INCOMPLETE',
    publication_state: 'failed',
    publication_error: 'merge request is not available',
  }),
  review({
    id: 'r-pending',
    mr_iid: 319,
    status: 'completed',
    verdict: 'PASS',
    publication_state: 'pending',
  }),
  review({ id: 'r-none', mr_iid: 320, status: 'failed', verdict: null }),
]) {
  return render(<MemoryRouter><ReviewTable reviews={reviews} projectNames={new Map()} /></MemoryRouter>)
}

function renderDetail(initial: ReturnType<typeof review>) {
  installFetch((url) => {
    if (url === `/api/v1/reviews/${initial.id}`) return jsonResponse(initial)
    if (url === `/api/v1/reviews/${initial.id}/graph`) return jsonResponse({ nodes: [], edges: [] })
    if (url.startsWith(`/api/v1/reviews/${initial.id}/findings`)) return jsonResponse({ findings: [finding()] })
    if (url === '/api/v1/projects/42') return jsonResponse({ id: 42, path_with_namespace: 'group/backend', profile: 'default', settings: {}, enabled: true, access: initial.access })
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    return jsonResponse({})
  })
  vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
  return renderWithProviders(
    <Routes>
      <Route path="/reviews/:id" element={<ReviewDetailPage />} />
    </Routes>,
    { route: `/reviews/${initial.id}` },
  )
}

function publicationPanel(): HTMLElement {
  const heading = screen.getByRole('heading', { name: '发布结果' })
  const panel = heading.closest('section')
  if (!panel) throw new Error('publication panel missing')
  return panel
}

class FakeEventSource {
  url: string
  constructor(url: string) {
    this.url = url
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

describe('review publication state', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('renders execution state and publication state in separate columns', () => {
    renderTable()
    expect(screen.getByRole('columnheader', { name: '执行状态' })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: '发布' })).toBeInTheDocument()
    expect(screen.getAllByText('已完成')).toHaveLength(2)
    expect(screen.getByText('发布失败')).toBeInTheDocument()
    expect(screen.getByText('merge request is not available')).toBeInTheDocument()
    expect(screen.getByText('待发布')).toBeInTheDocument()
    expect(screen.getByText('不适用')).toBeInTheDocument()
    expect(screen.getByText(/INCOMPLETE/)).toBeInTheDocument()
    expect(screen.getByText(/未完成/)).toBeInTheDocument()
  })

  it('shows publication state and the sanitized reason in review detail', async () => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    installFetch((url) => {
      if (url === '/api/v1/reviews/wf-pub') {
        return jsonResponse(review({
          id: 'wf-pub',
          status: 'completed',
          verdict: 'INCOMPLETE',
          publication_state: 'failed',
          publication_error: 'merge request is not available',
        }))
      }
      if (url === '/api/v1/reviews/wf-pub/graph') return jsonResponse({ nodes: [], edges: [] })
      if (url.startsWith('/api/v1/reviews/wf-pub/findings')) return jsonResponse({ findings: [finding()] })
      if (url === '/api/v1/projects/42') return jsonResponse({ error: { message: 'project unavailable' } }, 500)
      return jsonResponse({})
    })
    renderWithProviders(
      <Routes>
        <Route path="/reviews/:id" element={<ReviewDetailPage />} />
      </Routes>,
      { route: '/reviews/wf-pub' },
    )

    const publicationPanel = await screen.findByRole('heading', { name: '发布结果' })
    expect(publicationPanel).toBeInTheDocument()
    const panel = publicationPanel.closest('section')
    if (!panel) throw new Error('publication panel missing')
    expect(within(panel).getByText('发布状态')).toBeInTheDocument()
    expect(within(panel).getByText('发布失败')).toBeInTheDocument()
    expect(within(panel).getByText('merge request is not available')).toBeInTheDocument()
    expect(within(panel).getByText(/不能视为 PASS/)).toBeInTheDocument()

    expect(screen.getByText(/INCOMPLETE/)).toBeInTheDocument()
    expect(screen.getByText('已完成')).toBeInTheDocument()
  })

  it('explains a failed commit status as the blocking verdict when publication succeeded', async () => {
    renderDetail(review({
      id: 'wf-blocking',
      status: 'completed',
      verdict: 'BLOCKING',
      publication_state: 'published',
      publications: { commit_status: 'failed' },
    }))
    await screen.findByRole('heading', { name: '发布结果' })
    const panel = publicationPanel()
    expect(within(panel).getByText(/评审结论为 BLOCKING/)).toBeInTheDocument()
    expect(within(panel).getByText(/发布本身已成功/)).toBeInTheDocument()
    expect(within(panel).queryByText(/发布失败/)).not.toBeInTheDocument()
  })

  it('shows the stale skip reason and does not promise that republishing can repair it', async () => {
    renderDetail(review({
      id: 'wf-stale',
      status: 'completed',
      publication_state: 'skipped',
      publication_error: 'stale: merge request head changed',
    }))
    await screen.findByRole('heading', { name: '发布结果' })
    const panel = publicationPanel()
    expect(within(panel).getByText('跳过原因')).toBeInTheDocument()
    expect(within(panel).getAllByText(/stale: merge request head changed/).length).toBeGreaterThan(0)
    expect(within(panel).getByText(/过期结果/)).toBeInTheDocument()
    expect(within(panel).getByText(/不会自动修复/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重新发布' })).not.toBeInTheDocument()
  })

  it('offers a manual retry for a pending publication that already recorded a retryable failure', async () => {
    renderDetail(review({
      id: 'wf-retry-pending',
      status: 'completed',
      publication_state: 'pending',
      publication_error: 'retry pending',
    }))
    expect(await screen.findByRole('button', { name: '重新发布' })).toBeInTheDocument()
  })

  it('shows a non-stale skip reason without claiming there was nothing to publish', async () => {
    const withReason = renderDetail(review({
      id: 'wf-skipped-reason',
      status: 'completed',
      publication_state: 'skipped',
      publication_error: 'workflow superseded',
    }))
    await screen.findByRole('heading', { name: '发布结果' })
    const panel = publicationPanel()
    expect(within(panel).getAllByText(/workflow superseded/).length).toBeGreaterThan(0)
    expect(within(panel).queryByText(/没有需要发布的内容/)).not.toBeInTheDocument()
    withReason.unmount()

    renderDetail(review({
      id: 'wf-skipped-generic',
      status: 'completed',
      publication_state: 'skipped',
    }))
    await screen.findByRole('heading', { name: '发布结果' })
    expect(within(publicationPanel()).getByText('本次发布已跳过。')).toBeInTheDocument()
    expect(within(publicationPanel()).queryByText(/没有需要发布的内容/)).not.toBeInTheDocument()
  })

  it('offers republish for a failed workflow with manage access but not while publication is pending', async () => {
    const failed = renderDetail(review({
      id: 'wf-failed-publish',
      status: 'failed',
      publication_state: 'failed',
      publication_error: 'merge request is not available',
    }))
    expect(await screen.findByRole('button', { name: '重新发布' })).toBeInTheDocument()
    failed.unmount()

    renderDetail(review({
      id: 'wf-pending-publish',
      status: 'completed',
      publication_state: 'pending',
    }))
    await screen.findByRole('heading', { name: '发布结果' })
    expect(screen.queryByRole('button', { name: '重新发布' })).not.toBeInTheDocument()
  })
})
