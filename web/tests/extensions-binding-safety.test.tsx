import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { setCsrfToken } from '../src/api/client'
import type { BlockBinding } from '../src/api/types'
import { binding, revisionToken, skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

type BindingPage = { bindings: BlockBinding[]; next_cursor?: string }
type PageSource = BindingPage | Response | (() => BindingPage | Response | Promise<BindingPage | Response>)
type PutReply = (block: string, body: Record<string, unknown>, attempt: number) => Response | Promise<Response>

function setup({
  pages, put, skillNames = ['legacy-skill', 'new-skill'],
}: {
  pages: Record<string, PageSource>
  put: PutReply
  skillNames?: string[]
}) {
  const reads: string[] = []
  const writes: Array<{ block: string; body: Record<string, unknown> }> = []
  const handler = async (url: string, init?: RequestInit) => {
    const parsed = new URL(url, 'http://localhost')
    const method = (init?.method || 'GET').toUpperCase()
    if (parsed.pathname === '/api/v1/block-bindings' && method === 'GET') {
      const cursor = parsed.searchParams.get('cursor') || ''
      reads.push(cursor)
      const source = pages[cursor]
      if (!source) return jsonResponse({ error: { code: 'internal', message: 'unmapped page' } }, 500)
      const value = typeof source === 'function' ? await source() : await source
      return value instanceof Response ? value : jsonResponse(value)
    }
    if (parsed.pathname.startsWith('/api/v1/block-bindings/') && method === 'PUT') {
      const block = decodeURIComponent(parsed.pathname.split('/').pop() || '')
      const body = JSON.parse(String(init?.body)) as Record<string, unknown>
      const attempt = writes.filter((write) => write.block === block).length + 1
      writes.push({ block, body })
      return put(block, body, attempt)
    }
    if (parsed.pathname === '/api/v1/skills') {
      return jsonResponse({ skills: skillNames.map((name, index) => skill({ id: `k-${index}`, name })) })
    }
    if (parsed.pathname === '/api/v1/mcp-servers') return jsonResponse({ servers: [] })
    return jsonResponse({})
  }
  installFetch(handler)
  return { reads, writes }
}

function renderBindings() {
  return renderWithProviders(<ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />)
}

const savedPut = (): PutReply => (block, body) => jsonResponse(binding({
  block,
  mcp_tools: body.mcp_tools as string[],
  skills: body.skills as string[],
  revision: revisionToken('saved'),
}))

afterEach(() => {
  vi.unstubAllGlobals()
  setCsrfToken(null)
})

describe('binding pagination safety', () => {
  it('does not edit or save until every binding page is loaded', async () => {
    const firstPage = Array.from({ length: 50 }, (_, index) => binding({
      block: `block-${index}`,
      revision: revisionToken(`block-${index}`),
    }))
    const review = binding({
      block: 'review',
      mcp_tools: ['mcp__legacy__scan'],
      skills: ['legacy-skill'],
      revision: revisionToken('review-2'),
    })
    const state = setup({
      pages: {
        '': { bindings: firstPage, next_cursor: 'page-2' },
        'page-2': { bindings: [review] },
      },
      put: savedPut(),
    })
    const user = userEvent.setup()
    renderBindings()

    const newSkill = await screen.findByRole('button', { name: 'review 使用 Skill new-skill' })
    expect(screen.getByRole('button', { name: '加载更多绑定' })).toBeInTheDocument()
    expect(newSkill).toBeDisabled()
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeDisabled()
    expect(screen.getByText(/绑定列表尚未全部加载/)).toBeInTheDocument()

    await user.click(newSkill)
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    expect(state.writes).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: '加载更多绑定' }))
    expect(await screen.findByRole('button', { name: 'review 使用 Skill legacy-skill' })).toHaveAttribute('aria-pressed', 'true')
    await waitFor(() => expect(screen.getByRole('button', { name: 'review 使用 Skill new-skill' })).toBeEnabled())
    expect(screen.queryByText(/绑定列表尚未全部加载/)).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill new-skill' }))
    const save = screen.getByRole('button', { name: '保存绑定' })
    await waitFor(() => expect(save).toBeEnabled())
    await user.click(save)
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(state.writes[0]).toEqual({
      block: 'review',
      body: {
        scope: 'platform',
        mcp_tools: ['mcp__legacy__scan'],
        skills: ['legacy-skill', 'new-skill'],
        expected_revision: revisionToken('review-2'),
      },
    })
  })

  it('blocks edits and saves with a retry action when the next page fails', async () => {
    const review = binding({ block: 'review', skills: ['legacy-skill'], revision: revisionToken('review-1') })
    let pageFailures = 1
    const state = setup({
      pages: {
        '': { bindings: [review], next_cursor: 'page-2' },
        'page-2': () => {
          if (pageFailures > 0) {
            pageFailures -= 1
            return jsonResponse({ error: { code: 'internal', message: 'page read failed' } }, 500)
          }
          return { bindings: [binding({ block: 'reasoner', revision: revisionToken('reasoner-1') })] }
        },
      },
      put: savedPut(),
    })
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '加载更多绑定' }))
    expect(await screen.findByText('读取更多绑定失败')).toBeInTheDocument()
    expect(screen.getByText(/绑定列表未全部加载/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill legacy-skill' })).not.toBeInTheDocument()
    expect(state.writes).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByRole('button', { name: 'review 使用 Skill legacy-skill' })).toBeEnabled()
    expect(screen.queryByText('读取更多绑定失败')).not.toBeInTheDocument()
  })
})

