import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function deferredResponse() {
  let resolve!: (response: Response) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<Response>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

async function openAndSubmit(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: '创建 token' }))
  await user.type(screen.getByLabelText('名称'), 'ci')
  await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: '创建', exact: true }))
}

describe('one-time API token display', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('renders the secret only once and clears it when the dialog closes', async () => {
    const user = userEvent.setup()
    let created = false
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') {
        created = true
        return jsonResponse({ id: 't1', token: 'crsec_once_only' })
      }
      if (url.startsWith('/api/v1/api-tokens')) return jsonResponse({ tokens: created ? [{ id: 't1', name: 'ci' }] : [] })
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />)
    await user.click(await screen.findByRole('button', { name: '创建 token' }))
    await user.type(screen.getByLabelText('名称'), 'ci')
    const createButtons = screen.getAllByRole('button', { name: '创建', exact: true })
    await user.click(createButtons[createButtons.length - 1])
    const secretInput = await screen.findByDisplayValue('crsec_once_only')
    expect(secretInput).toHaveAttribute('readonly')
    await user.click(screen.getByRole('button', { name: '我已复制，关闭' }))
    await waitFor(() => expect(screen.queryByDisplayValue('crsec_once_only')).not.toBeInTheDocument())
  })

  it('sends only one request for two synchronous submissions', async () => {
    const user = userEvent.setup()
    const pending = deferredResponse()
    let posts = 0
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') {
        posts += 1
        return pending.promise
      }
      return jsonResponse({ tokens: [] })
    })
    renderWithProviders(<SystemPage />)
    await user.click(await screen.findByRole('button', { name: '创建 token' }))
    await user.type(screen.getByLabelText('名称'), 'ci')
    const submit = within(screen.getByRole('dialog')).getByRole('button', { name: '创建', exact: true })
    act(() => { fireEvent.click(submit); fireEvent.click(submit) })
    expect(posts).toBe(1)
    expect(submit).toBeDisabled()
    await act(async () => pending.resolve(jsonResponse({ id: 't1', token: 'test-secret-1' })))
  })

  it.each(['Escape', '遮罩', '关闭按钮'])('keeps a pending request guarded after closing with %s', async (method) => {
    const user = userEvent.setup()
    const pending = deferredResponse()
    let posts = 0
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') {
        posts += 1
        return pending.promise
      }
      return jsonResponse({ tokens: [] })
    })
    renderWithProviders(<SystemPage />)
    await openAndSubmit(user)
    const dialog = screen.getByRole('dialog')
    if (method === 'Escape') await user.keyboard('{Escape}')
    else if (method === '遮罩') fireEvent.mouseDown(dialog.parentElement!)
    else await user.click(within(dialog).getByRole('button', { name: '关闭对话框' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '创建 token' }))
    const reopened = screen.getByRole('dialog', { name: '创建 API token' })
    expect(within(reopened).getByRole('button', { name: '创建', exact: true })).toBeDisabled()
    expect(within(reopened).getByRole('button', { name: '停止等待' })).toBeEnabled()
    expect(within(reopened).getByRole('alert')).toHaveTextContent('明文无法再次查看')
    expect(posts).toBe(1)
    await act(async () => pending.resolve(jsonResponse({ id: 't1', token: 'test-secret-1' })))
  })

  it('never displays a late secret in a reopened dialog and refreshes the token list', async () => {
    const user = userEvent.setup()
    const pending = deferredResponse()
    let reads = 0
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') return pending.promise
      if (url.startsWith('/api/v1/api-tokens')) reads += 1
      return jsonResponse({ tokens: [] })
    })
    renderWithProviders(<SystemPage />)
    await openAndSubmit(user)
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: '创建 token' }))
    await act(async () => pending.resolve(jsonResponse({ id: 't1', token: 'test-secret-late' })))
    const dialog = screen.getByRole('dialog', { name: '创建 API token' })
    expect(screen.queryByDisplayValue('test-secret-late')).not.toBeInTheDocument()
    expect(within(dialog).getByRole('alert')).toHaveTextContent('明文无法再次查看')
    expect(screen.getByText(/API token 已创建，但明文无法再次查看/)).toBeInTheDocument()
    await waitFor(() => expect(reads).toBeGreaterThan(1))
  })

  it('does not put a late network failure in a reopened dialog', async () => {
    const user = userEvent.setup()
    const pending = deferredResponse()
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') return pending.promise
      return jsonResponse({ tokens: [] })
    })
    renderWithProviders(<SystemPage />)
    await openAndSubmit(user)
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: '创建 token' }))
    const dialog = screen.getByRole('dialog')
    const warning = within(dialog).getByRole('alert').textContent
    await act(async () => pending.reject(new TypeError('network failure')))
    expect(within(dialog).getByRole('alert')).toHaveTextContent(warning!)
    expect(within(dialog).queryByText('创建失败或结果未知')).not.toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '创建', exact: true })).toBeEnabled()
    expect(await screen.findByText(/创建结果未知/)).toBeInTheDocument()
  })

  it('lets the user stop waiting without implying that the server cancelled creation', async () => {
    const user = userEvent.setup()
    const first = deferredResponse()
    const second = deferredResponse()
    const signals: AbortSignal[] = []
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') {
        signals.push(init.signal as AbortSignal)
        return signals.length === 1 ? first.promise : second.promise
      }
      return jsonResponse({ tokens: [] })
    })
    renderWithProviders(<SystemPage />)
    await openAndSubmit(user)
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText(/关闭对话框不会取消请求/)).toBeInTheDocument()
    within(dialog).getByRole('button', { name: '关闭对话框' }).focus()
    await user.tab()
    expect(within(dialog).getByRole('button', { name: '关闭', exact: true })).toHaveFocus()
    await user.tab()
    expect(within(dialog).getByRole('button', { name: '停止等待' })).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(signals[0].aborted).toBe(true)
    expect(within(dialog).getByText(/可能已在服务端创建/)).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: '创建', exact: true }))
    expect(signals).toHaveLength(2)

    await act(async () => first.resolve(jsonResponse({ id: 't1', token: 'test-secret-late' })))
    expect(within(dialog).getByRole('button', { name: '创建', exact: true })).toBeDisabled()
    expect(screen.queryByDisplayValue('test-secret-late')).not.toBeInTheDocument()
    await act(async () => second.resolve(jsonResponse({ id: 't2', token: 'test-secret-current' })))
    expect(await screen.findByDisplayValue('test-secret-current')).toBeInTheDocument()
  })

  it('reports an uncertain result after a network failure and refreshes the list', async () => {
    const user = userEvent.setup()
    const pending = deferredResponse()
    let reads = 0
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') return pending.promise
      if (url.startsWith('/api/v1/api-tokens')) reads += 1
      return jsonResponse({ tokens: [] })
    })
    renderWithProviders(<SystemPage />)
    await openAndSubmit(user)
    await act(async () => pending.reject(new TypeError('network failure')))
    expect(await screen.findByText(/可能已在服务端创建/)).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('创建失败')
    await waitFor(() => expect(reads).toBeGreaterThan(1))
  })

  it('keeps the uncertain-result warning when an aborted fetch rejects', async () => {
    const user = userEvent.setup()
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') {
        return new Promise<Response>((_, reject) => {
          init.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
        })
      }
      return jsonResponse({ tokens: [] })
    })
    renderWithProviders(<SystemPage />)
    await openAndSubmit(user)
    await user.click(screen.getByRole('button', { name: '停止等待' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('可能已在服务端创建')
    await user.click(screen.getByRole('button', { name: '取消' }))
    await user.click(screen.getByRole('button', { name: '创建 token' }))
    expect(screen.getByRole('alert')).toHaveTextContent('可能已在服务端创建')
  })

  it('aborts the in-flight request when the page unmounts', async () => {
    const user = userEvent.setup()
    const pending = deferredResponse()
    let signal: AbortSignal | undefined
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') {
        signal = init.signal as AbortSignal
        return pending.promise
      }
      return jsonResponse({ tokens: [] })
    })
    const { unmount } = renderWithProviders(<SystemPage />)
    await openAndSubmit(user)
    unmount()
    expect(signal?.aborted).toBe(true)
    await act(async () => pending.reject(new DOMException('aborted', 'AbortError')))
  })
})
