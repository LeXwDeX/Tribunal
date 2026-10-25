import { useRef, useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {
  Badge, Button, CopyButton, Dialog, Drawer, ErrorState, Field, Metric, Panel, PanelBody, Segmented, SevCounts, SeverityBadge, Sparkline, StatusBadge, Switch, Tabs, ToastProvider, useToast,
} from '../src/components/ui'
import { ApiError } from '../src/api/client'
import ui from '../src/styles/ui.module.css'

const clipboardDescriptor = Object.getOwnPropertyDescriptor(navigator, 'clipboard')

function stubClipboard(writeText: (text: string) => Promise<void>) {
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } })
}

afterEach(() => {
  if (clipboardDescriptor) Object.defineProperty(navigator, 'clipboard', clipboardDescriptor)
  else Reflect.deleteProperty(navigator, 'clipboard')
  vi.useRealTimers()
})

function DialogHarness({ onClose }: { onClose: () => void }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button ref={undefined} onClick={() => setOpen(true)}>打开详情</Button>
      <Dialog
        open={open}
        onClose={() => { setOpen(false); onClose() }}
        title="节点详情"
        footer={(
          <>
            <Button variant="secondary">取消</Button>
            <Button variant="primary">确认</Button>
          </>
        )}
      >
        <p>正文</p>
      </Dialog>
    </>
  )
}

function ToastHarness() {
  const toast = useToast()
  return (
    <>
      <Button onClick={() => toast.push('error', '发生错误')}>显示错误</Button>
      <Button onClick={() => toast.push('success', '保存成功')}>显示成功</Button>
      <Button onClick={() => toast.push('info', '提示信息')}>显示提示</Button>
    </>
  )
}

