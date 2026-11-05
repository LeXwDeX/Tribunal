import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const COLOR_RE = /#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(/g
const INLINE_STYLE_RE = /\bstyle=\{\{/g
const JSX_COLOR_RE = /(?:fill|stroke|color|bgcolor)\s*=\s*["']\s*(#[0-9a-fA-F]{3,8}|rgba?\([^)]*\)|hsla?\([^)]*\))/g
const SPACE_VALUES = new Set(['1px', '4px', '6px', '8px', '12px', '16px', '20px', '24px', '32px', '48px'])
const FONT_VALUES = new Set(['11px', '11.5px', '12px', '12.5px', '13.5px', '14px', '15px', '17px', '21px', '22px', '30px'])
const SPACE_PROPS = /^(gap|padding(?:-[a-z]+)?|margin(?:-[a-z]+)?)$/

function lineOf(text, index) {
  return text.slice(0, index).split('\n').length
}

function withoutComments(text) {
  return text.replace(/\/\*[\s\S]*?\*\//g, (comment) => comment.replace(/[^\n]/g, ' '))
}

function declarations(text) {
  // Only declaration blocks: skip selectors, at-rule preludes and comments.
  const out = []
  for (const match of text.matchAll(/\{([^{}]*)\}/g)) {
    const body = match[1]
    const bodyStart = match.index + 1
    const parts = body.split(';')
    let offset = 0
    for (const part of parts) {
      const colon = part.indexOf(':')
      if (colon > 0) {
        out.push({
          prop: part.slice(0, colon).trim(),
          value: part.slice(colon + 1).trim(),
          index: bodyStart + offset + colon + 1,
        })
      }
      offset += part.length + 1
    }
  }
  return out
}

function blocks(text) {
  // Innermost rule blocks with the selector text that precedes them.
  const out = []
  for (const match of text.matchAll(/([^{}]*)\{([^{}]*)\}/g)) {
    const props = new Map()
    for (const part of match[2].split(';')) {
      const colon = part.indexOf(':')
      if (colon > 0) props.set(part.slice(0, colon).trim(), part.slice(colon + 1).trim())
    }
    out.push({ selector: match[1].trim(), props, index: match.index + match[1].length })
  }
  return out
}

function scanScrollbarGutter(clean, file) {
  // A vertical scrollbar that appears or disappears with content height must
  // not move centered content, so the root reserves its gutter and every
  // full-viewport fixed overlay is widened to cover that gutter.
  const diagnostics = []
  const rules = blocks(clean)
  if (file === 'global.css' || file.endsWith('/global.css')) {
    const reserved = rules.some((rule) => rule.selector === 'html'
      && /\bstable\b/.test(rule.props.get('scrollbar-gutter') || ''))
    if (!reserved) {
      diagnostics.push({
        file,
        line: 1,
        rule: 'root-scrollbar-gutter',
        severity: 'error',
        message: 'html 必须声明 scrollbar-gutter: stable，否则页面会随纵向滚动条出现/消失横向跳动',
      })
    }
  }
  for (const rule of rules) {
    if (rule.props.get('position') !== 'fixed' || rule.props.get('inset') !== '0') continue
    if (rule.props.get('width') === '100vw') continue
    diagnostics.push({
      file,
      line: lineOf(clean, rule.index),
      rule: 'overlay-gutter',
      severity: 'error',
      message: '全屏 fixed 遮罩需加 right: auto; width: 100vw，以覆盖根元素预留的滚动条槽',
    })
  }
  return diagnostics
}

export function scanCss(text, file) {
  const clean = withoutComments(text)
  const diagnostics = scanScrollbarGutter(clean, file)
  for (const decl of declarations(clean)) {
    for (const match of decl.value.matchAll(COLOR_RE)) {
      diagnostics.push({
        file,
        line: lineOf(clean, decl.index),
        rule: 'color-literal',
        severity: 'error',
        message: `颜色字面值 ${match[0]}；请使用 tokens.css 中的颜色变量`,
      })
    }
    if (decl.prop === 'font-size' || SPACE_PROPS.test(decl.prop)) {
      const allowed = decl.prop === 'font-size' ? FONT_VALUES : SPACE_VALUES
      for (const match of decl.value.matchAll(/\b(?:\d+(?:\.\d+)?px)\b/g)) {
        if (!allowed.has(match[0])) continue
        diagnostics.push({
          file,
          line: lineOf(clean, decl.index),
          rule: 'scale-literal',
          severity: 'error',
          message: `${decl.prop} 使用了设计刻度内的字面值 ${match[0]}；请使用 tokens.css 中的变量`,
        })
      }
    }
  }
  return diagnostics
}

function isTokenValue(node) {
  return (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node))
    && /^var\(--[\w-]+\)$/.test(node.text)
}

function isStaticValue(node) {
  if (!node) return false
  if (isTokenValue(node)) return true
  if (ts.isNumericLiteral(node) || ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return true
  if (node.kind === ts.SyntaxKind.TrueKeyword || node.kind === ts.SyntaxKind.FalseKeyword) return true
  if (ts.isPrefixUnaryExpression(node)) return isStaticValue(node.operand)
  if (ts.isParenthesizedExpression(node) || ts.isAsExpression(node) || ts.isSatisfiesExpression(node) || ts.isNonNullExpression(node)) {
    return isStaticValue(node.expression)
  }
  if (ts.isObjectLiteralExpression(node)) {
    return node.properties.length > 0 && node.properties.every((property) =>
      ts.isPropertyAssignment(property) && isStaticValue(property.initializer))
  }
  if (ts.isArrayLiteralExpression(node)) return node.elements.every((element) => isStaticValue(element))
  if (ts.isConditionalExpression(node)) return isStaticValue(node.whenTrue) && isStaticValue(node.whenFalse)
  return false
}

function isTokenOnly(node) {
  if (!node) return false
  if (isTokenValue(node)) return true
  if (ts.isParenthesizedExpression(node) || ts.isAsExpression(node) || ts.isSatisfiesExpression(node) || ts.isNonNullExpression(node)) {
    return isTokenOnly(node.expression)
  }
  if (ts.isConditionalExpression(node)) return isTokenOnly(node.whenTrue) && isTokenOnly(node.whenFalse)
  if (ts.isObjectLiteralExpression(node)) {
    return node.properties.length > 0 && node.properties.every((property) =>
      ts.isPropertyAssignment(property) && isTokenOnly(property.initializer))
  }
  return false
}

function colorInText(text) {
  return /#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(/.test(text)
}

export function scanTsx(text, file) {
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const diagnostics = []
  const lineOfNode = (node) => source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1

  const collectColors = (node) => {
    if (!node) return
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)
      || node.kind === ts.SyntaxKind.TemplateHead
      || node.kind === ts.SyntaxKind.TemplateMiddle
      || node.kind === ts.SyntaxKind.TemplateTail) {
      if (colorInText(node.text)) {
        diagnostics.push({
          file,
          line: lineOfNode(node),
          rule: 'color-literal',
          severity: 'error',
          message: `JSX 颜色字面值 ${node.text}；请使用 tokens.css 中的颜色变量`,
        })
      }
    }
    ts.forEachChild(node, collectColors)
  }

  const visit = (node) => {
    if (ts.isJsxAttribute(node) && node.initializer) {
      const initializer = node.initializer
      if (node.name.text === 'style') {
        if (ts.isJsxExpression(initializer) && initializer.expression) {
          collectColors(initializer.expression)
          if (isStaticValue(initializer.expression) && !isTokenOnly(initializer.expression)) {
            diagnostics.push({
              file,
              line: lineOfNode(node),
              rule: 'inline-style',
              severity: 'error',
              message: '静态内联 style；请放进 CSS Module；只有由数据计算的值可以保留',
            })
          }
        } else if (ts.isStringLiteral(initializer)) {
          collectColors(initializer)
        }
      } else if (['fill', 'stroke', 'color', 'bgcolor'].includes(node.name.text)) {
        if (ts.isJsxExpression(initializer) && initializer.expression) collectColors(initializer.expression)
        else if (ts.isStringLiteral(initializer)) collectColors(initializer)
      }
    }
    ts.forEachChild(node, visit)
  }

  visit(source)
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
