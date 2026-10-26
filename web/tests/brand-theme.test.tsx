import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { AppShell } from '../src/components/AppShell'
import { AuthScreen } from '../src/components/AuthScreen'
import { ToastProvider } from '../src/components/ui'
import { adminMe, createTestQueryClient, installFetch, jsonResponse } from './test-utils'
import indexHtml from '../index.html?raw'

const startupScript = indexHtml.match(/<script>([\s\S]*?)<\/script>/)?.[1]

// Node 26 exposes an unconfigured global localStorage; this jsdom setup has no storage implementation.
let originalStorage: PropertyDescriptor | undefined

beforeAll(() => {
  const values = new Map<string, string>()
  const storage = {
    clear: () => values.clear(),
    getItem: (key: string) => values.get(key) ?? null,
    removeItem: (key: string) => values.delete(key),
    setItem: (key: string, value: string) => values.set(key, String(value)),
  }
  originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, writable: true, value: storage })
})

afterAll(() => {
  if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage)
  else delete (globalThis as { localStorage?: unknown }).localStorage
})

function runStartupThemeScript() {
  if (!startupScript) throw new Error('startup theme script missing')
  new Function(startupScript)()
}

function mockPreferredTheme(theme: 'dark' | 'light') {
  vi.spyOn(window, 'matchMedia').mockImplementation((query: string) => ({
    matches: query.includes('prefers-color-scheme: dark') ? theme === 'dark' : false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }) as MediaQueryList)
}

// The shell polls running reviews for the navigation count.
function renderShell() {
  installFetch(() => jsonResponse({ reviews: [] }))
  return render(
    <QueryClientProvider client={createTestQueryClient()}>
      <MemoryRouter>
        <ToastProvider>
          <AppShell me={adminMe} />
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('startup theme preference', () => {
  beforeEach(() => {
    localStorage.clear()
    delete document.documentElement.dataset.theme
    mockPreferredTheme('light')
  })

  afterEach(() => {
    vi.restoreAllMocks()
    cleanup()
    localStorage.clear()
    delete document.documentElement.dataset.theme
  })

  it('falls back to the system preference when no theme is stored', () => {
    mockPreferredTheme('dark')
    runStartupThemeScript()
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it.each([
    ['dark', 'light'],
    ['light', 'dark'],
  ] as const)('uses the new tribunal-theme=%s before the system preference', (saved, preferred) => {
    localStorage.setItem('tribunal-theme', saved)
    mockPreferredTheme(preferred)
    runStartupThemeScript()
    expect(document.documentElement.dataset.theme).toBe(saved)
  })

  it('prefers a valid new preference over a legacy value without overwriting it', () => {
    localStorage.setItem('tribunal-theme', 'light')
    localStorage.setItem('crc-theme', 'dark')
    const setItem = vi.spyOn(localStorage, 'setItem')
    mockPreferredTheme('dark')
    runStartupThemeScript()
    expect(document.documentElement.dataset.theme).toBe('light')
    expect(localStorage.getItem('crc-theme')).toBe('dark')
    expect(setItem).not.toHaveBeenCalled()
  })

  it.each([
    ['dark', 'light'],
    ['light', 'dark'],
  ] as const)('migrates legacy crc-theme=%s and keeps the old value', (legacy, preferred) => {
    localStorage.setItem('crc-theme', legacy)
    mockPreferredTheme(preferred)
    runStartupThemeScript()
    expect(document.documentElement.dataset.theme).toBe(legacy)
    expect(localStorage.getItem('tribunal-theme')).toBe(legacy)
    expect(localStorage.getItem('crc-theme')).toBe(legacy)
  })

  it('keeps the legacy theme usable when the migration write is denied', () => {
    localStorage.setItem('crc-theme', 'dark')
    mockPreferredTheme('light')
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('storage denied')
    })
    expect(() => runStartupThemeScript()).not.toThrow()
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('does not crash when reading storage is denied', () => {
    mockPreferredTheme('dark')
    vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new Error('storage denied')
    })
    expect(() => runStartupThemeScript()).not.toThrow()
    expect(document.documentElement.dataset.theme).toBe('dark')
  })
})

describe('theme toggle persistence', () => {
  beforeEach(() => {
    localStorage.clear()
    document.documentElement.dataset.theme = 'light'
    mockPreferredTheme('light')
  })

  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    cleanup()
    localStorage.clear()
    delete document.documentElement.dataset.theme
  })

  it('stores the new key and keeps the legacy key for rollback', async () => {
    const user = userEvent.setup()
    localStorage.setItem('crc-theme', 'light')
    renderShell()
    await user.click(screen.getByRole('button', { name: '切换为暗色主题' }))
    expect(document.documentElement.dataset.theme).toBe('dark')
    expect(localStorage.getItem('tribunal-theme')).toBe('dark')
    expect(localStorage.getItem('crc-theme')).toBe('light')
  })

  it('keeps the selected theme when the storage write is denied', async () => {
    const user = userEvent.setup()
    renderShell()
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('storage denied')
    })
    await user.click(screen.getByRole('button', { name: '切换为暗色主题' }))
    expect(document.documentElement.dataset.theme).toBe('dark')
    expect(screen.getByRole('button', { name: '切换为亮色主题' })).toBeInTheDocument()
  })
})

describe('Tribunal product branding', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('shows the product name in the desktop and mobile shell brand', () => {
    renderShell()
    expect(screen.getAllByText('Tribunal')).toHaveLength(2)
  })

  it('shows the product name on the authentication screen', () => {
    render(<AuthScreen />)
    expect(screen.getByText('Tribunal')).toBeInTheDocument()
  })
})