describe('dialogs and form states', () => {
  it('traps focus, closes on Escape and restores focus to the trigger', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    render(<DialogHarness onClose={onClose} />)
    const trigger = screen.getByRole('button', { name: '打开详情' })
    await user.click(trigger)
    const dialog = screen.getByRole('dialog', { name: '节点详情' })
    expect(dialog).toBeInTheDocument()
    expect(screen.getByText('正文').parentElement).not.toBe(dialog)
    const close = screen.getByRole('button', { name: '关闭对话框' })
    expect(close).toHaveFocus()

    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(screen.getByRole('button', { name: '确认' })).toHaveFocus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(close).toHaveFocus()

    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(onClose).toHaveBeenCalled()
    await waitFor(() => expect(trigger).toHaveFocus())
  })

  it('renders labelled fields with an error alert', () => {
    render(
      <Field label="项目路径" htmlFor="project" error="必须填写项目路径">
        <input id="project" aria-invalid="true" />
      </Field>,
    )
    expect(screen.getByLabelText('项目路径')).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('必须填写项目路径')
  })

  it('renders API errors without substituting a success state', () => {
    render(<ErrorState error={new ApiError(500, 'internal', '存储不可用')} />)
    expect(screen.getByRole('alert')).toHaveTextContent('存储不可用')
    expect(screen.queryByText(/成功/)).not.toBeInTheDocument()
  })

  it('keeps drawer content and footer in separate regions', () => {
    render(
      <Drawer open onClose={vi.fn()} title="节点抽屉" footer={<Button>关闭</Button>}>
        <p>抽屉正文</p>
      </Drawer>,
    )
    const drawer = screen.getByRole('dialog', { name: '节点抽屉' })
    expect(screen.getByText('抽屉正文').parentElement).not.toBe(drawer)
    expect(screen.getByRole('button', { name: '关闭' })).toBeInTheDocument()
  })

  it('renders status, severity, and badge variants', () => {
    render(
      <>
        <StatusBadge status="running" />
        <SeverityBadge severity="P0" />
        <SeverityBadge severity="unknown" />
        <Badge tone="info" live plain>档位</Badge>
      </>,
    )
    expect(screen.getByText('运行中').className).toMatch(/badgeLive/)
    expect(screen.getByText('P0').className).toMatch(/sevP0/)
    expect(screen.getByText('unknown').className).toMatch(/sevInfo/)
    expect(screen.getByText('档位').className).toMatch(/badgeLive.*badgePlain|badgePlain.*badgeLive/)
  })

  it('supports segmented controls and tab counts', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <>
        <Segmented
          label="状态筛选"
          value="all"
          onChange={onChange}
          options={[{ value: 'all', label: '全部', count: 3 }, { value: 'failed', label: '失败', count: 1 }]}
        />
        <Tabs
          label="项目页签"
          value="overview"
          onChange={onChange}
          idPrefix="test-tab"
          items={[{ key: 'overview', label: '概览', count: 3, panelId: 'tab-panel-overview' }, { key: 'settings', label: '设置', panelId: 'tab-panel-settings' }]}
        />
      </>,
    )
    expect(screen.getByRole('button', { name: /全部/ })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('tab', { name: /概览/ })).toHaveTextContent('3')
    expect(screen.getByRole('tab', { name: /概览/ })).toHaveAttribute('id', 'test-tab-overview')
    expect(screen.getByRole('tab', { name: /概览/ })).toHaveAttribute('aria-controls', 'tab-panel-overview')
    expect(screen.getByRole('tab', { name: /设置/ })).not.toHaveAttribute('aria-controls')
    await user.click(screen.getByRole('button', { name: /失败/ }))
    expect(onChange).toHaveBeenCalledWith('failed')
  })

  it('renders sparklines only when there are at least two finite points', () => {
    const { rerender } = render(<Sparkline points={[1, Number.NaN, 3]} />)
    const svg = document.querySelector('svg')
    expect(svg).toHaveAttribute('aria-hidden', 'true')
    expect(svg?.querySelector('polyline')).toHaveAttribute('points', expect.stringContaining('64.00'))

    rerender(<Sparkline points={[1]} />)
    expect(document.querySelector('svg')).not.toBeInTheDocument()
  })

  it('renders severity counts with accessible zero states', () => {
    render(<SevCounts summary={{ by_severity: { P0: 0, P1: 2 } }} />)
    expect(screen.getByRole('img', { name: 'P0 0' }).className).toMatch(/zero/)
    expect(screen.getByRole('img', { name: 'P1 2' })).toHaveTextContent('2')
    expect(screen.getByRole('img', { name: 'P2 0' }).className).toMatch(/zero/)
  })

  it('keeps panel body and switch contracts explicit', () => {
    const onChange = vi.fn()
    render(
      <>
        <Panel title="面板" flush>
          <PanelBody><span>面板正文</span></PanelBody>
        </Panel>
        <div className={ui.metricStrip}><Metric label="指标" value={1} /></div>
        <Switch checked={false} onChange={onChange} label="启用" disabled />
      </>,
    )
    expect(screen.getByText('面板正文').parentElement).toHaveClass(ui.panelBody)
    expect(screen.getByText('面板正文').closest(`.${ui.panel}`)).toHaveClass(ui.panelFlush)
    expect(screen.getByText('指标').parentElement?.parentElement).toHaveClass(ui.metricStrip)
    expect(screen.getByText('指标').parentElement).toHaveClass(ui.metric)
    expect(screen.getByRole('switch', { name: '启用' })).toBeDisabled()
  })

  it('keeps non-flush panels compatible with direct children', () => {
    render(
      <Panel title="普通面板">
        <span>普通正文</span>
      </Panel>,
    )
    const body = screen.getByText('普通正文')
    expect(body.parentElement).toHaveClass(ui.panel)
    expect(body.parentElement).not.toHaveClass(ui.panelFlush)
  })
})

