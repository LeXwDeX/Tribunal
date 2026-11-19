import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, within } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import type { ReviewDetail, UnresolvedSignal } from '../src/api/types'
import { groupUnresolved, unresolvedCategoryLabel } from '../src/lib/diagnostics'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { review } from './fixtures'
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

function renderDetail(initial: ReviewDetail) {
  installFetch((url) => {
    if (url === `/api/v1/reviews/${initial.id}`) return jsonResponse(initial)
    if (url === `/api/v1/reviews/${initial.id}/graph`) return jsonResponse({ nodes: [], edges: [] })
    if (url.startsWith(`/api/v1/reviews/${initial.id}/findings`)) return jsonResponse({ findings: [] })
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

// jsdom toggles details on click but does not emit the toggle event a browser
// emits after the default action, so send it explicitly.
function openDisclosure(summary: HTMLElement) {
  fireEvent.click(summary)
  fireEvent(summary.closest('details')!, new Event('toggle'))
}

const INDEX_TEXT = '代码图谱索引（abcdef01）与本 MR 目标提交（33333333）不一致，本次未启用代码图谱工具，相关范围可能未被完整评审。'
const COVERAGE_RAW = 'diff collection omitted 1 of 20 changed file(s): 19 included in the materials snapshot, 1 ignored by rules; reasons: too_large=1; omitted paths: assets/hero.png'
const COVERAGE_TEXT = '变更文件收集不完整：共 20 个变更文件，有 1 个未进入评审材料（原因：文件过大 1 个）；未进入评审的路径：assets/hero.png。'
const BLOCKING_TEXT = '复核通道判定阻断，但未接受任何具体问题。'
const STORED_SUMMARY = '评审未完成：上游评审存在未解决的证据不足或阻断，不能给出 PASS。具体缺口：explore 等 5 个节点: block explore: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index abcdef01, target 33333333)；coverage: ' + COVERAGE_RAW
const SUMMARY_TEXT = `评审未完成：上游评审存在未解决的证据不足或阻断，不能给出 PASS。具体缺口：${INDEX_TEXT.replace(/。$/, '')}；${COVERAGE_TEXT.replace(/。$/, '')}。`

function toolGap(node: string, block: string): UnresolvedSignal {
  return {
    node,
    kind: 'evidence_gap',
    category: 'tool_unavailable',
    message: `block ${block}: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index abcdef01, target 33333333)`,
    text: INDEX_TEXT,
  }
}

const signals: UnresolvedSignal[] = [
  toolGap('explore', 'explore'),
  toolGap('cr.p1', 'code-review'),
  toolGap('cr.p2', 'code-review'),
  toolGap('rv.p1', 'review'),
  toolGap('reasoner', 'reasoner'),
  { node: 'coverage', kind: 'evidence_gap', category: 'evidence_gap', message: COVERAGE_RAW, text: COVERAGE_TEXT },
  { node: 'rv.p2', kind: 'evidence_gap', category: 'evidence_gap', message: 'pipeline log unavailable', text: 'pipeline log unavailable' },
  {
    node: 'review.gate',
    kind: 'review_blocking',
    category: 'review_blocking',
    message: 'review gate reported BLOCKING with no accepted findings',
    text: BLOCKING_TEXT,
  },
]

const incomplete = review({
  id: 'wf-groups-603',
  status: 'completed',
  verdict: 'INCOMPLETE',
  summary: STORED_SUMMARY,
  summary_text: SUMMARY_TEXT,
  unresolved: signals,
  findings_summary: { published: 0, marked: 0, dropped: 0, by_severity: { P0: 0, P1: 0, P2: 0, info: 0 } },
})

async function signalRows() {
  const list = await screen.findByRole('list', { name: '未解决信号' })
  return within(list).getAllByRole('listitem')
}

describe('unresolved signal groups (#603)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the tool gaps of five nodes as one row named 工具不可用', async () => {
    renderDetail(incomplete)
    const rows = await signalRows()
    expect(rows).toHaveLength(4)

    const [tool, coverage, pipeline, blocking] = rows
    expect(within(tool).getByText('工具不可用')).toBeInTheDocument()
    expect(within(tool).getByText(INDEX_TEXT)).toBeInTheDocument()
    expect(within(tool).getByText('explore · cr.p1 · cr.p2 · rv.p1 · reasoner')).toBeInTheDocument()
    expect(within(tool).queryByText('证据缺口')).not.toBeInTheDocument()
    // The status label must not call a missing tool an evidence gap.
    expect(screen.getAllByText('工具不可用')).toHaveLength(1)

    expect(within(coverage).getByText('证据缺口')).toBeInTheDocument()
    expect(within(coverage).getByText(COVERAGE_TEXT)).toBeInTheDocument()
    expect(within(coverage).getByText('coverage')).toBeInTheDocument()
    expect(within(pipeline).getByText('证据缺口')).toBeInTheDocument()
    expect(within(pipeline).getByText('pipeline log unavailable')).toBeInTheDocument()
    expect(within(blocking).getByText('评审阻断')).toBeInTheDocument()
    expect(within(blocking).getByText(BLOCKING_TEXT)).toBeInTheDocument()
  })

  it('keeps the raw messages in a collapsed 技术详情 of the group', async () => {
    renderDetail(incomplete)
    const [tool, coverage, pipeline] = await signalRows()

    const raw = within(tool).getAllByText(/MCP tools unavailable/)
    expect(raw).toHaveLength(5)
    expect(within(tool).getByText(/^explore: block explore: MCP tools unavailable/)).not.toBeVisible()
    expect(within(tool).getByText(/^cr\.p2: block code-review: MCP tools unavailable/)).not.toBeVisible()

    openDisclosure(within(tool).getByText('技术详情'))
    expect(within(tool).getByText(/^explore: block explore: MCP tools unavailable/)).toBeVisible()
    expect(within(tool).getByText(/^reasoner: block reasoner: MCP tools unavailable/)).toBeVisible()

    // A localized message keeps its original text for diagnosis.
    expect(within(coverage).getByText('技术详情')).toBeInTheDocument()
    expect(within(coverage).getByText(`coverage: ${COVERAGE_RAW}`)).toBeInTheDocument()
    // Nothing more technical to show when the text is the message itself.
    expect(within(pipeline).queryByText('技术详情')).not.toBeInTheDocument()
  })

  it('shows summary_text instead of the long stored summary', async () => {
    renderDetail(incomplete)
    expect(await screen.findByText(SUMMARY_TEXT)).toBeInTheDocument()
    expect(screen.queryByText(STORED_SUMMARY)).not.toBeInTheDocument()
    expect(screen.queryByText(/blocked by workspace index identity/, { selector: 'p' })).not.toBeInTheDocument()
  })

  it('renders a response without category, text and summary_text from the stored fields', async () => {
    renderDetail(review({
      id: 'wf-legacy-603',
      status: 'completed',
      verdict: 'INCOMPLETE',
      summary: '评审未完成：上游证据不足，不能给出 PASS。',
      unresolved: [
        { node: 'coverage', kind: 'evidence_gap', message: '缺少流水线日志' },
        { node: 'rv.p1', kind: 'evidence_gap', message: '缺少流水线日志' },
        { node: 'lanes.review', kind: 'review_blocking', message: '档位给出阻断但没有 finding' },
      ],
      findings_summary: { published: 0, marked: 0, dropped: 0, by_severity: { P0: 0, P1: 0, P2: 0, info: 0 } },
    }))
    expect(await screen.findByText('评审未完成：上游证据不足，不能给出 PASS。')).toBeInTheDocument()
    const rows = await signalRows()
    expect(rows).toHaveLength(2)
    expect(within(rows[0]).getByText('证据缺口')).toBeInTheDocument()
    expect(within(rows[0]).getByText('缺少流水线日志')).toBeInTheDocument()
    expect(within(rows[0]).getByText('coverage · rv.p1')).toBeInTheDocument()
    expect(within(rows[1]).getByText('评审阻断')).toBeInTheDocument()
    expect(within(rows[1]).getByText('档位给出阻断但没有 finding')).toBeInTheDocument()
    expect(screen.queryByText('工具不可用')).not.toBeInTheDocument()
  })
})

describe('groupUnresolved', () => {
  it('groups by category and text and keeps first-seen order and unique nodes', () => {
    const groups = groupUnresolved([
      { node: 'a', kind: 'evidence_gap', category: 'tool_unavailable', message: 'm1', text: 'T' },
      { node: 'b', kind: 'evidence_gap', category: 'evidence_gap', message: 'm2', text: 'T' },
      { node: 'c', kind: 'evidence_gap', category: 'tool_unavailable', message: 'm3', text: 'T' },
      { node: 'a', kind: 'evidence_gap', category: 'tool_unavailable', message: 'm4', text: 'T' },
    ])
    expect(groups.map((group) => [group.category, group.text, group.nodes])).toEqual([
      ['tool_unavailable', 'T', ['a', 'c']],
      ['evidence_gap', 'T', ['b']],
    ])
    expect(groups[0].signals).toHaveLength(3)
  })

  it('falls back to the kind and the message when the server sent no category or text', () => {
    const groups = groupUnresolved([
      { node: 'x', kind: 'review_blocking', message: 'blocked' },
      { node: 'y', kind: 'review_blocking', message: 'blocked' },
    ])
    expect(groups).toHaveLength(1)
    expect(groups[0]).toMatchObject({ category: 'review_blocking', text: 'blocked', nodes: ['x', 'y'], technical: false })
  })

  it('offers technical details only when a message differs from its text', () => {
    const [same] = groupUnresolved([{ node: 'x', kind: 'evidence_gap', message: 'm', text: 'm' }])
    const [localized] = groupUnresolved([{ node: 'x', kind: 'evidence_gap', message: 'm', text: '中文' }])
    expect(same.technical).toBe(false)
    expect(localized.technical).toBe(true)
  })

  it('labels every category and keeps an unknown one readable', () => {
    expect(unresolvedCategoryLabel('tool_unavailable')).toBe('工具不可用')
    expect(unresolvedCategoryLabel('evidence_gap')).toBe('证据缺口')
    expect(unresolvedCategoryLabel('review_blocking')).toBe('评审阻断')
    expect(unresolvedCategoryLabel('future_kind')).toBe('未解决信号')
  })
})
