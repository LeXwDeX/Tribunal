import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import type { Finding, ReviewDetail } from '../src/api/types'
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

describe('review coverage and material downgrades (#466)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the structured review scope and the generated-file disclosure', async () => {
    renderDetail(review({
      id: 'wf-scope-466',
      status: 'completed',
      verdict: 'INCOMPLETE',
      summary: '覆盖范围有限',
      unresolved: [{ node: 'validate', kind: 'evidence_gap', message: 'lane validate did not complete' }],
      scope: {
        full: false,
        head: '0123456789abcdef0123456789abcdef01234567',
        last_head: '89abcdef0123456789abcdef0123456789abcdef',
        files: ['a.go'],
        file_count: 12,
        unlocated: 2,
      },
      generated_files: 2,
      generated_paths: ['api.pb.go', 'gen/x.go'],
      findings_summary: { published: 0, marked: 0, dropped: 0, by_severity: { P0: 0, P1: 0, P2: 0, info: 0 } },
    }))
    const title = await screen.findByRole('heading', { name: '评审范围' })
    const panel = title.closest('section')!
    expect(panel).toHaveTextContent('增量评审')
    expect(panel).toHaveTextContent('结论仅覆盖本次增量变更的文件')
    expect(panel).toHaveTextContent('覆盖文件：12 个')
    expect(panel).toHaveTextContent('无法定位到行的问题：2 条')
    expect(panel).toHaveTextContent('跳过 2 个生成文件')
    expect(panel).toHaveTextContent('api.pb.go')
    expect(panel).toHaveTextContent('gen/x.go')
  })

  it('shows a full review scope without inventing an incremental file list', async () => {
    renderDetail(review({
      id: 'wf-full-466',
      status: 'completed',
      verdict: 'PASS',
      scope: { full: true, head: '0123456789abcdef0123456789abcdef01234567', file_count: 5, unlocated: 0 },
      findings_summary: { published: 0, marked: 0, dropped: 0, by_severity: { P0: 0, P1: 0, P2: 0, info: 0 } },
    }))
    const title = await screen.findByRole('heading', { name: '评审范围' })
    const panel = title.closest('section')!
    expect(panel).toHaveTextContent('全量评审')
    expect(panel).toHaveTextContent('覆盖文件：5 个')
    expect(panel).not.toHaveTextContent('无法定位到行的问题')
  })

  it('shows dossier material downgrades in the diagnostics panel', async () => {
    renderDetail(review({
      id: 'wf-material-466',
      status: 'completed',
      verdict: 'PASS',
      material_warnings: ['review ran without the code graph: project ignore rules blocked workspace indexing'],
      findings_summary: { published: 0, marked: 0, dropped: 0, by_severity: { P0: 0, P1: 0, P2: 0, info: 0 } },
    }))
    expect(await screen.findByText('评审材料降级')).toBeInTheDocument()
    expect(screen.getByText(/review ran without the code graph: project ignore rules blocked workspace indexing/)).toBeInTheDocument()
  })

  it('omits the scope panel when the API returns no coverage', async () => {
    renderDetail(review({ id: 'wf-no-scope-466', status: 'failed', verdict: null, error: 'provider timeout' }))
    await screen.findByRole('region', { name: '评审结论' })
    expect(screen.queryByRole('heading', { name: '评审范围' })).not.toBeInTheDocument()
  })
})