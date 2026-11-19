import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { DataTable } from '../src/components/ui'
import { FindingRow } from '../src/pages/ReviewDetailPage'
import { finding } from './fixtures'

// The findings table renders through the shared DataTable, whose cells are all
// vertically centred. The review module must neither restyle table elements
// nor top-align cells again.
describe('findings row alignment', () => {
  it('leaves cell alignment to the shared table styles', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/features/review/review.module.css'), 'utf8')
    expect(css).not.toContain('.findingsTable')
    expect(css).not.toContain('vertical-align: top')
    const shared = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')
    expect(shared).toMatch(/\.table td \{[^}]*vertical-align: middle/)
  })

  it('renders a long title, downgraded severity and annotation in one four-cell row', () => {
    const longTitle = `并发写 map 未加锁${'，缺少同步保护'.repeat(40)}`
    render(
      <DataTable label="问题列表">
        <tbody>
          <FindingRow
            finding={finding({
              title: longTitle,
              severity: 'P1',
              final_severity: 'P2',
              annotation: 'invalid',
              path: 'internal/very/long/path/with/a/long/module/name.ts',
              line: 120,
            })}
            index={0}
            expanded={false}
            onToggle={() => {}}
          />
        </tbody>
      </DataTable>,
    )

    const rows = screen.getAllByRole('row')
    expect(rows).toHaveLength(1)
    const cells = within(rows[0]).getAllByRole('cell')
    expect(cells).toHaveLength(4)
    expect(cells[0]).toHaveTextContent('P2')
    expect(cells[0]).toHaveTextContent('原级别 P1')
    expect(cells[1]).toHaveTextContent(longTitle)
    expect(cells[1]).toHaveTextContent('internal/very/long/path/with/a/long/module/name.ts:120')
    expect(cells[2]).toHaveTextContent('review.a')
    expect(cells[2]).toHaveTextContent('CONFIRMED')
    expect(cells[2]).toHaveTextContent('发布')
    expect(cells[3]).toHaveTextContent('无效')
    for (const cell of cells) {
      expect(cell).not.toHaveAttribute('style')
    }
    expect(within(cells[1]).getByRole('button', { name: `展开：${longTitle}` })).toHaveAttribute('aria-expanded', 'false')
  })
})
