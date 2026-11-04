import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const COLOR_RE = /#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(/g
const INLINE_STYLE_RE = /\bstyle=\{\{/g
const SCALE_RE = /(font-size|gap|padding(?:-[a-z]+)?|margin(?:-[a-z]+)?)\s*:\s*([^;{}]+)/g
const SPACE_VALUES = new Set(['1px', '4px', '6px', '8px', '12px', '16px', '20px', '24px', '32px', '48px'])
const FONT_VALUES = new Set(['11px', '11.5px', '12px', '12.5px', '13.5px', '14px', '15px', '17px', '21px', '22px', '30px'])

function lineOf(text, index) {
  return text.slice(0, index).split('\n').length
}

export function scanCss(text, file) {
  const diagnostics = []
  for (const match of text.matchAll(COLOR_RE)) {
    diagnostics.push({
      file,
      line: lineOf(text, match.index),
      rule: 'color-literal',
      severity: 'error',
      message: `颜色字面值 ${match[0]}；请使用 tokens.css 中的颜色变量`,
    })
  }
  for (const match of text.matchAll(SCALE_RE)) {
    const value = match[2]
    const literal = value.match(/\b(?:\d+(?:\.\d+)?px)\b/)
    if (!literal) continue
    const allowed = match[1] === 'font-size' ? FONT_VALUES : SPACE_VALUES
    if (!allowed.has(literal[0])) continue
    diagnostics.push({
      file,
      line: lineOf(text, match.index),
      rule: 'scale-literal',
      severity: 'error',
      message: `${match[1]} 使用了设计刻度内的字面值 ${literal[0]}；请使用 tokens.css 中的变量`,
    })
  }
  return diagnostics
}

export function scanTsx(text, file) {
  const diagnostics = []
  for (const match of text.matchAll(INLINE_STYLE_RE)) {
    diagnostics.push({
      file,
      line: lineOf(text, match.index),
      rule: 'inline-style',
      severity: 'warning',
      message: '内联 style；只允许由数据计算的值，静态样式请放进 CSS Module',
    })
  }
  return diagnostics
}

function walk(dir) {
  const out = []
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry)
    if (statSync(path).isDirectory()) out.push(...walk(path))
    else if (entry.endsWith('.css') || entry.endsWith('.tsx')) out.push(path)
  }
  return out
}

export function checkDir(root) {
  const diagnostics = []
  for (const path of walk(root)) {
    const text = readFileSync(path, 'utf8')
    const file = relative(root, path)
    if (path.endsWith('.css')) {
      if (path.endsWith('tokens.css')) continue
      diagnostics.push(...scanCss(text, file))
    } else {
      diagnostics.push(...scanTsx(text, file))
    }
  }
  return diagnostics
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const root = process.argv[2] || 'src'
  const diagnostics = checkDir(root)
  for (const d of diagnostics) {
    const label = d.severity === 'error' ? 'error' : 'warning'
    console.error(`${d.file}:${d.line}: ${label}: [${d.rule}] ${d.message}`)
  }
  const errors = diagnostics.filter((d) => d.severity === 'error').length
  const warnings = diagnostics.length - errors
  console.log(`webstylecheck: ${errors} error(s), ${warnings} warning(s)`)
  process.exit(errors ? 1 : 0)
}
