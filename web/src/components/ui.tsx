import {
  cloneElement, createContext, forwardRef, isValidElement, useCallback, useContext, useEffect, useId, useLayoutEffect, useRef,
  useState,
  type AnchorHTMLAttributes, type ButtonHTMLAttributes, type CSSProperties, type InputHTMLAttributes, type ReactElement,
  type ReactNode, type Ref, type TextareaHTMLAttributes,
} from 'react'
import { createPortal } from 'react-dom'
import { Link, type LinkProps } from 'react-router-dom'
import { ApiError, errorMessage } from '../api/client'
import {
  formatDate, formatRelative, labelForPublication, labelForStatus, publicationTone, safeExternalUrl, statusTone, verdictTone,
} from '../lib/format'
import type { FindingsSummary, PublicationState } from '../api/types'
import { Icon } from './Icons'
import styles from '../styles/ui.module.css'

export function classNames(...values: Array<string | false | null | undefined>): string {
  return values.filter(Boolean).join(' ')
}

type ButtonVariant = 'primary' | 'secondary' | 'danger' | 'ghost'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  loading?: boolean
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'secondary', loading, children, className, disabled, type = 'button', ...props }, ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      className={classNames(styles.button, styles[variant], className)}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      {...props}
    >
      {loading ? <span className={styles.spinner} aria-hidden="true" /> : null}
      {children}
    </button>
  )
})

// ButtonLink is navigation that looks like a Button. Pass `to` for in-app
// routes or `href` for a plain anchor; both share the Button size and variants.
type ButtonLinkProps = { variant?: ButtonVariant } & (
  | (LinkProps & { href?: never })
  | (AnchorHTMLAttributes<HTMLAnchorElement> & { href: string; to?: never })
)

export function ButtonLink({ variant = 'secondary', className, ...props }: ButtonLinkProps) {
  const classes = classNames(styles.button, styles[variant], className)
  if (props.to !== undefined) return <Link {...(props as LinkProps)} className={classes} />
  return <a {...(props as AnchorHTMLAttributes<HTMLAnchorElement>)} className={classes} />
}

// Toolbar holds filters and scope controls on one aligned row. Its height is
// fixed to the control height, so status messages belong below it.
export function Toolbar({
  children, actions, label, inset, className, status,
}: {
  children: ReactNode
  actions?: ReactNode
  label?: string
  inset?: boolean
  className?: string
  status?: ReactNode
}) {
  return (
    <>
      <div className={classNames(styles.filterBar, inset && styles.toolbar, className)} role={label ? 'toolbar' : undefined} aria-label={label}>
        <div className={styles.filterBarMain}>{children}</div>
        {actions ? <div className={styles.filterBarActions}>{actions}</div> : null}
      </div>
      {status !== undefined ? <div className={classNames(styles.toolbarStatus, inset && styles.toolbarStatusInset)} role="status">{status}</div> : null}
    </>
  )
}

// ToolbarField puts a compact label beside its control so toolbars never grow taller.
export function ToolbarField({
  label, htmlFor, grow, children,
}: { label: string; htmlFor?: string; grow?: boolean; children: ReactNode }) {
  return (
    <label className={classNames(styles.filterBarField, grow && styles.filterBarFieldGrow)} htmlFor={htmlFor}>
      <span>{label}</span>
      {children}
    </label>
  )
}

export function Badge({
  tone = 'neutral', live, plain, children,
}: { tone?: 'ok' | 'warn' | 'danger' | 'info' | 'neutral'; live?: boolean; plain?: boolean; children: ReactNode }) {
  const classes = {
    ok: styles.badgeOk, warn: styles.badgeWarn, danger: styles.badgeDanger, info: styles.badgeInfo, neutral: styles.badgeNeutral,
  }
  return (
    <span className={classNames(styles.badge, classes[tone], live && styles.badgeLive, plain && styles.badgePlain)}>
      {children}
    </span>
  )
}

export function StatusBadge({ status, label }: { status?: string; label?: string }) {
  const tone = statusTone(status)
  return (
    <Badge tone={tone} live={status === 'running' || status === 'refreshing'}>
      {label || labelForStatus(status)}
    </Badge>
  )
}

export function VerdictBadge({ verdict }: { verdict?: string | null }) {
  if (!verdict) return <Badge tone="neutral">—</Badge>
  if (verdict === 'INCOMPLETE') return <Badge tone="warn">INCOMPLETE（未完成）</Badge>
  return <Badge tone={verdictTone(verdict)}>{verdict}</Badge>
}

// Publication is tracked independently from execution; a completed workflow
// with a failed publication is not a success.
export function PublicationBadge({ state }: { state?: PublicationState }) {
  return <Badge tone={publicationTone(state)}>{labelForPublication(state)}</Badge>
}

const SEVERITY_CLASSES: Record<string, string> = {
  P0: styles.sevP0,
  P1: styles.sevP1,
  P2: styles.sevP2,
  info: styles.sevInfo,
}

export function SeverityBadge({ severity }: { severity?: string }) {
  if (!severity) return <span className={styles.visuallyEmpty}>—</span>
  return <span className={classNames(styles.sev, SEVERITY_CLASSES[severity] || styles.sevInfo)}>{severity}</span>
}

const SEVERITY_COUNTS = [
  { key: 'P0', color: 'var(--danger-solid)' },
  { key: 'P1', color: 'var(--danger-dot)' },
  { key: 'P2', color: 'var(--warn-dot)' },
] as const

export function SevCounts({ summary }: { summary?: FindingsSummary }) {
  const bySeverity = summary?.by_severity
  if (!bySeverity) return <span className={styles.visuallyEmpty}>—</span>
  return (
    <span className={styles.sevCounts}>
      {SEVERITY_COUNTS.map(({ key, color }) => {
        const count = bySeverity[key] ?? 0
        return (
          <span key={key} title={key} role="img" aria-label={`${key} ${count}`} className={count === 0 ? styles.zero : undefined}>
            {/* stylecheck-allow-inline: data-driven count color */}
            <i style={{ background: count === 0 ? 'var(--border)' : color }} />
            {count}
          </span>
        )
      })}
    </span>
  )
}

export function Segmented({
  options, value, onChange, label,
}: {
  options: Array<{ value: string; label: string; count?: number }>
  value: string
  onChange: (value: string) => void
  label: string
}) {
  return (
    <div className={styles.segmented} role="group" aria-label={label}>
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          aria-pressed={option.value === value}
          onClick={() => onChange(option.value)}
        >
          {option.label}
          {option.count != null ? <em>{option.count}</em> : null}
        </button>
      ))}
    </div>
  )
}

export function PageHeader({
  title, description, actions, meta,
}: { title: ReactNode; description?: ReactNode; actions?: ReactNode; meta?: ReactNode }) {
  return (
    <header className={styles.pageHeader}>
      <div className={styles.pageHeaderText}>
        <div className={styles.pageTitleRow}>
          <h1 className={styles.pageTitle}>{title}</h1>
          {meta ? <div className={styles.pageMeta}>{meta}</div> : null}
        </div>
        {description ? <p className={styles.pageDescription}>{description}</p> : null}
      </div>
      {actions ? <div className={styles.headerActions}>{actions}</div> : null}
    </header>
  )
}

