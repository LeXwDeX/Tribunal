import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'
import { QueryClientProvider } from '@tanstack/react-query'
import type { Finding, GraphNode, ReviewDetail } from '../src/api/types'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { ExecutionGraph } from '../src/features/review/ExecutionGraph'
import { NodeDrawer } from '../src/features/review/NodeDrawer'
import { findingFunnel, laneLabel, verdictCardState } from '../src/features/review/reviewView'
import { COLUMN_GAP, NODE_HEIGHT, NODE_WIDTH, ROW_GAP } from '../src/lib/dag'
import review$ from '../src/features/review/review.module.css'
import { finding, review } from './fixtures'
import { createTestQueryClient, installFetch, jsonResponse, renderWithProviders } from './test-utils'

class FakeEventSource {
  url: string
  constructor(url: string) {
    this.url = url
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

function renderDetail(initial: ReviewDetail, findings: Finding[] = [finding()]) {
  installFetch((url) => {
    if (url === `/api/v1/reviews/${initial.id}`) return jsonResponse(initial)
    if (url === `/api/v1/reviews/${initial.id}/graph`) return jsonResponse({ nodes: [], edges: [] })
    if (url.startsWith(`/api/v1/reviews/${initial.id}/findings`)) return jsonResponse({ findings })
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

async function verdictCard() {
  return screen.findByRole('region', { name: '评审结论' })
}

describe('verdictCardState', () => {
  it('uses the PASS style only for a PASS verdict on a run that neither failed nor was cancelled', () => {
    expect(verdictCardState(review({ status: 'completed', verdict: 'PASS' })).tone).toBe('PASS')
    expect(verdictCardState(review({ status: 'failed', verdict: 'PASS' })).tone).toBe('FAILED')
    expect(verdictCardState(review({ status: 'cancelled', verdict: 'PASS' })).tone).toBe('CANCELLED')
    expect(verdictCardState(review({ status: 'failed', verdict: 'PASS' })).mark).not.toBe('PASS')
    expect(verdictCardState(review({ status: 'cancelled', verdict: 'PASS' })).mark).not.toBe('PASS')
  })

  it('falls back to the status label when the verdict is empty', () => {
    expect(verdictCardState(review({ status: 'running', verdict: null }))).toEqual({ tone: 'PENDING', mark: '运行中' })
    expect(verdictCardState(review({ status: 'completed', verdict: 'INCOMPLETE' })).tone).toBe('INCOMPLETE')
    expect(verdictCardState(review({ status: 'completed', verdict: 'BLOCKING' })).tone).toBe('BLOCKING')
  })
})

describe('verdict card', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('renders PASS with the publication state, tokens and cost', async () => {
    renderDetail(review({
      id: 'wf-pass',
      status: 'completed',
      verdict: 'PASS',
      publication_state: 'published',
      usage: { tokens_in: 412000, tokens_out: 38000, cost_micros: 1_840_000 },
      findings_summary: { published: 0, marked: 0, dropped: 1, by_severity: { P0: 0, P1: 0, P2: 1, info: 0 } },
    }))
    const card = await verdictCard()
    expect(card).toHaveAttribute('data-verdict', 'PASS')
    expect(within(card).getByText('PASS')).toBeInTheDocument()
    expect(within(card).getByText('已发布')).toBeInTheDocument()
    expect(within(card).getByText('$1.8400')).toBeInTheDocument()
  })

  it('renders BLOCKING with a summary built only from findings_summary', async () => {
    renderDetail(review({
      id: 'wf-blocking',
      status: 'completed',
      verdict: 'BLOCKING',
      publication_state: 'published',
      findings_summary: { published: 2, marked: 1, dropped: 3, by_severity: { P0: 0, P1: 2, P2: 3, info: 1 } },
    }))
    const card = await verdictCard()
    expect(card).toHaveAttribute('data-verdict', 'BLOCKING')
    expect(card).toHaveTextContent('BLOCKING')
    expect(card).toHaveTextContent('已发布 2 条')
    expect(card).toHaveTextContent('P1 ×2')
    expect(card).toHaveTextContent('P2 ×3')
    expect(card).not.toHaveTextContent('P0 ×')
  })

  it('renders INCOMPLETE with the INCOMPLETE style', async () => {
    renderDetail(review({ id: 'wf-incomplete', status: 'completed', verdict: 'INCOMPLETE' }))
    const card = await verdictCard()
    expect(card).toHaveAttribute('data-verdict', 'INCOMPLETE')
    expect(card).toHaveTextContent('INCOMPLETE')
  })

  it('renders a failed run with the error and never with the PASS style', async () => {
    renderDetail(review({ id: 'wf-failed', status: 'failed', verdict: 'PASS', error: 'provider timeout' }))
    const card = await verdictCard()
    expect(card).toHaveAttribute('data-verdict', 'FAILED')
    expect(card).not.toHaveTextContent('PASS')
    expect(within(card).getByRole('alert')).toHaveTextContent('执行失败：provider timeout')
  })

  it('renders a cancelled run with the cancel reason and never with the PASS style', async () => {
    renderDetail(review({ id: 'wf-cancelled', status: 'cancelled', verdict: null, cancel_reason: '用户取消' }))
    const card = await verdictCard()
    expect(card).toHaveAttribute('data-verdict', 'CANCELLED')
    expect(card).not.toHaveTextContent('PASS')
    expect(card).toHaveTextContent('取消原因：用户取消')
  })

  it('counts the funnel from the loaded findings', async () => {
    renderDetail(review({ id: 'wf-funnel', status: 'completed', verdict: 'BLOCKING' }), [
      finding({ id: 'a', challenge_status: 'confirmed', decision: 'publish' }),
      finding({ id: 'b', challenge_status: 'confirmed', decision: 'publish' }),
      finding({ id: 'c', challenge_status: 'refuted', decision: 'drop' }),
      finding({ id: 'd', challenge_status: 'uncertain', decision: 'mark' }),
      finding({ id: 'e', challenge_status: 'skipped', decision: 'downgrade' }),
    ])
    const funnel = await screen.findByLabelText('Findings 漏斗')
    await waitFor(() => expect(funnel).toHaveTextContent('提出 5'))
    expect(funnel).toHaveTextContent('确认 2')
    expect(funnel).toHaveTextContent('推翻 1')
    expect(funnel).toHaveTextContent('存疑 1')
    expect(funnel).toHaveTextContent('未挑战 1')
    expect(funnel).toHaveTextContent('发布 2')
    expect(funnel).toHaveTextContent('丢弃 1')
    expect(funnel).toHaveTextContent('标记 1')
    expect(funnel).toHaveTextContent('降级 1')
  })
})

describe('findingFunnel', () => {
  it('counts challenge and decision outcomes', () => {
    const counts = findingFunnel([
      finding({ id: 'a', challenge_status: 'confirmed', decision: 'publish' }),
      finding({ id: 'b', challenge_status: undefined, decision: undefined }),
    ])
    expect(counts.proposed).toBe(2)
    expect(counts.challenge).toEqual({ confirmed: 1, refuted: 0, uncertain: 0, skipped: 1 })
    expect(counts.decision).toEqual({ publish: 1, drop: 0, mark: 0, downgrade: 0 })
  })
})

describe('findings table', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('filters by disposition with a segmented control that shows summary counts', async () => {
    const fetchMock = renderDetail(review({
      id: 'wf-seg',
      status: 'completed',
      verdict: 'BLOCKING',
      findings_summary: { published: 2, marked: 1, dropped: 3, by_severity: { P0: 0, P1: 2, P2: 3, info: 1 } },
    }))
    const group = await screen.findByRole('group', { name: '按处置过滤' })
    expect(within(group).getByRole('button', { name: /^全部/ })).toHaveAttribute('aria-pressed', 'true')
    expect(within(group).getByRole('button', { name: /发布/ })).toHaveTextContent('2')
    expect(within(group).getByRole('button', { name: /丢弃/ })).toHaveTextContent('3')
    await userEvent.setup().click(within(group).getByRole('button', { name: /丢弃/ }))
    expect(within(group).getByRole('button', { name: /丢弃/ })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('combobox', { name: '按严重级别过滤' })).toBeInTheDocument()
    void fetchMock
  })

  it('strikes through dropped findings and shows the evidence chain', async () => {
    renderDetail(review({ id: 'wf-chain', status: 'completed', verdict: 'PASS' }), [
      finding({ id: 'drop', title: '已丢弃的问题', challenge_status: 'refuted', decision: 'drop', origin_provider: 'claude' }),
    ])
    const toggle = await screen.findByRole('button', { name: /展开：已丢弃的问题/ })
    expect(toggle.closest('tr')?.querySelector(`.${review$.titleDropped}`)).not.toBeNull()
    const chain = screen.getByLabelText('证据链')
    expect(chain).toHaveTextContent('claude')
    expect(chain).toHaveTextContent('REFUTED')
    expect(chain).toHaveTextContent('丢弃')
  })

  it('shows the final severity and strikes the original one when downgraded', async () => {
    renderDetail(review({ id: 'wf-down', status: 'completed', verdict: 'PASS' }), [
      finding({ id: 'down', severity: 'P1', final_severity: 'P2', decision: 'downgrade' }),
    ])
    const toggle = await screen.findByRole('button', { name: /展开：并发写 map 未加锁/ })
    const row = toggle.closest('tr')!
    expect(row.querySelector(`.${review$.sevOriginal}`)).toHaveTextContent('原级别 P1')
    expect(within(row).getByText('P2')).toBeInTheDocument()
  })

  it('expands into a two-column detail with a timeline', async () => {
    renderDetail(review({ id: 'wf-timeline', status: 'completed', verdict: 'PASS' }), [
      finding({ challenge_node: 'verify.f1', decision_reason: '确认成立' }),
    ])
    await userEvent.setup().click(await screen.findByRole('button', { name: /展开：并发写 map 未加锁/ }))
    const timeline = screen.getByRole('list', { name: '处理过程' })
    expect(timeline).toHaveTextContent('提出')
    expect(timeline).toHaveTextContent('review.a')
    expect(timeline).toHaveTextContent('verify.f1')
    expect(timeline).toHaveTextContent('确认成立')
  })
})

describe('execution graph', () => {
  const graphNodes: GraphNode[] = [
    { id: 'plan', kind: 'plan', status: 'completed' },
    { id: 'review.a', kind: 'code-review', status: 'completed', depends_on: ['plan'] },
    { id: 'review.b', kind: 'review.standards', status: 'completed', depends_on: ['plan'] },
    { id: 'verify.f1', kind: 'verify', status: 'running', depends_on: ['review.a'] },
    { id: 'synthesize', kind: 'synthesize', status: 'pending', depends_on: ['verify.f1', 'review.b'] },
    { id: 'mystery', kind: 'custom', status: 'pending', depends_on: ['synthesize'] },
  ]

  it('uses 192×64 nodes on a 220/84 grid', () => {
    expect(NODE_WIDTH).toBe(192)
    expect(NODE_HEIGHT).toBe(64)
    expect(NODE_WIDTH + COLUMN_GAP).toBe(220)
    expect(NODE_HEIGHT + ROW_GAP).toBe(84)
  })

  it('maps lane kinds to Chinese stage names', () => {
    expect(laneLabel(0, ['plan'])).toBe('01 规划')
    expect(laneLabel(1, ['code-review', 'review.standards'])).toBe('02 提出')
    expect(laneLabel(2, ['verify', 'verify', 'code-review'])).toBe('03 挑战')
    expect(laneLabel(3, ['synthesize'])).toBe('04 裁决')
    expect(laneLabel(4, ['custom'])).toBe('05')
  })

  it('renders a lane header per layer', () => {
    const { container } = render(<ExecutionGraph nodes={graphNodes} edges={[]} onSelect={() => {}} />)
    const lanes = Array.from(container.querySelectorAll(`.${review$.graphLane}`)).map((item) => item.textContent)
    expect(lanes).toEqual(['01 规划', '02 提出', '03 挑战', '04 裁决', '05'])
  })

  it('keeps nodes keyboard focusable and highlights their edges on hover and focus', async () => {
    const onSelect = vi.fn()
    const { container } = render(<ExecutionGraph nodes={graphNodes} edges={[]} onSelect={onSelect} />)
    const node = screen.getByRole('button', { name: /节点 verify.f1/ })
    const hot = () => container.querySelectorAll(`path.${review$.edgeHot}`).length
    expect(hot()).toBe(0)
    fireEvent.mouseEnter(node)
    expect(hot()).toBe(2)
    fireEvent.mouseLeave(node)
    expect(hot()).toBe(0)
    const user = userEvent.setup()
    while (document.activeElement !== node) await user.tab()
    expect(hot()).toBe(2)
    await user.keyboard('{Enter}')
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ id: 'verify.f1' }))
  })
})

