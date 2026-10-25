import { useRef, useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {
  Badge, Button, Dialog, Drawer, ErrorState, Field, Metric, Panel, PanelBody, Segmented, SevCounts, SeverityBadge, Sparkline, StatusBadge, Switch, Tabs,
} from '../src/components/ui'
import { ApiError } from '../src/api/client'
import ui from '../src/styles/ui.module.css'

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
          items={[{ key: 'overview', label: '概览', count: 3, panelId: 'tab-panel-overview' }, { key: 'settings', label: '设置', panelId: 'tab-panel-settings' }]}
        />
      </>,
    )
    expect(screen.getByRole('button', { name: /全部/ })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('tab', { name: /概览/ })).toHaveTextContent('3')
    expect(screen.getByRole('tab', { name: /概览/ })).toHaveAttribute('aria-controls', 'tab-panel-overview')
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
        <Panel title="面板">
          <PanelBody><span>面板正文</span></PanelBody>
        </Panel>
        <div className={ui.metricStrip}><Metric label="指标" value={1} /></div>
        <Switch checked={false} onChange={onChange} label="启用" disabled />
      </>,
    )
    expect(screen.getByText('面板正文').parentElement).toHaveClass(ui.panelBody)
    expect(screen.getByText('指标').parentElement?.parentElement).toHaveClass(ui.metricStrip)
    expect(screen.getByText('指标').parentElement).toHaveClass(ui.metric)
    expect(screen.getByRole('switch', { name: '启用' })).toBeDisabled()
  })
})