export function Panel({
  title, aside, children, className, flush, note,
}: { title?: ReactNode; aside?: ReactNode; children: ReactNode; className?: string; flush?: boolean; note?: ReactNode }) {
  return (
    <section className={classNames(styles.panel, flush && styles.panelFlush, className)}>
      {title || aside ? (
        <div className={styles.panelHeader}>
          {title ? <h2 className={styles.panelTitle}>{title}</h2> : <span />}
          {aside ? <div className={styles.panelAside}>{aside}</div> : null}
        </div>
      ) : null}
      {children}
      {note ? <div className={flush ? styles.tableNote : styles.panelNote}>{note}</div> : null}
    </section>
  )
}

// PanelBody keeps its own padding, so full-bleed layouts belong inside <Panel flush>.
export function PanelBody({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={classNames(styles.panelBody, className)}>{children}</div>
}

export function Metric({ label, value, note }: { label: string; value: ReactNode; note?: ReactNode }) {
  const unavailable = value === null || value === undefined || value === '不可用'
  return (
    <div className={styles.metric}>
      <span className={styles.metricLabel}>{label}</span>
      <span className={classNames(styles.metricValue, unavailable && styles.metricUnavailable)}>{unavailable ? '不可用' : value}</span>
      {note ? <span className={styles.metricNote}>{note}</span> : null}
    </div>
  )
}

export function Sparkline({ points, color = 'var(--accent)' }: { points: number[]; color?: string }) {
  const values = points.filter((point) => Number.isFinite(point))
  if (values.length < 2) return null
  const max = Math.max(...values)
  const scale = max > 0 ? max : 1
  const step = 64 / (values.length - 1)
  const polyline = values.map((point, index) => {
    const x = index * step
    const y = 18 - (Math.min(Math.max(point, 0), scale) / scale) * 18
    return `${x.toFixed(2)},${y.toFixed(2)}`
  }).join(' ')
  return (
    <svg width="64" height="18" viewBox="0 0 64 18" aria-hidden="true" focusable="false">
      <polyline points={polyline} fill="none" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />
    </svg>
  )
}

export function FieldHint({ children, id }: { children: ReactNode; id?: string }) {
  return <span className={styles.hint} id={id}>{children}</span>
}

export function ActionHint({ children }: { children: ReactNode }) {
  return <p className={styles.actionHint}>{children}</p>
}

type NoteTone = 'neutral' | 'info' | 'warn' | 'danger' | 'success'

const NOTE_CLASSES: Record<NoteTone, string> = {
  neutral: styles.noteNeutral,
  info: styles.noteInfo,
  warn: styles.noteWarn,
  danger: styles.noteDanger,
  success: styles.noteSuccess,
}

export function Note({ tone = 'neutral', role, children }: { tone?: NoteTone; role?: React.AriaRole; children: ReactNode }) {
  return <div className={classNames(styles.note, NOTE_CLASSES[tone])} role={role}>{children}</div>
}

export function TableNote({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={classNames(styles.tableNote, className)}>{children}</div>
}

// Field links its hint and error to the control through aria-describedby, so
// screen readers announce both without the page wiring ids by hand.
export function Field({
  label, htmlFor, hint, error, children,
}: { label: string; htmlFor: string; hint?: ReactNode; error?: ReactNode; children: ReactNode }) {
  const hintId = hint ? `${htmlFor}-hint` : undefined
  const errorId = error ? `${htmlFor}-error` : undefined
  let control = children
  const child = isValidElement(children) ? children as ReactElement<{ 'aria-describedby'?: string }> : null
  const describedBy = [hintId, errorId].filter((id): id is string => Boolean(id))
  if (child && describedBy.length) {
    const existing = child.props['aria-describedby']?.split(/\s+/).filter(Boolean) ?? []
    control = cloneElement(child, { 'aria-describedby': [...new Set([...existing, ...describedBy])].join(' ') })
  }
  return (
    <div className={styles.field}>
      <label className={styles.label} htmlFor={htmlFor}>{label}</label>
      {control}
      {hint ? <FieldHint id={hintId}>{hint}</FieldHint> : null}
      {error ? <span className={styles.errorText} id={errorId} role="alert">{error}</span> : null}
    </div>
  )
}

export function EmptyState({ title, description, action }: { title: string; description?: ReactNode; action?: ReactNode }) {
  return (
    <div className={styles.empty}>
      <span className={styles.emptyTitle}>{title}</span>
      {description ? <span>{description}</span> : null}
      {action}
    </div>
  )
}

export function LoadingState({ label = '加载中…' }: { label?: string }) {
  return (
    <div className={styles.loading} role="status">
      <span className={styles.spinner} aria-hidden="true" />
      <span>{label}</span>
    </div>
  )
}

export function ErrorState({ error, onRetry, title = '加载失败', hint }: { error: unknown; onRetry?: () => void; title?: string; hint?: string | null }) {
  const forbidden = error instanceof ApiError && error.status === 403
  return (
    <div className={styles.errorBox} role="alert">
      <strong>{forbidden ? '没有访问权限' : title}</strong>
      <span>{forbidden ? '当前账号没有执行此操作的权限。' : errorMessage(error)}</span>
      {hint && !forbidden ? <span>{hint}</span> : null}
      {onRetry && !forbidden ? <Button variant="secondary" className={styles.smallButton} onClick={onRetry}>重试</Button> : null}
    </div>
  )
}

export function Pagination({
  hasNextPage, loading, onLoadMore, label = '加载更多',
}: { hasNextPage: boolean; loading: boolean; onLoadMore: () => void; label?: string }) {
  if (!hasNextPage) return null
  return (
    <div className={styles.loadMore}>
      <Button variant="secondary" onClick={onLoadMore} loading={loading}>{label}</Button>
    </div>
  )
}

export function Tabs({
  items, value, onChange, label, idPrefix = 'tab',
}: { items: Array<{ key: string; label: string; count?: number; panelId?: string }>; value: string; onChange: (key: string) => void; label: string; idPrefix?: string }) {
  const refs = useRef<Array<HTMLButtonElement | null>>([])
  const onKeyDown = (event: React.KeyboardEvent, index: number) => {
    let next = index
    if (event.key === 'ArrowRight') next = (index + 1) % items.length
    else if (event.key === 'ArrowLeft') next = (index - 1 + items.length) % items.length
    else if (event.key === 'Home') next = 0
    else if (event.key === 'End') next = items.length - 1
    else return
    event.preventDefault()
    onChange(items[next].key)
    refs.current[next]?.focus()
  }
  return (
    <div className={styles.tabs} role="tablist" aria-label={label}>
      {items.map((item, index) => {
        const selected = item.key === value
        return (
          <button
            key={item.key}
            ref={(element) => { refs.current[index] = element }}
            type="button"
            role="tab"
            id={`${idPrefix}-${item.key}`}
            aria-selected={selected}
            aria-controls={selected ? item.panelId : undefined}
            tabIndex={selected ? 0 : -1}
            className={classNames(styles.tab, selected && styles.tabActive)}
            onClick={() => onChange(item.key)}
            onKeyDown={(event) => onKeyDown(event, index)}
          >
            {item.label}
            {item.count != null ? <span className={styles.tabCount}>{item.count}</span> : null}
          </button>
        )
      })}
    </div>
  )
}

export function ExternalLink({ href, children }: { href?: string; children: ReactNode }) {
  const url = safeExternalUrl(href)
  if (!url) return null
  return (
    <a href={url} target="_blank" rel="noopener noreferrer" className={styles.inlineLink}>
      {children}<Icon name="external" size={14} />
    </a>
  )
}

export function RelativeTime({ value, fallback = '—' }: { value?: string; fallback?: string }) {
  if (!value) return <span>{fallback}</span>
  return (
    <time dateTime={value} title={formatDate(value)} className="tabular">
      {formatRelative(value)}
    </time>
  )
}

export function Switch({ checked, onChange, label, disabled }: { checked: boolean; onChange: (next: boolean) => void; label: string; disabled?: boolean }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={classNames(styles.switch, checked && styles.switchOn)}
    />
  )
}

/* ---------------------------------------------------------------------------
 * Form and control primitives
 * ------------------------------------------------------------------------- */

function assignRef<T>(ref: Ref<T> | undefined, node: T | null) {
  if (!ref) return
  if (typeof ref === 'function') ref(node)
  else (ref as { current: T | null }).current = node
}

function mergeRefs<T>(...refs: Array<Ref<T> | undefined>): (node: T | null) => void {
  return (node) => {
    for (const ref of refs) assignRef(ref, node)
  }
}

function viewportWidth() {
  return document.documentElement.clientWidth || window.innerWidth
}

// Popovers are portaled and fixed so they escape overflow containers. The
// position is computed from the anchor rect, which is data-driven inline style.
function usePopoverPosition(
  open: boolean,
  anchorRef: React.RefObject<HTMLElement | null>,
  matchAnchorWidth: boolean,
  width: number,
  estimatedHeight: number,
) {
  const [style, setStyle] = useState<CSSProperties>()
  useLayoutEffect(() => {
    if (!open) return
    const update = () => {
      const anchor = anchorRef.current
      if (!anchor) return
      const rect = anchor.getBoundingClientRect()
      const vw = viewportWidth()
      const vh = window.innerHeight
      const preferred = matchAnchorWidth ? Math.max(width, rect.width) : width
      const popupWidth = Math.min(preferred, Math.max(160, vw - 16))
      const left = Math.min(Math.max(8, rect.left), Math.max(8, vw - popupWidth - 8))
      const spaceBelow = vh - rect.bottom
      const openUp = spaceBelow < estimatedHeight + 16 && rect.top > spaceBelow
      // Never exceed the space actually available on the chosen side.
      const available = (openUp ? rect.top : spaceBelow) - 12
      const maxHeight = Math.max(0, Math.min(estimatedHeight, available))
      setStyle(openUp
        ? { left, width: popupWidth, bottom: vh - rect.top + 4, maxHeight }
        : { left, width: popupWidth, top: rect.bottom + 4, maxHeight })
    }
    update()
    window.addEventListener('resize', update)
    window.addEventListener('scroll', update, true)
    return () => {
      window.removeEventListener('resize', update)
      window.removeEventListener('scroll', update, true)
    }
  }, [open, anchorRef, matchAnchorWidth, width, estimatedHeight])
  return style
}

export interface TooltipPosition {
  left: number
  top: number
  placement: 'above' | 'below'
}

export interface TooltipAnchorRect {
  left: number
  top: number
  width: number
  height: number
}

// Pure geometry so the 390px edges, long-content widths and flip/clamp cases
// are testable without a layout engine. The tooltip CSS translates by -50%
// horizontally and by -100% vertically when placement is "above".
export function computeTooltipPosition(
  anchor: TooltipAnchorRect,
  tooltip: { width: number; height: number },
  viewport: { width: number; height: number },
  offset = 8,
): TooltipPosition {
  const minLeft = offset + tooltip.width / 2
  const maxLeft = viewport.width - offset - tooltip.width / 2
  const center = anchor.left + anchor.width / 2
  const left = maxLeft >= minLeft ? Math.min(Math.max(center, minLeft), maxLeft) : viewport.width / 2
  const anchorBottom = anchor.top + anchor.height
  if (anchor.top - offset - tooltip.height >= offset) {
    return { left, top: anchor.top - offset, placement: 'above' }
  }
  if (viewport.height - anchorBottom - offset - tooltip.height >= offset) {
    return { left, top: anchorBottom + offset, placement: 'below' }
  }
  return {
    left,
    top: Math.max(offset, Math.min(anchorBottom + offset, viewport.height - offset - tooltip.height)),
    placement: 'below',
  }
}

function useTooltipPosition(
  open: boolean,
  anchorRef: React.RefObject<HTMLElement | null>,
  tooltipRef: React.RefObject<HTMLElement | null>,
) {
  const [position, setPosition] = useState<{ style: CSSProperties; placement: 'above' | 'below' }>()
  useLayoutEffect(() => {
    if (!open) return
    const update = () => {
      const anchor = anchorRef.current
      const tooltip = tooltipRef.current
      if (!anchor || !tooltip) return
      const rect = anchor.getBoundingClientRect()
      const next = computeTooltipPosition(
        { left: rect.left, top: rect.top, width: rect.width, height: rect.height },
        { width: tooltip.offsetWidth, height: tooltip.offsetHeight },
        { width: viewportWidth(), height: window.innerHeight },
      )
      setPosition({ placement: next.placement, style: { left: next.left, top: next.top } })
    }
    update()
    window.addEventListener('resize', update)
    window.addEventListener('scroll', update, true)
    return () => {
      window.removeEventListener('resize', update)
      window.removeEventListener('scroll', update, true)
    }
  }, [open, anchorRef, tooltipRef])
  return position
}

type AriaInvalid = boolean | 'false' | 'true' | 'grammar' | 'spelling'

export interface SelectOption {
  value: string
  label: string
  disabled?: boolean
}

export interface SelectProps {
  id?: string
  className?: string
  value: string
  options: ReadonlyArray<SelectOption>
  onChange: (value: string) => void
  disabled?: boolean
  /** Shown when the value is empty and no option carries the empty value. */
  emptyLabel?: string
  'aria-label'?: string
  'aria-labelledby'?: string
  'aria-describedby'?: string
  'aria-invalid'?: AriaInvalid
  'aria-required'?: boolean | 'false' | 'true'
}

// Select is a select-only combobox: focus stays on the trigger and the active
// option is announced with aria-activedescendant. The popup is portaled so
// page overflow and table scroll containers never clip it.
export const Select = forwardRef<HTMLButtonElement, SelectProps>(function Select({
  id, className, value, options, onChange, disabled, emptyLabel = '继承',
  'aria-label': ariaLabel, 'aria-labelledby': ariaLabelledBy, 'aria-describedby': ariaDescribedBy,
  'aria-invalid': ariaInvalid, 'aria-required': ariaRequired,
}, ref) {
  const generatedId = useId()
  const triggerId = id ?? generatedId
  const listboxId = `${generatedId}-listbox`
  const triggerRef = useRef<HTMLButtonElement | null>(null)
  const listboxRef = useRef<HTMLDivElement | null>(null)
  const typeahead = useRef<{ buffer: string; timer: number | undefined }>({ buffer: '', timer: undefined })
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(-1)
  const popoverStyle = usePopoverPosition(open, triggerRef, true, 160, 320)

  const selectedIndex = options.findIndex((option) => option.value === value)
  const selectedOption = selectedIndex >= 0 ? options[selectedIndex] : undefined
  const display = selectedOption ? selectedOption.label : value === '' ? emptyLabel : value
  const enabledIndexes: number[] = []
  options.forEach((option, index) => { if (!option.disabled) enabledIndexes.push(index) })
  const firstEnabled = enabledIndexes[0] ?? -1
  const lastEnabled = enabledIndexes[enabledIndexes.length - 1] ?? -1
  const activeDescendant = open && activeIndex >= 0 && activeIndex < options.length
    ? `${listboxId}-option-${activeIndex}`
    : undefined

  const closeMenu = useCallback((restoreFocus: boolean) => {
    setOpen(false)
    setActiveIndex(-1)
    if (restoreFocus) triggerRef.current?.focus()
  }, [])

  const openMenu = useCallback((direction: 1 | -1) => {
    if (disabled) return
    setOpen(true)
    if (selectedIndex >= 0 && !options[selectedIndex]?.disabled) setActiveIndex(selectedIndex)
    else setActiveIndex(direction === 1 ? enabledIndexes[0] ?? -1 : enabledIndexes[enabledIndexes.length - 1] ?? -1)
  }, [disabled, selectedIndex, options, enabledIndexes])

  const choose = useCallback((index: number) => {
    const option = options[index]
    if (!option || option.disabled) return
    if (option.value !== value) onChange(option.value)
    closeMenu(true)
  }, [options, value, onChange, closeMenu])

  const moveEnabled = (from: number, direction: 1 | -1) => {
    if (!enabledIndexes.length) return -1
    if (from < 0) return direction === 1 ? enabledIndexes[0] : enabledIndexes[enabledIndexes.length - 1]
    const position = enabledIndexes.indexOf(from)
    if (position < 0) return direction === 1 ? enabledIndexes[0] : enabledIndexes[enabledIndexes.length - 1]
    const next = position + direction
    if (next < 0 || next >= enabledIndexes.length) return from
    return enabledIndexes[next]
  }

  const typeaheadSearch = (key: string) => {
    const state = typeahead.current
    if (state.timer !== undefined) window.clearTimeout(state.timer)
    state.buffer = `${state.buffer}${key}`.toLowerCase()
    state.timer = window.setTimeout(() => { state.buffer = ''; state.timer = undefined }, 500)
    const match = options.findIndex((option) => !option.disabled && option.label.toLowerCase().startsWith(state.buffer))
    if (match >= 0) { setActiveIndex(match); return }
    // Repeated presses of one character cycle through matching options.
    state.buffer = key.toLowerCase()
    const after = options.findIndex((option, index) => !option.disabled && index > activeIndex && option.label.toLowerCase().startsWith(state.buffer))
    if (after >= 0) setActiveIndex(after)
  }

  const onTriggerKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>) => {
    if (disabled) return
    const key = event.key
    if (!open) {
      if (key === 'Enter' || key === ' ' || key === 'ArrowDown' || key === 'ArrowUp') {
        event.preventDefault()
        openMenu(key === 'ArrowUp' ? -1 : 1)
        return
      }
      if (key === 'Home' || key === 'End') {
        event.preventDefault()
        openMenu(key === 'Home' ? 1 : -1)
        return
      }
      if (key.length === 1 && !event.metaKey && !event.ctrlKey && !event.altKey) {
        event.preventDefault()
        openMenu(1)
        typeaheadSearch(key)
      }
      return
    }
    if (key === 'ArrowDown' || key === 'ArrowUp') {
      event.preventDefault()
      setActiveIndex((current) => moveEnabled(current, key === 'ArrowDown' ? 1 : -1))
    } else if (key === 'Home') {
      event.preventDefault()
      setActiveIndex(firstEnabled)
    } else if (key === 'End') {
      event.preventDefault()
      setActiveIndex(lastEnabled)
    } else if (key === 'Enter' || key === ' ') {
      event.preventDefault()
      if (activeIndex >= 0) choose(activeIndex)
      else closeMenu(true)
    } else if (key === 'Escape') {
      event.preventDefault()
      event.stopPropagation()
      closeMenu(true)
    } else if (key === 'Tab') {
      closeMenu(false)
    } else if (key.length === 1 && !event.metaKey && !event.ctrlKey && !event.altKey) {
      event.preventDefault()
      typeaheadSearch(key)
    }
  }

  useEffect(() => {
    if (!disabled) return
    setOpen(false)
    setActiveIndex(-1)
  }, [disabled])

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target
      if (!(target instanceof Node)) return
      if (triggerRef.current?.contains(target) || listboxRef.current?.contains(target)) return
      closeMenu(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    return () => document.removeEventListener('mousedown', onPointerDown)
  }, [open, closeMenu])

  useEffect(() => {
    if (!open || activeIndex < 0) return
    const active = listboxRef.current?.querySelector<HTMLElement>(`[data-index="${activeIndex}"]`)
    if (active && typeof active.scrollIntoView === 'function') active.scrollIntoView({ block: 'nearest' })
  }, [open, activeIndex])

  return (
    <>
      <button
        ref={mergeRefs(ref, triggerRef)}
        id={triggerId}
        type="button"
        role="combobox"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listboxId : undefined}
        aria-activedescendant={activeDescendant}
        aria-label={ariaLabel}
        aria-labelledby={ariaLabelledBy}
        aria-describedby={ariaDescribedBy}
        aria-invalid={ariaInvalid}
        aria-required={ariaRequired}
        disabled={disabled}
        className={classNames(styles.selectTrigger, className)}
        onClick={() => {
          if (open) closeMenu(true)
          else openMenu(1)
        }}
        onKeyDown={onTriggerKeyDown}
      >
        <span className={classNames(styles.selectValue, !selectedOption && value === '' && styles.selectValueEmpty)}>
          {display}
        </span>
        <Icon name="chevronDown" size={16} className={styles.selectChevron} />
      </button>
      {open ? createPortal(
        <div
          ref={listboxRef}
          id={listboxId}
          role="listbox"
          aria-labelledby={triggerId}
          className={styles.selectPopover}
          style={popoverStyle}
        >
          {options.length === 0 ? (
            <div className={styles.selectEmpty} role="presentation">无可选项</div>
          ) : enabledIndexes.length === 0 ? (
            <div className={styles.selectEmpty} role="presentation">没有可用选项</div>
          ) : options.map((option, index) => {
            const selected = option.value === value
            return (
              <div
                key={option.value}
                id={`${listboxId}-option-${index}`}
                role="option"
                aria-selected={selected}
                aria-disabled={option.disabled ? true : undefined}
                data-index={index}
                className={classNames(
                  styles.selectOption,
                  index === activeIndex && styles.selectOptionActive,
                  selected && styles.selectOptionSelected,
                  option.disabled && styles.selectOptionDisabled,
                )}
                onMouseDown={(event) => event.preventDefault()}
                onMouseEnter={() => { if (!option.disabled) setActiveIndex(index) }}
                onClick={() => choose(index)}
              >
                <span className={styles.selectOptionLabel}>{option.label}</span>
                {selected ? <Icon name="check" size={14} className={styles.selectOptionCheck} /> : null}
              </div>
            )
          })}
        </div>,
        document.body,
      ) : null}
    </>
  )
})

export interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type'> {
  label: ReactNode
}

// Checkbox keeps a real input for label clicks, Space and form semantics, and
// paints the box on a sibling so focus-visible and checked states are shared.
export const Checkbox = forwardRef<HTMLInputElement, CheckboxProps>(function Checkbox({
  label, id, className, disabled, ...props
}, ref) {
  return (
    <label className={classNames(styles.checkbox, disabled && styles.checkboxDisabled, className)} htmlFor={id}>
      <input ref={ref} id={id} type="checkbox" disabled={disabled} className={styles.checkboxInput} {...props} />
      <span className={styles.checkboxBox} aria-hidden="true"><Icon name="check" size={12} /></span>
      {label !== null && label !== undefined && label !== '' ? <span className={styles.checkboxLabel}>{label}</span> : null}
    </label>
  )
})

const DATE_PATTERN = /^(\d{4})-(\d{2})-(\d{2})$/

// Dates are calendar days in the local time zone. Never parse YYYY-MM-DD with
// new Date(string): that is UTC midnight and shifts the day west of UTC.
export function parseLocalDate(value: string): Date | null {
  const match = DATE_PATTERN.exec(value)
  if (!match) return null
  const year = Number(match[1])
  const month = Number(match[2])
  const day = Number(match[3])
  if (year < 100) return null
  const date = new Date(year, month - 1, day)
  if (date.getFullYear() !== year || date.getMonth() !== month - 1 || date.getDate() !== day) return null
  return date
}

