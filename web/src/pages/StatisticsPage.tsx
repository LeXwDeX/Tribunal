import { Fragment, useId, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { getStats, listProjects } from '../api/api'
import type { DailyStats, Project, ProviderQualityRow, Stats } from '../api/types'
import {
  Badge, Button, EmptyState, ErrorState, LoadingState, Metric, PageHeader, Panel,
} from '../components/ui'
import { formatCost, formatNumber } from '../lib/format'
import { usePaginated } from '../lib/hooks'
import { formatMs, formatRatio, statNumber } from '../lib/stats'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'

type Row = Record<string, unknown>

const DATE_PATTERN = /^(\d{4})-(\d{2})-(\d{2})$/

function localMidnight(value: string): Date | null {
  const match = DATE_PATTERN.exec(value)
  if (!match) return null
  const year = Number(match[1])
  const month = Number(match[2])
  const day = Number(match[3])
  const date = new Date(year, month - 1, day)
  if (date.getFullYear() !== year || date.getMonth() !== month - 1 || date.getDate() !== day) return null
  return date
}

export function dateRange(value: { fromDate: string; toDate: string }): { from: string; to: string } | null {
  const start = localMidnight(value.fromDate)
  const end = localMidnight(value.toDate)
  if (!start || !end) return null
  const next = new Date(end.getFullYear(), end.getMonth(), end.getDate() + 1)
  if (Number.isNaN(next.getTime())) return null
  if (!(start.getTime() < next.getTime())) return null
  if (next.getFullYear() > 9999 || next.getUTCFullYear() > 9999) return null
  return { from: start.toISOString(), to: next.toISOString() }
}

function asRows(value: unknown): Row[] {
  if (Array.isArray(value)) return value.filter((item): item is Row => Boolean(item) && typeof item === 'object' && !Array.isArray(item))
  if (value && typeof value === 'object') {
    return Object.entries(value as Record<string, unknown>).map(([key, item]) => ({
      key,
      ...(item && typeof item === 'object' && !Array.isArray(item) ? item as Row : { value: item }),
    }))
  }
  return []
}

function decisionRows(value: unknown): Array<{ label: string; count: number }> {
  if (Array.isArray(value)) {
    return value.flatMap((item) => {
      if (!item || typeof item !== 'object') return []
      const row = item as Row
      const label = String(row.decision ?? row.key ?? row.name ?? '未知')
      const count = Number(row.count ?? row.value)
      return Number.isFinite(count) ? [{ label, count }] : []
    })
  }
  if (value && typeof value === 'object') {
    return Object.entries(value as Record<string, unknown>).flatMap(([label, count]) => {
      const numeric = Number(count)
      return Number.isFinite(numeric) ? [{ label, count: numeric }] : []
    })
  }
  return []
}

type FeedbackRole = 'proposer' | 'challenger'

type ProviderFeedbackCounts = {
  provider: string
  role: FeedbackRole
  total: number
  signals: Array<{ label: string; count: number }>
}

type FeedbackCounts = {
  total: number
  signals: Array<{ label: string; count: number }>
  providers: ProviderFeedbackCounts[] | null
}

const KNOWN_SIGNALS = ['thumbsup', 'thumbsdown', 'resolved_changed', 'resolved_unchanged']
const ROLE_LABELS: Record<FeedbackRole, string> = { proposer: '提出方', challenger: '挑战方' }

function signalRank(label: string): number {
  const rank = KNOWN_SIGNALS.indexOf(label)
  return rank === -1 ? KNOWN_SIGNALS.length : rank
}

function signalCounts(value: unknown): Array<{ label: string; count: number }> | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  const entries = Object.entries(value as Record<string, unknown>)
  const signals = entries.flatMap(([label, count]) => (
    typeof count === 'number' && Number.isFinite(count) && count >= 0 ? [{ label, count }] : []
  ))
  if (signals.length !== entries.length) return null
  signals.sort((a, b) => (
    signalRank(a.label) - signalRank(b.label)
    || (a.label < b.label ? -1 : a.label > b.label ? 1 : 0)
  ))
  return signals
}

function providerCounts(value: unknown): ProviderFeedbackCounts[] | null {
  if (value === undefined) return null
  if (!Array.isArray(value)) return null
  const rows: ProviderFeedbackCounts[] = []
  for (const item of value) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) return null
    const row = item as Row
    if (typeof row.provider !== 'string' || row.provider === '') return null
    if (row.role !== 'proposer' && row.role !== 'challenger') return null
    if (typeof row.total !== 'number' || !Number.isInteger(row.total) || row.total < 0) return null
    const signals = signalCounts(row.by_signal)
    if (!signals) return null
    rows.push({ provider: row.provider, role: row.role, total: row.total, signals })
  }
  return rows
}

function feedbackCounts(value: unknown): FeedbackCounts | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  const row = value as Row
  if (typeof row.total !== 'number' || !Number.isFinite(row.total) || row.total < 0) return null
  const signals = signalCounts(row.by_signal)
  if (!signals) return null
  const providers = providerCounts(row.by_provider)
  if (providers === null && row.by_provider !== undefined) return null
  return { total: row.total, signals, providers }
}

type QualityState =
  | { kind: 'missing' }
  | { kind: 'invalid' }
  | { kind: 'ok'; rows: ProviderQualityRow[] }

