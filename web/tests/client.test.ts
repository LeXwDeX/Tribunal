import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ApiError, UNAUTHORIZED_EVENT, getCsrfToken, request, segment, setCsrfToken, withQuery,
} from '../src/api/client'

const jsonResponse = (body: unknown, status = 200) => new Response(JSON.stringify(body), {
  status,
  headers: { 'content-type': 'application/json' },
})

describe('API client', () => {
  const fetchMock = vi.fn()

  beforeEach(() => {
    fetchMock.mockReset()
    vi.stubGlobal('fetch', fetchMock)
    setCsrfToken(null)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    setCsrfToken(null)
  })

  it('holds CSRF in memory and sends it on cookie writes only', async () => {
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse({ ok: true })))
    setCsrfToken('csrf-value')
    expect(getCsrfToken()).toBe('csrf-value')

    await request('/api/v1/projects', { method: 'POST', body: { name: 'x' } })
    const writeInit = fetchMock.mock.calls[0][1] as RequestInit
    const writeHeaders = new Headers(writeInit.headers)
    expect(writeHeaders.get('X-CSRF-Token')).toBe('csrf-value')
    expect(writeHeaders.get('Content-Type')).toBe('application/json')
    expect(writeHeaders.get('Authorization')).toBeNull()

    await request('/api/v1/projects')
    const readHeaders = new Headers((fetchMock.mock.calls[1][1] as RequestInit).headers)
    expect(readHeaders.get('X-CSRF-Token')).toBeNull()
  })

  it('dispatches unauthorized with the error code', async () => {
    const events: CustomEvent<{ code?: string }>[] = []
    const listener = (event: Event) => events.push(event as CustomEvent<{ code?: string }>)
    window.addEventListener(UNAUTHORIZED_EVENT, listener)
    fetchMock.mockResolvedValue(jsonResponse({ error: { code: 'session_expired', message: 'session expired', request_id: 'req-1' } }, 401))
    await expect(request('/api/v1/me')).rejects.toMatchObject({
      name: 'ApiError', status: 401, code: 'session_expired', message: 'session expired', requestId: 'req-1',
    })
    expect(events).toHaveLength(1)
    expect(events[0].detail?.code).toBe('session_expired')
    window.removeEventListener(UNAUTHORIZED_EVENT, listener)
  })

  it('dispatches unauthorized for a first-visit 401 without a cookie', async () => {
    const events: CustomEvent<{ code?: string }>[] = []
    const listener = (event: Event) => events.push(event as CustomEvent<{ code?: string }>)
    window.addEventListener(UNAUTHORIZED_EVENT, listener)
    fetchMock.mockResolvedValue(jsonResponse({ error: { code: 'unauthenticated', message: 'login required' } }, 401))
    await expect(request('/api/v1/me')).rejects.toMatchObject({ status: 401, code: 'unauthenticated' })
    expect(events).toHaveLength(1)
    expect(events[0].detail?.code).toBe('unauthenticated')
    window.removeEventListener(UNAUTHORIZED_EVENT, listener)
  })

  it('preserves field details from validation errors', async () => {
    fetchMock.mockResolvedValue(jsonResponse({
      error: { code: 'validation_failed', message: 'invalid', details: [{ field: 'token', message: 'missing scope' }] },
    }, 400))
    const error = await request('/api/v1/projects').catch((caught) => caught)
    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).details?.[0]).toEqual({ field: 'token', message: 'missing scope' })
  })

  it('encodes node path segments and query values', () => {
    expect(segment('verify.f7')).toBe('verify.f7')
    expect(segment('a/b c')).toBe('a%2Fb%20c')
    expect(withQuery('/x', { cursor: 'a&b', empty: '', missing: undefined })).toBe('/x?cursor=a%26b')
  })
})