export function formatLocalDate(date: Date): string {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
}

function addLocalDays(date: Date, days: number): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate() + days)
}

function addLocalMonths(date: Date, months: number): Date {
  const target = new Date(date.getFullYear(), date.getMonth() + months, 1)
  const lastDay = new Date(target.getFullYear(), target.getMonth() + 1, 0).getDate()
  return new Date(target.getFullYear(), target.getMonth(), Math.min(date.getDate(), lastDay))
}

function sameLocalDay(left: Date, right: Date) {
  return left.getFullYear() === right.getFullYear() && left.getMonth() === right.getMonth() && left.getDate() === right.getDate()
}

const WEEKDAYS = ['一', '二', '三', '四', '五', '六', '日']

export interface DatePickerProps {
  id?: string
  className?: string
  value: string
  onChange: (value: string) => void
  /** Reports whether the current text can be applied as a date. */
  onValidityChange?: (valid: boolean) => void
  min?: string
  max?: string
  disabled?: boolean
  placeholder?: string
  'aria-label'?: string
  'aria-labelledby'?: string
  'aria-describedby'?: string
  'aria-invalid'?: AriaInvalid
}

// DatePicker is a text field plus a calendar popup. Typing accepts only a
// local YYYY-MM-DD date; the calendar navigates with local calendar arithmetic
// so DST transitions never shift a selected day.
export const DatePicker = forwardRef<HTMLInputElement, DatePickerProps>(function DatePicker({
  id, className, value, onChange, onValidityChange, min, max, disabled, placeholder = 'YYYY-MM-DD',
  'aria-label': ariaLabel, 'aria-labelledby': ariaLabelledBy, 'aria-describedby': ariaDescribedBy,
  'aria-invalid': ariaInvalid,
}, ref) {
  const generatedId = useId()
  const errorId = `${generatedId}-date-error`
  const fieldRef = useRef<HTMLDivElement | null>(null)
  const inputRef = useRef<HTMLInputElement | null>(null)
  const popoverRef = useRef<HTMLDivElement | null>(null)
  // Only move focus into the grid when the calendar itself asked for it.
  // Typing while open updates the roving tab stop without stealing focus.
  const shouldFocusDay = useRef(false)
  const emitted = useRef(value)
  const [text, setText] = useState(value)
  const [error, setError] = useState<string | null>(null)
  const [open, setOpen] = useState(false)
  const [viewDate, setViewDate] = useState<Date>(() => parseLocalDate(value) ?? new Date())
  const [focusDate, setFocusDate] = useState<Date>(() => parseLocalDate(value) ?? new Date())
  const popoverStyle = usePopoverPosition(open, inputRef, false, 292, 360)

  const minDate = min ? parseLocalDate(min) : null
  const maxDate = max ? parseLocalDate(max) : null
  const minDay = minDate && !Number.isNaN(minDate.getTime()) ? minDate : null
  const maxDay = maxDate && !Number.isNaN(maxDate.getTime()) ? maxDate : null

  const rangeError = (date: Date): string | null => {
    if (minDay && date.getTime() < minDay.getTime()) return `日期不能早于 ${min}。`
    if (maxDay && date.getTime() > maxDay.getTime()) return `日期不能晚于 ${max}。`
    return null
  }
  const clampToRange = (date: Date): Date => {
    if (minDay && date.getTime() < minDay.getTime()) return minDay
    if (maxDay && date.getTime() > maxDay.getTime()) return maxDay
    return date
  }
  const isDisabledDay = (date: Date) => Boolean(rangeError(date))
  const firstAvailableDay = (date: Date): Date => {
    const start = new Date(date.getFullYear(), date.getMonth(), 1)
    for (let index = 0; index < 31; index += 1) {
      const candidate = addLocalDays(start, index)
      if (candidate.getMonth() !== start.getMonth()) break
      if (!isDisabledDay(candidate)) return candidate
    }
    return start
  }

  const reportedValidity = useRef<boolean | null>(null)
  const textValidity = (next: string): boolean => {
    const trimmed = next.trim()
    if (trimmed === '') return true
    const parsed = parseLocalDate(trimmed)
    if (!parsed) return false
    return rangeError(parsed) === null
  }
  const reportValidity = (valid: boolean) => {
    if (reportedValidity.current === valid) return
    reportedValidity.current = valid
    onValidityChange?.(valid)
  }

  useEffect(() => {
    reportValidity(textValidity(value))
    if (value === emitted.current) return
    emitted.current = value
    setText(value)
    setError(null)
    const parsed = parseLocalDate(value)
    if (parsed) {
      setViewDate(parsed)
      setFocusDate(parsed)
    }
  }, [value])

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target
      if (!(target instanceof Node)) return
      if (fieldRef.current?.contains(target) || popoverRef.current?.contains(target)) return
      setOpen(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    return () => document.removeEventListener('mousedown', onPointerDown)
  }, [open])

  useEffect(() => {
    if (!open) return
    const key = formatLocalDate(focusDate)
    const button = popoverRef.current?.querySelector<HTMLElement>(`[data-date="${key}"]`)
    if (!button) {
      // The roving tab stop must stay inside the rendered 42-day grid.
      const fallback = firstAvailableDay(viewDate)
      if (formatLocalDate(fallback) !== key) setFocusDate(fallback)
      return
    }
    if (!shouldFocusDay.current) return
    shouldFocusDay.current = false
    button.focus()
  }, [open, focusDate, viewDate])

  useEffect(() => {
    if (!disabled) return
    setOpen(false)
  }, [disabled])

  const openCalendar = () => {
    if (disabled) return
    const current = parseLocalDate(text.trim()) ?? parseLocalDate(value) ?? new Date()
    const target = clampToRange(current)
    shouldFocusDay.current = true
    setViewDate(target)
    setFocusDate(target)
    setOpen(true)
  }

  const closeCalendar = (restoreFocus: boolean) => {
    setOpen(false)
    if (restoreFocus) inputRef.current?.focus()
  }

  const selectDate = (date: Date) => {
    if (disabled || isDisabledDay(date)) return
    const formatted = formatLocalDate(date)
    setText(formatted)
    setError(null)
    reportValidity(true)
    emitted.current = formatted
    if (formatted !== value) onChange(formatted)
    closeCalendar(true)
  }

  const commitText = () => {
    const trimmed = text.trim()
    reportValidity(textValidity(trimmed))
    if (trimmed === '') {
      setError(null)
      if (value !== '') {
        emitted.current = ''
        onChange('')
      }
      return
    }
    const parsed = parseLocalDate(trimmed)
    if (!parsed) {
      setError('请输入 YYYY-MM-DD 格式的有效日期。')
      return
    }
    const message = rangeError(parsed)
    if (message) {
      setError(message)
      return
    }
    setError(null)
    const formatted = formatLocalDate(parsed)
    setText(formatted)
    emitted.current = formatted
    if (formatted !== value) onChange(formatted)
  }

  const onTextChange = (event: React.ChangeEvent<HTMLInputElement>) => {
    const next = event.target.value
    setText(next)
    reportValidity(textValidity(next))
    const trimmed = next.trim()
    if (trimmed === '') {
      setError(null)
      if (value !== '') {
        emitted.current = ''
        onChange('')
      }
      return
    }
    const parsed = parseLocalDate(trimmed)
    if (!parsed) {
      setError(trimmed.length >= 10 ? '请输入 YYYY-MM-DD 格式的有效日期。' : null)
      return
    }
    const message = rangeError(parsed)
    if (message) {
      setError(trimmed.length >= 10 ? message : null)
      return
    }
    setError(null)
    const formatted = formatLocalDate(parsed)
    emitted.current = formatted
    if (formatted !== value) onChange(formatted)
    if (open) {
      setViewDate(parsed)
      setFocusDate(parsed)
    }
  }

  const onInputKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Escape' && open) {
      event.preventDefault()
      event.stopPropagation()
      closeCalendar(true)
      return
    }
    if (event.key === 'ArrowDown' && !open) {
      event.preventDefault()
      openCalendar()
      return
    }
    if (event.key === 'Enter') {
      event.preventDefault()
      commitText()
    }
  }

  const onDayKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, date: Date) => {
    let next: Date | null = null
    switch (event.key) {
      case 'ArrowLeft': next = addLocalDays(date, -1); break
      case 'ArrowRight': next = addLocalDays(date, 1); break
      case 'ArrowUp': next = addLocalDays(date, -7); break
      case 'ArrowDown': next = addLocalDays(date, 7); break
      case 'Home': next = addLocalDays(date, -((date.getDay() + 6) % 7)); break
      case 'End': next = addLocalDays(date, 6 - ((date.getDay() + 6) % 7)); break
      case 'PageUp': next = addLocalMonths(date, event.shiftKey ? -12 : -1); break
      case 'PageDown': next = addLocalMonths(date, event.shiftKey ? 12 : 1); break
      case 'Enter':
      case ' ':
        event.preventDefault()
        selectDate(date)
        return
      case 'Escape':
        event.preventDefault()
        event.stopPropagation()
        closeCalendar(true)
        return
      case 'Tab':
        // Unmounting the focused day synchronously would drop focus to body.
        event.preventDefault()
        closeCalendar(true)
        return
      default:
        return
    }
    event.preventDefault()
    shouldFocusDay.current = true
    const target = clampToRange(next)
    setFocusDate(target)
    if (target.getMonth() !== viewDate.getMonth() || target.getFullYear() !== viewDate.getFullYear()) {
      setViewDate(target)
    }
  }

  const changeMonth = (delta: number) => {
    const first = new Date(viewDate.getFullYear(), viewDate.getMonth() + delta, 1)
    const target = clampToRange(first)
    shouldFocusDay.current = true
    setViewDate(new Date(target.getFullYear(), target.getMonth(), 1))
    setFocusDate(target)
  }

  const onPopoverKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== 'Escape') return
    event.preventDefault()
    event.stopPropagation()
    closeCalendar(true)
  }

  const onFieldBlur = (event: React.FocusEvent<HTMLDivElement>) => {
    const related = event.relatedTarget as Node | null
    if (related && (fieldRef.current?.contains(related) || popoverRef.current?.contains(related))) return
    if (disabled) return
    setOpen(false)
    commitText()
  }

  const monthStart = new Date(viewDate.getFullYear(), viewDate.getMonth(), 1)
  const gridStart = addLocalDays(monthStart, -((monthStart.getDay() + 6) % 7))
  const days = Array.from({ length: 42 }, (_, index) => addLocalDays(gridStart, index))
  const selectedDate = parseLocalDate(text.trim()) ?? parseLocalDate(value)
  const selectedKey = selectedDate ? formatLocalDate(selectedDate) : ''
  const today = new Date()
  const todayKey = formatLocalDate(today)
  const todayDisabled = isDisabledDay(today)
  const describedBy = [ariaDescribedBy, error ? errorId : undefined].filter(Boolean).join(' ') || undefined
  const invalid = error ? true : ariaInvalid

  return (
    <div ref={fieldRef} className={styles.dateField} onBlur={onFieldBlur}>
      <div className={styles.dateInputWrap}>
        <input
          ref={mergeRefs(ref, inputRef)}
          id={id}
          type="text"
          inputMode="text"
          autoComplete="off"
          spellCheck={false}
          className={classNames(styles.input, styles.dateInput, className)}
          value={text}
          placeholder={placeholder}
          disabled={disabled}
          aria-label={ariaLabel}
          aria-labelledby={ariaLabelledBy}
          aria-describedby={describedBy}
          aria-invalid={invalid}
          onChange={onTextChange}
          onKeyDown={onInputKeyDown}
        />
        <button
          type="button"
          className={styles.dateButton}
          aria-label={open ? '关闭日历' : '打开日历'}
          aria-expanded={open}
          disabled={disabled}
          onClick={() => { if (open) closeCalendar(false); else openCalendar() }}
        >
          <Icon name="calendar" size={16} />
        </button>
      </div>
      {error ? <span className={styles.dateError} id={errorId} role="alert">{error}</span> : null}
      {open ? createPortal(
        <div
          ref={popoverRef}
          role="dialog"
          aria-label="选择日期"
          className={styles.calendar}
          style={popoverStyle}
          onKeyDown={onPopoverKeyDown}
        >
          <div className={styles.calendarHeader}>
            <button type="button" className={styles.calendarNav} aria-label="上个月" onClick={() => changeMonth(-1)}>
              <Icon name="chevron" size={16} className={styles.calendarPrev} />
            </button>
            <span className={styles.calendarTitle} aria-live="polite">
              {`${viewDate.getFullYear()}年${viewDate.getMonth() + 1}月`}
            </span>
            <button type="button" className={styles.calendarNav} aria-label="下个月" onClick={() => changeMonth(1)}>
              <Icon name="chevron" size={16} />
            </button>
          </div>
          <div className={styles.calendarWeekdays} aria-hidden="true">
            {WEEKDAYS.map((weekday) => <span key={weekday}>{weekday}</span>)}
          </div>
          <div className={styles.calendarGrid}>
            {days.map((date) => {
              const key = formatLocalDate(date)
              const inMonth = date.getMonth() === viewDate.getMonth() && date.getFullYear() === viewDate.getFullYear()
              const selected = key === selectedKey
              return (
                <button
                  key={key}
                  type="button"
                  data-date={key}
                  aria-label={`${date.getFullYear()}年${date.getMonth() + 1}月${date.getDate()}日`}
                  aria-pressed={selected}
                  tabIndex={sameLocalDay(date, focusDate) ? 0 : -1}
                  disabled={isDisabledDay(date)}
                  className={classNames(
                    styles.calendarDay,
                    !inMonth && styles.calendarDayOutside,
                    key === todayKey && styles.calendarDayToday,
                    selected && styles.calendarDaySelected,
                  )}
                  onClick={() => selectDate(date)}
                  onFocus={() => setFocusDate(date)}
                  onKeyDown={(event) => onDayKeyDown(event, date)}
                >
                  {date.getDate()}
                </button>
              )
            })}
          </div>
          <div className={styles.calendarFooter}>
            <button type="button" className={styles.calendarAction} onClick={() => {
              setText('')
              setError(null)
              reportValidity(true)
              emitted.current = ''
              if (value !== '') onChange('')
              closeCalendar(true)
            }}>
              清除
            </button>
            <button type="button" className={styles.calendarAction} disabled={todayDisabled} onClick={() => selectDate(today)}>
              今天
            </button>
          </div>
        </div>,
        document.body,
      ) : null}
    </div>
  )
})

