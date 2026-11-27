import { describe, expect, it } from 'vitest'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'

const srcRoot = resolve(process.cwd(), 'src')

function withoutComments(text: string): string {
  return text.replace(/\/\*[\s\S]*?\*\//g, '')
}

const uiCss = withoutComments(readFileSync(resolve(srcRoot, 'styles/ui.module.css'), 'utf8'))
const globalCss = withoutComments(readFileSync(resolve(srcRoot, 'styles/global.css'), 'utf8'))

// Splits `css` into the bodies of every `@media <query> { ... }` block and the
// remaining text, matching braces so a rule inside the block cannot end it early.
function splitMedia(css: string, query: string): { inside: string; outside: string } {
  let inside = ''
  let outside = ''
  let cursor = 0
  for (let start = css.indexOf(`@media ${query}`); start !== -1; start = css.indexOf(`@media ${query}`, cursor)) {
    const open = css.indexOf('{', start)
    let depth = 0
    let end = open
    for (; end < css.length; end += 1) {
      if (css[end] === '{') depth += 1
      else if (css[end] === '}' && (depth -= 1) === 0) break
    }
    outside += css.slice(cursor, start)
    inside += css.slice(open + 1, end)
    cursor = end + 1
  }
  return { inside, outside: outside + css.slice(cursor) }
}

// Declarations of every rule whose selector list contains `selector` exactly
// (a grouped rule such as `.a, .b { ... }` counts for both), later rules winning.
function declarationsFor(css: string, selector: string): Map<string, string> {
  const props = new Map<string, string>()
  for (const rule of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    if (!rule[1].split(',').some((part) => part.trim() === selector)) continue
    for (const declaration of rule[2].split(';')) {
      const colon = declaration.indexOf(':')
      if (colon > 0) props.set(declaration.slice(0, colon).trim(), declaration.slice(colon + 1).trim())
    }
  }
  return props
}

const phone = splitMedia(uiCss, '(max-width: 900px)')

// --control-h is 36px on desktop and 44px at <= 900px (styles/tokens.css), so a
// control sized by it meets the 44px touch target on phones and stays compact
// with a mouse. The inline buttons and the option rows were fixed at 28px/32px.
describe('touch targets at phone width (#601)', () => {
  it.each(['.searchClear', '.dateButton', '.calendarNav'])('%s is as wide and tall as a control', (selector) => {
    const props = declarationsFor(phone.inside, selector)
    expect(props.get('width')).toBe('var(--control-h)')
    expect(props.get('height')).toBe('var(--control-h)')
  })

  it('gives the calendar footer actions the control size', () => {
    const props = declarationsFor(phone.inside, '.calendarAction')
    expect(props.get('min-width')).toBe('var(--control-h)')
    expect(props.get('min-height')).toBe('var(--control-h)')
  })

  // #657: day cells were 32px tall at every width.
  it('gives each calendar day cell the control size', () => {
    const props = declarationsFor(phone.inside, '.calendarDay')
    expect(props.get('min-width')).toBe('var(--control-h)')
    expect(props.get('height')).toBe('var(--control-h)')
  })

  // Seven 44px columns + 6 x 2px gaps + 2 x 8px padding + 2px border = 338px,
  // inside the 340px touch popover width DatePicker uses at this breakpoint.
  it('fits seven 44px day columns into the touch popover width', () => {
    expect(declarationsFor(phone.inside, '.calendar').get('padding')).toBe('var(--space-sm)')
    expect(declarationsFor(uiCss, '.calendarGrid').get('gap')).toBe('2px')
    const ui = readFileSync(resolve(srcRoot, 'components/ui.tsx'), 'utf8')
    expect(ui).toMatch(/CALENDAR_TOUCH_SIZE = \{ width: 340, height: 432 \}/)
    expect(7 * 44 + 6 * 2 + 2 * 8 + 2).toBeLessThanOrEqual(340)
  })

  it('gives each select option row the control height', () => {
    expect(declarationsFor(phone.inside, '.selectOption').get('min-height')).toBe('var(--control-h)')
  })

  it('keeps typed text clear of the 44px inline button (4px inset + 44px)', () => {
    expect(declarationsFor(phone.inside, '.searchField .searchInput').get('padding-right')).toBe('var(--space-4xl)')
    expect(declarationsFor(phone.inside, '.dateInputWrap .dateInput').get('padding-right')).toBe('var(--space-4xl)')
  })

  // The touch calendar popover is positioned for 432px (CALENDAR_TOUCH_SIZE in
  // ui.tsx). The gaps around the 44px header and footer buttons shrink by 4px
  // each to keep the natural height under that.
  it('trims the calendar gaps so the 44px buttons still fit the popover height', () => {
    expect(declarationsFor(phone.inside, '.calendarHeader').get('margin-bottom')).toBe('var(--space-2xs)')
    expect(declarationsFor(phone.inside, '.calendarFooter').get('padding-top')).toBe('var(--space-2xs)')
  })

  it('leaves the desktop sizes alone', () => {
    for (const selector of ['.searchClear', '.dateButton', '.calendarNav']) {
      const props = declarationsFor(phone.outside, selector)
      expect([selector, props.get('width'), props.get('height')]).toEqual([selector, '28px', '28px'])
    }
    expect(declarationsFor(phone.outside, '.calendarAction').get('min-height')).toBe('28px')
    expect(declarationsFor(phone.outside, '.calendarDay').get('height')).toBe('32px')
    expect(declarationsFor(phone.outside, '.calendarDay').get('min-width')).toBe('0')
    expect(declarationsFor(phone.outside, '.calendar').get('padding')).toBe('var(--space-md)')
    expect(declarationsFor(phone.outside, '.selectOption').get('min-height')).toBe('32px')
    expect(declarationsFor(phone.outside, '.calendarHeader').get('margin-bottom')).toBe('var(--space-sm)')
    expect(declarationsFor(phone.outside, '.calendarFooter').get('padding-top')).toBe('var(--space-sm)')
  })
})

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry)
    if (statSync(path).isDirectory()) walk(path, out)
    else out.push(path)
  }
  return out
}

// Windows forced-colors mode drops box-shadow and keeps only outlines, so a
// focus ring drawn with box-shadow must also carry a transparent outline, which
// that mode repaints in a system color. Removing the outline outright leaves a
// keyboard user with no visible focus there.
describe('focus ring in forced-colors mode (#601)', () => {
  it('never removes an outline outright', () => {
    const offenders: string[] = []
    for (const file of walk(srcRoot).filter((path) => path.endsWith('.css'))) {
      const css = withoutComments(readFileSync(file, 'utf8'))
      for (const match of css.matchAll(/\boutline(?:-style)?\s*:\s*(?:none|0)\s*(?:;|\}|$)/g)) {
        offenders.push(`${relative(srcRoot, file)}: ${match[0].trim()}`)
      }
    }
    expect(offenders).toEqual([])
  })

  it('keeps a transparent outline on the select trigger focus ring', () => {
    const props = declarationsFor(uiCss, '.selectTrigger:focus-visible')
    expect(props.get('box-shadow')).toContain('var(--accent-ring)')
    expect(props.get('outline')).toBe('2px solid transparent')
    expect(props.get('outline-offset')).toBe('2px')
  })

  it.each(['.input:focus', '.select:focus', '.textarea:focus'])('keeps a transparent outline on %s', (selector) => {
    const props = declarationsFor(globalCss, selector)
    expect(props.get('box-shadow')).toContain('var(--accent-ring)')
    expect(props.get('outline')).toBe('2px solid transparent')
    expect(props.get('outline-offset')).toBe('2px')
  })
})