describe('copy feedback lifecycle', () => {
  const copyButton = () => screen.getByRole('button', { name: '复制' })
  const liveRegion = () => copyButton().querySelector('[aria-live="polite"]')

  it('announces success and leaves the idle label out of the live region', async () => {
    vi.useFakeTimers()
    stubClipboard(() => Promise.resolve())
    render(<CopyButton value="secret" />)
    expect(liveRegion()).toBeEmptyDOMElement()

    await act(async () => { fireEvent.click(copyButton()) })
    expect(copyButton()).toHaveTextContent('已复制')
    expect(liveRegion()).toHaveTextContent('已复制')

    act(() => { vi.advanceTimersByTime(2500) })
    expect(copyButton()).toHaveTextContent('复制')
    expect(liveRegion()).toBeEmptyDOMElement()
  })

  it('announces clipboard failure', async () => {
    stubClipboard(() => Promise.reject(new Error('denied')))
    render(<CopyButton value="secret" />)
    await act(async () => { fireEvent.click(copyButton()) })
    expect(copyButton()).toHaveTextContent('复制失败，请手动复制')
    expect(liveRegion()).toHaveTextContent('复制失败，请手动复制')
  })

  it('keeps a single reset timer across repeated copies', async () => {
    vi.useFakeTimers()
    stubClipboard(() => Promise.resolve())
    const { unmount } = render(<CopyButton value="secret" />)
    await act(async () => { fireEvent.click(copyButton()) })
    await act(async () => { fireEvent.click(copyButton()) })
    expect(vi.getTimerCount()).toBe(1)
    unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('does not schedule a reset after unmount while the clipboard is pending', async () => {
    vi.useFakeTimers()
    let resolveWrite: (() => void) | undefined
    stubClipboard(() => new Promise<void>((resolve) => { resolveWrite = resolve }))
    const { unmount } = render(<CopyButton value="secret" />)
    fireEvent.click(copyButton())
    unmount()
    await act(async () => { resolveWrite?.() })
    expect(vi.getTimerCount()).toBe(0)
  })
})

describe('toast announcements and timers', () => {
  it('keeps a persistent polite region for success and info toasts', () => {
    render(
      <ToastProvider>
        <ToastHarness />
      </ToastProvider>,
    )
    const region = screen.getByRole('status')
    expect(region).toHaveAttribute('aria-live', 'polite')
    expect(region).toBeEmptyDOMElement()

    fireEvent.click(screen.getByRole('button', { name: '显示成功' }))
    fireEvent.click(screen.getByRole('button', { name: '显示提示' }))
    expect(region).toHaveTextContent('保存成功')
    expect(region).toHaveTextContent('提示信息')
  })

  it('keeps error urgency and insertion order inside the stable region', () => {
    render(
      <ToastProvider>
        <ToastHarness />
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', { name: '显示成功' }))
    fireEvent.click(screen.getByRole('button', { name: '显示错误' }))
    const region = screen.getByRole('status')
    expect(within(region).getByRole('alert')).toHaveTextContent('发生错误')
    expect(within(region).getAllByText(/保存成功|发生错误/).map((node) => node.textContent)).toEqual([
      '保存成功',
      '发生错误',
    ])
  })

  it('auto dismisses a toast and releases its timer', () => {
    vi.useFakeTimers()
    render(
      <ToastProvider>
        <ToastHarness />
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', { name: '显示成功' }))
    expect(vi.getTimerCount()).toBe(1)

    act(() => { vi.advanceTimersByTime(5999) })
    expect(screen.getByText('保存成功')).toBeInTheDocument()
    act(() => { vi.advanceTimersByTime(1) })
    expect(screen.queryByText('保存成功')).not.toBeInTheDocument()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('releases the timer when a toast is closed manually', () => {
    vi.useFakeTimers()
    render(
      <ToastProvider>
        <ToastHarness />
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', { name: '显示成功' }))
    expect(vi.getTimerCount()).toBe(1)

    fireEvent.click(screen.getByRole('button', { name: '关闭提示' }))
    expect(screen.queryByText('保存成功')).not.toBeInTheDocument()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('clears every timer on unmount', () => {
    vi.useFakeTimers()
    const { unmount } = render(
      <ToastProvider>
        <ToastHarness />
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', { name: '显示成功' }))
    fireEvent.click(screen.getByRole('button', { name: '显示错误' }))
    expect(vi.getTimerCount()).toBe(2)

    unmount()
    expect(vi.getTimerCount()).toBe(0)
  })
})

describe('modal focus boundaries', () => {
  function BoundaryHarness() {
    return (
      <>
        <button type="button">外部按钮</button>
        <Dialog open onClose={vi.fn()} title="焦点边界">
          <div aria-hidden="true"><button type="button">隐藏按钮</button></div>
          <button type="button" aria-hidden="false">可见按钮</button>
        </Dialog>
      </>
    )
  }

  it('skips aria-hidden subtrees and keeps aria-hidden=false controls in the trap', () => {
    render(<BoundaryHarness />)
    const close = screen.getByRole('button', { name: '关闭对话框' })
    const visible = screen.getByRole('button', { name: '可见按钮' })
    expect(close).toHaveFocus()

    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(visible).toHaveFocus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(close).toHaveFocus()
  })

  it('reclaims focus from outside the dialog for Tab and Shift+Tab', () => {
    render(<BoundaryHarness />)
    const close = screen.getByRole('button', { name: '关闭对话框' })
    const visible = screen.getByRole('button', { name: '可见按钮' })
    const outside = screen.getByRole('button', { name: '外部按钮' })

    outside.focus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(close).toHaveFocus()

    outside.focus()
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(visible).toHaveFocus()
  })

  it('focuses the dialog container when no focusable control remains', () => {
    render(<Dialog open onClose={vi.fn()} title="无控件" />)
    const dialog = screen.getByRole('dialog', { name: '无控件' })
    screen.getByRole('button', { name: '关闭对话框' }).setAttribute('aria-hidden', 'true')

    fireEvent.keyDown(document, { key: 'Tab' })
    expect(dialog).toHaveFocus()
  })
})
