import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const COLOR_RE = /#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(/g
const INLINE_STYLE_RE = /\bstyle=\{\{/g
const JSX_COLOR_RE = /(?:fill|stroke|color|bgcolor)\s*=\s*["']\s*(#[0-9a-fA-F]{3,8}|rgba?\([^)]*\)|hsla?\([^)]*\))/g
const SPACE_VALUES = new Set(['1px', '4px', '6px', '8px', '12px', '16px', '20px', '24px', '32px', '48px'])
const SPACE_PROPS = /^(gap|padding(?:-[a-z]+)?|margin(?:-[a-z]+)?)$/
const TOKENS_PATH = join(dirname(fileURLToPath(import.meta.url)), '../src/styles/tokens.css')

// fontScale reads the type scale from the --fs-* custom properties of
// tokens.css, so the guard cannot drift from the tokens it enforces. It maps
// each px value to the token names that define it.
export function fontScale(tokensText) {
  const scale = new Map()
  for (const match of withoutComments(tokensText).matchAll(/(--fs-[\w-]+)\s*:\s*(\d+(?:\.\d+)?px)\s*[;}]/g)) {
    const names = scale.get(match[2]) || []
    if (!names.includes(match[1])) names.push(match[1])
    scale.set(match[2], names)
  }
  if (scale.size === 0) throw new Error('tokens.css 中没有找到 --fs-* 字号变量')
  return scale
}

let defaultFontScale
function loadFontScale() {
  defaultFontScale ??= fontScale(readFileSync(TOKENS_PATH, 'utf8'))
  return defaultFontScale
}

// fontSizeLiterals returns the px font sizes of a font-size or font shorthand
// value. In the shorthand a size may carry a line-height after a slash (with or
// without spaces around it), which is not a font size.
function fontSizeLiterals(prop, value) {
  if (prop === 'font-size') return [...value.matchAll(/\b\d+(?:\.\d+)?px\b/g)].map((match) => match[0])
  // Join a spaced slash first so "14px / 20px" keeps 20px on the line-height side.
  return value.replace(/\s*\/\s*/g, '/').split(/\s+/)
    .map((part) => part.split('/')[0])
    .filter((part) => /^\d+(?:\.\d+)?px$/.test(part))
}

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
      if (colon > 0) props.set(part.slice(0, colon).trim(), part.slice(colon + 1).replace(/!important/, '').trim())
    }
    out.push({ selector: match[1].trim(), props, index: match.index + match[1].length })
  }
  return out
}

function isZero(value) {
  return /^0(?:px)?$/.test(value || '')
}

function edges(props) {
  // Resolve inset shorthand and longhands to [top, right, bottom, left].
  const box = (props.get('inset') || '').split(/\s+/).filter(Boolean)
  const [top, right = top, bottom = top, left = right] = box
  return [
    props.get('top') ?? top,
    props.get('right') ?? right,
    props.get('bottom') ?? bottom,
    props.get('left') ?? left,
  ]
}

