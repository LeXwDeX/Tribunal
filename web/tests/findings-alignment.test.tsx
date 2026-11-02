import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { FindingRow } from '../src/pages/ReviewDetailPage'
import { finding } from './fixtures'

// The bundled stylesheet emits global.css after CSS Modules, so .dataTable td
// and .findingsTable td have the same specificity and the later global rule
// wins. This assertion pins a selector that is stronger than the global rule
// without changing the shared table styles.
describe('findings row alignment', () => {
  it('centers all data cells against the same row middle line', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/features/review/review.module.css'), 'utf8')
    const rules = css.match(/\.findingsTable[^{]*\{[^}]*\}/g) ?? []
    expect(rules.length).toBeGreaterThan(0)
    const cellRule = rules.find((rule) => /^\.findingsTable\s+tbody\s+td\s*\{/.test(rule))
    expect(cellRule).toContain('vertical-align: middle')
    expect(rules.some((rule) => rule.includes('vertical-align: top'))).toBe(false)
  })

  it('renders a long title, downgraded severity and annotation in one four-cell row', () => {
    const longTitle = `并发写 map 未加锁${'，缺少同步保护'.repeat(40)}`
    render(
      <table className="dataTable">
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
      </table>,
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
