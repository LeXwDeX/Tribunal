import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
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

  it('only offers projects the caller can manage', async () => {
    const detail = review({ id: 'wf-374', project_id: 42, mr_iid: 7, access: 'manage' })
    installFetch((url) => {
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
    const select = within(dialog).getByLabelText('项目') as HTMLSelectElement
    const values = Array.from(select.options).map((option) => option.value)
    expect(values).toContain('42')
    expect(values).not.toContain('43')
  })
})