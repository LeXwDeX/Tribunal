export function formatNumber(value: unknown): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '不可用'
  return new Intl.NumberFormat('zh-CN').format(value)
}

export function formatCompactNumber(value: unknown): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—'
  const absolute = Math.abs(value)
  if (absolute >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)}B`
  if (absolute >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (absolute >= 1_000) return `${(value / 1_000).toFixed(1)}k`
  return String(value)
}

export function formatDuration(ms: unknown): string {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  const totalSeconds = Math.round(ms / 1000)
  if (totalSeconds < 60) return `${totalSeconds}s`
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  if (minutes < 60) return seconds ? `${minutes}m ${seconds}s` : `${minutes}m`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

export function formatCost(micros: unknown): string {
  if (typeof micros !== 'number' || !Number.isFinite(micros)) return '—'
  return `$${(micros / 1_000_000).toFixed(4)}`
}

export function formatTokens(input: unknown, output?: unknown): string {
  if (typeof input !== 'number' && typeof output !== 'number') return '—'
  return `${formatCompactNumber(input)} / ${formatCompactNumber(output)}`
}

export function formatDate(value?: string): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit',
  }).format(date)
}

export function formatRelative(value?: string): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  const diffMs = date.getTime() - Date.now()
  const abs = Math.abs(diffMs)
  const units: Array<[Intl.RelativeTimeFormatUnit, number]> = [
    ['year', 1000 * 60 * 60 * 24 * 365],
    ['month', 1000 * 60 * 60 * 24 * 30],
    ['day', 1000 * 60 * 60 * 24],
    ['hour', 1000 * 60 * 60],
    ['minute', 1000 * 60],
  ]
  const formatter = new Intl.RelativeTimeFormat('zh-CN', { numeric: 'auto' })
  for (const [unit, size] of units) {
    if (abs >= size) return formatter.format(Math.round(diffMs / size), unit)
  }
  return '刚刚'
}

export function shortSha(sha?: string): string {
  return sha ? sha.slice(0, 7) : '—'
}

export function safeExternalUrl(value?: string | null): string | undefined {
  if (!value) return undefined
  try {
    const url = new URL(value)
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.toString() : undefined
  } catch {
    return undefined
  }
}

export function readNumber(source: Record<string, unknown> | undefined, keys: string[]): number | undefined {
  if (!source) return undefined
  for (const key of keys) {
    const value = source[key]
    if (typeof value === 'number' && Number.isFinite(value)) return value
  }
  return undefined
}

export function readString(source: Record<string, unknown> | undefined, keys: string[]): string | undefined {
  if (!source) return undefined
  for (const key of keys) {
    const value = source[key]
    if (typeof value === 'string' && value) return value
  }
  return undefined
}

export function readRecord(source: Record<string, unknown> | undefined, keys: string[]): Record<string, unknown> | undefined {
  if (!source) return undefined
  for (const key of keys) {
    const value = source[key]
    if (value && typeof value === 'object' && !Array.isArray(value)) return value as Record<string, unknown>
  }
  return undefined
}

const STATUS_LABELS: Record<string, string> = {
  pending: '排队中',
  queued: '排队中',
  running: '运行中',
  completed: '已完成',
  failed: '失败',
  cancelled: '已取消',
  skipped: '跳过',
  new: '未初始化',
  onboarding: '初始化中',
  ready: '就绪',
  refreshing: '刷新中',
  error: '异常',
  enabled: '启用',
  disabled: '停用',
  passed: '通过',
  success: '成功',
  opened: '打开',
  merged: '已合并',
  closed: '已关闭',
}

const WEBHOOK_STATE_LABELS: Record<string, string> = {
  disabled: '未启用',
  pending: '配置中',
  enabled: '已启用',
  failed: '配置失败',
  unknown: '待确认',
}

export function labelForWebhookState(state?: string): string {
  if (!state) return '未启用'
  return WEBHOOK_STATE_LABELS[state] || state
}

export function webhookTone(state?: string): 'ok' | 'warn' | 'danger' | 'neutral' {
  switch (state) {
    case 'enabled':
      return 'ok'
    case 'failed':
      return 'danger'
    case 'pending':
    case 'unknown':
      return 'warn'
    default:
      return 'neutral'
  }
}

const TRIGGER_LABELS: Record<string, string> = {
  mr_open: 'MR 打开',
  mr_update: 'MR 更新',
  command: '评论命令',
  api: 'API 手动触发',
  push: '推送',
  register: '项目注册',
}

const DECISION_LABELS: Record<string, string> = {
  publish: '发布',
  drop: '丢弃',
  mark: '存疑',
  downgrade: '降级',
}

const CHALLENGE_LABELS: Record<string, string> = {
  confirmed: 'CONFIRMED',
  refuted: 'REFUTED',
  uncertain: 'UNCERTAIN',
  skipped: '未挑战',
}

const PUBLICATION_LABELS: Record<string, string> = {
  pending: '待发布',
  published: '已发布',
  skipped: '已跳过',
  failed: '发布失败',
}

const SOURCE_LABELS: Record<string, string> = {
  generated: '生成',
  manual: '人工',
}

export const labelForStatus = (status?: string): string => (status ? STATUS_LABELS[status] || status : '—')
export const labelForTrigger = (trigger?: string): string => (trigger ? TRIGGER_LABELS[trigger] || trigger : '—')
export const labelForDecision = (decision?: string): string => (decision ? DECISION_LABELS[decision] || decision : '—')
export const labelForChallenge = (status?: string): string => (status ? CHALLENGE_LABELS[status] || status : '未挑战')
export const labelForSource = (source?: string): string => (source ? SOURCE_LABELS[source] || source : '—')
export const labelForPublication = (state?: string): string => (state ? PUBLICATION_LABELS[state] || state : '不适用')

export function statusTone(status?: string): 'ok' | 'warn' | 'danger' | 'info' | 'neutral' {
  switch (status) {
    case 'ready':
    case 'passed':
    case 'success':
    case 'enabled':
      return 'ok'
    case 'completed':
    case 'cancelled':
    case 'skipped':
      return 'neutral'
    case 'running':
    case 'refreshing':
    case 'onboarding':
    case 'pending':
    case 'queued':
      return 'info'
    case 'failed':
    case 'error':
      return 'danger'
    case 'mark':
    case 'uncertain':
      return 'warn'
    default:
      return 'neutral'
  }
}

export function verdictTone(verdict?: string | null): 'ok' | 'warn' | 'danger' | 'neutral' {
  if (verdict === 'PASS') return 'ok'
  if (verdict === 'BLOCKING') return 'danger'
  if (verdict === 'INCOMPLETE') return 'warn'
  return 'neutral'
}

export function publicationTone(state?: string): 'ok' | 'warn' | 'danger' | 'info' | 'neutral' {
  switch (state) {
    case 'published':
      return 'ok'
    case 'failed':
      return 'danger'
    case 'pending':
      return 'info'
    case 'skipped':
    default:
      return 'neutral'
  }
}

export function decisionTone(decision?: string): 'ok' | 'warn' | 'danger' | 'info' | 'neutral' {
  switch (decision) {
    case 'publish':
      return 'info'
    case 'mark':
    case 'downgrade':
      return 'warn'
    case 'drop':
      return 'neutral'
    default:
      return 'neutral'
  }
}

export function formatJson(value: unknown): string {
  if (value === undefined || value === null) return ''
  if (typeof value === 'string') return value
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    return String(value)
  }
}

export function stringifyUnknown(value: unknown): string {
  if (typeof value === 'string') return value
  return formatJson(value)
}
