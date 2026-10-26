import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

const builtinSkill = skill({ id: 'k-builtin', name: 'code-review', description: 'Two-axis review', builtin: true })
const customSkill = skill({ id: 'k-team', name: 'team-standards', description: 'Team rules' })

function routes(writes: Array<{ url: string; init?: RequestInit }>) {
  let enabled = true
  return (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    if (method !== 'GET') {
      writes.push({ url, init })
      const body = JSON.parse(String(init?.body)) as { enabled: boolean }
      enabled = body.enabled
      return jsonResponse({ ...builtinSkill, enabled })
    }
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills: [{ ...builtinSkill, enabled }, customSkill] })
    if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
    return jsonResponse({ servers: [] })
  }
}

describe('built-in skills in the extensions console', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('marks built-in skills, hides their delete action and toggles them', async () => {
    const writes: Array<{ url: string; init?: RequestInit }> = []
    installFetch(routes(writes))
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    const builtinRow = (await screen.findByText('code-review')).closest('tr') as HTMLElement
    const customRow = screen.getByText('team-standards').closest('tr') as HTMLElement
    expect(within(builtinRow).getByText('内置')).toBeInTheDocument()
    expect(within(builtinRow).queryByRole('button', { name: '删除' })).not.toBeInTheDocument()
    expect(within(customRow).queryByText('内置')).not.toBeInTheDocument()
    expect(within(customRow).getByRole('button', { name: '删除' })).toBeInTheDocument()
    expect(within(builtinRow).getByText('启用')).toBeInTheDocument()

    await userEvent.click(within(builtinRow).getByRole('button', { name: '禁用' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0].url).toBe('/api/v1/skills/k-builtin')
    expect(writes[0].init?.method).toBe('PATCH')
    expect(JSON.parse(String(writes[0].init?.body))).toEqual({ enabled: false })
    expect(await within(builtinRow).findByText('已禁用')).toBeInTheDocument()
    expect(within(builtinRow).getByRole('button', { name: '启用' })).toBeInTheDocument()
  })

  it('shows the marker but no actions to a viewer', async () => {
    installFetch(routes([]))
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />, { me: viewerMe })
    const builtinRow = (await screen.findByText('code-review')).closest('tr') as HTMLElement
    expect(within(builtinRow).getByText('内置')).toBeInTheDocument()
    expect(within(builtinRow).queryByRole('button')).not.toBeInTheDocument()
  })
})
