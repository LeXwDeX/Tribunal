import { describe, expect, it } from 'vitest'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'

const srcRoot = resolve(process.cwd(), 'src')

// Guards for the design tokens of the #601 console restyle. The palette lives
// in two blocks of styles/tokens.css: :root (light) and :root[data-theme="dark"].
// A var() that no stylesheet defines silently resolves to the property's initial
// value, and a token that exists only in the dark block leaves the light theme
// without it. Neither shows up until someone views that theme.

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry)
    if (statSync(path).isDirectory()) walk(path, out)
    else out.push(path)
  }
  return out
}

// Comments may quote var(--x) or "--x:" as prose; they are not references.
function read(path: string): string {
  return readFileSync(path, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')
}

const files = walk(srcRoot)
const cssFiles = files.filter((path) => path.endsWith('.css'))
const sourceFiles = files.filter((path) => /\.(?:css|tsx?)$/.test(path))

// Custom property names declared anywhere in `css` (`--name:` at a declaration start).
function declaredProperties(css: string): Set<string> {
  const names = new Set<string>()
  for (const match of css.matchAll(/(?:^|[{;\s])(--[\w-]+)\s*:/g)) names.add(match[1])
  return names
}

// Custom properties declared by the innermost rule blocks whose selector passes
// `matches`, including a block nested in an @media rule.
function themeProperties(css: string, matches: (selector: string) => boolean): Set<string> {
  const names = new Set<string>()
  for (const block of css.matchAll(/([^{}]*)\{([^{}]*)\}/g)) {
    if (!matches(block[1].trim())) continue
    for (const name of declaredProperties(block[2])) names.add(name)
  }
  return names
}

const tokensCss = read(join(srcRoot, 'styles', 'tokens.css'))
const light = themeProperties(tokensCss, (selector) => selector === ':root')
const dark = themeProperties(tokensCss, (selector) => /\[data-theme=["']?dark["']?\]/.test(selector))

// The nine tokens every component leans on for surface, text, accent, primary
// action, controls and the neutral label tone.
const CORE_TOKENS = [
  '--bg',
  '--surface',
  '--text',
  '--muted',
  '--accent',
  '--primary-bg',
  '--primary-fg',
  '--control-bg',
  '--info-fg',
]

describe('design tokens (#601)', () => {
  it('defines every custom property that CSS and TSX read through var()', () => {
    // A local --name: inside a module counts as a definition, like tokens.css.
    const defined = new Set<string>()
    for (const file of cssFiles) {
      for (const name of declaredProperties(read(file))) defined.add(name)
    }

    let references = 0
    const undefinedReferences: string[] = []
    for (const file of sourceFiles) {
      for (const match of read(file).matchAll(/var\(\s*(--[\w-]+)/g)) {
        references += 1
        if (!defined.has(match[1])) undefinedReferences.push(`${relative(srcRoot, file)}: ${match[1]}`)
      }
    }

    expect(references).toBeGreaterThan(0)
    expect(undefinedReferences).toEqual([])
  })

  it('adds no token in the dark block that the light block does not define', () => {
    expect(dark.size).toBeGreaterThan(0)
    expect([...dark].filter((name) => !light.has(name))).toEqual([])
  })

  it.each([
    ['light', light],
    ['dark', dark],
  ])('defines the core palette in the %s theme', (_theme, defined) => {
    expect(CORE_TOKENS.filter((name) => !defined.has(name))).toEqual([])
  })
})
