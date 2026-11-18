import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SettingsPanel } from '../src/features/project/SettingsPanel'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('project settings revision fence', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('sends the loaded revision with a settings save', async () => {
    const patches: Array<Record<string, unknown>> = []
    installFetch((url, init) => {
      if (url === '/api/v1/projects/42' && init?.method === 'PATCH') {
        patches.push(JSON.parse(String(init.body)) as Record<string, unknown>)
        return jsonResponse(project({ revision: 4, settings: { language: 'zh-CN' } }))
      }
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project()] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<SettingsPanel project={project({ revision: 4 })} />)

    await user.click(screen.getByRole('button', { name: '保存设置' }))
    await waitFor(() => expect(patches).toHaveLength(1))
    expect(patches[0].revision).toBe(4)
    expect(patches[0].settings).toEqual({})
  })

  it('reloads the latest settings after a stale revision conflict', async () => {
    let getCount = 0
    installFetch((url, init) => {
      if (url === '/api/v1/projects/42' && init?.method === 'PATCH') {
        return jsonResponse({ error: { code: 'stale_revision', message: 'revision does not match' } }, 409)
      }
      if (url === '/api/v1/projects/42') {
        getCount += 1
        return jsonResponse(project({ revision: 5, settings: { language: 'en' } }))
      }
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [project()] })
      return jsonResponse({})
    })
    const user = userEvent.setup()
    renderWithProviders(<SettingsPanel project={project({ revision: 4, settings: { language: 'zh-CN' } })} />)

    await user.click(screen.getByRole('button', { name: '保存设置' }))
    expect(await screen.findByText(/已被其他人修改/)).toBeInTheDocument()
    await waitFor(() => expect(getCount).toBe(1))
    await waitFor(() => expect(screen.getByLabelText('评审语言')).toHaveTextContent('English'))
  })
})