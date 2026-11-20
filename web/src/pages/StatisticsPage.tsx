import { Fragment, useId, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { getReviewStats, getStats, listProjects } from '../api/api'
import type {
  DailyStats, FeedbackCollectionState, ModelStatsRow, Project, ProviderQualityRow, ReviewStats, Stats, WorkspaceCapacity,
} from '../api/types'
import {
  Badge, Button, DataTable, DatePicker, Disclosure, EmptyState, ErrorState, LoadingState, Metric, PageHeader, Panel,
  ReasonBadge, Select, TABLE_PAGE_SIZE, TableAction, TableActions, TableNote, TablePager, classNames, tableCell,
  type SelectOption,
} from '../components/ui'
import { formatBytes, formatLatency, formatNumber, formatPercent, formatRelative } from '../lib/format'
import { useClientPage, usePaginated } from '../lib/hooks'
import { formatMs, formatRatio, statNumber } from '../lib/stats'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'
import insights from './insights.module.css'

type Row = Record<string, unknown>

// Short identifiers (provider, block, project, day) stay on one line instead of
// breaking mid-word when a wide table squeezes its columns.
const identCell = classNames(tableCell.mono, tableCell.nowrap)

// Cost fields are removed from the API (#407/#430). Older responses may still
// carry them, but the console never renders them, including the raw dump.
const LEGACY_COST_KEYS = new Set(['cost_micros', 'price'])

function stripLegacyCostKeys(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stripLegacyCostKeys)
  if (value && typeof value === 'object') {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>)
        .filter(([key]) => !LEGACY_COST_KEYS.has(key))
        .map(([key, item]) => [key, stripLegacyCostKeys(item)]),
    )
  }
  return value
}

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
  if (Array.isArray(value)) {
    return value
      .filter((item): item is Row => Boolean(item) && typeof item === 'object' && !Array.isArray(item))
      .map((row) => Object.fromEntries(Object.entries(row).filter(([key]) => !LEGACY_COST_KEYS.has(key))))
  }
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
      <div className={insights.ratio}>
        <span className={insights.ratioEmpty}>无样本</span>
        <small>0 / 0</small>
      </div>
    )
  }
  const ratio = Math.min(Math.max(numerator / denominator, 0), 1)
  return (
    <div className={insights.ratio}>
      <b>{formatPercent(ratio)}</b>
      <span className={insights.ratioBar} aria-hidden="true">
        {/* stylecheck-allow-inline: computed ratio width */}
        <i className={insights.ratioFill} style={{ width: `${Number((ratio * 100).toFixed(2))}%` }} />
      </span>
      <small>{formatNumber(numerator)} / {formatNumber(denominator)}</small>
    </div>
  )
}

function ProposerQualityTable({ rows }: { rows: ProviderQualityRow[] }) {
  const paged = useClientPage(rows, TABLE_PAGE_SIZE)
  return (
    <>
      <DataTable
        label="按提出方统计的人工质量指标"
        minWidth="lg"
        isEmpty={rows.length === 0}
        empty={<p className="small muted">没有提出方样本。</p>}
      >
        <thead>
          <tr>
            <th scope="col">提供方</th>
            <th scope="col" className={tableCell.num}>提出问题</th>
            <th scope="col" className={tableCell.num}>已复核</th>
            <th scope="col" className={tableCell.num}>有效</th>
            <th scope="col" className={tableCell.num}>无效</th>
            <th scope="col" className={tableCell.num}>准确率</th>
            <th scope="col" className={tableCell.num}>被推翻</th>
            <th scope="col" className={tableCell.num}>推翻率</th>
          </tr>
        </thead>
        <tbody>
          {paged.items.map((row) => (
            <tr key={row.provider}>
              <td className={identCell}>{row.provider}</td>
              <td className={tableCell.num}>{formatNumber(row.proposed)}</td>
              <td className={tableCell.num}>{formatNumber(row.reviewed)}</td>
              <td className={tableCell.num}>{formatNumber(row.valid)}</td>
              <td className={tableCell.num}>{formatNumber(row.invalid)}</td>
              <td className={tableCell.num}><QualityRate numerator={row.valid} denominator={row.reviewed} /></td>
              <td className={tableCell.num}>{formatNumber(row.overturned)}</td>
              <td className={tableCell.num}><QualityRate numerator={row.overturned} denominator={row.proposed} /></td>
            </tr>
          ))}
        </tbody>
      </DataTable>
      <TablePager page={paged.page} pageCount={paged.pageCount} total={paged.total} onChange={paged.setPage} />
    </>
  )
}

function ChallengerQualityTable({ rows }: { rows: ProviderQualityRow[] }) {
  const paged = useClientPage(rows, TABLE_PAGE_SIZE)
  return (
    <>
      <DataTable
        label="按挑战方统计的人工质量指标"
        minWidth="md"
        isEmpty={rows.length === 0}
        empty={<p className="small muted">没有挑战方样本。</p>}
      >
        <thead>
          <tr>
            <th scope="col">提供方</th>
            <th scope="col" className={tableCell.num}>反驳数</th>
            <th scope="col" className={tableCell.num}>复核反驳</th>
            <th scope="col" className={tableCell.num}>误杀</th>
            <th scope="col" className={tableCell.num}>误杀率</th>
          </tr>
        </thead>
        <tbody>
          {paged.items.map((row) => (
            <tr key={row.provider}>
              <td className={identCell}>{row.provider}</td>
              <td className={tableCell.num}>{formatNumber(row.refuted)}</td>
              <td className={tableCell.num}>{formatNumber(row.refuted_reviewed)}</td>
              <td className={tableCell.num}>{formatNumber(row.false_refuted)}</td>
              <td className={tableCell.num}><QualityRate numerator={row.false_refuted} denominator={row.refuted_reviewed} /></td>
            </tr>
          ))}
        </tbody>
      </DataTable>
      <TablePager page={paged.page} pageCount={paged.pageCount} total={paged.total} onChange={paged.setPage} />
    </>
  )
}

