import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useNavigate, Route, Routes } from 'react-router-dom'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { ProjectDetailPage } from '../src/pages/ProjectDetailPage'
import { setCsrfToken } from '../src/api/client'
import type { BlockBinding } from '../src/api/types'
import { binding, project, skill } from './fixtures'
import {
  createTestQueryClient, installFetch, jsonResponse, renderWithProviders,
} from './test-utils'

const sharedSkill = skill({ id: 'k-shared', name: 'shared' })
const onlyBSkill = skill({ id: 'k-b-only', name: 'b-only' })

function bindingsPage(bindings: BlockBinding[]) {
  return { pages: [{ bindings }], pageParams: [undefined] }
}

function skillsPage() {
  return { pages: [{ skills: [sharedSkill, onlyBSkill] }], pageParams: [undefined] }
}

function seedManagerCache(
  queryClient: ReturnType<typeof createTestQueryClient>,
  projectId: number,
  bindings: BlockBinding[],
) {
  queryClient.setQueryData(['block-bindings', 'project', projectId], bindingsPage(bindings))
  queryClient.setQueryData(['skills', 'project', projectId], skillsPage())
  queryClient.setQueryData(['mcp-servers', 'project', projectId], { pages: [{ servers: [] }], pageParams: [undefined] })
}

describe('bindings reset when the extension scope changes', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    setCsrfToken(null)
  })

  it('drops the previous project draft even when both projects return the same binding snapshot', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/block-bindings')) {
        return jsonResponse({ bindings: [binding({ scope: 'project', block: 'review', skills: ['shared'] })] })
      }
      if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills: [sharedSkill, onlyBSkill] })
      if (url.startsWith('/api/v1/mcp-servers')) return jsonResponse({ servers: [] })
      return jsonResponse({})
    })

    const queryClient = createTestQueryClient()
    // A and B return the same grant content, so the JSON content snapshot does
    // not change. Only the scope identity differs.
    const sameGrant = [binding({ scope: 'project', block: 'review', skills: ['shared'] })]
    seedManagerCache(queryClient, 42, sameGrant)
    seedManagerCache(queryClient, 43, sameGrant)

    let switchProject: (id: number) => void = () => {}
    function Harness() {
      const [projectId, setProjectId] = useState(42)
      switchProject = setProjectId
      return (
        <ExtensionsManager
          scope="project"
          projectId={projectId}
          canManage
          tab="bindings"
          onTabChange={() => {}}
        />
      )
    }

    const user = userEvent.setup()
    renderWithProviders(<Harness />, { queryClient })

    const shared = await screen.findByRole('button', { name: 'review 使用 Skill shared' })
    await user.click(shared)
    expect(shared).toHaveAttribute('aria-pressed', 'false')

    act(() => switchProject(43))
    await waitFor(() => expect(screen.getByRole('button', { name: 'review 使用 Skill shared' })).toHaveAttribute('aria-pressed', 'true'))
    expect(screen.queryByText('先保存绑定再恢复默认，未保存的更改会丢失。')).not.toBeInTheDocument()

    // A refresh after the switch must follow project B, not resurrect A's draft.
    act(() => {
      queryClient.setQueryData(
        ['block-bindings', 'project', 43],
        bindingsPage([binding({ scope: 'project', block: 'review', skills: ['shared', 'b-only'] })]),
      )
    })
    await waitFor(() => expect(screen.getByRole('button', { name: 'review 使用 Skill b-only' })).toHaveAttribute('aria-pressed', 'true'))
    expect(screen.getByRole('button', { name: 'review 使用 Skill shared' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.queryByText('先保存绑定再恢复默认，未保存的更改会丢失。')).not.toBeInTheDocument()
  })
})

describe('project detail extension bindings across cached projects', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    setCsrfToken(null)
  })

  it('shows project B grants and never submits project A drafts to B', async () => {
    const writes: Array<{ url: string; method: string; body?: unknown }> = []
    const aBindings = [binding({ scope: 'project', project_id: 42, block: 'review', skills: ['shared'] })]
    const bBindings = [binding({ scope: 'project', project_id: 43, block: 'review', skills: ['shared'] })]

    installFetch((url, init) => {
      const method = (init?.method || 'GET').toUpperCase()
      const parsed = new URL(url, 'http://localhost')
      if (parsed.pathname.startsWith('/api/v1/block-bindings') && method === 'PUT') {
        writes.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
        return jsonResponse({})
      }
      if (parsed.pathname === '/api/v1/block-bindings') {
        const projectId = Number(parsed.searchParams.get('project_id'))
        return jsonResponse({ bindings: projectId === 43 ? bBindings : aBindings })
      }
      if (parsed.pathname === '/api/v1/mcp-servers') return jsonResponse({ servers: [] })
      if (parsed.pathname === '/api/v1/skills') return jsonResponse({ skills: [sharedSkill, onlyBSkill] })
      if (parsed.pathname.startsWith('/api/v1/projects/')) {
        const id = Number(parsed.pathname.split('/').pop())
        return jsonResponse(project({
          id,
          path_with_namespace: id === 43 ? 'group/frontend' : 'group/backend',
        }))
      }
      if (parsed.pathname === '/api/v1/stats') return jsonResponse({})
      if (parsed.pathname === '/api/v1/reviews') return jsonResponse({ reviews: [] })
      return jsonResponse({})
    })

    const queryClient = createTestQueryClient()
    queryClient.setQueryData(['project', 42], project({ id: 42, path_with_namespace: 'group/backend' }))
    queryClient.setQueryData(['project', 43], project({ id: 43, path_with_namespace: 'group/frontend' }))
    seedManagerCache(queryClient, 42, aBindings)
    seedManagerCache(queryClient, 43, bBindings)

    function ProjectSwitch() {
      const navigate = useNavigate()
      return <button type="button" onClick={() => navigate('/projects/43/extensions')}>打开项目 B</button>
    }

    const user = userEvent.setup()
    renderWithProviders(
      <>
        <ProjectSwitch />
        <Routes>
          <Route path="/projects/:id/:tab" element={<ProjectDetailPage />} />
        </Routes>
      </>,
      { route: '/projects/42/extensions', queryClient },
    )

    await user.click(await screen.findByRole('tab', { name: '块绑定' }))
    const sharedInA = await screen.findByRole('button', { name: 'review 使用 Skill shared' })
    await user.click(sharedInA)
    expect(sharedInA).toHaveAttribute('aria-pressed', 'false')

    await user.click(screen.getByRole('button', { name: '打开项目 B' }))
    expect(await screen.findByRole('heading', { name: 'group/frontend' })).toBeInTheDocument()

    await waitFor(() => expect(screen.getByRole('button', { name: 'review 使用 Skill shared' })).toHaveAttribute('aria-pressed', 'true'))
    expect(screen.queryByText('先保存绑定再恢复默认，未保存的更改会丢失。')).not.toBeInTheDocument()

    setCsrfToken('csrf-switch')
    await user.click(screen.getByRole('button', { name: 'review 使用 Skill b-only' }))
    await user.click(screen.getByRole('button', { name: '保存绑定' }))

    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toMatchObject({
      url: '/api/v1/block-bindings/review',
      method: 'PUT',
      body: { scope: 'project', project_id: 43, mcp_tools: [], skills: ['shared', 'b-only'] },
    })
  })
})