export type NumberFieldProps = Omit<InputHTMLAttributes<HTMLInputElement>, 'type'>

// NumberField keeps role=spinbutton and native min/max/step semantics while
// the CSS removes the browser spinner controls.
export const NumberField = forwardRef<HTMLInputElement, NumberFieldProps>(function NumberField({ className, ...props }, ref) {
  return <input ref={ref} {...props} type="number" className={classNames(styles.input, styles.numberField, className)} />
})

export interface SearchFieldProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type' | 'value' | 'onChange'> {
  value: string
  onChange: (value: string) => void
  clearLabel?: string
}

// SearchField is a controlled searchbox with one shared clear affordance; the
// WebKit cancel button is removed in CSS so the two never render side by side.
export const SearchField = forwardRef<HTMLInputElement, SearchFieldProps>(function SearchField({
  value, onChange, className, clearLabel = '清除搜索', disabled, ...props
}, ref) {
  return (
    <span className={styles.searchField}>
      <Icon name="search" size={16} className={styles.searchIcon} />
      <input
        ref={ref}
        {...props}
        type="search"
        role="searchbox"
        disabled={disabled}
        className={classNames(styles.input, styles.searchInput, className)}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
      {value !== '' ? (
        <button type="button" className={styles.searchClear} aria-label={clearLabel} disabled={disabled} onClick={() => onChange('')}>
          <Icon name="close" size={14} />
        </button>
      ) : null}
    </span>
  )
})