function QualityPanel({ value }: { value: unknown }) {
  const state = parseProviderQuality(value)
  if (state.kind === 'missing') {
    return (
      <Panel title="人工质量指标" aside="来自 stats.quality_by_provider">
        <EmptyState title="人工质量指标不可用" description="stats 未返回 quality_by_provider；旧版服务端可能不支持人工标注指标。" />
      </Panel>
    )
  }
  if (state.kind === 'invalid') {
    return (
      <Panel title="人工质量指标" aside="来自 stats.quality_by_provider">
        <EmptyState
          title="人工质量指标数据格式无效"
          description="stats.quality_by_provider 含负数、非整数、重复 provider、越界比例或计数不一致；为避免显示错误比例，不展示任何行。"
        />
      </Panel>
    )
  }
  const proposed = state.rows.filter((row) => row.proposed > 0)
  const refuted = state.rows.filter((row) => row.refuted > 0)
  return (
    <Panel
      title="人工质量指标"
      aside="来自 stats.quality_by_provider"
      note="准确率 = 人工标注有效 / 已复核（有效+无效）；推翻率 = 提出方被挑战推翻 / 提出问题；误杀率 = 反驳后人工判有效 / 复核反驳。未标注、未确定不计入准确率与误杀率分母，但仍计入提出数、反驳数和推翻率。缺少 provider 归因的角色不计入。零分母显示“无样本”，不显示 0%。人工标注不改变发布结果或评审结论，也不同于下方 GitLab 反馈信号。"
    >
      {state.rows.length === 0 ? (
        <EmptyState title="没有人工质量样本" description="筛选范围内没有可归因的提出或反驳记录。" />
      ) : (
        <>
          <h3 className={insights.subTitle}>提出方</h3>
          <ProposerQualityTable rows={proposed} />
          <h3 className={insights.subTitle}>挑战方</h3>
          <ChallengerQualityTable rows={refuted} />
        </>
      )}
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
  if (!isNonNegativeInteger(value.runs) || !isNonNegativeInteger(value.duration_samples)) return null
  if (value.duration_samples > value.runs) return null
  const avg = nullableDuration(value.avg_duration_ms)
  const p95 = nullableDuration(value.p95_duration_ms)
  if (avg === undefined || p95 === undefined) return null
  if (value.duration_samples === 0 && (avg !== null || p95 !== null)) return null
  return {
    day: value.day,
    runs: value.runs,
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
  if (!isNonNegativeInteger(row.runs)) return null
  const hasAny = TREND_FIELDS.some((key) => Object.prototype.hasOwnProperty.call(row, key))
  if (allowLegacy && !hasAny) {
    return {
      runs: row.runs,
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

// Chinese header for a stats field. Fields without an entry keep their API name.
const COLUMN_LABELS: Record<string, string> = {
  key: '名称',
  value: '值',
  provider: '提供方',
  model: '模型',
  calls: '调用数',
  errors: '错误数',
  error_rate: '错误率',
  input_tokens: '输入 token',
  output_tokens: '输出 token',
  avg_latency_ms: '平均延迟',
  p50_latency_ms: 'P50 延迟',
  errors_by_kind: '错误类别',
}

function columnLabel(column: string): string {
  return COLUMN_LABELS[column] ?? column
}

// cellValue renders one generic cell. Numbers never show as raw floats: rate
// and ratio fields become percentages, *_ms fields latencies, the rest grouped
// numbers.
function cellValue(column: string, value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'number') {
    if (!Number.isFinite(value)) return String(value)
    if (/(^|_)(rate|ratio)$/.test(column)) return formatPercent(value)
    if (/_ms$/.test(column)) return formatLatency(value)
    return formatNumber(value)
  }
  if (typeof value === 'string') return value
  if (typeof value === 'boolean') return value ? 'true' : 'false'
  try { return JSON.stringify(value) } catch { return String(value) }
}

function DimensionTable({ rows, emptyLabel, label }: { rows: Row[]; emptyLabel: string; label: string }) {
  const columns = useMemo(() => {
    const set = new Set<string>()
    for (const row of rows.slice(0, 12)) {
      for (const key of Object.keys(row)) {
        if (!LEGACY_COST_KEYS.has(key)) set.add(key)
      }
    }
    return Array.from(set)
  }, [rows])
  // A column is numeric when every value it carries is a number, so its header
  // and cells align the same way.
  const numeric = useMemo(() => new Set(columns.filter((column) => {
    const values = rows.map((row) => row[column]).filter((value) => value !== null && value !== undefined)
    return values.length > 0 && values.every((value) => typeof value === 'number')
  })), [columns, rows])
  const paged = useClientPage(rows, TABLE_PAGE_SIZE)
  return (
    <>
      <DataTable
        label={label}
        minWidth="md"
        isEmpty={rows.length === 0}
        empty={<EmptyState title={emptyLabel} description="stats API 没有返回该维度的数据。" />}
      >
        <thead>
          <tr>
            {columns.map((column) => (
              <th key={column} scope="col" className={numeric.has(column) ? tableCell.num : undefined}>{columnLabel(column)}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {paged.items.map((row, index) => (
            <tr key={String(row.id ?? row.key ?? index)}>
              {columns.map((column) => (
                <td key={column} className={numeric.has(column) ? tableCell.num : undefined}>{cellValue(column, row[column])}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </DataTable>
      <TablePager page={paged.page} pageCount={paged.pageCount} total={paged.total} onChange={paged.setPage} />
    </>
  )
}

function ProviderAttribution({ providers }: { providers: ProviderFeedbackCounts[] | null }) {
  const paged = useClientPage(providers ?? [], TABLE_PAGE_SIZE)
  return (
    <>
      <h3 className={insights.subTitle}>按 provider 与角色归因</h3>
      <DataTable
        label="按 Provider 与角色归因的反馈计数"
        minWidth="sm"
        isEmpty={paged.total === 0}
        empty={providers === null ? (
          <EmptyState title="按 provider 归因不可用" description="stats.feedback 未返回 by_provider；旧版服务端可能不支持归因。原始信号总数仍显示在上方。" />
        ) : (
          <EmptyState title="没有可归因的反馈" description="当前筛选范围内没有同时关联 finding 与挑战节点的反馈；summary、未关联或关联不一致的不计入。" />
        )}
      >
        <thead>
          <tr>
            <th scope="col">提供方</th>
            <th scope="col">角色</th>
            <th scope="col" className={tableCell.num}>计数</th>
            <th scope="col">信号</th>
          </tr>
        </thead>
        <tbody>
          {paged.items.map((row) => (
            <tr key={`${row.provider}:${row.role}`}>
              <td className={identCell}>{row.provider}</td>
              <td><Badge tone={row.role === 'proposer' ? 'info' : 'neutral'}>{ROLE_LABELS[row.role]}</Badge></td>
              <td className={tableCell.num}>{formatNumber(row.total)}</td>
              <td>
                <span className={ui.inlineList}>
                  {row.signals.map((signal) => (
                    <span key={signal.label} className="small">{signal.label}: {formatNumber(signal.count)}</span>
                  ))}
                </span>
              </td>
            </tr>
          ))}
        </tbody>
      </DataTable>
      <TablePager page={paged.page} pageCount={paged.pageCount} total={paged.total} onChange={paged.setPage} />
      <TableNote>
        计数是反馈归因，不是正确率或误杀率。同一信号可同时计入提出方和挑战方，两个角色的计数不能相加为反馈总数。summary、未关联或关联不一致的不计入。
      </TableNote>
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
            ? 'stats 未返回 feedback；旧版服务端可能不支持该字段。'
            : 'stats.feedback 格式无效；为避免显示错误总数，不展示任何反馈计数。'}
        />
      </Panel>
    )
  }
  return (
    <Panel
      title="反馈信号"
      aside="来自 stats.feedback"
      note="按 GitLab 信号展示原始计数，不是模型准确率，也不是人工标注的误杀率。人工评估见上方“人工质量指标”。"
    >
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
        <EmptyState title="没有反馈信号" description="筛选范围内的反馈总数为 0。" />
      ) : (
        <span className="small muted">stats.feedback 没有返回按信号的明细。</span>
      )}
      <ProviderAttribution providers={feedback.providers} />
    </Panel>
  )
}

function DailyTrendTable({ rows, label, countLabel }: { rows: DailyStats[]; label: string; countLabel: string }) {
  const paged = useClientPage(rows, TABLE_PAGE_SIZE)
  return (
    <>
      <DataTable label={label} minWidth="md">
        <caption className={`small ${insights.trendCaption}`}>
          按工作流创建时间归属 UTC 日；只显示有执行的日期，不补零。
        </caption>
        <thead>
          <tr>
            <th scope="col">日期 (UTC)</th>
            <th scope="col" className={tableCell.num}>{countLabel}</th>
            <th scope="col" className={tableCell.num}>平均用时</th>
            <th scope="col" className={tableCell.num}>P95 用时</th>
            <th scope="col" className={tableCell.num}>耗时样本</th>
          </tr>
        </thead>
        <tbody>
          {paged.items.map((row) => (
            <tr key={row.day}>
              <td className={identCell}>
                <time dateTime={`${row.day}T00:00:00Z`}>{row.day}</time>
              </td>
              <td className={tableCell.num}>{formatNumber(row.runs)}</td>
              <td className={tableCell.num}>{formatMs(row.avg_duration_ms)}</td>
              <td className={tableCell.num}>{formatMs(row.p95_duration_ms)}</td>
              <td className={tableCell.num}>{formatNumber(row.duration_samples)}</td>
            </tr>
          ))}
        </tbody>
      </DataTable>
      <TablePager page={paged.page} pageCount={paged.pageCount} total={paged.total} onChange={paged.setPage} />
    </>
  )
}

const TREND_REASONS = {
  missing: '该行没有每日趋势数据；旧版服务端可能不支持。',
  invalid: '该行每日趋势格式无效，未显示任何行。',
} as const

function DimensionTrend({ trend, label, countLabel }: { trend: Trend; label: string; countLabel: string }) {
  if (trend.kind === 'missing') return <p className="small muted">{TREND_REASONS.missing}</p>
  if (trend.kind === 'invalid') return <p className="small muted">{TREND_REASONS.invalid}</p>
  if (trend.kind === 'empty') return <p className="small muted">该范围内没有每日执行数据。</p>
  return <DailyTrendTable rows={trend.rows} label={label} countLabel={countLabel} />
}

const CHART_W = 900
const CHART_H = 200
const CHART_GRID = [0.25, 0.5, 0.75, 1]

// DailyRunsChart draws one bar per returned UTC day. Days without runs are
// absent from stats.trend and stay absent here: the chart never fills gaps.
export function DailyRunsChart({ rows }: { rows: DailyStats[] }) {
  const peak = Math.max(0, ...rows.map((row) => row.runs))
  const max = Math.max(1, peak)
  const slot = CHART_W / rows.length
  const width = Math.min(Math.max(slot - 14, 2), 56)
  const labelEvery = Math.max(1, Math.ceil(rows.length / 8))
  return (
    <div className={insights.chartWrap}>
      <svg
        className={insights.chart}
        viewBox={`0 0 ${CHART_W} ${CHART_H}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={`每日运行次数柱状图，共 ${rows.length} 个有执行的 UTC 日，数据见下方表格`}
      >
        {CHART_GRID.map((ratio) => (
          <line
            key={ratio}
            className={insights.chartGrid}
            x1="0"
            x2={CHART_W}
            y1={CHART_H - ratio * CHART_H}
            y2={CHART_H - ratio * CHART_H}
            vectorEffect="non-scaling-stroke"
          />
        ))}
        {rows.map((row, index) => {
          const height = (row.runs / max) * CHART_H
          return (
            <rect key={row.day} x={index * slot + (slot - width) / 2} y={CHART_H - height} width={width} height={height}>
              <title>{`${row.day} · ${formatNumber(row.runs)} 次`}</title>
            </rect>
          )
        })}
      </svg>
      <div className={insights.chartAxis} aria-hidden="true">
        {rows.map((row, index) => <span key={row.day}>{index % labelEvery === 0 ? row.day.slice(5) : ''}</span>)}
      </div>
      <div className={insights.legend}>
        <span><i />工作流次数 · UTC 日桶，无记录的日不补零 · 峰值 {formatNumber(peak)}</span>
      </div>
    </div>
  )
}

function OverallTrend({ trend }: { trend: Trend }) {
  if (trend.kind === 'missing') {
    return <EmptyState title="每日趋势不可用" description="stats 未返回 trend；旧版服务端可能不支持每日趋势。" />
  }
  if (trend.kind === 'invalid') {
    return <EmptyState title="每日趋势数据格式无效" description="stats.trend 含负数、非数字或无效日期；为避免显示错误合计，不展示任何趋势行。" />
  }
  if (trend.kind === 'empty') {
    return <EmptyState title="没有每日趋势" description="trend 为空表示筛选范围内没有执行。缺失日期不补零。" />
  }
  return (
    <>
      <DailyRunsChart rows={trend.rows} />
      <DailyTrendTable rows={trend.rows} label="每日运行趋势" countLabel="工作流次数" />
    </>
  )
}

function ProjectStatsTable({ rows }: { rows: ProjectRow[] }) {
  const [expanded, setExpanded] = useState<number | null>(null)
  const detailBase = useId()
  const paged = useClientPage(rows, TABLE_PAGE_SIZE)
  return (
    <>
      <DataTable label="按项目统计明细" minWidth="xl">
        <thead>
          <tr>
            <th scope="col">项目</th>
            <th scope="col" className={tableCell.num}>工作流次数</th>
            <th scope="col" className={tableCell.num}>BLOCKING</th>
            <th scope="col" className={tableCell.num}>已发布</th>
            <th scope="col" className={tableCell.num}>平均用时</th>
            <th scope="col" className={tableCell.num}>P95 用时</th>
            <th scope="col" className={tableCell.num}>耗时样本</th>
            <th scope="col" className={tableCell.actions}>操作</th>
          </tr>
        </thead>
        <tbody>
          {paged.items.map((row) => {
            const label = `项目 #${row.project_id}`
            const open = expanded === row.project_id
            const detailId = `${detailBase}-${row.project_id}`
            return (
              <Fragment key={row.project_id}>
                <tr>
                  <td className={identCell}>{label}</td>
                  <td className={tableCell.num}>{formatNumber(row.runs)}</td>
                  <td className={tableCell.num}>{formatNumber(row.blocking)}</td>
                  <td className={tableCell.num}>{formatNumber(row.published)}</td>
                  <td className={tableCell.num}>{formatMs(row.avg_duration_ms)}</td>
                  <td className={tableCell.num}>{formatMs(row.p95_duration_ms)}</td>
                  <td className={tableCell.num}>{row.duration_samples === null ? '不可用' : formatNumber(row.duration_samples)}</td>
                  <td className={tableCell.actions}>
                    {row.trend.kind === 'missing' || row.trend.kind === 'invalid' ? (
                      <ReasonBadge
                        tone={row.trend.kind === 'missing' ? 'neutral' : 'warn'}
                        reason={TREND_REASONS[row.trend.kind]}
                      >
                        {row.trend.kind === 'missing' ? '不可用' : '格式无效'}
                      </ReasonBadge>
                    ) : (
                      <TableActions>
                        <TableAction
                          aria-expanded={open}
                          aria-controls={open ? detailId : undefined}
                          aria-label={`${open ? '收起' : '查看'} ${label} 每日趋势`}
                          onClick={() => setExpanded(open ? null : row.project_id)}
                        >
                          {open ? '收起' : '每日趋势'}
                        </TableAction>
                      </TableActions>
                    )}
                  </td>
                </tr>
                {open ? (
                  <tr>
                    <td colSpan={8} className={insights.trendCell}>
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
      </DataTable>
      <TablePager page={paged.page} pageCount={paged.pageCount} total={paged.total} onChange={paged.setPage} />
    </>
  )
}

function ProjectStatsPanel({ value }: { value: unknown }) {
  const state = parseProjectStats(value)

  if (state.kind === 'missing') {
    return (
      <Panel title="项目统计" aside="来自 stats.by_project">
        <EmptyState title="项目统计不可用" description="stats 未返回 by_project；旧版服务端可能不支持按项目统计。" />
      </Panel>
    )
  }
  if (state.kind === 'invalid') {
    return (
      <Panel title="项目统计" aside="来自 stats.by_project">
        <EmptyState title="项目统计数据格式无效" description="stats.by_project 含负数、非数字或无效字段；为避免显示错误合计，不展示任何项目行。" />
      </Panel>
    )
  }
  if (!state.rows.length) {
    return (
      <Panel title="项目统计" aside="来自 stats.by_project">
        <EmptyState title="没有项目统计" description="筛选范围内没有项目工作流。" />
      </Panel>
    )
  }

  return (
    <Panel
      title="项目统计"
      aside="来自 stats.by_project"
      note="运行次数统计窗口内全部工作流（含评审与 onboarding）；BLOCKING 与已发布也按项目统计。日期按工作流创建时间归入 UTC 日；筛选按浏览器本地日。"
    >
      <ProjectStatsTable rows={state.rows} />
    </Panel>
  )
}

function GroupStatsTable({ rows, dimension }: { rows: GroupRow[]; dimension: 'provider' | 'block' }) {
  const [expanded, setExpanded] = useState<string | null>(null)
  const detailBase = useId()
  const paged = useClientPage(rows, TABLE_PAGE_SIZE)
  const keyLabel = dimension === 'provider' ? 'Provider' : 'Block'
  return (
    <>
      <DataTable label={`按 ${keyLabel} 统计明细`} minWidth="lg">
        <thead>
          <tr>
            <th scope="col">{dimension === 'provider' ? '提供方' : 'Block'}</th>
            <th scope="col" className={tableCell.num}>已启动节点数</th>
            <th scope="col" className={tableCell.num}>平均用时</th>
            <th scope="col" className={tableCell.num}>P95 用时</th>
            <th scope="col" className={tableCell.num}>耗时样本</th>
            <th scope="col" className={tableCell.actions}>操作</th>
          </tr>
        </thead>
        <tbody>
          {paged.items.map((row, index) => {
            const open = expanded === row.key
            const detailId = `${detailBase}-${index}`
            return (
              <Fragment key={row.key}>
                <tr>
                  <td className={identCell}>{row.key}</td>
                  <td className={tableCell.num}>{formatNumber(row.runs)}</td>
                  <td className={tableCell.num}>{formatMs(row.avg_duration_ms)}</td>
                  <td className={tableCell.num}>{formatMs(row.p95_duration_ms)}</td>
                  <td className={tableCell.num}>{formatNumber(row.duration_samples)}</td>
                  <td className={tableCell.actions}>
                    <TableActions>
                      <TableAction
                        aria-expanded={open}
                        aria-controls={open ? detailId : undefined}
                        aria-label={`${open ? '收起' : '查看'} ${row.key} 每日趋势`}
                        onClick={() => setExpanded(open ? null : row.key)}
                      >
                        {open ? '收起' : '每日趋势'}
                      </TableAction>
                    </TableActions>
                  </td>
                </tr>
                {open ? (
                  <tr>
                    <td colSpan={6} className={insights.trendCell}>
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
      </DataTable>
      <TablePager page={paged.page} pageCount={paged.pageCount} total={paged.total} onChange={paged.setPage} />
    </>
  )
}

function GroupStatsPanel({ value, dimension }: { value: unknown; dimension: 'provider' | 'block' }) {
  const state = parseGroupStats(value, dimension)
  const title = dimension === 'provider' ? 'Provider 统计' : 'Block 统计'

  if (state.kind === 'missing') {
    return (
      <Panel title={title} aside={`来自 stats.by_${dimension}`}>
        <EmptyState title={`${title}不可用`} description={`stats 未返回 by_${dimension}；旧版服务端可能不支持节点维度统计。`} />
      </Panel>
    )
  }
  if (state.kind === 'invalid') {
    return (
      <Panel title={title} aside={`来自 stats.by_${dimension}`}>
        <EmptyState title={`${title}数据格式无效`} description={`stats.by_${dimension} 含负数、非数字或无效字段；为避免显示错误合计，不展示任何行。`} />
      </Panel>
    )
  }
  if (!state.rows.length) {
    return (
      <Panel title={title} aside={`来自 stats.by_${dimension}`}>
        <EmptyState title={`没有 ${dimension} 统计`} description="筛选范围内没有该维度的已启动节点。" />
      </Panel>
    )
  }

  return (
    <Panel
      title={title}
      aside={`来自 stats.by_${dimension}`}
      note={dimension === 'provider'
        ? '已启动节点数：有 started_at 的节点计入，每个当前保存的节点行只计一次，不按状态筛选；不等于 LLM 调用次数或重试次数。无 provider 的节点只计入 block。显示的是当前保存的最新一次尝试，不是重试历史。'
        : '与 Provider 统计同一已启动节点口径；Block 始终存在，无 provider 的节点也会显示在这里。'}
    >
      <GroupStatsTable rows={state.rows} dimension={dimension} />
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
      <div className={ui.metricStrip}>
        <Metric label="运行次数" value={runs === null ? '不可用' : runs} note={blocking === null ? '含评审与 onboarding · BLOCKING 次数不可用' : `含评审与 onboarding · BLOCKING ${blocking} 次`} />
        <Metric label="BLOCKING 比例" value={formatRatio(statNumber(stats, 'blocking_ratio'))} note="来自 stats.blocking_ratio" />
        <Metric label="已发布问题" value={published === null ? '不可用' : published} note="来自 stats.published" />
        <Metric label="规则丢弃比例" value={formatRatio(statNumber(stats, 'overturned_ratio'))} note="规则 drop 占比，不是模型准确率" />
        <Metric
          label="平均用时"
          value={formatMs(avgDuration)}
          note={`${durationSamples === null ? '样本数不可用' : `样本 ${formatNumber(durationSamples)}`} · P95 ${formatMs(p95Duration)}`}
        />
      </div>

      <Panel
        title="每日运行趋势"
        aside="来自 stats.trend · UTC 日"
        note="趋势按工作流创建时间归入 UTC 日，只返回有执行的日期，不补零。用时为空表示没有有效的起止时间配对；样本数是参与计算的配对数。"
      >
        <OverallTrend trend={trend} />
      </Panel>

      <Panel
        title="问题处置分布"
        aside="来自 stats.decisions"
        note="人工标注在评审详情的问题展开行维护；这里只显示规则处置分布，人工评估汇总见下方“人工质量指标”。"
      >
        {decisions.length ? (
          <div className={styles.budgetRow}>
            {decisions.map((row) => <Badge key={row.label} tone={row.label === 'publish' ? 'ok' : row.label === 'mark' ? 'warn' : 'neutral'}>{row.label}: {row.count}</Badge>)}
          </div>
        ) : <EmptyState title="没有处置分布数据" description="stats.decisions 缺失或为空；显示不可用，不显示零值。" />}
      </Panel>

      <QualityPanel value={stats.quality_by_provider} />

      <FeedbackPanel value={stats.feedback} />
      <CollectionHealthPanel value={stats.feedback_collection} />
      <WorkspaceCapacityPanel value={stats.workspace_capacity} />

      <ProjectStatsPanel value={stats.by_project} />
      <GroupStatsPanel value={stats.by_provider} dimension="provider" />
      <GroupStatsPanel value={stats.by_block} dimension="block" />

      <ModelStatsPanel rows={stats.by_model} fallback={byModel} />

      <Disclosure summary="查看原始 stats 响应">
        <pre className={`pre statsJson ${ui.jsonBlock}`}>{JSON.stringify(stripLegacyCostKeys(stats), null, 2)}</pre>
      </Disclosure>
    </>
  )
}

// CollectionHealthPanel exposes the feedback collector state. A missing field
// means an older server, not a healthy collector; a stored error class stays
// visible until the next successful pass clears it.
function CollectionHealthPanel({ value }: { value?: FeedbackCollectionState }) {
  if (!value) {
    return (
      <Panel title="反馈采集健康" aside="来自 stats.feedback_collection">
        <EmptyState title="反馈采集健康不可用" description="stats 未返回 feedback_collection；旧版服务端可能不支持该字段。" />
      </Panel>
    )
  }
  const failing = Boolean(value.last_error_class)
  return (
    <Panel
      title="反馈采集健康"
      aside="来自 stats.feedback_collection"
      note="最后成功采集时间只在采集成功后推进；失败时保留上一次成功时间并记录固定错误类别。"
    >
      <div className={styles.statGrid}>
        <Metric
          label="最后成功采集"
          value={value.last_collected_at ? formatRelative(value.last_collected_at) : '从未成功'}
          note={value.last_collected_at ? value.last_collected_at : '尚无成功的采集记录'}
        />
        <Metric
          label="最近错误类别"
          value={failing ? value.last_error_class || '不可用' : '无错误'}
          note={value.last_error_at ? `最近错误时间 ${formatRelative(value.last_error_at)}` : '没有记录错误时间'}
        />
      </div>
      {failing ? (
        <p className="small" role="alert">
          反馈采集最近失败（{value.last_error_class}）；采集成功后清除。
        </p>
      ) : (
        <p className="small" role="status">当前没有记录的采集错误。</p>
      )}
    </Panel>
  )
}

// WorkspaceCapacityPanel discloses the configured workspace cap and the last
// measurement. Over-limit is an operational warning, never a silent state.
function WorkspaceCapacityPanel({ value }: { value?: WorkspaceCapacity }) {
  if (!value) {
    return (
      <Panel title="工作区容量" aside="来自 stats.workspace_capacity">
        <EmptyState title="工作区容量不可用" description="stats 未返回 workspace_capacity；旧版服务端可能不支持该字段。" />
      </Panel>
    )
  }
  const limitConfigured = value.limit_bytes > 0
  const ratio = limitConfigured ? value.total_bytes / value.limit_bytes : null
  return (
    <Panel
      title="工作区容量"
      aside="来自 stats.workspace_capacity"
      note="容量取工作区根目录最近一次测量；未配置上限时 limit_bytes 为 0，并非容量无限。"
    >
      <div className={styles.statGrid}>
        <Metric
          label="最近测量"
          value={value.measured_at ? formatRelative(value.measured_at) : '尚未测量'}
          note={value.measured_at || '没有测量时间'}
        />
        <Metric label="已用" value={formatBytes(value.total_bytes)} note={`${formatNumber(value.total_bytes)} 字节`} />
        <Metric
          label="上限"
          value={limitConfigured ? formatBytes(value.limit_bytes) : '未配置'}
          note={limitConfigured ? (ratio === null ? '' : `使用率 ${formatRatio(ratio)}`) : '容量回收默认禁用'}
        />
      </div>
      {value.over_limit ? (
        <p className="small" role="alert">
          工作区容量已超过配置上限；回收默认禁用，需要人工清理或调整 workspace.max_bytes。
        </p>
      ) : null}
    </Panel>
  )
}

// ModelStatsTable renders only the fields the API returns, under Chinese
// headers. errors_by_kind is the only structured cell: it holds fixed error
// categories (never an upstream error body) as badges that wrap instead of
// truncating.
function ModelStatsTable({ rows }: { rows: ModelStatsRow[] }) {
  const paged = useClientPage(rows, TABLE_PAGE_SIZE)
  return (
    <>
      <DataTable label="按模型统计明细" minWidth="lg">
        <thead>
          <tr>
            <th scope="col">模型 / 提供方</th>
            <th scope="col" className={tableCell.num}>{COLUMN_LABELS.calls}</th>
            <th scope="col" className={tableCell.num}>错误 / 错误率</th>
            <th scope="col" className={tableCell.num}>输入 / 输出 token</th>
            <th scope="col" className={tableCell.num}>平均 / P50 延迟</th>
            <th scope="col">{COLUMN_LABELS.errors_by_kind}</th>
          </tr>
        </thead>
        <tbody>
          {paged.items.map((row) => {
            const kinds = Object.entries(row.errors_by_kind ?? {}).sort(([a], [b]) => a.localeCompare(b))
            return (
              <tr key={`${row.provider}/${row.model}`}>
                <td>
                  <span className={insights.stacked}>
                    <span className={insights.stackedMono}>{row.model}</span>
                    <span className={insights.stackedSub}>{row.provider}</span>
                  </span>
                </td>
                <td className={tableCell.num}>{formatNumber(row.calls)}</td>
                <td className={tableCell.num}>
                  <span className={insights.stackedEnd}>
                    <span>{formatNumber(row.errors)}</span>
                    <span className={insights.stackedSub}>{formatPercent(row.error_rate)}</span>
                  </span>
                </td>
                <td className={tableCell.num}>
                  <span className={insights.stackedEnd}>
                    <span>{formatNumber(row.input_tokens)}</span>
                    <span className={insights.stackedSub}>{formatNumber(row.output_tokens)}</span>
                  </span>
                </td>
                <td className={tableCell.num}>
                  <span className={insights.stackedEnd}>
                    <span>{formatLatency(row.avg_latency_ms)}</span>
                    <span className={insights.stackedSub}>{formatLatency(row.p50_latency_ms)}</span>
                  </span>
                </td>
                <td>
                  {kinds.length ? (
                    <span className={insights.kindList}>
                      {kinds.map(([kind, count]) => <Badge key={kind} tone="warn">{kind}: {formatNumber(count)}</Badge>)}
                    </span>
                  ) : (
                    <span className={tableCell.muted}>无错误类别</span>
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </DataTable>
      <TablePager page={paged.page} pageCount={paged.pageCount} total={paged.total} onChange={paged.setPage} />
    </>
  )
}

function ModelStatsPanel({ rows, fallback }: { rows?: ModelStatsRow[]; fallback: Row[] }) {
  if (!rows?.length) {
    return (
      <Panel title="按模型" aside="来自 stats.by_model">
        <DimensionTable rows={fallback} emptyLabel="没有按模型统计" label="按模型统计明细" />
      </Panel>
    )
  }
  return (
    <Panel
      title="按模型"
      aside="来自 stats.by_model"
      note="调用与错误来自调用明细或长期聚合；错误类别（errors_by_kind）只记录固定类别，不包含上游响应正文。"
    >
      <ModelStatsTable rows={rows} />
    </Panel>
  )
}

function readCount(stats: ReviewStats, key: keyof ReviewStats): number | null {
  const value = stats[key]
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
}

// ReviewScopePanel keeps the review counters separate from the mixed workflow
// totals. Onboarding runs share the workflows table but never carry a review
// verdict, so they cannot be part of a blocking ratio a reviewer interprets.
function ReviewScopePanel({ stats }: { stats: ReviewStats }) {
  const runs = readCount(stats, 'runs')
  const incomplete = readCount(stats, 'incomplete')
  const blocking = readCount(stats, 'blocking')
  const allRuns = readCount(stats, 'all_runs')
  const allBlocking = readCount(stats, 'all_blocking')
  return (
    <Panel title="评审口径" aside="来自 stats/reviews · kind=review">
      <section className={ui.metricStrip} aria-label="评审口径指标">
        <Metric
          label="评审运行"
          value={runs === null ? '不可用' : formatNumber(runs)}
          note={`仅 kind=review；onboarding 不计入（全量 ${allRuns === null ? '不可用' : formatNumber(allRuns)} 次）`}
        />
        <Metric
          label="INCOMPLETE"
          value={incomplete === null ? '不可用' : formatNumber(incomplete)}
          note={`占评审运行 ${formatRatio(stats.incomplete_ratio ?? null)}`}
        />
        <Metric
          label="BLOCKING 比例"
          value={formatRatio(stats.blocking_ratio ?? null)}
          note={`评审分母 ${blocking === null ? '不可用' : formatNumber(blocking)}/${runs === null ? '不可用' : formatNumber(runs)}；全量含 onboarding ${formatRatio(stats.all_blocking_ratio ?? null)}（${allBlocking === null ? '不可用' : formatNumber(allBlocking)}/${allRuns === null ? '不可用' : formatNumber(allRuns)}）`}
        />
      </section>
      {stats.model_details_purged ? (
        <p className="small" role="status">
          所选区间早于 LLM 调用明细保留期{typeof stats.llm_call_retention_hours === 'number' ? `（${stats.llm_call_retention_hours} 小时）` : ''}：
          按模型与调用的明细已删除，下方模型统计显示 0 不代表没有调用；工作流用量汇总仍保留。
        </p>
      ) : null}
    </Panel>
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
  const [fromValid, setFromValid] = useState(true)
  const [toValid, setToValid] = useState(true)

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
  const reviewStats = useQuery({
    queryKey: ['stats', 'reviews', range?.from ?? '', range?.to ?? '', applied.projectId],
    queryFn: () => {
      if (!range) throw new Error('日期范围无效。')
      return getReviewStats({ from: range.from, to: range.to, project_id: applied.projectId || undefined })
    },
    enabled: range !== null,
  })

  const apply = () => {
    if (!fromValid || !toValid) { setValidation('请修正日期格式。'); return }
    if (!fromDate || !toDate) { setValidation('请选择开始和结束日期。'); return }
    if (fromDate > toDate) { setValidation('开始日期不能晚于结束日期。'); return }
    if (!dateRange({ fromDate, toDate })) { setValidation('日期范围无效，请重新选择。'); return }
    setValidation('')
    setApplied({ fromDate, toDate, projectId })
  }

  const projectChoices: SelectOption[] = [
    { value: '', label: '全部项目' },
    ...projects.items.map((project) => ({ value: String(project.id), label: project.path_with_namespace })),
  ]

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="统计与反馈"
          description="按时间与项目查看运行统计、每日趋势和节点维度；不可用的值不显示为零。"
          actions={(
            <div className={insights.headerFilters} role="group" aria-label="筛选">
              <div className={insights.filterField}>
                <span className={insights.filterLabel}>开始日期</span>
                <div className={insights.dateFilter}>
                  <DatePicker
                    value={fromDate}
                    onChange={setFromDate}
                    onValidityChange={setFromValid}
                    aria-label="开始日期"
                  />
                </div>
              </div>
              <div className={insights.filterField}>
                <span className={insights.filterLabel}>结束日期</span>
                <div className={insights.dateFilter}>
                  <DatePicker
                    value={toDate}
                    onChange={setToDate}
                    onValidityChange={setToValid}
                    aria-label="结束日期"
                  />
                </div>
              </div>
              <div className={insights.filterField}>
                <span className={insights.filterLabel}>项目</span>
                <div className={insights.filterSelect}>
                  <Select
                    value={projectId}
                    options={projectChoices}
                    onChange={setProjectId}
                    aria-label="项目"
                    emptyLabel="全部项目"
                  />
                </div>
              </div>
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
          )}
        />
        {validation || projects.isError ? (
          <div className={insights.filterStatus}>
            {validation ? <span className={styles.resultError} role="alert">{validation}</span> : null}
            {projects.isError ? (
              <>
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
              </>
            ) : null}
          </div>
        ) : null}

        {range && reviewStats.isPending ? <LoadingState label="正在读取评审口径统计…" /> : null}
        {reviewStats.isError ? <ErrorState error={reviewStats.error} onRetry={() => { void reviewStats.refetch() }} title="评审口径统计加载失败" /> : null}
        {reviewStats.data ? <ReviewScopePanel stats={reviewStats.data} /> : null}
        {range && stats.isPending ? <LoadingState label="正在读取统计数据…" /> : null}
        {stats.isError ? <ErrorState error={stats.error} onRetry={() => { void stats.refetch() }} title="统计数据加载失败" /> : null}
        {stats.isSuccess && !stats.data ? <EmptyState title="stats 返回了空响应" description="页面不会用示例数据补全；请检查服务端统计实现。" /> : null}
        {stats.data ? <StatsContent stats={stats.data} /> : null}
      </div>
    </main>
  )
}
