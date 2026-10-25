import {
  createContext, forwardRef, useCallback, useContext, useEffect, useId, useRef, useState,
  type ButtonHTMLAttributes, type ReactNode,
} from 'react'
import { createPortal } from 'react-dom'
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
  return <span className={classNames(styles.sev, SEVERITY_CLASSES[severity])}>{severity}</span>
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

export function PageHeader({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <header className={styles.pageHeader}>
      <div className={styles.pageHeaderText}>
        <h1 className={styles.pageTitle}>{title}</h1>
        {description ? <p className={styles.pageDescription}>{description}</p> : null}
      </div>
      {actions ? <div className={styles.headerActions}>{actions}</div> : null}
    </header>
  )
}

export function Panel({
  title, aside, children, className,
}: { title?: ReactNode; aside?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={classNames(styles.panel, className)}>
      {title || aside ? (
        <div className={styles.panelHeader}>
          {title ? <h2 className={styles.panelTitle}>{title}</h2> : <span />}
          {aside ? <div className={styles.panelAside}>{aside}</div> : null}
        </div>
      ) : null}
      {children}
    </section>
  )
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

export function Field({
  label, htmlFor, hint, error, children,
}: { label: string; htmlFor: string; hint?: ReactNode; error?: ReactNode; children: ReactNode }) {
  return (
    <div className={styles.field}>
      <label className={styles.label} htmlFor={htmlFor}>{label}</label>
      {children}
      {hint ? <span className={styles.hint}>{hint}</span> : null}
      {error ? <span className={styles.errorText} id={`${htmlFor}-error`} role="alert">{error}</span> : null}
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

export function ErrorState({ error, onRetry, title = '加载失败' }: { error: unknown; onRetry?: () => void; title?: string }) {
  const forbidden = error instanceof ApiError && error.status === 403
  return (
    <div className={styles.errorBox} role="alert">
      <strong>{forbidden ? '没有访问权限' : title}</strong>
      <span>{forbidden ? '当前账号没有执行此操作的权限。' : errorMessage(error)}</span>
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
  items, value, onChange, label,
}: { items: Array<{ key: string; label: string; count?: number }>; value: string; onChange: (key: string) => void; label: string }) {
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
            id={`tab-${item.key}`}
            aria-selected={selected}
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

const FOCUSABLE = 'a[href],button:not([disabled]),textarea:not([disabled]),input:not([disabled]):not([type="hidden"]),select:not([disabled]),[tabindex]:not([tabindex="-1"])'

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
    const focusables = Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE))
    ;(focusables[0] || container).focus()
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        closeRef.current()
        return
      }
      if (event.key !== 'Tab') return
      const elements = Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE))
        .filter((element) => !element.hasAttribute('aria-hidden'))
      if (!elements.length) {
        event.preventDefault()
        container.focus()
        return
      }
      const first = elements[0]
      const last = elements[elements.length - 1]
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
        className={styles.modal}
        style={wide ? { width: 'min(920px, 100%)' } : undefined}
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
        {footer ? <div className={styles.modalFooter}>{footer}</div> : null}
      </aside>
    </div>,
    document.body,
  )
}

export function ConfirmDialog({
  open, onClose, onConfirm, title, description, confirmLabel = '确认', pending, error, danger = true,
}: {
  open: boolean
  onClose: () => void
  onConfirm: () => void
  title: string
  description: string
  confirmLabel?: string
  pending?: boolean
  error?: unknown
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
      {error ? <ErrorState error={error} title="操作失败" /> : null}
    </Dialog>
  )
}

type ToastTone = 'success' | 'error' | 'info'
interface ToastItem { id: number; tone: ToastTone; message: string }

const ToastContext = createContext<{ push: (tone: ToastTone, message: string) => void } | null>(null)

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const idRef = useRef(0)
  const timers = useRef<number[]>([])

  const remove = useCallback((id: number) => {
    setItems((list) => list.filter((item) => item.id !== id))
  }, [])

  const push = useCallback((tone: ToastTone, message: string) => {
    idRef.current += 1
    const id = idRef.current
    setItems((list) => [...list, { id, tone, message }])
    const timer = window.setTimeout(() => remove(id), 6000)
    timers.current.push(timer)
  }, [remove])

  useEffect(() => () => {
    for (const timer of timers.current) window.clearTimeout(timer)
  }, [])

  return (
    <ToastContext.Provider value={{ push }}>
      {children}
      <div className={styles.toastRegion} role="status" aria-live="polite">
        {items.map((item) => (
          <div key={item.id} className={classNames(styles.toast, item.tone === 'success' && styles.toastSuccess, item.tone === 'error' && styles.toastError)}>
            <span style={{ flex: 1 }}>{item.message}</span>
            <button type="button" className={styles.iconButton} style={{ width: 28, height: 28 }} onClick={() => remove(item.id)} aria-label="关闭提示">
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
  const onCopy = async () => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable')
      await navigator.clipboard.writeText(value)
      setState('copied')
    } catch {
      setState('failed')
    }
    window.setTimeout(() => setState('idle'), 2500)
  }
  return (
    <Button variant="secondary" onClick={() => { void onCopy() }}>
      <Icon name={state === 'copied' ? 'check' : 'copy'} size={15} />
      {state === 'copied' ? '已复制' : state === 'failed' ? '复制失败，请手动复制' : label}
    </Button>
  )
}

export function SecretField({ label, value }: { label: string; value: string }) {
  const id = useId()
  return (
    <div className={styles.field}>
      <label className={styles.label} htmlFor={id}>{label}</label>
      <div className={styles.secret}>
        <input id={id} className={styles.input} readOnly value={value} onFocus={(event) => event.currentTarget.select()} />
        <CopyButton value={value} />
      </div>
      <span className={styles.hint}>只显示这一次，关闭后将无法再次查看。</span>
    </div>
  )
}
