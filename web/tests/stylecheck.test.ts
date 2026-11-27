import { describe, expect, it } from 'vitest'
// @ts-expect-error plain ESM helper script, not part of the app tsconfig
import { checkDir, fontScale, scanCss, scanNativeControls, scanTsx } from '../scripts/stylecheck.mjs'
import { readFileSync } from 'node:fs'

describe('web style checks', () => {
  it('requires the root element to reserve the scrollbar gutter', () => {
    const missing = scanCss('html { min-width: 320px; }', 'styles/global.css')
    expect(missing.filter((d) => d.rule === 'root-scrollbar-gutter')).toHaveLength(1)
    const present = scanCss('html { min-width: 320px; scrollbar-gutter: stable; }', 'styles/global.css')
    expect(present.filter((d) => d.rule === 'root-scrollbar-gutter')).toHaveLength(0)
    const otherFile = scanCss('.x { display: block; }', 'x.module.css')
    expect(otherFile.filter((d) => d.rule === 'root-scrollbar-gutter')).toHaveLength(0)
  })

  it('requires full-viewport fixed overlays to cover the scrollbar gutter', () => {
    const narrow = scanCss('.overlay { position: fixed; inset: 0; background: var(--overlay); }', 'x.module.css')
    expect(narrow.filter((d) => d.rule === 'overlay-gutter')).toHaveLength(1)
    const wide = scanCss('.overlay { position: fixed; inset: 0; right: auto; width: 100vw; }', 'x.module.css')
    expect(wide.filter((d) => d.rule === 'overlay-gutter')).toHaveLength(0)
    const anchored = scanCss('.toast { position: fixed; right: 16px; bottom: 16px; }', 'x.module.css')
    expect(anchored.filter((d) => d.rule === 'overlay-gutter')).toHaveLength(0)
  })

  it('matches root and overlay variants of the scrollbar gutter rules', () => {
    for (const css of [':root { scrollbar-gutter: stable; }', 'html, body { scrollbar-gutter: stable both-edges; }']) {
      expect(scanCss(css, 'styles/global.css').filter((d) => d.rule === 'root-scrollbar-gutter')).toHaveLength(0)
    }
    expect(scanCss('html { scrollbar-gutter: auto; }', 'styles\\global.css')
      .filter((d) => d.rule === 'root-scrollbar-gutter')).toHaveLength(1)
    for (const css of [
      '.o { position: fixed; inset: 0px; }',
      '.o { position: fixed; inset: 0 0 0 0; }',
      '.o { position: fixed !important; inset: 0 !important; }',
      '.o { position: fixed; top: 0; right: 0; bottom: 0; left: 0; }',
    ]) {
      expect(scanCss(css, 'x.module.css').filter((d) => d.rule === 'overlay-gutter')).toHaveLength(1)
    }
    for (const css of [
      '.o { position: fixed; inset: 0; right: auto; width: 100dvw; }',
      '.o { position: fixed; inset: 0 auto 0 0; width: var(--nav-w); }',
    ]) {
      expect(scanCss(css, 'x.module.css').filter((d) => d.rule === 'overlay-gutter')).toHaveLength(0)
    }
  })

  it('finds no scrollbar gutter violations in the application styles', () => {
    const diagnostics = checkDir('src')
    expect(diagnostics.filter((d) => d.rule === 'root-scrollbar-gutter' || d.rule === 'overlay-gutter')).toEqual([])
  })

  it('finds no color, scale literal or off-scale font size violations in the application styles', () => {
    const diagnostics = checkDir('src')
    expect(diagnostics.filter((d) => ['color-literal', 'scale-literal', 'font-scale'].includes(d.rule))).toEqual([])
  })

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

  it('accepts off-scale spacing literals while the migration continues', () => {
    const diagnostics = scanCss('.x { padding: 14px; gap: 9px; }', 'x.module.css')
    expect(diagnostics.filter((d) => d.rule === 'scale-literal')).toHaveLength(0)
  })

  it('reads the type scale from the --fs-* tokens', () => {
    const scale = fontScale(readFileSync('src/styles/tokens.css', 'utf8'))
    expect([...scale.keys()].sort((a, b) => parseFloat(a) - parseFloat(b)))
      .toEqual(['12px', '13px', '14px', '15px', '16px', '18px', '28px'])
    expect(scale.get('13px')).toEqual(['--fs-body-sm', '--fs-label'])
    // Values of the pre-#601 scale are no longer on it.
    for (const stale of ['11px', '11.5px', '12.5px', '13.5px', '17px', '21px', '22px', '30px']) {
      expect([stale, scale.has(stale)]).toEqual([stale, false])
    }
  })

  it('follows a change to the tokens instead of a fixed list', () => {
    const scale = fontScale(':root { --fs-body: 15px; /* --fs-old: 11px; */ }\n@media (x) { :root { --fs-h1: 30px } }')
    expect([...scale.keys()]).toEqual(['15px', '30px'])
    expect(scanCss('.x { font-size: 30px; }', 'x.module.css', { fontScale: scale })
      .filter((d) => d.rule === 'font-scale')).toHaveLength(0)
    expect(scanCss('.x { font-size: 14px; }', 'x.module.css', { fontScale: scale })
      .filter((d) => d.rule === 'font-scale')).toHaveLength(1)
  })

  it('refuses to run without a type scale', () => {
    expect(() => fontScale(':root { --space-sm: 8px; }')).toThrow(/--fs-/)
  })

  it('rejects an off-scale font size in font-size and the font shorthand', () => {
    for (const css of [
      '.x { font-size: 22px; }',
      '.x { font-size: 11.5px; }',
      '.x { font: 600 22px/1.1 var(--font-mono); }',
      '.x { font: var(--fw-bold) 17px var(--font-sans); }',
    ]) {
      const diagnostics = scanCss(css, 'x.module.css')
      expect([css, diagnostics.filter((d) => d.rule === 'font-scale').length]).toEqual([css, 1])
    }
  })

  it('passes on-scale sizes, and asks a literal for its token by name', () => {
    for (const css of ['.x { font-size: 13px; }', '.x { font: 600 28px/1.1 var(--font-mono); }']) {
      expect([css, scanCss(css, 'x.module.css').filter((d) => d.rule === 'font-scale')]).toEqual([css, []])
    }
    const [literal] = scanCss('.x { font-size: 13px; }', 'x.module.css').filter((d) => d.rule === 'scale-literal')
    expect(literal.message).toContain('var(--fs-body-sm) 或 var(--fs-label)')
  })

  it('accepts type scale tokens and non-px sizes', () => {
    for (const css of [
      '.x { font-size: var(--fs-body); }',
      '.x { font: var(--fw-bold) var(--fs-h2)/1.1 var(--font-mono); }',
      '.x { font: inherit; }',
      '.x { font-size: 0.9em; }',
      '.x { font: 600 var(--fs-body)/20px var(--font-mono); }',
    ]) {
      const diagnostics = scanCss(css, 'x.module.css')
      expect([css, diagnostics]).toEqual([css, []])
    }
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

  it('rejects static styles behind type assertions and non-null wrappers', () => {
    for (const code of [
      '<div style={{ top: 1 } as const} />',
      '<div style={{ top: 1 } satisfies object} />',
      '<div style={{ top: 1 }!} />',
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
      '<div style={{ boxShadow: `#fff ${offset}px 0` }} />',
      '<div style={{ boxShadow: `${first} #fff ${second}` }} />',
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

describe('native control checks', () => {
  it('rejects native selects, page-level disclosures, control inputs and DOM titles', () => {
    const diagnostics = scanNativeControls(`
      <div>
        <select><option value="a">A</option></select>
        <details><summary>更多</summary></details>
        <input type="checkbox" />
        <input type="date" />
        <input type="number" />
        <input type='search' />
        <span title="提示">x</span>
      </div>
    `, 'pages/X.tsx')
    expect(diagnostics.filter((d) => d.rule === 'native-select')).toHaveLength(2)
    expect(diagnostics.filter((d) => d.rule === 'native-details')).toHaveLength(2)
    expect(diagnostics.filter((d) => d.rule === 'native-input-type')).toHaveLength(4)
    expect(diagnostics.filter((d) => d.rule === 'dom-title')).toHaveLength(1)
    expect(diagnostics.every((d) => d.severity === 'error')).toBe(true)
  })

  it('accepts shared primitives and component title props', () => {
    const diagnostics = scanNativeControls(`
      <div>
        <Select value="" options={[]} onChange={() => {}} />
        <Disclosure summary="更多">内容</Disclosure>
        <Checkbox label="启用" />
        <DatePicker value="" onChange={() => {}} />
        <NumberField value="1" onChange={() => {}} />
        <SearchField value="" onChange={() => {}} />
        <Textarea value="" onChange={() => {}} />
        <Panel title="标题" />
      </div>
    `, 'pages/X.tsx')
    expect(diagnostics).toHaveLength(0)
  })

  it('allows the primitive implementation file', () => {
    const code = '<details><summary>x</summary></details><input type="checkbox" /><input type="number" /><input type="search" /><span title="t" />'
    expect(scanNativeControls(code, 'components/ui.tsx')).toHaveLength(0)
    expect(scanNativeControls(code, 'components\\ui.tsx')).toHaveLength(0)
  })

  it('allows text and password inputs', () => {
    const diagnostics = scanNativeControls('<input type="text" /><input type="password" /><input value="x" />', 'pages/X.tsx')
    expect(diagnostics).toHaveLength(0)
  })

  it('finds no native control violations in the application sources', () => {
    const diagnostics = checkDir('src')
    expect(diagnostics.filter((d) => (
      ['native-select', 'native-details', 'native-input-type', 'dom-title'].includes(d.rule)
    ))).toEqual([])
  })
})