describe('binding revision conflicts', () => {
  it('reloads every page after a 409 without retrying the write', async () => {
    const reviewV1 = binding({ block: 'review', skills: ['legacy-skill'], revision: revisionToken('review-1') })
    const reasoner = binding({ block: 'reasoner', skills: ['reasoner-skill'], revision: revisionToken('reasoner-1') })
    let review = reviewV1
    const state = setup({
      pages: {
        '': () => ({ bindings: [review, reasoner], next_cursor: 'page-2' }),
        'page-2': () => ({ bindings: [binding({ block: 'synthesize', revision: revisionToken('synthesize-1') })] }),
      },
      put: (block) => {
        if (block === 'review') {
          review = binding({ block: 'review', skills: ['remote-skill'], revision: revisionToken('review-2') })
          return jsonResponse({ error: { code: 'conflict', message: 'stale revision' } }, 409)
        }
        return jsonResponse(binding({ block, revision: revisionToken('conflict-updated') }))
      },
    })
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '加载更多绑定' }))
    await screen.findByRole('button', { name: 'reasoner 使用 Skill reasoner-skill' })
    await user.click(screen.getByRole('button', { name: 'review 使用 Skill new-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('检测到 1 个块')
    expect(state.writes).toHaveLength(1)
    expect(state.reads.slice(-2)).toEqual(['', 'page-2'])
    expect(state.reads.filter((cursor) => cursor === '').length).toBeGreaterThanOrEqual(2)
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'review 使用 Skill legacy-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill new-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill remote-skill' })).toHaveAttribute('aria-pressed', 'false')
  })

  it('counts a conflict and an error together without retrying or losing drafts', async () => {
    let review = binding({ block: 'review', skills: ['legacy-skill'], revision: revisionToken('review-1') })
    const reasoner = binding({ block: 'reasoner', skills: ['reasoner-skill'], revision: revisionToken('reasoner-1') })
    const state = setup({
      pages: {
        '': () => ({ bindings: [review, reasoner], next_cursor: 'page-2' }),
        'page-2': () => ({ bindings: [binding({ block: 'synthesize', revision: revisionToken('synthesize-1') })] }),
      },
      put: (block) => {
        if (block === 'review') {
          review = binding({ block: 'review', skills: ['remote-skill'], revision: revisionToken('review-2') })
          return jsonResponse({ error: { code: 'conflict', message: 'stale revision' } }, 409)
        }
        return jsonResponse({ error: { code: 'internal', message: 'reasoner write failed' } }, 500)
      },
    })
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '加载更多绑定' }))
    await screen.findByRole('button', { name: 'reasoner 使用 Skill reasoner-skill' })
    await user.click(screen.getByRole('button', { name: 'review 使用 Skill new-skill' }))
    await user.click(screen.getByRole('button', { name: 'reasoner 使用 Skill new-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))

    await waitFor(() => expect(state.writes).toHaveLength(2))
    // Both requests failed, so a conflict must not be omitted from the count.
    expect(await screen.findByText('部分绑定未保存（2/2 个请求失败）。')).toBeInTheDocument()
    // The non-conflict error detail stays visible.
    expect(await screen.findByText('reasoner write failed')).toBeInTheDocument()
    // The 409 refreshes every page and never retries a write.
    await screen.findByText(/检测到 1 个块/)
    expect(state.reads.slice(-2)).toEqual(['', 'page-2'])
    expect(state.reads.filter((cursor) => cursor === '').length).toBeGreaterThanOrEqual(2)
    expect(state.writes).toHaveLength(2)
    // Only review conflicts; reasoner keeps its unsaved draft; review keeps its
    // draft next to the remote value.
    expect(screen.getAllByText('冲突')).toHaveLength(1)
    expect(screen.getByRole('button', { name: 'review 使用 Skill legacy-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill new-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'review 使用 Skill remote-skill' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: 'reasoner 使用 Skill reasoner-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'reasoner 使用 Skill new-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeDisabled()
  })

  it('reloads every page when every submitted binding conflicts', async () => {
    let review = binding({ block: 'review', skills: ['legacy-skill'], revision: revisionToken('review-1') })
    let reasoner = binding({ block: 'reasoner', skills: ['reasoner-skill'], revision: revisionToken('reasoner-1') })
    const state = setup({
      pages: {
        '': () => ({ bindings: [review, reasoner], next_cursor: 'page-2' }),
        'page-2': () => ({ bindings: [binding({ block: 'synthesize', revision: revisionToken('synthesize-1') })] }),
      },
      put: (block) => {
        if (block === 'review') {
          review = binding({ block: 'review', skills: ['remote-review'], revision: revisionToken('review-2') })
        } else {
          reasoner = binding({ block: 'reasoner', skills: ['remote-reasoner'], revision: revisionToken('reasoner-2') })
        }
        return jsonResponse({ error: { code: 'conflict', message: 'stale revision' } }, 409)
      },
    })
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '加载更多绑定' }))
    await screen.findByRole('button', { name: 'reasoner 使用 Skill reasoner-skill' })
    await user.click(screen.getByRole('button', { name: 'review 使用 Skill new-skill' }))
    await user.click(screen.getByRole('button', { name: 'reasoner 使用 Skill new-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('检测到 2 个块')
    expect(state.writes).toHaveLength(2)
    expect(state.reads.slice(-2)).toEqual(['', 'page-2'])
    expect(state.reads.filter((cursor) => cursor === '').length).toBeGreaterThanOrEqual(2)
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeDisabled()
  })

  it('stays locked when the conflict refresh fails and retries only the read', async () => {
    const reviewV1 = binding({ block: 'review', skills: ['legacy-skill'], revision: revisionToken('review-1') })
    let review = reviewV1
    let failNextFirstPage = false
    const state = setup({
      pages: {
        '': () => {
          if (failNextFirstPage) {
            failNextFirstPage = false
            return jsonResponse({ error: { code: 'internal', message: 'refresh failed' } }, 500)
          }
          return { bindings: [review], next_cursor: 'page-2' }
        },
        'page-2': () => ({ bindings: [binding({ block: 'reasoner', revision: revisionToken('reasoner-1') })] }),
      },
      put: () => {
        review = binding({ block: 'review', skills: ['remote-skill'], revision: revisionToken('review-2') })
        failNextFirstPage = true
        return jsonResponse({ error: { code: 'conflict', message: 'stale revision' } }, 409)
      },
    })
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '加载更多绑定' }))
    await screen.findByRole('button', { name: 'reasoner 使用 Skill reasoner-skill' }).catch(() => undefined)
    await user.click(screen.getByRole('button', { name: 'review 使用 Skill new-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))

    expect(await screen.findByText('绑定版本已过期，读取最新绑定失败')).toBeInTheDocument()
    expect(screen.getByText(/不会自动重试/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill legacy-skill' })).not.toBeInTheDocument()
    expect(state.writes).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('检测到 1 个块')
    expect(state.writes).toHaveLength(1)
  })

  it('refreshes all pages when every request conflicts, then uses the new revision', async () => {
    let review = binding({ block: 'review', skills: ['legacy-skill'], revision: revisionToken('review-1') })
    const state = setup({
      pages: {
        '': () => ({ bindings: [review] }),
      },
      put: (block, body, attempt) => {
        if (attempt === 1) {
          review = binding({
            block: 'review',
            skills: body.skills as string[],
            revision: revisionToken('review-2'),
          })
          return jsonResponse({ error: { code: 'conflict', message: 'stale revision' } }, 409)
        }
        review = binding({ block, skills: body.skills as string[], revision: revisionToken('review-3') })
        return jsonResponse(review)
      },
    })
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill new-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    await waitFor(() => expect(screen.getByRole('button', { name: 'review 使用 Skill legacy-skill' })).toHaveAttribute('aria-pressed', 'true'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(state.reads.filter((cursor) => cursor === '').length).toBeGreaterThanOrEqual(2)

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill legacy-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(2))
    expect(state.writes[1]).toEqual({
      block: 'review',
      body: {
        scope: 'platform',
        mcp_tools: [],
        skills: ['new-skill'],
        expected_revision: revisionToken('review-2'),
      },
    })
  })

  it.each([
    ['missing', undefined],
    ['empty', ''],
    ['not-a-string', 42],
    ['null', null],
    ['an array', []],
    ['the absent sentinel', 'absent'],
  ])('fails closed when a binding revision is %s', async (_label, revision) => {
    const malformed: Record<string, unknown> = {
      ...binding({ block: 'review', skills: ['legacy-skill'], revision: revisionToken('review-1') }),
    }
    if (revision === undefined) delete malformed.revision
    else malformed.revision = revision
    const state = setup({
      pages: { '': { bindings: [malformed as unknown as BlockBinding] } },
      put: savedPut(),
    })
    renderBindings()

    expect(await screen.findByText(/绑定版本信息缺失或格式错误/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '保存绑定' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'review 使用 Skill legacy-skill' })).not.toBeInTheDocument()
    expect(state.writes).toHaveLength(0)
  })
})

describe('binding creation overwrite confirmation', () => {
  it('asks before creating absent rows with a full override', async () => {
    let current = [binding({ block: 'code-review', revision: revisionToken('code-review-1') })]
    const state = setup({
      pages: { '': () => ({ bindings: current }) },
      put: (block, body) => {
        const created = binding({
          block,
          mcp_tools: body.mcp_tools as string[],
          skills: body.skills as string[],
          revision: revisionToken('created'),
        })
        current = [...current, created]
        return jsonResponse(created)
      },
    })
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill new-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))

    const dialog = await screen.findByRole('dialog', { name: '新建绑定会覆盖继承授权？' })
    expect(within(dialog).getByText(/将新建 review 的绑定/)).toBeInTheDocument()
    expect(within(dialog).getByText(/完整覆盖/)).toBeInTheDocument()
    expect(within(dialog).getByText(/未选中的继承授权或配置文件授权不会保留/)).toBeInTheDocument()
    expect(state.writes).toHaveLength(0)

    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'review 使用 Skill new-skill' })).toHaveAttribute('aria-pressed', 'true')
    expect(state.writes).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    const confirm = await screen.findByRole('dialog', { name: '新建绑定会覆盖继承授权？' })
    await user.click(within(confirm).getByRole('button', { name: '新建并保存' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(state.writes[0]).toEqual({
      block: 'review',
      body: {
        scope: 'platform',
        mcp_tools: [],
        skills: ['new-skill'],
        expected_revision: 'absent',
      },
    })
  })
})
