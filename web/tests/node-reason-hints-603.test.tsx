import { afterEach, describe, expect, it, vi } from 'vitest'
import { existsSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { screen } from '@testing-library/react'
import type { GraphNode } from '../src/api/types'
import { NodeDrawer } from '../src/features/review/NodeDrawer'
import { describeNodeReason } from '../src/lib/diagnostics'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

// The generic sentence for a reason without a hint.
const GENERIC = /^节点未完成（原因代码：/

describe('node reason hints (#603)', () => {
  it('explains unsafe_model_response and keeps the code in parentheses', () => {
    expect(describeNodeReason('unsafe_model_response', 'failed')).toBe(
      '模型返回了无法安全执行的响应（例如工具调用参数不是合法 JSON），节点结果未被采用。（unsafe_model_response）',
    )
  })

  // Each reason below is persisted on a node by the server: the DAG engine
  // (pkg/dag), the model provider kinds (internal/llm), the review run
  // outcomes (internal/review) and the cancel reasons of internal/app.
  const persisted: Record<string, string[]> = {
    'engine failures and skips': [
      'required_node_failed', 'unsatisfied', 'deps_skipped', 'condition_false', 'budget_exhausted',
      'queue_timeout', 'timeout', 'executor_error', 'invalid_output', 'expand_failed', 'panic',
    ],
    'model provider and agent': [
      'rate_limited', 'overloaded', 'context_too_long', 'auth', 'bad_request', 'unsafe_model_response',
      'transient', 'egress_denied',
    ],
    'review run outcomes': ['no_submit', 'invalid_submit', 'sandbox_denied'],
    'cancellation': [
      'canceled', 'cancelled', 'user_cancelled', 'superseded', 'deadline', 'context_cancelled', 'workflow_cancelled',
      'workspace material identity unavailable; rerun review',
      'repository skill provenance unavailable or unsupported; rerun review',
      'repository or stored ignore configuration is invalid',
    ],
    'states that carry a reason': ['ready', 'recovered'],
  }
  for (const [group, reasons] of Object.entries(persisted)) {
    it.each(reasons)(`${group}: %s has a Chinese hint and keeps the code`, (reason) => {
      const text = describeNodeReason(reason, 'failed')
      expect(text).not.toMatch(GENERIC)
      expect(text).toMatch(/[一-鿿]/)
      expect(text?.endsWith(`（${reason}）`)).toBe(true)
    })
  }

  // internal/llm/errors.go is the single list of provider failure kinds. A new
  // kind without a hint would show the generic sentence again.
  const llmErrors = resolve(process.cwd(), '..', 'internal', 'llm', 'errors.go')
  it.skipIf(!existsSync(llmErrors))('has a hint for every LLM error kind of the server', () => {
    const kinds = [...readFileSync(llmErrors, 'utf8').matchAll(/^\s*Err\w+\s+ErrKind\s*=\s*"([a-z_]+)"/gm)].map((match) => match[1])
    expect(kinds).toContain('unsafe_model_response')
    expect(kinds.length).toBeGreaterThanOrEqual(9)
    for (const kind of kinds) {
      expect(describeNodeReason(kind, 'failed'), kind).not.toMatch(GENERIC)
    }
  })

  it('keeps an unknown reason readable and never reads the prototype', () => {
    expect(describeNodeReason('brand_new_class', 'failed')).toBe('节点未完成（原因代码：brand_new_class）。')
    expect(describeNodeReason('constructor', 'failed')).toBe('节点未完成（原因代码：constructor）。')
  })

  it('falls back to the node status when the server sent no reason', () => {
    expect(describeNodeReason(undefined, 'failed')).toBe('节点执行失败，服务端没有返回原因代码。')
    expect(describeNodeReason(undefined, 'skipped')).toBe('该节点已跳过，评审范围不完整。')
    expect(describeNodeReason(undefined, 'completed')).toBeNull()
  })
})

function renderDrawer(node: GraphNode, detail: Record<string, unknown>) {
  installFetch((url) => {
    if (url.includes('/nodes/') && url.endsWith('/calls')) {
      return jsonResponse({ calls: [], next_cursor: '', retention: { details_purged: false } })
    }
    if (url.includes('/nodes/')) return jsonResponse(detail)
    return jsonResponse({})
  })
  return renderWithProviders(<NodeDrawer reviewId="wf-1" node={node} onClose={() => {}} />)
}

describe('node drawer reason (#603)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('explains an unsafe model response instead of showing only the reason code', async () => {
    renderDrawer(
      { id: 'cr.p1', kind: 'code-review', status: 'failed', reason: 'unsafe_model_response' },
      { id: 'cr.p1', kind: 'code-review', status: 'failed', reason: 'unsafe_model_response', error: 'node failed' },
    )
    expect(await screen.findByText(/模型返回了无法安全执行的响应/)).toBeInTheDocument()
    expect(screen.queryByText(/原因代码：unsafe_model_response/)).not.toBeInTheDocument()
  })
})