function scanScrollbarGutter(clean, file) {
  // A vertical scrollbar that appears or disappears with content height must
  // not move centered content, so the root reserves its gutter and every
  // full-viewport fixed overlay is widened to cover that gutter.
  const diagnostics = []
  const rules = blocks(clean)
  if (/(?:^|[\\/])global\.css$/.test(file)) {
    const reserved = rules.some((rule) => rule.selector.split(',').some((part) => ['html', ':root'].includes(part.trim()))
      && /^stable\b/.test(rule.props.get('scrollbar-gutter') || ''))
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
    if (rule.props.get('position') !== 'fixed') continue
    if (!edges(rule.props).every(isZero)) continue
    if (/^100[dsl]?vw$/.test(rule.props.get('width') || '')) continue
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

export function scanCss(text, file, options = {}) {
  const clean = withoutComments(text)
  const diagnostics = scanScrollbarGutter(clean, file)
  const scale = options.fontScale || loadFontScale()
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
    if (decl.prop === 'font-size' || decl.prop === 'font') {
      // A size on the scale must name its token; a size off the scale has no
      // token and breaks the type scale.
      for (const size of fontSizeLiterals(decl.prop, decl.value)) {
        const tokens = scale.get(size)
        diagnostics.push(tokens ? {
          file,
          line: lineOf(clean, decl.index),
          rule: 'scale-literal',
          severity: 'error',
          message: `${decl.prop} 使用了设计刻度内的字面值 ${size}；请使用 ${tokens.map((name) => `var(${name})`).join(' 或 ')}`,
        } : {
          file,
          line: lineOf(clean, decl.index),
          rule: 'font-scale',
          severity: 'error',
          message: `${decl.prop} 的 ${size} 不在 tokens.css 的 --fs-* 字号刻度内（${[...scale.keys()].sort((a, b) => parseFloat(a) - parseFloat(b)).join('、')}）；请改用刻度变量`,
        })
      }
    } else if (SPACE_PROPS.test(decl.prop)) {
      for (const match of decl.value.matchAll(/\b(?:\d+(?:\.\d+)?px)\b/g)) {
        if (!SPACE_VALUES.has(match[0])) continue
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

const NATIVE_CONTROL_TAGS = new Map([
  ['select', 'native-select'],
  ['option', 'native-select'],
  ['details', 'native-details'],
  ['summary', 'native-details'],
])
const NATIVE_INPUT_TYPES = new Set(['checkbox', 'date', 'number', 'search'])

// components/ui.tsx owns the primitives: Checkbox/NumberField/SearchField
// render native inputs and Disclosure renders details/summary on purpose.
// Add a file here only when the native control is the documented exception.
export const NATIVE_CONTROL_ALLOWLIST = new Map([
  ['components/ui.tsx', new Set(['native-select', 'native-details', 'native-input-type', 'dom-title'])],
])

function allowNativeControl(file, rule) {
  const rules = NATIVE_CONTROL_ALLOWLIST.get(file.replace(/\\/g, '/'))
  return Boolean(rules && rules.has(rule))
}

function intrinsicTagName(element) {
  const tagName = element.tagName
  if (!tagName || !ts.isIdentifier(tagName)) return undefined
  return /^[a-z]/.test(tagName.text) ? tagName.text : undefined
}

function literalAttributeValue(attribute) {
  const initializer = attribute.initializer
  if (!initializer) return undefined
  if (ts.isStringLiteral(initializer)) return initializer.text
  if (ts.isJsxExpression(initializer) && initializer.expression) {
    const expression = initializer.expression
    if (ts.isStringLiteral(expression) || ts.isNoSubstitutionTemplateLiteral(expression)) return expression.text
  }
  return undefined
}

export function scanNativeControls(text, file) {
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const diagnostics = []
  const lineOfNode = (node) => source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1
  const report = (node, rule, message) => {
    if (allowNativeControl(file, rule)) return
    diagnostics.push({ file, line: lineOfNode(node), rule, severity: 'error', message })
  }

  const visit = (node) => {
    const element = ts.isJsxElement(node) ? node.openingElement : ts.isJsxSelfClosingElement(node) ? node : null
    if (element) {
      const tag = intrinsicTagName(element)
      if (tag) {
        const tagRule = NATIVE_CONTROL_TAGS.get(tag)
        if (tagRule) {
          report(element, tagRule, tagRule === 'native-select'
            ? '禁止原生 <select>/<option>；请使用 components/ui.tsx 的 Select'
            : '禁止页面级 <details>/<summary>；请使用 Disclosure')
        }
        const attributes = element.attributes.properties.filter(ts.isJsxAttribute)
        if (tag === 'input') {
          const typeAttribute = attributes.find((attribute) => attribute.name.text === 'type')
          const type = typeAttribute ? literalAttributeValue(typeAttribute) : undefined
          if (type && NATIVE_INPUT_TYPES.has(type)) {
            report(typeAttribute, 'native-input-type',
              `禁止原生 input type="${type}"；请使用 Checkbox、DatePicker、NumberField 或 SearchField`)
          }
        }
        const titleAttribute = attributes.find((attribute) => attribute.name.text === 'title')
        if (titleAttribute) {
          report(titleAttribute, 'dom-title', '禁止 intrinsic DOM title；请使用 Tooltip 或删除')
        }
      }
    }
    ts.forEachChild(node, visit)
  }

  visit(source)
  return diagnostics
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
  // The scanned tree carries its own tokens; check against those.
  const tokensPath = join(root, 'styles/tokens.css')
  const options = existsSync(tokensPath) ? { fontScale: fontScale(readFileSync(tokensPath, 'utf8')) } : {}
  for (const path of walk(root)) {
    const text = readFileSync(path, 'utf8')
    const file = relative(root, path)
    if (path.endsWith('.css')) {
      if (path.endsWith('tokens.css')) continue
      diagnostics.push(...scanCss(text, file, options))
    } else {
      diagnostics.push(...scanTsx(text, file))
      diagnostics.push(...scanNativeControls(text, file))
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
