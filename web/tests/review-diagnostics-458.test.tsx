import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import type { Finding, GraphNode, ReviewDetail } from '../src/api/types'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { NodeDrawer } from '../src/features/review/NodeDrawer'
import { finding, review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

class FakeEventSource {
  url: string
  constructor(url: string) {
    this.url = url
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

function renderDetail(initial: ReviewDetail, findings: Finding[] = []) {
  installFetch((url) => {
    if (url === `/api/v1/reviews/${initial.id}`) return jsonResponse(initial)
    if (url === `/api/v1/reviews/${initial.id}/graph`) return jsonResponse({ nodes: [], edges: [] })
    if (url.startsWith(`/api/v1/reviews/${initial.id}/findings`)) return jsonResponse({ findings })
    if (url === '/api/v1/projects/42') {
      return jsonResponse({ id: 42, path_with_namespace: 'group/backend', profile: 'default', settings: {}, enabled: true, access: initial.access })
    }
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

const incomplete: ReviewDetail = review({
  id: 'wf-incomplete-458',
  status: 'completed',
  verdict: 'INCOMPLETE',
  summary: '评审未完成：上游评审存在未解决的证据不足，不能给出 PASS。',
  warnings: ['验证通道 BLOCKED：没有流水线'],
  unresolved: [
    { node: 'coverage', kind: 'evidence_gap', message: '缺少流水线日志' },
    { node: 'lanes.review', kind: 'review_blocking', message: '档位给出阻断但没有 finding' },
  ],
  findings_summary: { published: 0, marked: 0, dropped: 0, by_severity: { P0: 0, P1: 0, P2: 0, info: 0 } },
})

describe('review-level diagnostics', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('explains an INCOMPLETE review with its summary, warnings and unresolved signals', async () => {
    renderDetail(incomplete)
    expect(await screen.findByText('未完成原因')).toBeInTheDocument()
    expect(screen.getByText('评审未完成：上游评审存在未解决的证据不足，不能给出 PASS。')).toBeInTheDocument()
    expect(screen.getByText(/验证通道 BLOCKED：没有流水线/)).toBeInTheDocument()
    expect(screen.getByText(/证据缺口/)).toBeInTheDocument()
    expect(screen.getByText(/缺少流水线日志/)).toBeInTheDocument()
    expect(screen.getByText(/评审阻断/)).toBeInTheDocument()
    expect(screen.getByText(/档位给出阻断但没有 finding/)).toBeInTheDocument()
  })

  it('never presents zero findings on an INCOMPLETE review as a clean result', async () => {
    renderDetail(incomplete)
    await screen.findByText('未完成原因')
    const card = screen.getByRole('region', { name: '评审结论' })
    expect(card).toHaveTextContent('未完成')
    expect(card).toHaveTextContent('不代表没有问题')
    expect(screen.queryByText('没有记录 Finding。')).not.toBeInTheDocument()
  })

  it('shows the review diagnostics section when warnings exist without an INCOMPLETE verdict', async () => {
    renderDetail(review({
      id: 'wf-warn-458',
      status: 'completed',
      verdict: 'PASS',
      warnings: ['验证通道 BLOCKED：没有流水线'],
      findings_summary: { published: 1, marked: 0, dropped: 0, by_severity: { P0: 0, P1: 1, P2: 0, info: 0 } },
    }), [finding()])
    expect(await screen.findByText('评审诊断')).toBeInTheDocument()
    expect(screen.getByText(/验证通道 BLOCKED：没有流水线/)).toBeInTheDocument()
  })

  it('separates a failed run from a reviewed scope in the verdict card', async () => {
    renderDetail(review({ id: 'wf-failed-458', status: 'failed', verdict: null, error: 'provider timeout' }))
    const card = await screen.findByRole('region', { name: '评审结论' })
    expect(card).toHaveTextContent('未评审')
    expect(card).toHaveTextContent('执行失败')
  })
})

function renderDrawer(node: GraphNode | null, detail: Record<string, unknown>, calls: Record<string, unknown>) {
  installFetch((url) => {
    if (url.includes('/nodes/') && url.endsWith('/calls')) return jsonResponse(calls)
    if (url.includes('/nodes/')) return jsonResponse(detail)
    return jsonResponse({})
  })
  return renderWithProviders(
    <NodeDrawer reviewId="wf-1" node={node} onClose={() => {}} />,
  )
}

describe('node drawer diagnostics', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('does not show a red error box for a skipped node and names the reason', async () => {
    renderDrawer(
      { id: 'verify.f2', kind: 'verify', status: 'skipped', reason: 'required_node_failed' },
      { id: 'verify.f2', kind: 'verify', status: 'skipped', reason: 'required_node_failed', error: 'required dependency did not complete' },
      { calls: [], next_cursor: '', retention: { details_purged: false } },
    )
    expect(await screen.findByText(/必需的前置节点失败/)).toBeInTheDocument()
    expect(screen.queryByText('节点错误')).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('translates a failed node reason instead of only saying node failed', async () => {
    renderDrawer(
      { id: 'review.a', kind: 'review', status: 'failed', reason: 'sandbox_denied' },
      { id: 'review.a', kind: 'review', status: 'failed', reason: 'sandbox_denied', error: 'node failed' },
      { calls: [], next_cursor: '', retention: { details_purged: false } },
    )
    expect(await screen.findByText(/证据沙箱或 ignore 策略/)).toBeInTheDocument()
  })

  it('distinguishes a code node from deleted call details', async () => {
    renderDrawer(
      { id: 'synthesize', kind: 'synthesize', status: 'completed', token_free: true },
      { id: 'synthesize', kind: 'synthesize', status: 'completed', token_free: true },
      { calls: [], next_cursor: '', retention: { details_purged: false } },
    )
    expect(await screen.findByText('这是一段代码节点，没有模型调用。')).toBeInTheDocument()

    renderDrawer(
      { id: 'review.a', kind: 'review', status: 'completed' },
      { id: 'review.a', kind: 'review', status: 'completed', calls_purged: true, calls_retention_hours: 720 },
      { calls: [], next_cursor: '', retention: { details_purged: true } },
    )
    const matches = await screen.findAllByText(/明细已按保留策略删除|调用明细已按保留策略删除/)
    expect(matches.length).toBeGreaterThan(0)
  })
})