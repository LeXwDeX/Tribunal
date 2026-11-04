import { describe, expect, it } from 'vitest'
// @ts-expect-error plain ESM helper script, not part of the app tsconfig
import { scanCss, scanTsx } from '../scripts/stylecheck.mjs'

describe('web style checks', () => {
  it('rejects color literals in component CSS', () => {
    const diagnostics = scanCss('.x { color: #ffffff; background: rgba(0, 0, 0, .2); }', 'x.module.css')
    expect(diagnostics.filter((d) => d.rule === 'color-literal')).toHaveLength(2)
  })

  it('accepts color variables', () => {
    const diagnostics = scanCss('.x { color: var(--text); background: var(--surface); }', 'x.module.css')
    expect(diagnostics.filter((d) => d.rule === 'color-literal')).toHaveLength(0)
  })

  it('rejects every scale literal in a shorthand declaration', () => {
    const diagnostics = scanCss('.x { font-size: 14px; padding: 16px 20px; gap: 8px; }', 'x.module.css')
    expect(diagnostics.filter((d) => d.rule === 'scale-literal')).toHaveLength(4)
  })

  it('ignores id selectors and comments', () => {
    const diagnostics = scanCss('#abc { display: block; } /* color: #fff */', 'x.module.css')
    expect(diagnostics).toHaveLength(0)
  })

  it('accepts off-scale literals while the migration continues', () => {
    const diagnostics = scanCss('.x { padding: 14px; gap: 9px; }', 'x.module.css')
    expect(diagnostics.filter((d) => d.rule === 'scale-literal')).toHaveLength(0)
  })

  it('rejects static inline styles', () => {
    const diagnostics = scanTsx('<div style={{ top: 1 }} />', 'x.tsx')
    expect(diagnostics).toHaveLength(1)
    expect(diagnostics[0].severity).toBe('error')
    expect(diagnostics[0].rule).toBe('inline-style')
  })

  it('allows a data-driven style object', () => {
    const diagnostics = scanTsx('<div style={{ top: position }} />', 'x.tsx')
    expect(diagnostics).toHaveLength(0)
  })

  it('ignores dynamic style props', () => {
    const diagnostics = scanTsx('<div style={position} />', 'x.tsx')
    expect(diagnostics).toHaveLength(0)
  })

  it('rejects literal colors in JSX attributes and expressions', () => {
    const diagnostics = scanTsx('<svg><rect fill="#fff" /><rect fill={\'#000\'} stroke="currentColor" /></svg>', 'x.tsx')
    expect(diagnostics.filter((d) => d.rule === 'color-literal')).toHaveLength(2)
  })

  it('rejects colors inside a data-driven style object', () => {
    const diagnostics = scanTsx('<div style={{ color: count === 0 ? \'#fff\' : color }} />', 'x.tsx')
    expect(diagnostics.filter((d) => d.rule === 'color-literal')).toHaveLength(1)
    expect(diagnostics.filter((d) => d.rule === 'inline-style')).toHaveLength(0)
  })
})
