import { afterEach, describe, expect, it, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { screen } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import type { ReviewDetail } from '../src/api/types'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { ModelsPage, summarizeParams } from '../src/pages/ModelsPage'
import { review } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

class FakeEventSource {
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

function renderDetail(initial: ReviewDetail) {
  installFetch((url) => {
    if (url === `/api/v1/reviews/${initial.id}`) return jsonResponse(initial)
    if (url === `/api/v1/reviews/${initial.id}/graph`) return jsonResponse({ nodes: [], edges: [] })
    if (url.startsWith(`/api/v1/reviews/${initial.id}/findings`)) return jsonResponse({ findings: [] })
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    return jsonResponse({})
  })
  vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
  renderWithProviders(
    <Routes><Route path="/reviews/:id" element={<ReviewDetailPage />} /></Routes>,
    { route: `/reviews/${initial.id}` },
  )
}

describe('usage interruptions on review detail (#346)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the recovery count next to the incomplete-usage notice', async () => {
    renderDetail(review({ usage_incomplete: true, usage_interruptions: 2 }))
    expect(await screen.findByText('单节点最多中断恢复 2 次')).toBeInTheDocument()
    expect(screen.getByText('用量不完整')).toBeInTheDocument()
  })

  it('shows nothing when usage is complete and never interrupted', async () => {
    renderDetail(review({ usage_incomplete: false }))
    await screen.findByText('用量')
    expect(screen.queryByText(/中断恢复/)).not.toBeInTheDocument()
    expect(screen.queryByText('用量不完整')).not.toBeInTheDocument()
  })
})

describe('model card params summary (#346)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('labels explicit zeros and false, skips unset defaults, and never shows price', () => {
    expect(summarizeParams({
      max_tokens: 64000, context_window: 0, temperature: 0, forced_tool_choice: '',
      strict_role_alternation: false, rpm: 0, max_concurrency: 4,
      reasoning: { effort: 'high' }, thinking: { budget_tokens: 2048 },
      price: { input_per_mtok: 3, output_per_mtok: 15 },
    })).toEqual([
      ['max_tokens', '64000'], ['context_window', '0（未知）'], ['temperature', '0'],
      ['strict_role_alternation', 'false（关闭）'], ['max_concurrency', '4'],
      ['reasoning.effort', 'high'], ['thinking.budget_tokens', '2048'],
    ])
    expect(summarizeParams({ temperature: null as unknown as undefined, reasoning: null as never, thinking: null as never, price: null as never })).toEqual([])
    expect(summarizeParams({ price: { input_per_mtok: 0, output_per_mtok: 0 } })).toEqual([])
    expect(summarizeParams({ max_tokens: 0, context_window: 0, max_concurrency: 0, strict_role_alternation: false })).toEqual([
      ['max_tokens', '0（模型默认，不发送）'], ['context_window', '0（未知）'],
      ['max_concurrency', '0（不限制本地并发）'], ['strict_role_alternation', 'false（关闭）'],
    ])
  })

  it('renders readable key/value pairs instead of raw JSON', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/models')) {
        return jsonResponse({ models: [{
          id: 'user:aaaaaaaa-1111', display_name: 'card', source: 'user', read_only: false,
          protocol: 'openai-chat', base_url: 'https://example.com/v1', model: 'm',
          params: { max_tokens: 64000, context_window: 0, reasoning: { effort: 'high' } },
          has_api_key: true, available: true, revision: 1, referenced_by: [],
        }] })
      }
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    const group = await screen.findByRole('group', { name: 'card 参数' })
    expect(group).toHaveTextContent('max_tokens 64000')
    expect(group).toHaveTextContent('context_window 0（未知）')
    expect(group).toHaveTextContent('reasoning.effort high')
    expect(group).not.toHaveTextContent('{')
  })

  it('leaves temperature empty in the edit form when the API returns null', async () => {
    const user = userEvent.setup()
    installFetch((url) => {
      if (url.startsWith('/api/v1/models')) {
        return jsonResponse({ models: [{
          id: 'user:aaaaaaaa-1111', display_name: 'card', source: 'user', read_only: false,
          protocol: 'openai-chat', base_url: 'https://example.com/v1', model: 'm',
          params: { max_tokens: 100, temperature: null, max_concurrency: 1 },
          has_api_key: true, available: true, revision: 1, referenced_by: [],
        }] })
      }
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '编辑' }))
    expect(screen.getByLabelText('temperature')).toHaveValue(null)
  })
})
