import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'
import { ReviewDetailPage } from '../src/pages/ReviewDetailPage'
import { finding, review } from './fixtures'
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

describe('review trigger project picker', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('only offers manageable projects and submits the selected id with full', async () => {
    const detail = review({ id: 'wf-374', project_id: 42, mr_iid: 7, access: 'manage' })
    const triggers: Array<{ url: string; method: string; body: unknown }> = []
    installFetch((url, init) => {
      if (url.includes('/merge_requests/7/reviews') && init?.method === 'POST') {
        triggers.push({ url, method: init.method, body: JSON.parse(String(init.body)) })
        return jsonResponse({ job_id: 'job-374' })
      }
      if (url === `/api/v1/reviews/${detail.id}`) return jsonResponse(detail)
      if (url === `/api/v1/reviews/${detail.id}/graph`) return jsonResponse({ nodes: [], edges: [] })
      if (url.startsWith(`/api/v1/reviews/${detail.id}/findings`)) return jsonResponse({ findings: [finding()] })
      if (url === '/api/v1/projects/42') {
        return jsonResponse({ id: 42, path_with_namespace: 'group/backend', profile: 'default', settings: {}, enabled: true, access: 'manage' })
      }
      if (url.startsWith('/api/v1/projects')) {
        return jsonResponse({
          projects: [
            { id: 42, path_with_namespace: 'group/backend', access: 'manage', enabled: true },
            { id: 43, path_with_namespace: 'group/readonly', access: 'read', enabled: true },
            { id: 44, path_with_namespace: 'group/second', access: 'manage', enabled: true },
          ],
        })
      }
      return jsonResponse({})
    })
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    const user = userEvent.setup()
    renderWithProviders(
      <Routes>
        <Route path="/reviews/:id" element={<ReviewDetailPage />} />
      </Routes>,
      { route: `/reviews/${detail.id}` },
    )

    await user.click(await screen.findByRole('button', { name: '重新触发评审' }))
    const dialog = await screen.findByRole('dialog', { name: '手动触发评审' })
    const picker = within(dialog).getByRole('combobox', { name: '项目' })
    expect(picker).toHaveTextContent('group/backend')

    await user.click(picker)
    const options = await screen.findAllByRole('option')
    const values = options.map((option) => option.textContent)
    expect(values).toContain('group/backend')
    expect(values).toContain('group/second')
    expect(values).not.toContain('group/readonly')
    await user.click(screen.getByRole('option', { name: 'group/backend' }))
    expect(picker).toHaveTextContent('group/backend')

    await user.click(picker)
    await user.click(await screen.findByRole('option', { name: 'group/second' }))
    expect(picker).toHaveTextContent('group/second')

    await user.click(within(dialog).getByRole('checkbox', { name: '全量评审（full）' }))
    await user.click(within(dialog).getByRole('button', { name: '触发评审' }))
    await waitFor(() => expect(triggers).toEqual([{
      url: '/api/v1/projects/44/merge_requests/7/reviews',
      method: 'POST',
      body: { full: true },
    }]))
  })
})