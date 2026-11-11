import { describe, expect, it } from 'vitest'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'

const root = resolve(process.cwd(), 'src')

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry)
    if (statSync(path).isDirectory()) walk(path, out)
    else out.push(path)
  }
  return out
}

function classNames(css: string): Set<string> {
  const clean = css.replace(/\/\*[\s\S]*?\*\//g, '')
  const names = new Set<string>()
  for (const match of clean.matchAll(/\.([A-Za-z_][\w-]*)/g)) names.add(match[1])
  return names
}

function moduleImports(tsx: string): Array<{ alias: string; file: string }> {
  const imports: Array<{ alias: string; file: string }> = []
  for (const match of tsx.matchAll(/import\s+(\w+)\s+from\s+['"]([^'"]+\.module\.css)['"]/g)) {
    imports.push({ alias: match[1], file: match[2] })
  }
  return imports
}

describe('CSS module class references', () => {
  it('every styles.<name> reference is defined in the imported module', () => {
    const files = walk(root)
    const cssClasses = new Map<string, Set<string>>()
    for (const file of files.filter((path) => path.endsWith('.module.css'))) {
      cssClasses.set(file, classNames(readFileSync(file, 'utf8')))
    }

    const missing: string[] = []
    for (const file of files.filter((path) => path.endsWith('.tsx'))) {
      const source = readFileSync(file, 'utf8')
      // An import specifier repeats the file stem (e.g. `ui.module.css`), which
      // would otherwise be scanned as the class reference `ui.module`.
      const code = source.replace(/(['"])[^'"]+\.module\.css\1/g, '$1$1')
      for (const imported of moduleImports(source)) {
        const cssFile = resolve(dirname(file), imported.file)
        const defined = cssClasses.get(cssFile)
        if (!defined) continue
        for (const match of code.matchAll(new RegExp(`\\b${imported.alias}\\.([A-Za-z_][\\w-]*)`, 'g'))) {
          const name = match[1]
          if (!defined.has(name)) missing.push(`${file}: ${imported.alias}.${name}`)
        }
      }
    }

    expect(missing).toEqual([])
  })

  it('defines the shared form control classes used by pages', () => {
    const ui = classNames(readFileSync(join(root, 'styles/ui.module.css'), 'utf8'))
    expect([...ui].filter((name) => ['input', 'select', 'textarea'].includes(name)).sort())
      .toEqual(['input', 'select', 'textarea'])
  })
})