function isNullableRatio(value: unknown): value is number | null {
  return value === null || (typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1)
}

function parseProviderQuality(value: unknown): QualityState {
  if (value === undefined || value === null) return { kind: 'missing' }
  if (!Array.isArray(value)) return { kind: 'invalid' }
  const rows: ProviderQualityRow[] = []
  const seen = new Set<string>()
  for (const item of value) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) return { kind: 'invalid' }
    const row = item as Row
    if (typeof row.provider !== 'string' || !row.provider.trim() || seen.has(row.provider)) return { kind: 'invalid' }
    const counts = ['proposed', 'reviewed', 'valid', 'invalid', 'overturned', 'refuted', 'refuted_reviewed', 'false_refuted'] as const
    if (!counts.every((key) => isNonNegativeInteger(row[key]))) return { kind: 'invalid' }
    const proposed = row.proposed as number
    const reviewed = row.reviewed as number
    const valid = row.valid as number
    const invalid = row.invalid as number
    const overturned = row.overturned as number
    const refuted = row.refuted as number
    const refutedReviewed = row.refuted_reviewed as number
    const falseRefuted = row.false_refuted as number
    if (reviewed !== valid + invalid || reviewed > proposed || overturned > proposed) return { kind: 'invalid' }
    if (refutedReviewed > refuted || falseRefuted > refutedReviewed) return { kind: 'invalid' }
    if (!isNullableRatio(row.accuracy) || !isNullableRatio(row.overturned_ratio) || !isNullableRatio(row.false_kill_rate)) {
      return { kind: 'invalid' }
    }
    if (reviewed === 0 ? row.accuracy !== null : row.accuracy === null) return { kind: 'invalid' }
    if (proposed === 0 ? row.overturned_ratio !== null || overturned !== 0 : row.overturned_ratio === null) return { kind: 'invalid' }
    if (refutedReviewed === 0 ? row.false_kill_rate !== null : row.false_kill_rate === null) return { kind: 'invalid' }
    seen.add(row.provider)
    rows.push({
      provider: row.provider,
      proposed,
      reviewed,
      valid,
      invalid,
      accuracy: row.accuracy,
      overturned,
      overturned_ratio: row.overturned_ratio,
      refuted,
      refuted_reviewed: refutedReviewed,
      false_refuted: falseRefuted,
      false_kill_rate: row.false_kill_rate,
    })
  }
  return { kind: 'ok', rows }
}

function QualityRate({ numerator, denominator }: { numerator: number; denominator: number }) {
  if (denominator === 0) {
    return (
      <div className={styles.tablePrimary}>
        <span>无样本</span>
        <span className={`mono small ${styles.tableMeta}`}>0 / 0</span>
      </div>
    )
  }
  return (
    <div className={styles.tablePrimary}>
      <span className="tabular">{formatRatio(numerator / denominator)}</span>
      <span className={`mono small ${styles.tableMeta}`}>{formatNumber(numerator)} / {formatNumber(denominator)}</span>
    </div>
  )
}

