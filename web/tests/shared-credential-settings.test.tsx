import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import { SettingsPanel } from '../src/features/project/SettingsPanel'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('shared-credential project settings', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('disables and explains the commit status override for shared projects', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
      return jsonResponse({})
    })
    const shared = project({ shared_credential: true, access: 'manage' })
    renderWithProviders(<SettingsPanel project={shared} />)

    const select = await screen.findByLabelText('BLOCKING 时写 commit status')
    expect(select).toBeDisabled()
    expect(screen.getByText(/共享模式不支持 commit status/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Key/ })).not.toBeInTheDocument()
  })

  it('keeps the commit status override editable for project-token projects', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
      return jsonResponse({})
    })
    renderWithProviders(<SettingsPanel project={project({ access: 'manage' })} />)

    const select = await screen.findByLabelText('BLOCKING 时写 commit status')
    expect(select).toBeEnabled()
    expect(screen.queryByText(/共享模式不支持 commit status/)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '更新访问 Key' })).toBeInTheDocument()
  })
})