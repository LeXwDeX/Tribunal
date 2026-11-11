import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import {
  ActionHint, Field, FieldHint, Note, Panel, TableNote, Toolbar,
} from '../src/components/ui'

describe('shared form and explanation primitives', () => {
  it('links a field hint to its control with aria-describedby', () => {
    render(
      <Field label="名称" htmlFor="name" hint="最长 100 个字符。">
        <input id="name" />
      </Field>,
    )
    const input = screen.getByLabelText('名称')
    expect(input).toHaveAttribute('aria-describedby', 'name-hint')
    expect(screen.getByText('最长 100 个字符。')).toHaveAttribute('id', 'name-hint')
  })

  it('keeps an existing aria-describedby alongside the field hint', () => {
    render(
      <Field label="名称" htmlFor="name" hint="提示。">
        <input id="name" aria-describedby="existing" />
      </Field>,
    )
    expect(screen.getByLabelText('名称')).toHaveAttribute('aria-describedby', 'existing name-hint')
  })

  it('renders the toolbar status after the toolbar, not inside it', () => {
    render(
      <Toolbar label="筛选" status="正在刷新…">
        <span>筛选条件</span>
      </Toolbar>,
    )
    const toolbar = screen.getByRole('toolbar')
    const status = screen.getByRole('status')
    expect(toolbar).not.toContainElement(status)
    expect(toolbar.compareDocumentPosition(status) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('renders a panel note after its children', () => {
    render(
      <Panel title="面板" note="说明文字。">
        <p>主体内容</p>
      </Panel>,
    )
    const body = screen.getByText('主体内容')
    const note = screen.getByText('说明文字。')
    expect(body.compareDocumentPosition(note) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('renders the standalone explanation primitives', () => {
    render(
      <div>
        <FieldHint>字段说明</FieldHint>
        <ActionHint>操作说明</ActionHint>
        <Note tone="warn" role="status">警告说明</Note>
        <TableNote>表格说明</TableNote>
      </div>,
    )
    expect(screen.getByText('字段说明')).toBeInTheDocument()
    expect(screen.getByText('操作说明')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('警告说明')
    expect(screen.getByText('表格说明')).toBeInTheDocument()
  })
})