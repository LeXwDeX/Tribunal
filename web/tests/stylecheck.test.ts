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

  it('rejects static inline styles, including string and literal-only conditional values', () => {
    for (const code of [
      '<div style={{ top: 1 }} />',
      "<div style={{ textAlign: 'left' }} />",
      '<div style={{ opacity: true ? 1 : 0 }} />',
      '<div style={true ? { top: 1 } : { top: 2 }} />',
    ]) {
      const diagnostics = scanTsx(code, 'x.tsx')
      expect(diagnostics.filter((d) => d.rule === 'inline-style' && d.severity === 'error')).toHaveLength(1)
    }
  })

  it('allows token-only style objects in both string literal forms', () => {
    for (const code of [
      "<div style={{ color: 'var(--text)' }} />",
      '<div style={{ color: `var(--text)` }} />',
    ]) {
      expect(scanTsx(code, 'x.tsx')).toHaveLength(0)
    }
  })

  it('rejects static styles behind type assertions', () => {
    for (const code of [
      '<div style={{ top: 1 } as const} />',
      '<div style={{ top: 1 } satisfies object} />',
    ]) {
      const diagnostics = scanTsx(code, 'x.tsx')
      expect(diagnostics.filter((d) => d.rule === 'inline-style')).toHaveLength(1)
    }
  })

  it('allows a data-driven style object', () => {
    const diagnostics = scanTsx('<div style={{ top: position }} />', 'x.tsx')
    expect(diagnostics).toHaveLength(0)
  })

  it('ignores dynamic style props', () => {
    const diagnostics = scanTsx('<div style={position} />', 'x.tsx')
    expect(diagnostics).toHaveLength(0)
  })

  it('rejects literal colors in direct JSX attributes', () => {
    const diagnostics = scanTsx('<svg><rect fill="#fff" stroke="currentColor" /></svg>', 'x.tsx')
    expect(diagnostics.filter((d) => d.rule === 'color-literal')).toHaveLength(1)
  })

  it('rejects literal colors in JSX expression trees', () => {
    for (const code of [
      '<svg><rect fill={\'#000\'} /></svg>',
      "<svg><rect fill={('#fff')} /></svg>",
      "<svg><rect fill={active ? '#fff' : '#000'} /></svg>",
      '<div style={{ boxShadow: `${offset}px 0 #fff` }} />',
    ]) {
      const diagnostics = scanTsx(code, 'x.tsx')
      expect(diagnostics.filter((d) => d.rule === 'color-literal').length).toBeGreaterThan(0)
    }
  })

  it('rejects literal colors in style objects and their branches', () => {
    for (const code of [
      "<div style = {{ color: '#fff' }} />",
      "<div style={active ? { color: '#fff' } : { color: '#000' }} />",
    ]) {
      const diagnostics = scanTsx(code, 'x.tsx')
      expect(diagnostics.filter((d) => d.rule === 'color-literal').length).toBeGreaterThan(0)
    }
  })

  it('rejects colors inside a data-driven style object', () => {
    const diagnostics = scanTsx('<div style={{ color: count === 0 ? \'#fff\' : color }} />', 'x.tsx')
    expect(diagnostics.filter((d) => d.rule === 'color-literal')).toHaveLength(1)
    expect(diagnostics.filter((d) => d.rule === 'inline-style')).toHaveLength(0)
  })
})
