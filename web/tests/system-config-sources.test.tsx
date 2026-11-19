import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

const warnings = ['providers.claude.reasoning.effort is not sent by anthropic without output_config_effort']

const defaultConfig = {
  server: { listen: ':9090' },
  jobs: { workers: 3 },
  providers: [{ name: 'primary.eu', model: 'claude-sonnet', api_key_configured: true }],
}

const defaultOrigins = {
  'server.listen': 'base',
  'server.write_timeout': 'overlay',
  'jobs.workers': 'overlay',
  'providers.primary.eu.model': 'base',
}

function mock({ config = defaultConfig, origins = defaultOrigins }: { config?: unknown; origins?: Record<string, string> } = {}) {
  return installFetch((url) => {
    const path = new URL(url, 'http://localhost').pathname
    if (path === '/api/v1/me') return jsonResponse({ id: 7, username: 'alice', role: 'admin', global_read: false, csrf_token: 'x' })
    if (path === '/api/v1/api-tokens') return jsonResponse({ tokens: [], next_cursor: '' })
    if (path === '/api/v1/settings') return jsonResponse({})
    if (path === '/api/v1/config/status') {
      return jsonResponse({
        applied: false,
        restart_required: true,
        base_revision: 'aaaaaaaaaaaaaaaa',
        overlay_revision: 'bbbbbbbbbbbbbbbb',
        overlay_present: true,
        warnings,
        last_error: '最近一次重载失败：server.overlay.yaml',
        last_error_at: '2026-11-11T08:00:00Z',
        updated_at: '2026-11-11T07:00:00Z',
      })
    }
    if (path === '/api/v1/config') {
      return jsonResponse({
        config,
        origins,
        warnings,
        base_revision: 'aaaaaaaaaaaaaaaa',
        overlay_revision: 'bbbbbbbbbbbbbbbb',
        overlay_present: true,
        applied: false,
        restart_required: true,
      })
    }
    return jsonResponse({})
  })
}

describe('configuration sources panel (#410)', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows file-effective values, origins, warnings, and pending restart', async () => {
    mock()
    const user = userEvent.setup()
    renderWithProviders(<SystemPage />)
    await user.click(screen.getByRole('tab', { name: '平台配置（只读）' }))

    expect(await screen.findByText('配置来源与待重启')).toBeInTheDocument()
    expect(screen.getAllByText('待重启').length).toBeGreaterThan(0)
    expect(await screen.findByText('server.listen')).toBeInTheDocument()
    expect(screen.getByText(':9090')).toBeInTheDocument()
    expect(screen.getByText('jobs.workers')).toBeInTheDocument()
    expect(screen.getByText('providers.primary.eu.model')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet')).toBeInTheDocument()
    expect(screen.getAllByText('overlay').length).toBeGreaterThan(0)
    expect(screen.getAllByText('基础配置').length).toBeGreaterThan(0)
    expect(screen.getByRole('alert')).toHaveTextContent('server.overlay.yaml')
    expect(screen.getAllByRole('status').some((node) => node.textContent?.includes('reasoning.effort'))).toBe(true)
  })

  it('renders origins in the shared table and pages long lists on the client', async () => {
    const keys = Array.from({ length: 25 }, (_, index) => `item${String(index + 1).padStart(2, '0')}`)
    mock({
      config: { limits: Object.fromEntries(keys.map((key, index) => [key, index + 1])) },
      origins: Object.fromEntries(keys.map((key) => [`limits.${key}`, 'base'])),
    })
    const user = userEvent.setup()
    renderWithProviders(<SystemPage />)
    await user.click(screen.getByRole('tab', { name: '平台配置（只读）' }))

    const table = await screen.findByRole('table', { name: '配置来源' })
    expect(within(table).getAllByRole('columnheader').map((header) => header.textContent)).toEqual(['配置项', '文件生效值', '来源'])
    expect(within(table).getAllByRole('row')).toHaveLength(21)
    expect(screen.getByText('第 1 / 2 页 · 共 25 条')).toBeInTheDocument()
    expect(within(table).getByText('limits.item20')).toBeInTheDocument()
    expect(within(table).queryByText('limits.item21')).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '下一页' }))
    expect(screen.getByText('第 2 / 2 页 · 共 25 条')).toBeInTheDocument()
    expect(within(screen.getByRole('table', { name: '配置来源' })).getAllByRole('row')).toHaveLength(6)
    expect(screen.getByText('limits.item25')).toBeInTheDocument()
    expect(screen.queryByText('limits.item20')).not.toBeInTheDocument()
  })
})
