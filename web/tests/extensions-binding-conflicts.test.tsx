import { describe, expect, it } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient } from '@tanstack/react-query'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import type { BlockBinding, Skill } from '../src/api/types'
import { binding, skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

type ServerPage = { bindings: BlockBinding[]; next_cursor?: string }
type Write = { url: string; method: string; body: Record<string, unknown> }

const skills: Skill[] = [
  skill({ id: 'k-code', name: 'code-review' }),
  skill({ id: 'k-team', name: 'team-rules' }),
  skill({ id: 'k-reasoner', name: 'reasoner-rules' }),
  skill({ id: 'k-extra', name: 'reasoner-extra' }),
  skill({ id: 'k-another', name: 'another-skill' }),
]

const v1 = [
  binding({ block: 'review', skills: ['code-review', 'team-rules'] }),
  binding({ block: 'reasoner', skills: ['reasoner-rules'] }),
]

function setup({
  bindings, nextCursor, deferBlockPut,
}: { bindings: BlockBinding[]; nextCursor?: string; deferBlockPut?: string }) {
  let page: ServerPage = { bindings, ...(nextCursor ? { next_cursor: nextCursor } : {}) }
  const writes: Write[] = []
  let deferNext = deferBlockPut
  let releaseDeferredPut: ((ok: boolean) => void) | undefined
  const applyPut = (block: string, body: Record<string, unknown>) => {
    const next = {
      ...(page.bindings.find((item) => item.block === block) || binding({ block })),
      ...body,
    } as BlockBinding
    page = {
      ...page,
      bindings: page.bindings.some((item) => item.block === block)
        ? page.bindings.map((item) => (item.block === block ? next : item))
        : [...page.bindings, next],
    }
    return next
  }
  const handler = (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    const body = init?.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined
    if (url.startsWith('/api/v1/block-bindings') && method === 'PUT' && body) {
      const block = decodeURIComponent(url.split('/').pop() || '')
      writes.push({ url, method, body })
      if (deferNext === block) {
        deferNext = undefined
        return new Promise<Response>((resolve) => {
          releaseDeferredPut = (ok) => resolve(ok
            ? jsonResponse(applyPut(block, body))
            : jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500))
        })
      }
      return jsonResponse(applyPut(block, body))
    }
    if (url.startsWith('/api/v1/block-bindings')) {
      return jsonResponse({ bindings: page.bindings, ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) })
    }
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills })
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
    return jsonResponse({})
  }

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: Infinity } },
  })
  queryClient.setQueryData(['block-bindings', 'platform', undefined], {
    pages: [{ bindings, ...(nextCursor ? { next_cursor: nextCursor } : {}) }],
    pageParams: [undefined],
  })
  queryClient.setQueryData(['skills', 'platform', undefined], { pages: [{ skills }], pageParams: [undefined] })
  queryClient.setQueryData(['mcp-servers', 'platform', undefined], { pages: [{ servers: [] }], pageParams: [undefined] })

  return {
    queryClient,
    handler,
    writes,
    releaseDeferredPut: (ok: boolean) => releaseDeferredPut?.(ok),
    setServer: (next: ServerPage) => {
      page = next
      queryClient.setQueryData(['block-bindings', 'platform', undefined], {
        pages: [next],
        pageParams: [undefined],
      })
    },
  }
}

function renderBindings(state: ReturnType<typeof setup>) {
  installFetch(state.handler)
  return renderWithProviders(
    <ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />,
    { queryClient: state.queryClient },
  )
}

const skillButton = (block: string, name: string) => screen.getByRole('button', { name: `${block} 使用 Skill ${name}` })

