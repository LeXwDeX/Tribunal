import type { ApiErrorBody } from './types'

export const UNAUTHORIZED_EVENT = 'crc:unauthorized'
export const LOGOUT_EVENT = 'crc:logged-out'

// CSRF tokens stay in memory only. They must never be persisted to storage.
let csrfToken: string | null = null

export function setCsrfToken(token: string | null | undefined) {
  csrfToken = token || null
}

export function getCsrfToken(): string | null {
  return csrfToken
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId?: string
  readonly details?: Array<{ field?: string; message?: string }>

  constructor(status: number, code: string, message: string, requestId?: string, details?: Array<{ field?: string; message?: string }>) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
    this.details = details
  }
}

export interface RequestOptions {
  method?: string
  body?: unknown
  signal?: AbortSignal
  headers?: Record<string, string>
}

export function segment(value: string | number): string {
  return encodeURIComponent(String(value))
}

export function withQuery(path: string, query: Record<string, unknown>): string {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === '') continue
    params.set(key, String(value))
  }
  const encoded = params.toString()
  return encoded ? `${path}?${encoded}` : path
}

function isWrite(method: string): boolean {
  return !['GET', 'HEAD', 'OPTIONS'].includes(method.toUpperCase())
}

async function parseError(response: Response): Promise<ApiError> {
  let body: ApiErrorBody | undefined
  try {
    body = (await response.json()) as ApiErrorBody
  } catch {
    body = undefined
  }
  const envelope = body?.error
  return new ApiError(
    response.status,
    envelope?.code || 'http_error',
    envelope?.message || `请求失败（HTTP ${response.status}）`,
    envelope?.request_id,
    envelope?.details,
  )
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = (options.method || 'GET').toUpperCase()
  const headers: Record<string, string> = { Accept: 'application/json', ...options.headers }
  let body: BodyInit | undefined

  if (options.body !== undefined) {
    if (options.body instanceof FormData) {
      body = options.body
    } else {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(options.body)
    }
  }
  if (isWrite(method) && csrfToken) {
    headers['X-CSRF-Token'] = csrfToken
  }

  let response: Response
  try {
    response = await fetch(path, {
      method,
      headers,
      body,
      signal: options.signal,
      credentials: 'same-origin',
    })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw new ApiError(0, 'network_error', '无法连接服务器，请检查网络后重试。')
  }

  if (!response.ok) {
    const apiError = await parseError(response)
    if (response.status === 401 && typeof window !== 'undefined') {
      window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT, { detail: { code: apiError.code } }))
    }
    throw apiError
  }

  if (response.status === 204) return undefined as T
  const contentType = response.headers.get('content-type') || ''
  if (!contentType.includes('application/json')) {
    return (await response.text()) as T
  }
  return (await response.json()) as T
}

export function streamUrl(reviewId: string): string {
  return `/api/v1/reviews/${segment(reviewId)}/stream`
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    const fieldDetails = (error.details || [])
      .map((detail) => [detail.field, detail.message].filter(Boolean).join(': '))
      .filter(Boolean)
    return fieldDetails.length ? `${error.message}（${fieldDetails.join('；')}）` : error.message
  }
  if (error instanceof Error) return error.message
  return '发生未知错误。'
}
