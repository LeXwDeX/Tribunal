import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

// Colour contrast of the design tokens (#657). jsdom cannot paint, so axe's
// color-contrast rule is disabled in tests/a11y-axe.test.tsx; this file checks
// the same WCAG 2.1 AA thresholds on the token pairs the components use:
// 4.5:1 for text (1.4.3) and 3:1 for the non-text parts that identify a
// control or its state (1.4.11). No pair relies on the 3:1 large-text
// allowance. The light palette is :root, the dark palette is
// :root[data-theme="dark"] over the light values.

type Rgba = [number, number, number, number]

const tokensCss = readFileSync(resolve(process.cwd(), 'src/styles/tokens.css'), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, '')

function block(selector: string): Map<string, string> {
  const start = tokensCss.indexOf(`${selector} {`)
  if (start === -1) throw new Error(`${selector} block missing from tokens.css`)
  const open = tokensCss.indexOf('{', start)
  const body = tokensCss.slice(open + 1, tokensCss.indexOf('}', open))
  const props = new Map<string, string>()
  for (const match of body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) props.set(match[1], match[2].trim())
  return props
}

const light = block(':root')
const THEMES = {
  light,
  dark: new Map([...light, ...block(':root[data-theme="dark"]')]),
}

function parseColor(value: string): Rgba {
  const hex = value.match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/i)
  if (hex) {
    const digits = hex[1].length === 3 ? hex[1].replace(/./g, '$&$&') : hex[1]
    return [0, 2, 4].map((i) => parseInt(digits.slice(i, i + 2), 16)).concat(1) as Rgba
  }
  const rgb = value.match(/^rgba?\(([^)]+)\)$/)
  if (rgb) {
    const [r, g, b, a = 1] = rgb[1].split(',').map((part) => Number(part.trim()))
    return [r, g, b, a]
  }
  throw new Error(`unsupported colour ${value}`)
}

// Alpha colours (dark badge tints, the focus halo) are composited over what
// they are painted on.
function over(fg: Rgba, bg: Rgba): Rgba {
  const alpha = fg[3]
  return [0, 1, 2].map((i) => fg[i] * alpha + bg[i] * (1 - alpha)).concat(1) as Rgba
}

function luminance([r, g, b]: Rgba): number {
  const [lr, lg, lb] = [r, g, b].map((channel) => {
    const c = channel / 255
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * lr + 0.7152 * lg + 0.0722 * lb
}

function contrast(a: Rgba, b: Rgba): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

function tokenContrast(theme: Map<string, string>, fg: string, bg: string): number {
  const read = (name: string) => {
    const value = theme.get(`--${name}`)
    if (!value) throw new Error(`--${name} missing`)
    return parseColor(value)
  }
  const page = read('surface')
  const background = over(read(bg), page)
  return contrast(over(read(fg), background), background)
}

const SURFACES = ['bg', 'surface', 'surface-2', 'surface-3']

// Text: body copy, muted and secondary text, labels, badges, buttons and code.
const TEXT_PAIRS: Array<[string, string]> = [
  ...SURFACES.map((bg): [string, string] => ['text', bg]),
  ['text', 'control-bg'],
  ...SURFACES.map((bg): [string, string] => ['muted', bg]),
  ...SURFACES.map((bg): [string, string] => ['muted-strong', bg]),
  ['link', 'surface'],
  ['nav-text', 'nav'], ['nav-text', 'nav-hover'], ['nav-muted', 'nav'], ['nav-strong', 'nav-active'], ['nav-accent-fg', 'nav'],
  ['primary-fg', 'primary-bg'], ['primary-fg', 'primary-hover'], ['on-brand', 'brand'],
  ['on-accent', 'accent'], ['accent', 'surface'], ['accent', 'accent-soft'],
  ['on-danger-solid', 'danger-solid'], ['on-danger-solid', 'danger-solid-hover'],
  ['ok-fg', 'ok-bg'], ['warn-fg', 'warn-bg'], ['danger-fg', 'danger-bg'], ['info-fg', 'info-bg'], ['neutral-fg', 'neutral-bg'],
  ['code-fg', 'code-bg'],
  // Status text on the plain surface, including the verdict mark: it is 18px
  // at weight 600, below WCAG large text (24px, or 18.66px bold), so it needs 4.5:1.
  ...['ok-fg', 'warn-fg', 'danger-fg', 'info-fg'].map((fg): [string, string] => [fg, 'surface']),
]


// Non-text: control outlines (inputs, selects, checkboxes, secondary buttons,
// the calendar's today mark), the focus outline and input focus border
// (--accent), and status dots. Not listed on purpose: --border and
// --border-strong only paint dividers and container edges, which WCAG 1.4.11
// does not require; --accent-ring is a halo drawn outside an --accent focus
// border, so the border carries the 3:1 focus indication.
const NON_TEXT_PAIRS: Array<[string, string]> = [
  ...[...SURFACES, 'control-bg'].map((bg): [string, string] => ['control-border', bg]),
  ...[...SURFACES, 'control-bg', 'nav'].map((bg): [string, string] => ['accent', bg]),
  ...['ok-dot', 'warn-dot', 'danger-dot', 'info-dot', 'neutral-dot'].map((fg): [string, string] => [fg, 'surface']),
]

describe('contrast helpers', () => {
  it('matches the WCAG reference values', () => {
    expect(contrast(parseColor('#000'), parseColor('#fff'))).toBeCloseTo(21, 5)
    expect(contrast(parseColor('#777777'), parseColor('#ffffff'))).toBeCloseTo(4.48, 2)
    expect(over(parseColor('rgba(0, 0, 0, 0.5)'), parseColor('#ffffff'))).toEqual([127.5, 127.5, 127.5, 1])
  })
})

describe.each(Object.entries(THEMES))('%s theme token contrast (#657)', (_name, theme) => {
  it.each(TEXT_PAIRS)('text --%s on --%s is at least 4.5:1', (fg, bg) => {
    expect(tokenContrast(theme, fg, bg)).toBeGreaterThanOrEqual(4.5)
  })

  it.each(NON_TEXT_PAIRS)('non-text --%s on --%s is at least 3:1', (fg, bg) => {
    expect(tokenContrast(theme, fg, bg)).toBeGreaterThanOrEqual(3)
  })
})