export type TextareaProps = TextareaHTMLAttributes<HTMLTextAreaElement> & { code?: boolean }

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(function Textarea({ code, className, ...props }, ref) {
  return <textarea ref={ref} className={classNames(styles.textarea, code && styles.textareaCode, className)} {...props} />
})

export interface TooltipProps {
  content: ReactNode
  /**
   * One ReactElement: only a single element can receive the injected
   * aria-describedby, so fragments, strings and arrays are not accepted.
   */
  children: ReactElement<{ 'aria-describedby'?: string }>
  className?: string
}

// Tooltip shows while hovered or focused, hides on Escape, and exposes its
// text via aria-describedby. It never sets a DOM title and portals above
// dialogs with measured viewport clamping.
export function Tooltip({ content, children, className }: TooltipProps) {
  const tooltipId = useId()
  const anchorRef = useRef<HTMLSpanElement | null>(null)
  const tooltipRef = useRef<HTMLDivElement | null>(null)
  const [hovered, setHovered] = useState(false)
  const [focused, setFocused] = useState(false)
  const [suppressed, setSuppressed] = useState(false)
  const open = (hovered || focused) && !suppressed
  const position = useTooltipPosition(open, anchorRef, tooltipRef)

  useEffect(() => {
    if (!hovered && !focused) setSuppressed(false)
  }, [hovered, focused])

  if (content === null || content === undefined || content === '') return children
  const existing = children.props['aria-describedby']?.split(/\s+/).filter(Boolean) ?? []
  const trigger = cloneElement(children, { 'aria-describedby': [...new Set([...existing, tooltipId])].join(' ') })
  return (
    <>
      <span
        ref={anchorRef}
        className={classNames(styles.tooltipAnchor, className)}
        onMouseEnter={() => setHovered(true)}
        onMouseLeave={() => setHovered(false)}
        onFocus={() => setFocused(true)}
        onBlur={(event) => {
          const related = event.relatedTarget as Node | null
          if (!related || !event.currentTarget.contains(related)) setFocused(false)
        }}
        onKeyDown={(event) => {
          if (event.key !== 'Escape' || !open) return
          event.preventDefault()
          event.stopPropagation()
          setSuppressed(true)
        }}
      >
        {trigger}
      </span>
      {createPortal(
        <div
          ref={tooltipRef}
          role="tooltip"
          id={tooltipId}
          hidden={!open}
          className={classNames(styles.tooltip, position?.placement === 'below' ? styles.tooltipBelow : styles.tooltipAbove)}
          style={position?.style}
        >
          {content}
        </div>,
        document.body,
      )}
    </>
  )
}