function ProposerQualityTable({ rows }: { rows: ProviderQualityRow[] }) {
  return (
    <div className="tableWrap">
      <table className="dataTable" aria-label="按提出方统计的人工质量指标" style={{ minWidth: 760 }}>
        <thead>
          <tr>
            <th scope="col">Provider</th>
            <th scope="col">提出问题</th>
            <th scope="col">已复核</th>
            <th scope="col">有效</th>
            <th scope="col">无效</th>
            <th scope="col">准确率</th>
            <th scope="col">被推翻</th>
            <th scope="col">推翻率</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.provider}>
              <td className="mono">{row.provider}</td>
              <td className="tabular">{formatNumber(row.proposed)}</td>
              <td className="tabular">{formatNumber(row.reviewed)}</td>
              <td className="tabular">{formatNumber(row.valid)}</td>
              <td className="tabular">{formatNumber(row.invalid)}</td>
              <td><QualityRate numerator={row.valid} denominator={row.reviewed} /></td>
              <td className="tabular">{formatNumber(row.overturned)}</td>
              <td><QualityRate numerator={row.overturned} denominator={row.proposed} /></td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ChallengerQualityTable({ rows }: { rows: ProviderQualityRow[] }) {
  return (
    <div className="tableWrap">
      <table className="dataTable" aria-label="按挑战方统计的人工质量指标" style={{ minWidth: 560 }}>
        <thead>
          <tr>
            <th scope="col">Provider</th>
            <th scope="col">反驳数</th>
            <th scope="col">复核反驳</th>
            <th scope="col">误杀</th>
            <th scope="col">误杀率</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.provider}>
              <td className="mono">{row.provider}</td>
              <td className="tabular">{formatNumber(row.refuted)}</td>
              <td className="tabular">{formatNumber(row.refuted_reviewed)}</td>
              <td className="tabular">{formatNumber(row.false_refuted)}</td>
              <td><QualityRate numerator={row.false_refuted} denominator={row.refuted_reviewed} /></td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function QualityPanel({ value }: { value: unknown }) {
  const state = parseProviderQuality(value)
  if (state.kind === 'missing') {
    return (
      <Panel title="人工质量指标" aside="来自 stats.quality_by_provider">
        <EmptyState title="人工质量指标不可用" description="stats 响应没有 quality_by_provider 字段；旧版服务端可能不返回人工标注指标。" />
      </Panel>
    )
  }
  if (state.kind === 'invalid') {
    return (
      <Panel title="人工质量指标" aside="来自 stats.quality_by_provider">
        <EmptyState
          title="人工质量指标数据格式无效"
          description="stats.quality_by_provider 含负数、非整数、重复 provider、越界比例或彼此不一致的计数；为避免显示错误比例，这里不展示任何行。"
        />
      </Panel>
    )
  }
  const proposed = state.rows.filter((row) => row.proposed > 0)
  const refuted = state.rows.filter((row) => row.refuted > 0)
  return (
    <Panel title="人工质量指标" aside="来自 stats.quality_by_provider · 人工标注">
      {state.rows.length === 0 ? (
        <EmptyState title="没有人工质量样本" description="筛选范围内没有可归因的提出或反驳记录；这是已统计的空结果，不是不可用。" />
      ) : (
        <>
          <h3 className="small" style={providerTitleStyle}>提出方</h3>
          {proposed.length ? (
            <ProposerQualityTable rows={proposed} />
          ) : (
            <p className="small muted">没有提出方样本。</p>
          )}
          <h3 className="small" style={providerTitleStyle}>挑战方</h3>
          {refuted.length ? (
            <ChallengerQualityTable rows={refuted} />
          ) : (
            <p className="small muted">没有挑战方样本。</p>
          )}
        </>
      )}
      <div className={styles.notice}>
        准确率 = 人工标注有效 / 已复核（有效 + 无效）；推翻率 = 提出方问题被挑战推翻 / 提出问题；误杀率 = 反驳后经人工标注有效 / 复核的反驳。未标注和未确定不计入准确率或误杀率的分母，但仍计入提出数、反驳数和推翻率。缺少 provider 归因的角色不计入。没有样本时显示“无样本”，不显示 0.0%。人工标注不会改变发布结果或评审结论；它不同于下方 GitLab 原始反馈信号。
      </div>
    </Panel>
  )
}

type Trend =
  | { kind: 'missing' }
  | { kind: 'invalid' }
  | { kind: 'empty' }
  | { kind: 'ok'; rows: DailyStats[] }

type BreakdownState<T> =
  | { kind: 'missing' }
  | { kind: 'invalid' }
  | { kind: 'ok'; rows: T[] }

type BreakdownRow = {
  runs: number
  cost_micros: number
  duration_samples: number | null
  avg_duration_ms: number | null
  p95_duration_ms: number | null
  trend: Trend
}

type ProjectRow = BreakdownRow & {
  project_id: number
  blocking: number
  published: number
}

type GroupRow = BreakdownRow & { key: string }

const UTC_DAY_PATTERN = /^(\d{4})-(\d{2})-(\d{2})$/
const TREND_FIELDS = ['duration_samples', 'avg_duration_ms', 'p95_duration_ms', 'trend'] as const

function isObject(value: unknown): value is Row {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value)
}

function isNonNegativeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
}

function isNonNegativeNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}

function isUtcDay(value: string): boolean {
  const match = UTC_DAY_PATTERN.exec(value)
  if (!match) return false
  const year = Number(match[1])
  const month = Number(match[2])
  const day = Number(match[3])
  const date = new Date(Date.UTC(year, month - 1, day))
  return date.getUTCFullYear() === year && date.getUTCMonth() === month - 1 && date.getUTCDate() === day
}

function nullableDuration(value: unknown): number | null | undefined {
  if (value === null) return null
  return isNonNegativeNumber(value) ? value : undefined
}

function readDurationSamples(value: unknown, runs: unknown): number | null {
  if (!isNonNegativeInteger(value)) return null
  if (isNonNegativeInteger(runs) && value > runs) return null
  return value
}

function readDuration(value: unknown): number | null {
  return isNonNegativeNumber(value) ? value : null
}

function parseDailyStats(value: unknown): DailyStats | null {
  if (!isObject(value)) return null
  if (typeof value.day !== 'string' || !isUtcDay(value.day)) return null
  if (!isNonNegativeInteger(value.runs) || !isNonNegativeInteger(value.cost_micros) || !isNonNegativeInteger(value.duration_samples)) return null
  if (value.duration_samples > value.runs) return null
  const avg = nullableDuration(value.avg_duration_ms)
  const p95 = nullableDuration(value.p95_duration_ms)
  if (avg === undefined || p95 === undefined) return null
  if (value.duration_samples === 0 && (avg !== null || p95 !== null)) return null
  return {
    day: value.day,
    runs: value.runs,
    cost_micros: value.cost_micros,
    duration_samples: value.duration_samples,
    avg_duration_ms: avg,
    p95_duration_ms: p95,
  }
}

function parseDailyTrend(value: unknown): Trend {
  if (value === undefined || value === null) return { kind: 'missing' }
  if (!Array.isArray(value)) return { kind: 'invalid' }
  if (value.length === 0) return { kind: 'empty' }
  const rows: DailyStats[] = []
  const seen = new Set<string>()
  for (const item of value) {
    const row = parseDailyStats(item)
    if (!row || seen.has(row.day)) return { kind: 'invalid' }
    seen.add(row.day)
    rows.push(row)
  }
  return { kind: 'ok', rows }
}

