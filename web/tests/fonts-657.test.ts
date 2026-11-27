import { describe, expect, it } from 'vitest'
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'

// The console self-hosts its Latin faces from styles/fonts.css (#657): woff2
// only, the weights the stylesheets use, font-display: swap, and the files
// shipped by the pinned @fontsource packages.

const root = process.cwd()
const fontsCss = readFileSync(resolve(root, 'src/styles/fonts.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')
const tokensCss = readFileSync(resolve(root, 'src/styles/tokens.css'), 'utf8')
const faces = [...fontsCss.matchAll(/@font-face\s*\{([^}]*)\}/g)].map((match) => {
  const props = new Map<string, string>()
  for (const declaration of match[1].split(';')) {
    const colon = declaration.indexOf(':')
    if (colon > 0) props.set(declaration.slice(0, colon).trim(), declaration.slice(colon + 1).trim())
  }
  return props
})

function firstFamily(token: string): string {
  const value = tokensCss.match(new RegExp(`${token}:\\s*"([^"]+)"`))
  if (!value) throw new Error(`${token} missing from tokens.css`)
  return value[1]
}

// Fontsource's latin range minus U+2000-206F (General Punctuation). Chinese UI
// text uses those marks (“ ” ‘ ’ — …); served from Plex they would mix Latin
// widths into CJK lines, so they fall through to the system CJK font instead.
const LATIN_RANGE = 'U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, '
  + 'U+0329, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD'

describe('self-hosted fonts (#657)', () => {
  it('declares the first family of each font stack at 400, 500 and 600', () => {
    const declared = faces.map((face) => `${face.get('font-family')} ${face.get('font-weight')}`).sort()
    const families = [firstFamily('--font-sans'), firstFamily('--font-mono')]
    expect(declared).toEqual(families.flatMap((family) => ['400', '500', '600'].map((weight) => `"${family}" ${weight}`)).sort())
  })

  it('uses only woff2 sources from the @fontsource packages, with swap and the latin range', () => {
    for (const face of faces) {
      const src = face.get('src') ?? ''
      const urls = [...src.matchAll(/url\("([^"]+)"\)/g)].map((match) => match[1])
      expect(urls).toHaveLength(1)
      expect(urls[0]).toMatch(/^@fontsource\/[\w-]+\/files\/[\w-]+-latin-\d00-normal\.woff2$/)
      expect(src).toContain('format("woff2")')
      expect(existsSync(resolve(root, 'node_modules', urls[0]))).toBe(true)
      expect(face.get('font-display')).toBe('swap')
      expect(face.get('font-style')).toBe('normal')
      expect(face.get('unicode-range')).toBe(LATIN_RANGE)
    }
  })

  it('ships the six woff2 files and no woff fallback in web/dist', () => {
    const assets = readdirSync(resolve(root, 'dist/assets'))
    expect(assets.filter((name) => name.endsWith('.woff'))).toEqual([])
    expect(assets.filter((name) => name.endsWith('.woff2'))).toHaveLength(6)
    const css = assets.filter((name) => name.endsWith('.css'))
      .map((name) => readFileSync(resolve(root, 'dist/assets', name), 'utf8')).join('\n')
    expect(css.match(/@font-face/g)).toHaveLength(6)
    expect(css).not.toMatch(/\.woff\)/)
    expect(css).not.toContain('U+2000-206F')
  })
})
