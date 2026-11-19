import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import type { ReviewDetail } from '../src/api/types'
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

const GAP = 'block code-review: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index unknown, target unknown)'
const RAW_WARNINGS = [
  `rv.p1: ${GAP}`,
  `reasoner: ${GAP}`,
  'unresolved evidence or review-level blocking cannot be settled as PASS',
]
const WARNINGS_TEXT = [
  '无法确认代码图谱索引与本 MR 目标提交一致（工作区索引未就绪或索引失败），本次未启用代码图谱工具，相关范围可能未被完整评审。（rv.p1 · reasoner）',
  '存在未解决的证据不足或阻断，不能判定为通过。',
]
const NO_FINDINGS = { published: 0, marked: 0, dropped: 0, by_severity: { P0: 0, P1: 0, P2: 0, info: 0 } }

function incomplete(overrides: Partial<ReviewDetail>): ReviewDetail {
  return review({
    id: 'wf-warnings-603',
    status: 'completed',
    verdict: 'INCOMPLETE',
    summary: '评审未完成：上游评审存在未解决的证据不足或阻断，不能给出 PASS。',
    unresolved: [{ node: 'coverage', kind: 'evidence_gap', category: 'evidence_gap', message: '缺少流水线日志', text: '缺少流水线日志' }],
    findings_summary: NO_FINDINGS,
    ...overrides,
  })
}

describe('localized warnings (#603)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows warnings_text instead of the raw warnings', async () => {
    renderDetail(incomplete({ warnings: RAW_WARNINGS, warnings_text: WARNINGS_TEXT }))
    const list = await screen.findByRole('list', { name: '评审告警' })
    expect(within(list).getAllByRole('listitem').map((item) => item.textContent)).toEqual(WARNINGS_TEXT)
    expect(screen.queryByText(/MCP tools unavailable/)).not.toBeInTheDocument()
    expect(screen.queryByText(/cannot be settled as PASS/)).not.toBeInTheDocument()
  })

  // Every warning repeated an unresolved signal: the server sends [], which
  // must not bring the raw warnings back.
  it('shows no 告警 section when warnings_text is an empty array', async () => {
    renderDetail(incomplete({ warnings: [`explore: ${GAP}`], warnings_text: [] }))
    expect(await screen.findByText('未完成原因')).toBeInTheDocument()
    expect(screen.getByRole('list', { name: '未解决信号' })).toBeInTheDocument()
    expect(screen.queryByRole('list', { name: '评审告警' })).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '告警' })).not.toBeInTheDocument()
    expect(screen.queryByText(/MCP tools unavailable/)).not.toBeInTheDocument()
  })

  it('falls back to the raw warnings when warnings_text is absent', async () => {
    renderDetail(incomplete({ warnings: ['验证通道 BLOCKED：没有流水线', 'pipeline log unavailable'] }))
    const list = await screen.findByRole('list', { name: '评审告警' })
    expect(within(list).getAllByRole('listitem').map((item) => item.textContent)).toEqual([
      '验证通道 BLOCKED：没有流水线', 'pipeline log unavailable',
    ])
  })

  it('shows no diagnostics for a completed review whose warnings were all repeats', async () => {
    renderDetail(review({
      id: 'wf-warnings-pass-603',
      status: 'completed',
      verdict: 'PASS',
      warnings: ['explore: x'],
      warnings_text: [],
      findings_summary: NO_FINDINGS,
    }))
    expect(await screen.findByRole('region', { name: '评审结论' })).toBeInTheDocument()
    expect(screen.queryByText('评审诊断')).not.toBeInTheDocument()
    expect(screen.queryByText('explore: x')).not.toBeInTheDocument()
  })
})