function parseBreakdown(row: Row, allowLegacy: boolean): BreakdownRow | null {
  if (!isNonNegativeInteger(row.runs) || !isNonNegativeInteger(row.cost_micros)) return null
  const hasAny = TREND_FIELDS.some((key) => Object.prototype.hasOwnProperty.call(row, key))
  if (allowLegacy && !hasAny) {
    return {
      runs: row.runs,
      cost_micros: row.cost_micros,
      duration_samples: null,
      avg_duration_ms: null,
      p95_duration_ms: null,
      trend: { kind: 'missing' },
    }
  }
  const hasAll = TREND_FIELDS.every((key) => Object.prototype.hasOwnProperty.call(row, key))
  if (!hasAll || !isNonNegativeInteger(row.duration_samples)) return null
  if (row.duration_samples > row.runs) return null
  const avg = nullableDuration(row.avg_duration_ms)
  const p95 = nullableDuration(row.p95_duration_ms)
  if (avg === undefined || p95 === undefined) return null
  if (row.duration_samples === 0 && (avg !== null || p95 !== null)) return null
  const trend = parseDailyTrend(row.trend)
  if (trend.kind === 'missing' || trend.kind === 'invalid') return null
  return {
    runs: row.runs,
    cost_micros: row.cost_micros,
    duration_samples: row.duration_samples,
    avg_duration_ms: avg,
    p95_duration_ms: p95,
    trend,
  }
}

function parseProjectStats(value: unknown): BreakdownState<ProjectRow> {
  if (value === undefined || value === null) return { kind: 'missing' }
  if (!Array.isArray(value)) return { kind: 'invalid' }
  const rows: ProjectRow[] = []
  const seen = new Set<number>()
  for (const item of value) {
    if (!isObject(item)) return { kind: 'invalid' }
    if (!isNonNegativeInteger(item.project_id) || item.project_id === 0 || seen.has(item.project_id)) return { kind: 'invalid' }
    if (!isNonNegativeInteger(item.blocking) || !isNonNegativeInteger(item.published)) return { kind: 'invalid' }
    const breakdown = parseBreakdown(item, true)
    if (!breakdown) return { kind: 'invalid' }
    seen.add(item.project_id)
    rows.push({ project_id: item.project_id, blocking: item.blocking, published: item.published, ...breakdown })
  }
  return { kind: 'ok', rows }
}

function parseGroupStats(value: unknown, keyName: 'provider' | 'block'): BreakdownState<GroupRow> {
  if (value === undefined || value === null) return { kind: 'missing' }
  if (!Array.isArray(value)) return { kind: 'invalid' }
  const rows: GroupRow[] = []
  const seen = new Set<string>()
  for (const item of value) {
    if (!isObject(item)) return { kind: 'invalid' }
    const key = item[keyName]
    if (typeof key !== 'string' || !key.trim() || seen.has(key)) return { kind: 'invalid' }
    const breakdown = parseBreakdown(item, false)
    if (!breakdown) return { kind: 'invalid' }
    seen.add(key)
    rows.push({ key, ...breakdown })
  }
  return { kind: 'ok', rows }
}

function cellValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'number') return Number.isInteger(value) ? new Intl.NumberFormat('zh-CN').format(value) : String(value)
  if (typeof value === 'string') return value
  if (typeof value === 'boolean') return value ? 'true' : 'false'
  try { return JSON.stringify(value) } catch { return String(value) }
}

