import { useEffect, useMemo, useRef } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { getConfig, getConfigStatus } from '../api/api'
import type { ConfigOrigin } from '../api/types'
import { Badge, EmptyState, ErrorState, LoadingState, Note, Panel, PanelBody } from '../components/ui'
import ui from '../styles/ui.module.css'

const STATUS_POLL_MS = 5000

const ORIGIN_LABELS: Record<ConfigOrigin, string> = {
  default: '内置默认',
  base: '基础配置',
  overlay: 'overlay',
}

const ORIGIN_TONES: Record<ConfigOrigin, 'neutral' | 'info' | 'ok'> = {
  default: 'neutral',
  base: 'info',
  overlay: 'ok',
}

function shortRevision(revision: string): string {
  return revision ? revision.slice(0, 12) : '—'
}

// valueAt resolves a dotted origin path against the effective configuration
// object. Provider origins use the provider name (providers.<name>.<field>)
// while the API returns providers as a name-carrying array. Array segments are
// matched against the longest element name first, which also covers provider
// names that contain dots; numeric segments remain array indexes.
function valueAt(root: unknown, path: string): unknown {
  const segments = path.replace(/\[(\d+)\]/g, '.$1').split('.').filter(Boolean)
  let current: unknown = root
  for (let index = 0; index < segments.length;) {
    if (current === null || typeof current !== 'object') return undefined
    if (!Array.isArray(current)) {
      current = (current as Record<string, unknown>)[segments[index]]
      index += 1
      continue
    }
    const items = current
    let matched = false
    for (let end = segments.length; end > index; end -= 1) {
      const name = segments.slice(index, end).join('.')
      const item = items.find((candidate) => (
        candidate !== null && typeof candidate === 'object' && (candidate as { name?: unknown }).name === name
      ))
      if (item !== undefined) {
        current = item
        index = end
        matched = true
        break
      }
    }
    if (matched) continue
    if (!/^\d+$/.test(segments[index])) return undefined
    current = items[Number(segments[index])]
    index += 1
  }
  return current
}

function formatValue(value: unknown): string {
  if (value === null) return 'null'
  if (typeof value === 'string') return value === '' ? '（空）' : value
  return String(value)
}

// useConfigSources polls the file-effective snapshot and refreshes the
// configuration-dependent screens when the last-good revision changes.
function useConfigSources(enabled: boolean) {
  const queryClient = useQueryClient()
  const status = useQuery({
    queryKey: ['config-status'],
    queryFn: getConfigStatus,
    enabled,
    refetchInterval: STATUS_POLL_MS,
    retry: false,
  })
  const snapshot = useQuery({
    queryKey: ['config'],
    queryFn: getConfig,
    enabled,
    refetchInterval: STATUS_POLL_MS,
    retry: false,
  })

  const revision = status.data ? `${status.data.base_revision}:${status.data.overlay_revision}` : null
  const previousRevision = useRef<string | null>(null)
  useEffect(() => {
    if (!revision) return
    if (previousRevision.current !== null && previousRevision.current !== revision) {
      for (const queryKey of [['settings'], ['providers'], ['profiles'], ['models'], ['config']]) {
        void queryClient.invalidateQueries({ queryKey })
      }
    }
    previousRevision.current = revision
  }, [queryClient, revision])

  return { status, snapshot }
}

export function ConfigSourcesPanel({ enabled }: { enabled: boolean }) {
  const { status, snapshot } = useConfigSources(enabled)
  const rows = useMemo(() => {
    if (!snapshot.data) return []
    return Object.entries(snapshot.data.origins ?? {})
      .map(([path, origin]) => ({ path, origin, value: valueAt(snapshot.data.config, path) }))
      .filter((row) => row.value === null || ['string', 'number', 'boolean'].includes(typeof row.value))
      .sort((left, right) => left.path.localeCompare(right.path))
  }, [snapshot.data])

  return (
    <Panel
      title="配置来源与待重启"
      aside={status.data ? (
        <Badge tone={status.data.restart_required ? 'warn' : 'ok'}>
          {status.data.restart_required ? '待重启' : '已应用'}
        </Badge>
      ) : undefined}
      note="文件生效值按“内置默认 < --config < overlay”合并，不含数据库覆盖。运行中的服务使用启动时快照；显示“待重启”时，界面显示的是重载后的文件值，重启后生效。"
    >
      <PanelBody>
        {status.isPending ? <LoadingState label="正在读取配置状态…" /> : null}
        {status.isError ? <ErrorState error={status.error} onRetry={() => { void status.refetch() }} /> : null}
        {status.isSuccess ? (
          <div className={ui.stack}>
            <div className={ui.row}>
              <Badge tone={status.data.restart_required ? 'warn' : 'ok'}>
                {status.data.restart_required ? '待重启' : '已应用'}
              </Badge>
              <span className="small muted">
                base {shortRevision(status.data.base_revision)} · overlay{' '}
                {status.data.overlay_present ? shortRevision(status.data.overlay_revision) : '未挂载'}
              </span>
            </div>
            {status.data.last_error ? (
              <Note tone="danger" role="alert">最近一次重载失败，继续使用上一份可用快照：{status.data.last_error}</Note>
            ) : null}
            {status.data.warnings?.length ? (
              <Note tone="warn" role="status">
                <strong>配置告警</strong>
                <ul>
                  {status.data.warnings.map((warning) => <li key={warning}>{warning}</li>)}
                </ul>
              </Note>
            ) : null}
          </div>
        ) : null}

        {snapshot.isPending ? <LoadingState label="正在读取生效配置…" /> : null}
        {snapshot.isError ? <ErrorState error={snapshot.error} onRetry={() => { void snapshot.refetch() }} /> : null}
        {snapshot.isSuccess && rows.length === 0 ? (
          <EmptyState title="没有可显示的配置项" description="服务端返回的生效配置为空。" />
        ) : null}
        {snapshot.isSuccess && rows.length ? (
          <div className="tableWrap">
            <table className="dataTable">
              <thead>
                <tr>
                  <th scope="col">配置项</th>
                  <th scope="col">文件生效值</th>
                  <th scope="col">来源</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => (
                  <tr key={row.path}>
                    <td className="mono small">{row.path}</td>
                    <td className="mono small">{formatValue(row.value)}</td>
                    <td><Badge tone={ORIGIN_TONES[row.origin]} plain>{ORIGIN_LABELS[row.origin] ?? row.origin}</Badge></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </PanelBody>
    </Panel>
  )
}