describe('node drawer', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows params, output JSON with a copy button and one details block per LLM turn, and closes on Escape', async () => {
    installFetch((url) => {
      if (url === '/api/v1/reviews/wf/nodes/review.a') {
        return jsonResponse({
          id: 'review.a', kind: 'code-review', status: 'completed', provider: 'claude', attempt: 2, duration_ms: 48000,
          params: { focus: 'internal/**', max_turns: 8 },
          output: { findings: [] },
          usage: { tokens_in: 100, tokens_out: 20, cost_micros: 310000 },
        })
      }
      if (url.startsWith('/api/v1/reviews/wf/nodes/review.a/calls')) {
        return jsonResponse({
          calls: [
            { id: 'c1', turn: 1, input_tokens: 18204, output_tokens: 1022, latency_ms: 6100, stop_reason: 'tool_use', request: { a: 1 } },
            { id: 'c2', turn: 2, input_tokens: 22980, output_tokens: 640, latency_ms: 3400, stop_reason: 'end_turn' },
          ],
        })
      }
      return jsonResponse({})
    })
    const onClose = vi.fn()
    const client = createTestQueryClient()
    render(
      <QueryClientProvider client={client}>
        <NodeDrawer reviewId="wf" node={{ id: 'review.a', kind: 'code-review', status: 'completed' }} onClose={onClose} />
      </QueryClientProvider>,
    )
    const dialog = await screen.findByRole('dialog', { name: 'review.a' })
    await within(dialog).findByText('internal/**')
    expect(within(dialog).getByText('max_turns')).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '复制 JSON' })).toBeInTheDocument()
    expect(dialog).toHaveTextContent('$0.3100')
    expect(dialog).toHaveTextContent('第 2 次尝试')
    await waitFor(() => expect(dialog.querySelectorAll('details')).toHaveLength(2))
    const summaries = Array.from(dialog.querySelectorAll('details > summary')).map((item) => item.textContent)
    expect(summaries[0]).toContain('#1')
    expect(summaries[0]).toContain('18,204')
    expect(summaries[0]).toContain('tool_use')
    await userEvent.setup().keyboard('{Escape}')
    expect(onClose).toHaveBeenCalled()
  })
})