function DimensionTable({ rows, emptyLabel }: { rows: Row[]; emptyLabel: string }) {
  const columns = useMemo(() => {
    const set = new Set<string>()
    for (const row of rows.slice(0, 12)) for (const key of Object.keys(row)) set.add(key)
    return Array.from(set)
  }, [rows])
  if (!rows.length) return <EmptyState title={emptyLabel} description="stats API 没有返回该维度的数据。" />
  return (
    <div className="tableWrap">
      <table className="dataTable" style={{ minWidth: Math.max(560, columns.length * 130) }}>
        <thead><tr>{columns.map((column) => <th key={column} scope="col">{column}</th>)}</tr></thead>
        <tbody>
          {rows.map((row, index) => (
            <tr key={String(row.id ?? row.key ?? index)}>
              {columns.map((column) => <td key={column} className={typeof row[column] === 'number' ? 'tabular' : undefined}>{cellValue(row[column])}</td>)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ProviderAttribution({ providers }: { providers: ProviderFeedbackCounts[] | null }) {
  return (
    <>
      <h3 className="small" style={providerTitleStyle}>按 provider 与角色归因</h3>
      {providers === null ? (
        <EmptyState title="按 provider 归因不可用" description="stats.feedback 没有 by_provider 字段；旧版服务端可能不返回归因。原始信号总数仍显示在上方。" />
      ) : providers.length === 0 ? (
        <EmptyState title="没有可归因的反馈" description="当前筛选范围内没有同时关联 finding 与挑战节点的反馈；summary、未关联或关联不一致的反馈不计入。" />
      ) : (
        <div className="tableWrap">
          <table className="dataTable" style={{ minWidth: 480 }}>
            <thead>
              <tr>
                <th scope="col">Provider</th>
                <th scope="col">角色</th>
                <th scope="col">计数</th>
                <th scope="col">信号</th>
              </tr>
            </thead>
            <tbody>
              {providers.map((row) => (
                <tr key={`${row.provider}:${row.role}`}>
                  <td className="mono">{row.provider}</td>
                  <td><Badge tone={row.role === 'proposer' ? 'info' : 'neutral'}>{ROLE_LABELS[row.role]}</Badge></td>
                  <td className="tabular">{row.total}</td>
                  <td>
                    <span style={{ display: 'inline-flex', gap: 10, flexWrap: 'wrap' }}>
                      {row.signals.map((signal) => (
                        <span key={signal.label} className="small">{signal.label}: {signal.count}</span>
                      ))}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className={styles.notice}>
        计数是反馈归因，不代表判断正确率或误杀率。同一信号可能同时计入提出方和挑战方，不能把两个角色的计数相加当作反馈总数。summary、未关联或关联不一致的反馈不计入。
      </div>
    </>
  )
}

function FeedbackPanel({ value }: { value: unknown }) {
  const missing = value === undefined || value === null
  const feedback = missing ? null : feedbackCounts(value)
  if (!feedback) {
    return (
      <Panel title="反馈信号" aside="来自 stats.feedback">
        <EmptyState
          title="反馈统计不可用"
          description={missing
            ? 'stats 响应没有 feedback 字段；旧版服务端可能不返回该字段。'
            : 'stats.feedback 格式无效；为避免显示错误总数，这里不展示任何反馈计数。'}
        />
      </Panel>
    )
  }
  return (
    <Panel title="反馈信号" aside="来自 stats.feedback">
      <div className={styles.statGrid}>
        <Metric label="反馈总数" value={feedback.total} note="按反馈时间与项目筛选计数" />
      </div>
      {feedback.signals.length ? (
        <div className={styles.budgetRow}>
          {feedback.signals.map((signal) => (
            <Badge key={signal.label} tone={signal.label === 'thumbsup' ? 'ok' : signal.label === 'thumbsdown' ? 'warn' : 'neutral'}>
              {signal.label}: {signal.count}
            </Badge>
          ))}
        </div>
      ) : feedback.total === 0 ? (
        <EmptyState title="没有反馈信号" description="筛选范围内的反馈总数为 0；这是已统计的空结果，不是不可用。" />
      ) : (
        <span className="small muted">stats.feedback 没有返回按信号的明细。</span>
      )}
      <ProviderAttribution providers={feedback.providers} />
      <div className={styles.notice}>
        按 GitLab 反馈信号展示原始计数；这是原始反馈，不代表模型准确率，也不是人工标注的误杀率。人工评估见上方“人工质量指标”。
      </div>
    </Panel>
  )
}

function DailyTrendTable({ rows, label, countLabel }: { rows: DailyStats[]; label: string; countLabel: string }) {
  return (
    <div className="tableWrap">
      <table className="dataTable" aria-label={label} style={{ minWidth: 640 }}>
        <caption className="small" style={trendCaptionStyle}>
          按工作流创建时间归属 UTC 日；只显示有执行的日期，不补零。
        </caption>
        <thead>
          <tr>
            <th scope="col">日期 (UTC)</th>
            <th scope="col">{countLabel}</th>
            <th scope="col">成本</th>
            <th scope="col">平均用时</th>
            <th scope="col">P95 用时</th>
            <th scope="col">耗时样本</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.day}>
              <td><time dateTime={`${row.day}T00:00:00Z`} className="mono">{row.day}</time></td>
              <td className="tabular">{formatNumber(row.runs)}</td>
              <td className="tabular">{formatCost(row.cost_micros)}</td>
              <td className="tabular">{formatMs(row.avg_duration_ms)}</td>
              <td className="tabular">{formatMs(row.p95_duration_ms)}</td>
              <td className="tabular">{formatNumber(row.duration_samples)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function DimensionTrend({ trend, label, countLabel }: { trend: Trend; label: string; countLabel: string }) {
  if (trend.kind === 'missing') return <p className="small muted">该行没有每日趋势数据；旧版服务端可能不返回。</p>
  if (trend.kind === 'invalid') return <p className="small muted">该行每日趋势格式无效，未显示任何行。</p>
  if (trend.kind === 'empty') return <p className="small muted">该范围内没有每日执行数据。</p>
  return <DailyTrendTable rows={trend.rows} label={label} countLabel={countLabel} />
}

function OverallTrend({ trend }: { trend: Trend }) {
  if (trend.kind === 'missing') {
    return <EmptyState title="每日趋势不可用" description="stats 响应没有 trend 字段；旧版服务端可能不返回每日趋势。" />
  }
  if (trend.kind === 'invalid') {
    return <EmptyState title="每日趋势数据格式无效" description="stats.trend 中含负数、非数字或无效日期；为避免显示错误合计，这里不展示任何趋势行。" />
  }
  if (trend.kind === 'empty') {
    return <EmptyState title="没有每日趋势" description="trend 为空表示筛选范围内没有执行；这是已统计的空结果，不是不可用。缺失日期不会补零。" />
  }
  return <DailyTrendTable rows={trend.rows} label="每日运行趋势" countLabel="工作流次数" />
}

function ProjectStatsPanel({ value }: { value: unknown }) {
  const [expanded, setExpanded] = useState<number | null>(null)
  const detailBase = useId()
  const state = parseProjectStats(value)

  if (state.kind === 'missing') {
    return (
      <Panel title="项目统计" aside="来自 stats.by_project">
        <EmptyState title="项目统计不可用" description="stats 响应没有 by_project 字段；旧版服务端可能不返回按项目统计。" />
      </Panel>
    )
  }
  if (state.kind === 'invalid') {
    return (
      <Panel title="项目统计" aside="来自 stats.by_project">
        <EmptyState title="项目统计数据格式无效" description="stats.by_project 中含负数、非数字或无效字段；为避免显示错误合计，这里不展示任何项目行。" />
      </Panel>
    )
  }
  if (!state.rows.length) {
    return (
      <Panel title="项目统计" aside="来自 stats.by_project">
        <EmptyState title="没有项目统计" description="筛选范围内没有项目工作流；这是已统计的空结果，不是不可用。" />
      </Panel>
    )
  }

  return (
    <Panel title="项目统计" aside="按工作流创建时间 · 来自 stats.by_project">
      <p className="small muted">运行次数统计窗口内全部工作流（含评审与 onboarding）；BLOCKING 与已发布仍按项目工作流统计。</p>
      <div className="tableWrap">
        <table className="dataTable" aria-label="按项目统计明细" style={{ minWidth: 920 }}>
          <thead>
            <tr>
              <th scope="col">项目</th>
              <th scope="col">工作流次数</th>
              <th scope="col">BLOCKING</th>
              <th scope="col">已发布</th>
              <th scope="col">成本</th>
              <th scope="col">平均用时</th>
              <th scope="col">P95 用时</th>
              <th scope="col">耗时样本</th>
              <th scope="col">每日趋势</th>
            </tr>
          </thead>
          <tbody>
            {state.rows.map((row, index) => {
              const label = `项目 #${row.project_id}`
              const open = expanded === row.project_id
              const detailId = `${detailBase}-${index}`
              return (
                <Fragment key={row.project_id}>
                  <tr>
                    <td className="mono">{label}</td>
                    <td className="tabular">{formatNumber(row.runs)}</td>
                    <td className="tabular">{formatNumber(row.blocking)}</td>
                    <td className="tabular">{formatNumber(row.published)}</td>
                    <td className="tabular">{formatCost(row.cost_micros)}</td>
                    <td className="tabular">{formatMs(row.avg_duration_ms)}</td>
                    <td className="tabular">{formatMs(row.p95_duration_ms)}</td>
                    <td className="tabular">{row.duration_samples === null ? '不可用' : formatNumber(row.duration_samples)}</td>
                    <td>
                      {row.trend.kind === 'missing' || row.trend.kind === 'invalid' ? (
                        <span className="small muted">{row.trend.kind === 'missing' ? '不可用' : '格式无效'}</span>
                      ) : (
                        <button
                          type="button"
                          className={styles.expandButton}
                          aria-expanded={open}
                          aria-controls={open ? detailId : undefined}
                          aria-label={`${open ? '收起' : '查看'} ${label} 每日趋势`}
                          onClick={() => setExpanded(open ? null : row.project_id)}
                        >
                          {open ? '收起' : '每日趋势'}
                        </button>
                      )}
                    </td>
                  </tr>
                  {open ? (
                    <tr>
                      <td colSpan={9} style={trendCellStyle}>
                        <div id={detailId} role="group" aria-label={`${label} 每日趋势`}>
                          <DimensionTrend trend={row.trend} label={`${label} 每日趋势`} countLabel="工作流次数" />
                        </div>
                      </td>
                    </tr>
                  ) : null}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      </div>
      <div className={styles.notice}>日期按工作流创建时间归入 UTC 日；筛选范围仍按浏览器本地日选择。</div>
    </Panel>
  )
}

function GroupStatsPanel({ value, dimension }: { value: unknown; dimension: 'provider' | 'block' }) {
  const [expanded, setExpanded] = useState<string | null>(null)
  const detailBase = useId()
  const state = parseGroupStats(value, dimension)
  const title = dimension === 'provider' ? 'Provider 统计' : 'Block 统计'

  if (state.kind === 'missing') {
    return (
      <Panel title={title} aside={`来自 stats.by_${dimension}`}>
        <EmptyState title={`${title}不可用`} description={`stats 响应没有 by_${dimension} 字段；旧版服务端可能不返回节点维度统计。`} />
      </Panel>
    )
  }
  if (state.kind === 'invalid') {
    return (
      <Panel title={title} aside={`来自 stats.by_${dimension}`}>
        <EmptyState title={`${title}数据格式无效`} description={`stats.by_${dimension} 中含负数、非数字或无效字段；为避免显示错误合计，这里不展示任何行。`} />
      </Panel>
    )
  }
  if (!state.rows.length) {
    return (
      <Panel title={title} aside={`来自 stats.by_${dimension}`}>
        <EmptyState title={`没有 ${dimension} 统计`} description="筛选范围内没有该维度的已启动节点；这是已统计的空结果，不是不可用。" />
      </Panel>
    )
  }

  return (
    <Panel title={title} aside={`按已启动节点统计 · 来自 stats.by_${dimension}`}>
      <div className="tableWrap">
        <table className="dataTable" aria-label={`按 ${dimension} 统计明细`} style={{ minWidth: 820 }}>
          <thead>
            <tr>
              <th scope="col">{dimension === 'provider' ? 'Provider' : 'Block'}</th>
              <th scope="col">已启动节点数</th>
              <th scope="col">成本</th>
              <th scope="col">平均用时</th>
              <th scope="col">P95 用时</th>
              <th scope="col">耗时样本</th>
              <th scope="col">每日趋势</th>
            </tr>
          </thead>
          <tbody>
            {state.rows.map((row, index) => {
              const open = expanded === row.key
              const detailId = `${detailBase}-${index}`
              return (
                <Fragment key={row.key}>
                  <tr>
                    <td className="mono">{row.key}</td>
                    <td className="tabular">{formatNumber(row.runs)}</td>
                    <td className="tabular">{formatCost(row.cost_micros)}</td>
                    <td className="tabular">{formatMs(row.avg_duration_ms)}</td>
                    <td className="tabular">{formatMs(row.p95_duration_ms)}</td>
                    <td className="tabular">{formatNumber(row.duration_samples)}</td>
                    <td>
                      <button
                        type="button"
                        className={styles.expandButton}
                        aria-expanded={open}
                        aria-controls={open ? detailId : undefined}
                        aria-label={`${open ? '收起' : '查看'} ${row.key} 每日趋势`}
                        onClick={() => setExpanded(open ? null : row.key)}
                      >
                        {open ? '收起' : '每日趋势'}
                      </button>
                    </td>
                  </tr>
                  {open ? (
                    <tr>
                      <td colSpan={7} style={trendCellStyle}>
                        <div id={detailId} role="group" aria-label={`${row.key} 每日趋势`}>
                          <DimensionTrend trend={row.trend} label={`${row.key} 每日趋势`} countLabel="已启动节点数" />
                        </div>
                      </td>
                    </tr>
                  ) : null}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      </div>
      {dimension === 'provider' ? (
        <div className={styles.notice}>
          运行次数按已启动节点统计：只要节点有开始时间（started_at）就计入，每个当前保存的节点行只计一次。不按状态字段筛选，运行中、已完成和失败的节点都会计入；也不等于 LLM 调用次数或重试尝试次数。没有 provider 的节点只计入 block。这里显示当前保存的最新一次尝试，不是重试尝试历史。
        </div>
      ) : (
        <div className={styles.notice}>
          与 Provider 统计使用同一已启动节点口径；Block 始终存在，因此没有 provider 的节点仍会显示在这里。
        </div>
      )}
    </Panel>
  )
}

function StatsContent({ stats }: { stats: Stats }) {
  const decisions = decisionRows(stats.decisions)
  const byModel = asRows(stats.by_model)
  const runs = statNumber(stats, 'runs')
  const blocking = statNumber(stats, 'blocking')
  const published = statNumber(stats, 'published')
  const hasDurationSamples = Object.prototype.hasOwnProperty.call(stats, 'duration_samples')
  const durationSamples = hasDurationSamples ? readDurationSamples(stats.duration_samples, stats.runs) : null
  const durationsTrusted = durationSamples === null ? !hasDurationSamples : durationSamples > 0
  const avgDuration = durationsTrusted ? readDuration(stats.avg_duration_ms) : null
  const p95Duration = durationsTrusted ? readDuration(stats.p95_duration_ms) : null
  const trend = parseDailyTrend(stats.trend)

  return (
    <>
      <div className={styles.statGrid}>
        <Metric label="运行次数" value={runs === null ? '不可用' : runs} note={blocking === null ? '包含评审与 onboarding · BLOCKING 次数不可用' : `包含评审与 onboarding · BLOCKING ${blocking} 次`} />
        <Metric label="BLOCKING 比例" value={formatRatio(statNumber(stats, 'blocking_ratio'))} note="来自 stats.blocking_ratio" />
        <Metric label="已发布问题" value={published === null ? '不可用' : published} note="来自 stats.published" />
        <Metric label="规则丢弃比例" value={formatRatio(statNumber(stats, 'overturned_ratio'))} note="规则 drop 占比，不是模型准确率" />
        <Metric
          label="平均用时"
          value={formatMs(avgDuration)}
          note={`${durationSamples === null ? '样本数不可用' : `样本 ${formatNumber(durationSamples)}`} · P95 ${formatMs(p95Duration)}`}
        />
        <Metric label="成本" value={(() => { const cost = statNumber(stats, 'cost_micros'); return cost === null ? '不可用' : formatCost(cost) })()} note="按 API 的 micros 原值换算" />
      </div>

      <Panel title="每日运行趋势" aside="来自 stats.trend · UTC 日">
        <OverallTrend trend={trend} />
        <div className={styles.notice}>
          趋势按工作流创建时间归入 UTC 日，只返回有执行的日期，不补零。用时为空时表示没有有效的开始与结束时间配对；样本数显示参与计算的配对数量。
        </div>
      </Panel>

      <Panel title="问题处置分布" aside="来自 stats.decisions">
        {decisions.length ? (
          <div className={styles.budgetRow}>
            {decisions.map((row) => <Badge key={row.label} tone={row.label === 'publish' ? 'ok' : row.label === 'mark' ? 'warn' : 'neutral'}>{row.label}: {row.count}</Badge>)}
          </div>
        ) : <EmptyState title="没有处置分布数据" description="stats.decisions 缺失或为空；显示不可用，而不是零值。" />}
        <div className={styles.notice}>
          人工标注在评审详情的问题展开行中维护；这里只显示规则处置分布，人工评估汇总见下方“人工质量指标”。
        </div>
      </Panel>

      <QualityPanel value={stats.quality_by_provider} />

      <FeedbackPanel value={stats.feedback} />

      <ProjectStatsPanel value={stats.by_project} />
      <GroupStatsPanel value={stats.by_provider} dimension="provider" />
      <GroupStatsPanel value={stats.by_block} dimension="block" />

      <Panel title="按模型" aside="from stats.by_model">
        <DimensionTable rows={byModel} emptyLabel="没有按模型统计" />
      </Panel>

      <details>
        <summary style={{ minHeight: 44, display: 'flex', alignItems: 'center', cursor: 'pointer' }}>查看原始 stats 响应</summary>
        <pre className="pre statsJson" style={jsonStyle}>{JSON.stringify(stats, null, 2)}</pre>
      </details>
    </>
  )
}

export function StatisticsPage() {
  const initial = useMemo(() => {
    const to = new Date()
    const from = new Date(to.getTime() - 7 * 24 * 60 * 60 * 1000)
    const localDate = (date: Date) => `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
    return {
      fromDate: localDate(from),
      toDate: localDate(to),
      projectId: '',
    }
  }, [])
  const [fromDate, setFromDate] = useState(initial.fromDate)
  const [toDate, setToDate] = useState(initial.toDate)
  const [projectId, setProjectId] = useState(initial.projectId)
  const [applied, setApplied] = useState(initial)
  const [validation, setValidation] = useState('')

  const projects = usePaginated<Project>(
    ['projects', 'statistics'],
    (cursor) => listProjects({ limit: 100 }, cursor),
    'projects',
  )
  const range = dateRange(applied)
  const stats = useQuery({
    queryKey: ['stats', 'page', range?.from ?? '', range?.to ?? '', applied.projectId],
    queryFn: () => {
      if (!range) throw new Error('日期范围无效。')
      return getStats({ from: range.from, to: range.to, project_id: applied.projectId || undefined })
    },
    enabled: range !== null,
  })

  const apply = () => {
    if (!fromDate || !toDate) { setValidation('请选择开始和结束日期。'); return }
    if (fromDate > toDate) { setValidation('开始日期不能晚于结束日期。'); return }
    if (!dateRange({ fromDate, toDate })) { setValidation('日期范围无效，请重新选择。'); return }
    setValidation('')
    setApplied({ fromDate, toDate, projectId })
  }

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="统计与反馈"
          description="按时间与项目查看运行统计、每日趋势和节点维度数据；不可用的值不会显示为零。"
        />
        <Panel title="筛选">
          <div className={styles.inlineFilters}>
            <label style={fieldStyle}>
              <span className="small">开始日期</span>
              <input className="input" name="from" type="date" value={fromDate} onChange={(event) => setFromDate(event.target.value)} />
            </label>
            <label style={fieldStyle}>
              <span className="small">结束日期</span>
              <input className="input" name="to" type="date" value={toDate} onChange={(event) => setToDate(event.target.value)} />
            </label>
            <label style={fieldStyle}>
              <span className="small">项目</span>
              <select className="select" name="project_id" value={projectId} onChange={(event) => setProjectId(event.target.value)}>
                <option value="">全部项目</option>
                {projects.items.map((project) => <option key={project.id} value={project.id}>{project.path_with_namespace}</option>)}
              </select>
            </label>
            {projects.hasNextPage ? (
              <Button
                variant="secondary"
                onClick={projects.fetchNextPage}
                loading={projects.isFetchingNextPage}
              >
                {projects.isFetchNextPageError ? '重试加载更多项目' : '加载更多项目'}
              </Button>
            ) : null}
            <Button variant="primary" onClick={apply}>应用</Button>
          </div>
          {validation ? <span className={styles.resultError} role="alert">{validation}</span> : null}
          {projects.isError ? (
            <div className={styles.scopeBar}>
              <span className="small muted">
                {!projects.items.length
                  ? '项目筛选列表加载失败，仍可查看全部项目统计。'
                  : projects.isRefetchError
                    ? '项目列表刷新失败；已加载的项目与当前选择仍保留。'
                    : '更多项目加载失败；已加载的项目仍可选择。'}
              </span>
              {projects.items.length === 0 || projects.isRefetchError ? (
                <Button variant="secondary" onClick={projects.refetch} loading={projects.isFetching}>
                  {projects.items.length ? '重试刷新项目' : '重试加载项目'}
                </Button>
              ) : null}
            </div>
          ) : null}
        </Panel>

        {range && stats.isPending ? <LoadingState label="正在读取统计数据…" /> : null}
        {stats.isError ? <ErrorState error={stats.error} onRetry={() => { void stats.refetch() }} title="统计数据加载失败" /> : null}
        {stats.isSuccess && !stats.data ? <EmptyState title="stats 返回了空响应" description="页面不会用示例数据补全；请检查服务端统计实现。" /> : null}
        {stats.data ? <StatsContent stats={stats.data} /> : null}
      </div>
    </main>
  )
}

const providerTitleStyle = { margin: '16px 0 8px', fontWeight: 600 } as const
const fieldStyle = { display: 'flex', flexDirection: 'column', gap: 6, minWidth: 0 } as const
const jsonStyle = { background: 'var(--surface-2)', border: '1px solid var(--border)', borderRadius: 8, padding: 12, color: 'var(--text)' } as const
const trendCaptionStyle = { textAlign: 'left', padding: '0 10px 8px', color: 'var(--muted)' } as const
const trendCellStyle = { padding: 12, background: 'var(--surface-2)' } as const
