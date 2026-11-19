import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')

// The tab strip is a horizontally scrolling flex row. A flex item shrinks by
// default, so at a narrow width every tab was squeezed below its label and the
// labels overlapped the neighbouring tabs instead of the strip scrolling.
describe('tab strip at narrow widths (#601)', () => {
  it('keeps each tab at its label width so the strip scrolls instead of overlapping', () => {
    const tabRule = css.match(/\.tab\s*\{[^}]*\}/)?.[0] ?? ''
    expect(tabRule).toContain('flex: none')
    expect(tabRule).toContain('white-space: nowrap')
  })

  it('still scrolls the strip horizontally without a vertical scrollbar', () => {
    const tabsRule = css.match(/\.tabs\s*\{[^}]*\}/)?.[0] ?? ''
    expect(tabsRule).toContain('overflow-x: auto')
    expect(tabsRule).toContain('overflow-y: hidden')
  })
})
