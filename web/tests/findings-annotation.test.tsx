import { act } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { setFindingAnnotation } from '../src/api/api'
import { setCsrfToken } from '../src/api/client'
import type { Finding, FindingAnnotation, Me } from '../src/api/types'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { finding, project, review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

type WriteStep = { response: Response | Promise<Response>; next?: Finding }

class FakeEventSource {
  url: string
  constructor(url: string) {
    this.url = url
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

function routeReview(initial: Finding, options: { writes?: WriteStep[]; access?: 'read' | 'manage' } = {}) {
  let current = initial
  const writes: Array<{ url: string; init?: RequestInit }> = []
  const findingsUrls: string[] = []
  let statsCalls = 0

  installFetch((url, init) => {
    const method = (init?.method || 'GET').toUpperCase()
    if (url === '/api/v1/reviews/wf-1/findings/f1/annotation' && method === 'PUT') {
      writes.push({ url, init })
      const step = options.writes?.shift()
      if (step) {
        if (step.next) current = step.next
        return step.response
      }
      const body = JSON.parse(String(init?.body)) as { annotation: FindingAnnotation }
      current = {
        ...current,
        annotation: body.annotation,
        annotated_by: body.annotation ? 'alice' : '',
        annotated_at: body.annotation ? '2026-10-23T08:00:00Z' : '',
      }
      return jsonResponse(current)
    }
    if (url.startsWith('/api/v1/reviews/wf-1/findings')) {
      findingsUrls.push(url)
      return jsonResponse({ findings: [current] })
    }
    if (url === '/api/v1/reviews/wf-1/graph') return jsonResponse({ nodes: [], edges: [] })
    if (url === '/api/v1/reviews/wf-1') {
      return jsonResponse(review({ id: 'wf-1', project_id: 42, status: 'completed', access: options.access ?? 'manage' }))
    }
    if (url === '/api/v1/projects/42') return jsonResponse(project())
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    if (url.startsWith('/api/v1/stats')) {
      statsCalls += 1
      return jsonResponse({ runs: 0 })
    }
    return jsonResponse({})
  })

  return { writes, findingsUrls, statsCalls: () => statsCalls }
}

function StatsProbe() {
  const query = useQuery({
    queryKey: ['stats', 'probe'],
    queryFn: async () => {
      const response = await fetch('/api/v1/stats?probe=1')
      return response.json() as Promise<unknown>
    },
  })
  return <span data-testid="stats-probe">{query.data ? 'loaded' : 'pending'}</span>
}

function ReviewRoute() {
  return (
    <Routes>
      <Route path="/reviews/:id" element={<ReviewDetailPage />} />
    </Routes>
  )
}

function renderReview(options: { me?: Me; probe?: boolean } = {}) {
  return renderWithProviders(
    <>
      <ReviewRoute />
      {options.probe ? <StatsProbe /> : null}
    </>,
    { route: '/reviews/wf-1', me: options.me },
  )
}

async function expandFinding(user: ReturnType<typeof userEvent.setup>) {
  const toggle = await screen.findByRole('button', { name: /展开：并发写 map 未加锁/ })
  await user.click(toggle)
}

describe('finding annotation API', () => {
  beforeEach(() => setCsrfToken('csrf-annotation'))
  afterEach(() => {
    vi.unstubAllGlobals()
    setCsrfToken(null)
  })

  it('encodes both path segments and sends the annotation with CSRF', async () => {
    const calls: Array<{ url: string; init?: RequestInit }> = []
    installFetch((url, init) => {
      calls.push({ url, init })
      return jsonResponse(finding({ id: 'f 1', workflow_id: 'wf/one', annotation: 'invalid' }))
    })

    const updated = await setFindingAnnotation('wf/one', 'f 1', 'invalid')

    expect(updated.annotation).toBe('invalid')
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/reviews/wf%2Fone/findings/f%201/annotation')
    expect(calls[0].init?.method).toBe('PUT')
    expect(new Headers(calls[0].init?.headers).get('X-CSRF-Token')).toBe('csrf-annotation')
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ annotation: 'invalid' })
  })
})

describe('finding annotation controls', () => {
  beforeEach(() => {
    setCsrfToken('csrf-test')
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    setCsrfToken(null)
  })

  it('lets an admin save an annotation and refetches findings and stats', async () => {
    const user = userEvent.setup()
    const state = routeReview(finding({ annotation: '' }))
    renderReview({ probe: true })
    await waitFor(() => expect(state.statsCalls()).toBe(1))

    await expandFinding(user)
    const group = screen.getByRole('group', { name: '人工标注' })
    await user.click(within(group).getByRole('button', { name: '有效' }))
    await user.click(screen.getByRole('button', { name: '保存标注' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))

    const saved = screen.getByRole('status', { name: '已保存的人工标注' })
    await waitFor(() => expect(within(saved).getByText('有效')).toBeInTheDocument())
    expect(within(saved).getByText(/标注人 alice/)).toBeInTheDocument()
    expect(saved.querySelector('time')?.getAttribute('datetime')).toBe('2026-10-23T08:00:00Z')

    expect(state.writes[0].url).toBe('/api/v1/reviews/wf-1/findings/f1/annotation')
    expect(state.writes[0].init?.method).toBe('PUT')
    expect(new Headers(state.writes[0].init?.headers).get('X-CSRF-Token')).toBe('csrf-test')
    expect(JSON.parse(String(state.writes[0].init?.body))).toEqual({ annotation: 'valid' })

    await waitFor(() => expect(state.findingsUrls.length).toBeGreaterThan(1))
    await waitFor(() => expect(state.statsCalls()).toBeGreaterThan(1))
  })

  it.each([
    ['corrects', 'valid', '无效', 'invalid'],
    ['clears', 'valid', '未标注', ''],
  ] as const)('%s a saved annotation as admin', async (_label, initial, option, expected) => {
    const user = userEvent.setup()
    const state = routeReview(finding({
      annotation: initial,
      annotated_by: 'reviewer',
      annotated_at: '2026-10-22T08:00:00Z',
    }))
    renderReview()

    await expandFinding(user)
    const group = screen.getByRole('group', { name: '人工标注' })
    await user.click(within(group).getByRole('button', { name: option }))
    await user.click(screen.getByRole('button', { name: '保存标注' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(JSON.parse(String(state.writes[0].init?.body))).toEqual({ annotation: expected })

    const saved = screen.getByRole('status', { name: '已保存的人工标注' })
    await waitFor(() => expect(within(saved).getByText(option)).toBeInTheDocument())
    if (expected === '') {
      expect(within(saved).queryByText(/标注人/)).not.toBeInTheDocument()
      expect(saved.querySelector('time')).toBeNull()
    } else {
      expect(within(saved).getByText(/标注人 alice/)).toBeInTheDocument()
    }
  })

  it('disables the annotation controls while a save is pending', async () => {
    const user = userEvent.setup()
    let resolveWrite!: (response: Response) => void
    const pending = new Promise<Response>((resolve) => { resolveWrite = resolve })
    const initial = finding({ annotation: '' })
    const updated: Finding = {
      ...initial,
      annotation: 'valid',
      annotated_by: 'alice',
      annotated_at: '2026-10-23T08:00:00Z',
    }
    routeReview(initial, { writes: [{ response: pending, next: updated }] })
    renderReview()

    await expandFinding(user)
    const group = screen.getByRole('group', { name: '人工标注' })
    await user.click(within(group).getByRole('button', { name: '有效' }))
    await user.click(screen.getByRole('button', { name: '保存标注' }))

    expect(screen.getByRole('button', { name: '保存标注' })).toBeDisabled()
    expect(within(group).getByRole('button', { name: '有效' })).toBeDisabled()
    expect(within(group).getByRole('button', { name: '无效' })).toBeDisabled()
    expect(within(group).getByRole('button', { name: '未确定' })).toBeDisabled()
    expect(within(group).getByRole('button', { name: '未标注' })).toBeDisabled()

    await act(async () => { resolveWrite(jsonResponse(updated)) })
    await waitFor(() => expect(screen.getByRole('button', { name: '保存标注' })).not.toBeDisabled())
  })

  it('keeps the prior saved annotation after a failed write and retries', async () => {
    const user = userEvent.setup()
    const initial = finding({
      annotation: 'valid',
      annotated_by: 'reviewer',
      annotated_at: '2026-10-22T08:00:00Z',
    })
    const updated: Finding = {
      ...initial,
      annotation: 'invalid',
      annotated_by: 'alice',
      annotated_at: '2026-10-23T08:00:00Z',
    }
    const state = routeReview(initial, {
      writes: [
        { response: jsonResponse({ error: { code: 'internal', message: 'write failed' } }, 500) },
        { response: jsonResponse(updated), next: updated },
      ],
    })
    renderReview({ probe: true })
    await waitFor(() => expect(state.statsCalls()).toBe(1))
    const findingsBefore = state.findingsUrls.length

    await expandFinding(user)
    const group = screen.getByRole('group', { name: '人工标注' })
    await user.click(within(group).getByRole('button', { name: '无效' }))
    await user.click(screen.getByRole('button', { name: '保存标注' }))

    const alert = await screen.findByRole('alert')
    expect(within(alert).getByText(/write failed/)).toBeInTheDocument()
    const saved = screen.getByRole('status', { name: '已保存的人工标注' })
    expect(within(saved).getByText('有效')).toBeInTheDocument()
    expect(within(saved).getByText(/标注人 reviewer/)).toBeInTheDocument()
    expect(state.findingsUrls).toHaveLength(findingsBefore)
    expect(state.statsCalls()).toBe(1)

    await user.click(within(alert).getByRole('button', { name: '重试' }))
    await waitFor(() => expect(state.writes).toHaveLength(2))
    await waitFor(() => expect(within(saved).getByText('无效')).toBeInTheDocument())
    expect(within(saved).getByText(/标注人 alice/)).toBeInTheDocument()
    await waitFor(() => expect(state.findingsUrls.length).toBeGreaterThan(findingsBefore))
    await waitFor(() => expect(state.statsCalls()).toBeGreaterThan(1))
  })

  it('shows a saved annotation, actor and time read-only to viewers', async () => {
    const user = userEvent.setup()
    routeReview(finding({
      annotation: 'valid',
      annotated_by: 'reviewer',
      annotated_at: '2026-10-23T06:30:00Z',
    }), { access: 'read' })
    renderReview({ me: viewerMe })

    await expandFinding(user)
    const saved = screen.getByRole('status', { name: '已保存的人工标注' })
    expect(within(saved).getByText('有效')).toBeInTheDocument()
    expect(within(saved).getByText(/标注人 reviewer/)).toBeInTheDocument()
    expect(saved.querySelector('time')?.getAttribute('datetime')).toBe('2026-10-23T06:30:00Z')
    expect(screen.queryByRole('group', { name: '人工标注' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存标注' })).not.toBeInTheDocument()
  })

  it('refetches the same active finding filter after a successful annotation', async () => {
    const user = userEvent.setup()
    const state = routeReview(finding({ annotation: '' }))
    renderReview()
    await screen.findByRole('button', { name: /展开：并发写 map 未加锁/ })

    await user.click(screen.getByRole('button', { name: '发布' }))
    await user.selectOptions(screen.getByRole('combobox', { name: '按严重级别过滤' }), 'P1')
    await waitFor(() => expect(
      state.findingsUrls.some((url) => url.includes('decision=publish') && url.includes('severity=P1')),
    ).toBe(true))
    const before = state.findingsUrls.length

    await expandFinding(user)
    const group = screen.getByRole('group', { name: '人工标注' })
    await user.click(within(group).getByRole('button', { name: '有效' }))
    await user.click(screen.getByRole('button', { name: '保存标注' }))

    await waitFor(() => expect(state.findingsUrls.length).toBeGreaterThan(before))
    const last = state.findingsUrls[state.findingsUrls.length - 1]
    expect(last).toContain('decision=publish')
    expect(last).toContain('severity=P1')
  })
})
