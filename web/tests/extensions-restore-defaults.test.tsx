import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { setCsrfToken } from '../src/api/client'
import type { BlockBinding, Skill } from '../src/api/types'
import { binding, skill } from './fixtures'
import { adminMe, installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

type Grant = { mcp_tools: string[]; skills: string[] }
type Change = {
  block: string
  status: 'change' | 'unchanged' | 'skipped'
  reason: string
  add: Grant
  remove: Grant
  ownership_changed: boolean
}
type RestoreResult = { changes: Change[]; preview_token: string; restart_required: boolean }
type RestoreBody = { blocks?: string[]; dry_run: boolean; preview_token?: string }
type Recorded = { url: string; init?: RequestInit; body?: RestoreBody }

const grant = (overrides: Partial<Grant> = {}): Grant => ({ mcp_tools: [], skills: [], ...overrides })
const change = (overrides: Partial<Change> = {}): Change => ({
  block: 'review',
  status: 'change',
  reason: '',
  add: grant(),
  remove: grant(),
  ownership_changed: false,
  ...overrides,
})

const singlePreview: RestoreResult = {
  changes: [change({
    add: grant({
      mcp_tools: ['mcp__codebase-memory-mcp__search_graph_with_a_very_long_tool_name'],
      skills: ['code-review'],
    }),
    remove: grant({ skills: ['old-skill'] }),
    ownership_changed: true,
  })],
  preview_token: 'token-1',
  restart_required: true,
}

function setup({
  initialBindings = [binding({ block: 'review', skills: ['coding-standards'] })],
  preview = () => jsonResponse(singlePreview),
  execute = () => jsonResponse(singlePreview),
  nextCursor,
  nextPage,
  skillNames = [],
  failPut,
}: {
  initialBindings?: BlockBinding[]
  preview?: (body: RestoreBody) => Response | Promise<Response>
  execute?: (body: RestoreBody) => Response | Promise<Response>
  nextCursor?: string
  nextPage?: BlockBinding[] | Promise<BlockBinding[]>
  skillNames?: string[]
  failPut?: (block: string, attempt: number) => boolean
} = {}) {
  let current = initialBindings.map((item) => ({ ...item }))
  const writes: Recorded[] = []
  const putAttempts = new Map<string, number>()
  let reads = 0

  const handler = (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    const body = init?.body ? JSON.parse(String(init.body)) as RestoreBody : undefined
    if (url.startsWith('/api/v1/block-bindings/restore-defaults')) {
      const recorded = { url, init, body }
      writes.push(recorded)
      return body?.dry_run ? preview(body) : execute(body as RestoreBody)
    }
    if (url.startsWith('/api/v1/block-bindings')) {
      if (method === 'GET') {
        reads += 1
        const cursor = new URL(url, 'http://localhost').searchParams.get('cursor')
        if (cursor) {
          if (nextPage instanceof Promise) {
            return nextPage.then((bindings) => jsonResponse({ bindings }))
          }
          return jsonResponse({ bindings: nextPage || [] })
        }
        return jsonResponse({ bindings: current, ...(nextCursor ? { next_cursor: nextCursor } : {}) })
      }
      if (method === 'PUT' && body) {
        writes.push({ url, init, body })
        const block = decodeURIComponent(url.split('/').pop() || '')
        const attempt = (putAttempts.get(block) || 0) + 1
        putAttempts.set(block, attempt)
        if (failPut?.(block, attempt)) {
          return jsonResponse({ error: { code: 'internal', message: `${block} failed` } }, 500)
        }
        const next = {
          ...(current.find((item) => item.block === block) || binding({ block })),
          ...body,
        } as BlockBinding
        current = current.some((item) => item.block === block)
          ? current.map((item) => (item.block === block ? next : item))
          : [...current, next]
        return jsonResponse(next)
      }
    }
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
    if (url.startsWith('/api/v1/skills')) {
      return jsonResponse({ skills: skillNames.map((name, index): Skill => skill({ id: `k-${index}`, name })) })
    }
    return jsonResponse({})
  }

  return {
    handler,
    writes,
    restoreCalls: () => writes.filter((write) => write.url.includes('/restore-defaults')),
    reads: () => reads,
  }
}

function renderBindings() {
  return renderWithProviders(<ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />)
}

function restoreBody(write: Recorded): RestoreBody {
  expect(write.init?.body).toBeTruthy()
  return JSON.parse(String(write.init?.body)) as RestoreBody
}

describe('restore default block bindings', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    setCsrfToken(null)
  })

  it('shows restore controls only to platform administrators', async () => {
    installFetch(setup().handler)
    const admin = renderBindings()
    expect(await screen.findByRole('button', { name: '恢复全部默认' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复 review 的默认绑定' })).toBeInTheDocument()
    admin.unmount()

    installFetch(setup().handler)
    const viewer = renderWithProviders(
      <ExtensionsManager scope="platform" tab="bindings" onTabChange={() => {}} />,
      { me: viewerMe },
    )
    await screen.findByRole('table')
    expect(screen.queryByRole('button', { name: '恢复全部默认' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '恢复 review 的默认绑定' })).not.toBeInTheDocument()
    viewer.unmount()

    installFetch(setup().handler)
    renderWithProviders(
      <ExtensionsManager scope="project" projectId={42} canManage tab="bindings" onTabChange={() => {}} />,
      { me: adminMe },
    )
    await screen.findByRole('table')
    expect(screen.queryByRole('button', { name: '恢复全部默认' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '恢复 review 的默认绑定' })).not.toBeInTheDocument()
  })

  it('previews one block, shows the diff, and writes only after confirmation', async () => {
    const state = setup()
    installFetch(state.handler)
    setCsrfToken('csrf-restore')
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '恢复 review 的默认绑定' }))
    const dialog = await screen.findByRole('dialog', { name: '恢复 review 的默认绑定' })
    expect(await within(dialog).findByRole('region', { name: 'review' })).toBeInTheDocument()
    expect(within(dialog).getByText('code-review')).toBeInTheDocument()
    expect(within(dialog).getByText('old-skill')).toBeInTheDocument()
    expect(within(dialog).getByText('mcp__codebase-memory-mcp__search_graph_with_a_very_long_tool_name')).toBeInTheDocument()
    expect(within(dialog).getByText('新增 Skill')).toBeInTheDocument()
    expect(within(dialog).getByText('删除 Skill')).toBeInTheDocument()
    expect(within(dialog).getByText('新增 MCP 工具')).toBeInTheDocument()
    expect(within(dialog).getByText('恢复后由默认扩展管理。')).toBeInTheDocument()
    expect(within(dialog).getByText('恢复后需要重启服务才能生效。')).toBeInTheDocument()

    expect(state.restoreCalls()).toHaveLength(1)
    expect(restoreBody(state.restoreCalls()[0])).toEqual({ blocks: ['review'], dry_run: true })
    expect(new Headers(state.restoreCalls()[0].init?.headers).get('X-CSRF-Token')).toBe('csrf-restore')

    await user.click(within(dialog).getByRole('button', { name: '确认恢复' }))
    await waitFor(() => expect(state.restoreCalls()).toHaveLength(2))
    expect(restoreBody(state.restoreCalls()[1])).toEqual({
      blocks: ['review'], dry_run: false, preview_token: 'token-1',
    })
    expect(new Headers(state.restoreCalls()[1].init?.headers).get('X-CSRF-Token')).toBe('csrf-restore')
    expect(await screen.findByText('默认绑定已恢复，重启服务后生效。')).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    await waitFor(() => expect(state.reads()).toBeGreaterThanOrEqual(2))
  })

  it('lets the server choose every target block for a full restore', async () => {
    const allPreview: RestoreResult = {
      changes: [
        change({
          add: grant({ skills: ['code-review'] }),
          remove: grant({ skills: ['old-skill'] }),
          ownership_changed: true,
        }),
        change({ block: 'reasoner', status: 'unchanged' }),
        change({ block: 'onboarding', status: 'skipped', reason: 'no_registered_defaults' }),
        change({ block: 'synthesize', ownership_changed: true }),
        change({ block: 'verify', status: 'skipped', reason: 'file_binding' }),
      ],
      preview_token: 'all-token',
      restart_required: true,
    }
    const state = setup({ preview: () => jsonResponse(allPreview), execute: () => jsonResponse(allPreview) })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '恢复全部默认' }))
    const dialog = await screen.findByRole('dialog', { name: '恢复全部默认绑定' })
    expect(await within(dialog).findByRole('region', { name: 'review' })).toBeInTheDocument()
    expect(within(dialog).getByRole('region', { name: 'reasoner' })).toHaveTextContent('无需更改')
    expect(within(dialog).getByRole('region', { name: 'onboarding' })).toHaveTextContent('没有已登记的内置默认，保留现有绑定。')
    expect(within(dialog).getByRole('region', { name: 'verify' })).toHaveTextContent('该块在配置文件中显式声明，不会恢复。')
    const ownership = within(dialog).getByRole('region', { name: 'synthesize' })
    expect(ownership).toHaveTextContent('仅所有权变化')
    expect(ownership).toHaveTextContent('绑定内容不变，恢复后由默认扩展管理。')

    expect(state.restoreCalls()).toHaveLength(1)
    expect(restoreBody(state.restoreCalls()[0])).not.toHaveProperty('blocks')
    expect(restoreBody(state.restoreCalls()[0])).toEqual({ dry_run: true })

    await user.click(within(dialog).getByRole('button', { name: '确认恢复' }))
    await waitFor(() => expect(state.restoreCalls()).toHaveLength(2))
    expect(restoreBody(state.restoreCalls()[1])).not.toHaveProperty('blocks')
    expect(restoreBody(state.restoreCalls()[1])).toEqual({ dry_run: false, preview_token: 'all-token' })
  })

  it('shows a preview error and never writes', async () => {
    const state = setup({
      preview: () => jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500),
    })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '恢复 review 的默认绑定' }))
    const dialog = await screen.findByRole('dialog', { name: '恢复 review 的默认绑定' })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('预览失败')
    expect(within(dialog).getByText('预览失败时不会写入任何绑定。')).toBeInTheDocument()
    expect(within(dialog).queryByRole('button', { name: '确认恢复' })).not.toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '重新预览' })).toBeInTheDocument()

    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(state.restoreCalls()).toHaveLength(1)
    expect(state.restoreCalls().every((write) => restoreBody(write).dry_run)).toBe(true)
  })

  it('closes on Escape without executing a restore', async () => {
    const state = setup()
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '恢复 review 的默认绑定' }))
    expect(await screen.findByRole('dialog', { name: '恢复 review 的默认绑定' })).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(state.restoreCalls()).toHaveLength(1)
    expect(state.restoreCalls().every((write) => restoreBody(write).dry_run)).toBe(true)
  })

  it('keeps the dialog open with pending and error states when execution fails', async () => {
    let finish: ((response: Response) => void) | undefined
    const state = setup({
      execute: () => new Promise<Response>((resolve) => { finish = resolve }),
    })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '恢复 review 的默认绑定' }))
    const dialog = await screen.findByRole('dialog', { name: '恢复 review 的默认绑定' })
    await within(dialog).findByRole('region', { name: 'review' })
    const confirm = within(dialog).getByRole('button', { name: '确认恢复' })
    await user.click(confirm)

    await waitFor(() => expect(confirm).toHaveAttribute('aria-busy', 'true'))
    expect(within(dialog).getByRole('button', { name: '取消' })).toBeDisabled()
    expect(finish).toBeDefined()
    finish?.(jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500))

    expect(await within(dialog).findByRole('alert')).toHaveTextContent('恢复失败')
    expect(within(dialog).getByRole('button', { name: '确认恢复' })).toBeEnabled()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(state.restoreCalls()).toHaveLength(2)
  })

  it('requires a new preview after the token expires without resubmitting automatically', async () => {
    let previews = 0
    const state = setup({
      preview: () => {
        previews += 1
        return jsonResponse({ ...singlePreview, preview_token: previews === 1 ? 'stale-token' : 'fresh-token' })
      },
      execute: () => jsonResponse({ error: { code: 'conflict', message: 'stale preview' } }, 409),
    })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: '恢复 review 的默认绑定' }))
    const dialog = await screen.findByRole('dialog', { name: '恢复 review 的默认绑定' })
    await within(dialog).findByRole('region', { name: 'review' })
    await user.click(within(dialog).getByRole('button', { name: '确认恢复' }))

    expect(await within(dialog).findByText('预览已过期，绑定已变化。请重新预览后再确认。')).toBeInTheDocument()
    expect(state.restoreCalls()).toHaveLength(2)
    expect(within(dialog).queryByRole('button', { name: '确认恢复' })).not.toBeInTheDocument()

    await user.click(within(dialog).getByRole('button', { name: '重新预览' }))
    await waitFor(() => expect(state.restoreCalls()).toHaveLength(3))
    expect(await within(dialog).findByRole('button', { name: '确认恢复' })).toBeInTheDocument()
    expect(restoreBody(state.restoreCalls()[2])).toEqual({ blocks: ['review'], dry_run: true })
    expect(restoreBody(state.restoreCalls()[1]).dry_run).toBe(false)
  })

  it('keeps a draft when the next bindings page arrives with new blocks and references', async () => {
    const state = setup({
      initialBindings: [binding({ block: 'review', skills: ['coding-standards'] })],
      nextCursor: 'page-2',
      nextPage: [binding({ block: 'reasoner', skills: ['reasoner-standards'] })],
    })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    const reviewSkill = await screen.findByRole('button', { name: 'review 使用 Skill coding-standards' })
    await user.click(reviewSkill)
    expect(reviewSkill).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: '加载更多绑定' }))
    const nextPageSkill = await screen.findByRole('button', { name: 'reasoner 使用 Skill reasoner-standards' })
    expect(nextPageSkill).toHaveAttribute('aria-pressed', 'true')

    expect(reviewSkill).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '恢复 review 的默认绑定' })).toBeDisabled()
    expect(screen.getByText('先保存绑定再恢复默认，未保存的更改会丢失。')).toBeInTheDocument()
  })

  it('keeps a draft edited while a page request is still in flight', async () => {
    let resolvePage: ((bindings: BlockBinding[]) => void) | undefined
    const page = new Promise<BlockBinding[]>((resolve) => { resolvePage = resolve })
    const state = setup({
      initialBindings: [binding({ block: 'review', skills: ['coding-standards'] })],
      nextCursor: 'page-2',
      nextPage: page,
    })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    const reviewSkill = await screen.findByRole('button', { name: 'review 使用 Skill coding-standards' })
    await user.click(screen.getByRole('button', { name: '加载更多绑定' }))
    await user.click(reviewSkill)
    expect(reviewSkill).toHaveAttribute('aria-pressed', 'false')

    resolvePage?.([binding({ block: 'reasoner', skills: ['reasoner-standards'] })])
    await screen.findByRole('button', { name: 'reasoner 使用 Skill reasoner-standards' })
    expect(reviewSkill).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeDisabled()
  })

  it('keeps the failed draft and the saved baseline after a partial save refetch', async () => {
    const state = setup({
      initialBindings: [
        binding({ block: 'review', skills: ['coding-standards'] }),
        binding({ block: 'reasoner', skills: ['reasoner-standards'] }),
      ],
      skillNames: ['coding-standards', 'reasoner-standards'],
      failPut: (block, attempt) => block === 'reasoner' && attempt === 1,
    })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await user.click(await screen.findByRole('button', { name: 'review 使用 Skill coding-standards' }))
    await user.click(screen.getByRole('button', { name: 'reasoner 使用 Skill reasoner-standards' }))
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    expect(await screen.findByText('部分绑定未保存（1/2 个请求失败）。')).toBeInTheDocument()
    expect(await screen.findByText('保存绑定失败')).toBeInTheDocument()
    await waitFor(() => expect(state.reads()).toBeGreaterThanOrEqual(2))
    expect(screen.getByRole('button', { name: 'review 使用 Skill coding-standards' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: 'reasoner 使用 Skill reasoner-standards' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeDisabled()
    expect(state.writes.filter((write) => write.url === '/api/v1/block-bindings/review' && write.init?.method === 'PUT')).toHaveLength(1)
    expect(state.writes.filter((write) => write.url === '/api/v1/block-bindings/reasoner' && write.init?.method === 'PUT')).toHaveLength(1)

    const save = screen.getByRole('button', { name: '保存绑定' })
    await waitFor(() => expect(save).toBeEnabled())
    await user.click(save)
    await waitFor(() => expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeEnabled())
    expect(screen.queryByText('先保存绑定再恢复默认，未保存的更改会丢失。')).not.toBeInTheDocument()
    expect(screen.queryByText('保存绑定失败')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'reasoner 使用 Skill reasoner-standards' })).toHaveAttribute('aria-pressed', 'false')
    expect(state.writes.filter((write) => write.url === '/api/v1/block-bindings/review' && write.init?.method === 'PUT')).toHaveLength(1)
    expect(state.writes.filter((write) => write.url === '/api/v1/block-bindings/reasoner' && write.init?.method === 'PUT')).toHaveLength(2)
  })

  it('disables restore while bindings have unsaved changes and re-enables after saving', async () => {
    const state = setup()
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    const all = await screen.findByRole('button', { name: '恢复全部默认' })
    const one = screen.getByRole('button', { name: '恢复 review 的默认绑定' })
    expect(all).toBeEnabled()
    expect(one).toBeEnabled()

    await user.click(screen.getByRole('button', { name: 'review 使用 Skill coding-standards' }))
    expect(await screen.findByText('先保存绑定再恢复默认，未保存的更改会丢失。')).toBeInTheDocument()
    expect(all).toBeDisabled()
    expect(one).toBeDisabled()
    expect(state.restoreCalls()).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: '保存绑定' }))
    await waitFor(() => expect(state.writes.some((write) => write.url === '/api/v1/block-bindings/review')).toBe(true))
    const save = state.writes.find((write) => write.url === '/api/v1/block-bindings/review')
    expect(save?.init?.method).toBe('PUT')
    expect(save?.body).toMatchObject({ scope: 'platform', mcp_tools: [], skills: [] })
    await waitFor(() => expect(screen.getByRole('button', { name: '恢复全部默认' })).toBeEnabled())
    expect(screen.queryByText('先保存绑定再恢复默认，未保存的更改会丢失。')).not.toBeInTheDocument()
    expect(state.restoreCalls()).toHaveLength(0)
  })

  it('offers a restore action for every bindable block even before later pages load', async () => {
    const state = setup({
      initialBindings: [binding({ block: 'review', skills: ['coding-standards'] })],
      nextCursor: 'next-page',
    })
    installFetch(state.handler)
    const user = userEvent.setup()
    renderBindings()

    await screen.findByRole('button', { name: '加载更多绑定' })
    expect(await screen.findByRole('button', { name: '恢复 onboarding 的默认绑定' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复 synthesize 的默认绑定' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '恢复 onboarding 的默认绑定' }))
    await screen.findByRole('dialog', { name: '恢复 onboarding 的默认绑定' })
    expect(restoreBody(state.restoreCalls()[0])).toEqual({ blocks: ['onboarding'], dry_run: true })
  })
})
