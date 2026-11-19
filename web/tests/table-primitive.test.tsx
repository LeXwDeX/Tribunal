import { describe, expect, it } from 'vitest'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { DataTable, ReasonBadge, TableAction, TableActions, TablePager, tableCell } from '../src/components/ui'
import { formatLatency, formatPercent } from '../src/lib/format'

const root = resolve(process.cwd(), 'src')

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry)
    if (statSync(path).isDirectory()) walk(path, out)
    else out.push(path)
  }
  return out
}

const files = walk(root)
const rel = (path: string) => relative(root, path)

// Regression guard for the shared table standard
// (docs/design/07-web-console.md, AGENTS.md §6).
describe('table primitive guard', () => {
  it('renders <table> only inside the shared DataTable primitive', () => {
    const offenders = files
      .filter((path) => path.endsWith('.tsx') && rel(path) !== join('components', 'ui.tsx'))
      .filter((path) => /<table[\s>]/.test(readFileSync(path, 'utf8')))
      .map(rel)
    expect(offenders).toEqual([])
  })

  it('keeps legacy table classes out of the source', () => {
    const offenders = files
      .filter((path) => /\.(tsx|ts|css)$/.test(path))
      .filter((path) => /\b(dataTable|tableWrap)\b/.test(readFileSync(path, 'utf8')))
      .map(rel)
    expect(offenders).toEqual([])
  })

  it('does not style table elements outside the shared styles', () => {
    const shared = new Set([join('styles', 'ui.module.css'), join('styles', 'global.css')])
    const offenders: string[] = []
    for (const path of files.filter((file) => file.endsWith('.css') && !shared.has(rel(file)))) {
      const css = readFileSync(path, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')
      for (const match of css.matchAll(/([^{}]+)\{/g)) {
        const selector = match[1].trim()
        if (selector.startsWith('@')) continue
        if (/(^|[\s>+~,(])(table|thead|tbody|tr|th|td)(?=$|[\s.:#[>+~,)])/.test(selector)) {
          offenders.push(`${rel(path)}: ${selector}`)
        }
      }
    }
    expect(offenders).toEqual([])
  })

  it('never top-aligns table cells anywhere', () => {
    const offenders = files
      .filter((path) => path.endsWith('.css'))
      .filter((path) => /\b(td|th|table|Table|Cell)\b[^{}]*\{[^}]*vertical-align:\s*top/.test(readFileSync(path, 'utf8')))
      .map(rel)
    expect(offenders).toEqual([])
  })

  it('never caps table height or scrolls tables vertically', () => {
    const css = readFileSync(join(root, 'styles', 'ui.module.css'), 'utf8')
    const block = (name: string) => css.match(new RegExp(`\\.${name}\\s*\\{([^}]*)\\}`))?.[1] ?? ''
    expect(block('tableScroll')).not.toMatch(/max-height|overflow-y:\s*(auto|scroll)|overflow:\s*(auto|scroll)/)
    expect(css).toMatch(/\.table td \{[^}]*vertical-align: middle/)
    expect(css).toMatch(/\.table th \{[^}]*vertical-align: middle/)
    // Badges inside tables are never clipped with an ellipsis.
    expect(css).toMatch(/\.table \.badge \{[^}]*overflow: visible/)
  })
})

describe('DataTable', () => {
  it('renders the table with an accessible name and cell helpers', () => {
    render(
      <DataTable label="示例">
        <thead><tr><th scope="col">名称</th><th scope="col" className={tableCell.num}>数量</th><th scope="col" className={tableCell.actions}>操作</th></tr></thead>
        <tbody>
          <tr>
            <td>a</td>
            <td className={tableCell.num}>1,024</td>
            <td className={tableCell.actions}><TableActions><TableAction tone="danger">删除</TableAction></TableActions></td>
          </tr>
        </tbody>
      </DataTable>,
    )
    const table = screen.getByRole('table', { name: '示例' })
    expect(within(table).getByRole('button', { name: '删除' })).toBeInTheDocument()
  })

  it('shows loading, error and empty states instead of an empty table', () => {
    const { rerender } = render(<DataTable label="t" isEmpty loading><tbody /></DataTable>)
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(screen.getByText('加载中…')).toBeInTheDocument()
    rerender(<DataTable label="t" isEmpty error={new Error('x')} errorTitle="读取失败" onRetry={() => {}}><tbody /></DataTable>)
    expect(screen.getByText('读取失败')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
    rerender(<DataTable label="t" isEmpty><tbody /></DataTable>)
    expect(screen.getByText('暂无数据')).toBeInTheDocument()
  })

  it('exposes a status reason through aria-describedby', async () => {
    const user = userEvent.setup()
    render(<ReasonBadge tone="neutral" reason="评审已取消">已跳过</ReasonBadge>)
    const anchor = screen.getByText('已跳过').closest('[tabindex]') as HTMLElement
    await user.tab()
    expect(anchor).toHaveFocus()
    expect(anchor).toHaveAccessibleDescription('评审已取消')
  })

  it('pages client rows', async () => {
    const user = userEvent.setup()
    let page = 1
    const { rerender } = render(<TablePager page={page} pageCount={3} total={45} onChange={(next) => { page = next }} />)
    expect(screen.getByText('第 1 / 3 页 · 共 45 条')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '上一页' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '下一页' }))
    expect(page).toBe(2)
    rerender(<TablePager page={1} pageCount={1} total={3} onChange={() => {}} />)
    expect(screen.queryByRole('navigation', { name: '表格分页' })).not.toBeInTheDocument()
  })
})

describe('table formatters', () => {
  it('formats ratios as readable percentages', () => {
    expect(formatPercent(0.0013736263736263737)).toBe('0.14%')
    expect(formatPercent(0.25)).toBe('25%')
    expect(formatPercent(0)).toBe('0%')
    expect(formatPercent(undefined)).toBe('—')
    // Near-zero and near-one ratios never round to a misleading 0% or 100%.
    expect(formatPercent(0.000001)).toBe('<0.01%')
    expect(formatPercent(0.99999)).toBe('>99.9%')
    expect(formatPercent(1)).toBe('100%')
  })

  it('formats latencies in readable units', () => {
    expect(formatLatency(6815.031593406594)).toBe('6.8s')
    expect(formatLatency(412.4)).toBe('412ms')
    expect(formatLatency(90_000)).toBe('1m 30s')
    expect(formatLatency(null)).toBe('—')
  })
})
