import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import type { BlockBinding, Skill } from '../src/api/types'
import { binding, skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const boundSkills: Skill[] = [
  skill({ id: 'k-code', name: 'code-review' }),
  skill({ id: 'k-team', name: 'team-rules' }),
]
const bindings: BlockBinding[] = [binding({ block: 'review', skills: ['code-review', 'team-rules'] })]

function setup() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: Infinity } },
  })
  queryClient.setQueryData(['block-bindings', 'platform', undefined], { pages: [{ bindings }], pageParams: [undefined] })
  queryClient.setQueryData(['skills', 'platform', undefined], { pages: [{ skills: boundSkills }], pageParams: [undefined] })
  queryClient.setQueryData(['mcp-servers', 'platform', undefined], { pages: [{ servers: [] }], pageParams: [undefined] })
  installFetch((url) => {
    if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings })
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills: boundSkills })
    if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
    return jsonResponse({})
  })
  return queryClient
}

function Harness() {
  const [tab, setTab] = useState('bindings')
  return <ExtensionsManager scope="platform" tab={tab} onTabChange={setTab} />
}

describe('unsaved block binding tab switch', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('confirms before discarding edits and keeps them on cancel', async () => {
    const user = userEvent.setup()
    renderWithProviders(<Harness />, { queryClient: setup() })
    const toggle = await screen.findByRole('button', { name: 'review 使用 Skill team-rules' })
    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('tab', { name: /^Skill/ }))
    const dialog = await screen.findByRole('dialog', { name: '放弃未保存的绑定修改？' })
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'review 使用 Skill team-rules' })).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('tab', { name: /^Skill/ }))
    await user.click(await screen.findByRole('button', { name: '放弃并切换' }))
    expect(await screen.findByRole('tab', { name: /^Skill/, selected: true })).toBeInTheDocument()
  })

  it('switches without confirmation when the matrix is clean', async () => {
    const user = userEvent.setup()
    renderWithProviders(<Harness />, { queryClient: setup() })
    await screen.findByRole('button', { name: 'review 使用 Skill team-rules' })
    await user.click(screen.getByRole('tab', { name: /^Skill/ }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(await screen.findByRole('tab', { name: /^Skill/, selected: true })).toBeInTheDocument()
  })
})