export interface DisclosureProps {
  summary: ReactNode
  children: ReactNode
  defaultOpen?: boolean
  open?: boolean
  onToggle?: (open: boolean) => void
  className?: string
}

// Disclosure keeps native details/summary semantics and only hides the marker.
// State follows the native toggle event, which fires after details.open has
// changed; no microtask or click-order assumption is involved. Enter and Space
// are handled explicitly so the behaviour does not depend on browser keyboard
// activation of summary.
export function Disclosure({ summary, children, defaultOpen = false, open, onToggle, className }: DisclosureProps) {
  const detailsRef = useRef<HTMLDetailsElement | null>(null)
  const [internalOpen, setInternalOpen] = useState(defaultOpen)
  const initialOpen = useRef(defaultOpen)
  const expanded = open ?? internalOpen

  useEffect(() => {
    if (open !== undefined) return
    // Uncontrolled details stay unmanaged by React, so an unrelated render
    // cannot reset the attribute between the click and its toggle event.
    const details = detailsRef.current
    if (details && details.open !== initialOpen.current) details.open = initialOpen.current
  }, [open])

  // A controlled parent that ignores a toggle must keep the old state. The
  // internal state bump guarantees this check runs even when the prop is
  // unchanged, so React's value-based attribute diff cannot leave it open.
  useLayoutEffect(() => {
    if (open === undefined) return
    const details = detailsRef.current
    if (details && details.open !== open) details.open = open
  }, [open, internalOpen])

  const sync = (next: boolean) => {
    if (next === expanded) return
    setInternalOpen(next)
    onToggle?.(next)
  }

  return (
    <details
      ref={detailsRef}
      className={classNames(styles.disclosure, className)}
      open={open}
      onToggle={(event) => sync(event.currentTarget.open)}
    >
      <summary
        className={styles.disclosureSummary}
        aria-expanded={expanded}
        onKeyDown={(event) => {
          if (event.key !== 'Enter' && event.key !== ' ') return
          event.preventDefault()
          const details = detailsRef.current
          if (!details) return
          const next = !details.open
          if (open === undefined) details.open = next
          sync(next)
        }}
      >
        <Icon name="chevron" size={15} className={styles.disclosureChevron} />
        <span>{summary}</span>
      </summary>
      <div className={styles.disclosureBody}>{children}</div>
    </details>
  )
}

const FOCUSABLE = 'a[href],button:not([disabled]),textarea:not([disabled]),input:not([disabled]):not([type="hidden"]),select:not([disabled]),[tabindex]:not([tabindex="-1"])'

// Only the topmost modal handles Tab and Escape. A confirmation rendered inside
// a drawer would otherwise be trapped by the drawer's own handler.
const modalStack: symbol[] = []