describe('binding draft conflicts', () => {
  it('marks a two-sided change as a conflict and blocks save and restore', async () => {
    const state = setup({ bindings: v1 })
    renderBindings(state)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill team-rules' }))
    state.setServer({
      bindings: [
        binding({ block: 'review', skills: ['team-rules'] }),
        binding({ block: 'reasoner', skills: ['reasoner-rules'] }),
      ],
    })

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('检测到 1 个块')
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeDisabled()
    expect(screen.getByText('冲突')).toBeInTheDocument()
    const resolveReview = screen.getByRole('button', { name: '放弃 review 的本地更改并使用最新绑定' })
    expect(resolveReview).toBeInTheDocument()
    expect(screen.getByRole('table')).not.toContainElement(resolveReview)
    expect(screen.getByRole('alert').parentElement).toContainElement(resolveReview)
    expect(resolveReview).toHaveTextContent('放弃 review 的本地更改并使用最新绑定')
    expect(skillButton('review', 'code-review')).toHaveAttribute('aria-pressed', 'true')
    expect(skillButton('review', 'team-rules')).toHaveAttribute('aria-pressed', 'false')
    expect(state.writes).toHaveLength(0)

    // Refetching the same V2 must not silently clear the conflict.
    state.setServer({
      bindings: [
        binding({ block: 'review', skills: ['team-rules'] }),
        binding({ block: 'reasoner', skills: ['reasoner-rules'] }),
      ],
    })
    expect(screen.getByRole('alert')).toHaveTextContent('检测到 1 个块')
    expect(screen.getByRole('button', { name: '放弃 review 的本地更改并使用最新绑定' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '放弃 review 的本地更改并使用最新绑定' }))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(skillButton('review', 'code-review')).toHaveAttribute('aria-pressed', 'false')
    expect(skillButton('review', 'team-rules')).toHaveAttribute('aria-pressed', 'true')

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill another-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(state.writes[0]).toMatchObject({
      url: '/api/v1/block-bindings/review',
      method: 'PUT',
      body: { skills: ['team-rules', 'another-skill'] },
    })
  })


  it('does not advance a baseline past a remote update received while the save is in flight', async () => {
    const state = setup({ bindings: v1, deferBlockPut: 'reasoner' })
    renderBindings(state)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill team-rules' }))
    await user.click(screen.getByRole('button', { name: 'reasoner 使用 Skill reasoner-extra' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))

    await waitFor(() => expect(state.writes).toHaveLength(2))
    // Another admin restores review to [] while the review PUT has succeeded
    // and the reasoner PUT is still pending.
    state.setServer({
      bindings: [
        binding({ block: 'review', skills: [] }),
        binding({ block: 'reasoner', skills: ['reasoner-rules'] }),
      ],
    })
    expect(await screen.findByRole('alert')).toHaveTextContent('检测到 1 个块')

    await act(async () => { state.releaseDeferredPut(false) })
    expect(await screen.findByText('部分绑定未保存（1/2 个请求失败）。')).toBeInTheDocument()
    expect(screen.getByText(/检测到 1 个块/)).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '放弃 review 的本地更改并使用最新绑定' }))
    expect(skillButton('review', 'code-review')).toHaveAttribute('aria-pressed', 'false')
    expect(skillButton('review', 'team-rules')).toHaveAttribute('aria-pressed', 'false')
    expect(skillButton('reasoner', 'reasoner-extra')).toHaveAttribute('aria-pressed', 'true')

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill another-skill' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes.length).toBeGreaterThanOrEqual(4))
    const reviewWrites = state.writes.filter((write) => write.url.endsWith('/review'))
    expect(reviewWrites[reviewWrites.length - 1].body).toMatchObject({ skills: ['another-skill'] })
    expect(reviewWrites.some((write) => (write.body.skills as string[]).includes('code-review'))).toBe(true)
  })

  it('discards only the conflicted block and keeps unrelated drafts', async () => {
    const state = setup({ bindings: v1 })
    renderBindings(state)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill team-rules' }))
    await user.click(screen.getByRole('button', { name: 'reasoner 使用 Skill reasoner-extra' }))
    state.setServer({
      bindings: [
        binding({ block: 'review', skills: ['team-rules'] }),
        binding({ block: 'reasoner', skills: ['reasoner-rules'] }),
      ],
    })

    expect(await screen.findByRole('alert')).toHaveTextContent('检测到 1 个块')
    expect(skillButton('reasoner', 'reasoner-extra')).toHaveAttribute('aria-pressed', 'true')

    await user.click(screen.getByRole('button', { name: '放弃 review 的本地更改并使用最新绑定' }))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(skillButton('review', 'code-review')).toHaveAttribute('aria-pressed', 'false')
    expect(skillButton('review', 'team-rules')).toHaveAttribute('aria-pressed', 'true')
    expect(skillButton('reasoner', 'reasoner-extra')).toHaveAttribute('aria-pressed', 'true')

    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(1))
    expect(state.writes[0].url).toBe('/api/v1/block-bindings/reasoner')
    expect(state.writes[0].body).toMatchObject({ skills: ['reasoner-rules', 'reasoner-extra'] })
    expect(state.writes.some((write) => write.url.endsWith('/review'))).toBe(false)
  })

  it('does not conflict when the remote value already equals the local target', async () => {
    const state = setup({ bindings: v1 })
    renderBindings(state)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill team-rules' }))
    state.setServer({ bindings: [binding({ block: 'review', skills: ['code-review'] }), v1[1]] })

    await waitFor(() => expect(skillButton('review', 'code-review')).toHaveAttribute('aria-pressed', 'true'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText('冲突')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeEnabled()
  })

  it('does not treat a block missing from a partial page as a deletion', async () => {
    const state = setup({ bindings: [v1[0]], nextCursor: 'page-2' })
    renderBindings(state)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'reasoner 使用 Skill reasoner-extra' }))
    state.setServer({ bindings: [binding({ block: 'review', skills: ['team-rules'] })], next_cursor: 'page-2' })

    await waitFor(() => expect(screen.getByRole('button', { name: '加载更多绑定' })).toBeInTheDocument())
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(skillButton('review', 'team-rules')).toHaveAttribute('aria-pressed', 'true')
    expect(skillButton('reasoner', 'reasoner-extra')).toHaveAttribute('aria-pressed', 'true')
  })

  it('treats a missing block as a deletion conflict after every page is loaded', async () => {
    const state = setup({ bindings: v1 })
    renderBindings(state)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill team-rules' }))
    state.setServer({ bindings: [v1[1]] })

    expect(await screen.findByRole('alert')).toHaveTextContent('检测到 1 个块')
    expect(screen.getByRole('button', { name: '保存绑定' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '放弃 review 的本地更改并使用最新绑定' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '放弃 review 的本地更改并使用最新绑定' }))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(skillButton('review', 'code-review')).toHaveAttribute('aria-pressed', 'false')
    expect(skillButton('review', 'team-rules')).toHaveAttribute('aria-pressed', 'false')
  })
})