function useModalFocus(open: boolean, onClose: () => void, containerRef: React.RefObject<HTMLElement | null>) {
  const closeRef = useRef(onClose)
  closeRef.current = onClose
  const returnFocusRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    if (!open) return
    const previous = document.activeElement
    returnFocusRef.current = previous instanceof HTMLElement ? previous : null
    const container = containerRef.current
    if (!container) return
    const getFocusable = () => Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE))
      .filter((element) => !element.closest('[aria-hidden="true"]'))
    const focusables = getFocusable()
    ;(focusables[0] || container).focus()
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    const token = Symbol('modal')
    modalStack.push(token)
    const isTopmost = () => modalStack[modalStack.length - 1] === token

    const onKeyDown = (event: KeyboardEvent) => {
      if (!isTopmost()) return
      if (event.key === 'Escape') {
        event.preventDefault()
        closeRef.current()
        return
      }
      if (event.key !== 'Tab') return
      const elements = getFocusable()
      if (!elements.length) {
        event.preventDefault()
        container.focus()
        return
      }
      const first = elements[0]
      const last = elements[elements.length - 1]
      if (!container.contains(document.activeElement)) {
        event.preventDefault()
        ;(event.shiftKey ? last : first).focus()
        return
      }
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      const index = modalStack.lastIndexOf(token)
      if (index >= 0) modalStack.splice(index, 1)
      document.body.style.overflow = previousOverflow
      const target = returnFocusRef.current
      if (target?.isConnected) target.focus()
    }
  }, [open, containerRef])
}

interface DialogProps {
  open: boolean
  onClose: () => void
  title: string
  description?: string
  children?: ReactNode
  footer?: ReactNode
  wide?: boolean
}

export function Dialog({ open, onClose, title, description, children, footer, wide }: DialogProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const titleId = useId()
  const descriptionId = useId()
  useModalFocus(open, onClose, containerRef)
  if (!open) return null
  return createPortal(
    <div
      className={styles.modalOverlay}
      onMouseDown={(event) => { if (event.target === event.currentTarget) onClose() }}
    >
      <div
        ref={containerRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descriptionId : undefined}
        tabIndex={-1}
        className={classNames(styles.modal, wide && styles.modalWide)}
      >
        <div className={styles.modalHeader}>
          <div className={styles.stack}>
            <h2 className={styles.modalTitle} id={titleId}>{title}</h2>
            {description ? <p className={styles.modalDescription} id={descriptionId}>{description}</p> : null}
          </div>
          <button type="button" className={styles.iconButton} onClick={onClose} aria-label="关闭对话框">
            <Icon name="close" />
          </button>
        </div>
        {children != null ? <div className={styles.modalBody}>{children}</div> : null}
        {footer ? <div className={styles.modalFooter}>{footer}</div> : null}
      </div>
    </div>,
    document.body,
  )
}

export function Drawer({ open, onClose, title, description, children, footer }: DialogProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const titleId = useId()
  const descriptionId = useId()
  useModalFocus(open, onClose, containerRef)
  if (!open) return null
  return createPortal(
    <div className={styles.drawerOverlay} onMouseDown={(event) => { if (event.target === event.currentTarget) onClose() }}>
      <aside
        ref={containerRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descriptionId : undefined}
        tabIndex={-1}
        className={styles.drawer}
      >
        <div className={styles.drawerHeader}>
          <div className={styles.stack}>
            <h2 className={styles.modalTitle} id={titleId}>{title}</h2>
            {description ? <p className={styles.modalDescription} id={descriptionId}>{description}</p> : null}
          </div>
          <button type="button" className={styles.iconButton} onClick={onClose} aria-label="关闭抽屉">
            <Icon name="close" />
          </button>
        </div>
        {children != null ? <div className={styles.drawerBody}>{children}</div> : null}
        {footer ? <div className={styles.drawerFooter}>{footer}</div> : null}
      </aside>
    </div>,
    document.body,
  )
}

export function ConfirmDialog({
  open, onClose, onConfirm, title, description, confirmLabel = '确认', pending, error, errorHint, danger = true,
}: {
  open: boolean
  onClose: () => void
  onConfirm: () => void
  title: string
  description: string
  confirmLabel?: string
  pending?: boolean
  error?: unknown
  errorHint?: string | null
  danger?: boolean
}) {
  return (
    <Dialog
      open={open}
      onClose={pending ? () => {} : onClose}
      title={title}
      description={description}
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant={danger ? 'danger' : 'primary'} onClick={onConfirm} loading={pending}>{confirmLabel}</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} title="操作失败" hint={errorHint} /> : null}
    </Dialog>
  )
}

type ToastTone = 'success' | 'error' | 'info'
interface ToastItem { id: number; tone: ToastTone; message: string }

const ToastContext = createContext<{ push: (tone: ToastTone, message: string) => void } | null>(null)

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const idRef = useRef(0)
  const timers = useRef(new Map<number, number>())

  const remove = useCallback((id: number) => {
    const timer = timers.current.get(id)
    if (timer !== undefined) {
      window.clearTimeout(timer)
      timers.current.delete(id)
    }
    setItems((list) => list.filter((item) => item.id !== id))
  }, [])

  const push = useCallback((tone: ToastTone, message: string) => {
    idRef.current += 1
    const id = idRef.current
    setItems((list) => [...list, { id, tone, message }])
    const timer = window.setTimeout(() => remove(id), 6000)
    timers.current.set(id, timer)
  }, [remove])

  useEffect(() => () => {
    for (const timer of timers.current.values()) window.clearTimeout(timer)
    timers.current.clear()
  }, [])

  return (
    <ToastContext.Provider value={{ push }}>
      {children}
      <div className={styles.toastRegion} role="status" aria-live="polite">
        {items.map((item) => (
          <div key={item.id} role={item.tone === 'error' ? 'alert' : undefined} className={classNames(styles.toast, item.tone === 'success' && styles.toastSuccess, item.tone === 'error' && styles.toastError)}>
            <span className={styles.toastMessage}>{item.message}</span>
            <button type="button" className={styles.iconButton} onClick={() => remove(item.id)} aria-label="关闭提示">
              <Icon name="close" size={14} />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast() {
  const context = useContext(ToastContext)
  if (!context) throw new Error('useToast must be used inside ToastProvider')
  return context
}

export function CopyButton({ value, label = '复制' }: { value: string; label?: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle')
  const resetTimer = useRef<number | null>(null)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      if (resetTimer.current !== null) window.clearTimeout(resetTimer.current)
    }
  }, [])
  const onCopy = async () => {
    let next: 'copied' | 'failed'
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable')
      await navigator.clipboard.writeText(value)
      next = 'copied'
    } catch {
      next = 'failed'
    }
    if (!mounted.current) return
    setState(next)
    if (resetTimer.current !== null) window.clearTimeout(resetTimer.current)
    resetTimer.current = window.setTimeout(() => {
      resetTimer.current = null
      setState('idle')
    }, 2500)
  }
  const status = state === 'copied' ? '已复制' : state === 'failed' ? '复制失败，请手动复制' : ''
  return (
    <Button variant="secondary" aria-label={label} onClick={() => { void onCopy() }}>
      <Icon name={state === 'copied' ? 'check' : 'copy'} size={15} />
      <span aria-hidden="true">{status || label}</span>
      <span className="srOnly" role="status" aria-live="polite">{status}</span>
    </Button>
  )
}

export function SecretField({ label, value }: { label: string; value: string }) {
  const id = useId()
  return (
    <div className={styles.field}>
      <label className={styles.label} htmlFor={id}>{label}</label>
      <div className={styles.secret}>
        <input id={id} className="input" readOnly value={value} onFocus={(event) => event.currentTarget.select()} />
        <CopyButton value={value} />
      </div>
      <span className={styles.hint}>只显示这一次，关闭后将无法再次查看。</span>
    </div>
  )
}