function setupPostSave({ resetAfterPut, failFirstGet }: { resetAfterPut?: boolean; failFirstGet?: boolean } = {}) {
  let current = [binding({ block: 'review', skills: [] as string[] })]
  let failNextGet = Boolean(failFirstGet)
  const writes: Write[] = []
  const handler = (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    const body = init?.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined
    if (url.startsWith('/api/v1/block-bindings') && method === 'PUT' && body) {
      writes.push({ url, method, body })
      current = [binding({ block: 'review', skills: body.skills as string[] })]
      if (resetAfterPut) current = [binding({ block: 'review', skills: [] })]
      return jsonResponse(current[0])
    }
    if (url.startsWith('/api/v1/block-bindings')) {
      if (failNextGet) {
        failNextGet = false
        return jsonResponse({ error: { code: 'internal', message: 'read failed' } }, 500)
      }
      return jsonResponse({ bindings: current })
    }
    if (url.startsWith('/api/v1/skills')) {
      return jsonResponse({ skills: [
        skill({ id: 'k-code', name: 'code-review' }),
        skill({ id: 'k-team', name: 'team-rules' }),
      ] })
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
    return jsonResponse({})
  }
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: Infinity } },
  })
  queryClient.setQueryData(['block-bindings', 'platform', undefined], {
    pages: [{ bindings: current }],
    pageParams: [undefined],
  })
  queryClient.setQueryData(['skills', 'platform', undefined], {
    pages: [{ skills: [skill({ id: 'k-code', name: 'code-review' }), skill({ id: 'k-team', name: 'team-rules' })] }],
    pageParams: [undefined],
  })
  queryClient.setQueryData(['mcp-servers', 'platform', undefined], { pages: [{ servers: [] }], pageParams: [undefined] })
  return { queryClient, handler, writes }
}

describe('saved binding read-back', () => {
  it('applies an identical JSON read-back after save instead of trusting the old cache', async () => {
    const state = setupPostSave({ resetAfterPut: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderWithProviders(
      <ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />,
      { queryClient: state.queryClient },
    )

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill code-review' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(screen.getByRole('button', { name: '保存绑定' })).toBeEnabled())

    expect(skillButton('review', 'code-review')).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill team-rules' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes).toHaveLength(2))
    expect(state.writes[1].body).toMatchObject({ skills: ['team-rules'] })
  })

  it('shows a read-back failure without claiming the save failed and retries only the read', async () => {
    const state = setupPostSave({ failFirstGet: true })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderWithProviders(
      <ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />,
      { queryClient: state.queryClient },
    )

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill code-review' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))

    expect(await screen.findByText('绑定已保存，但读取最新绑定失败')).toBeInTheDocument()
    expect(screen.queryByText('保存绑定失败')).not.toBeInTheDocument()
    expect(state.writes).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByRole('button', { name: 'review 使用 Skill code-review' })).toHaveAttribute('aria-pressed', 'true')
    expect(state.writes).toHaveLength(1)
  })
})
