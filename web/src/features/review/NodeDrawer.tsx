import { useQuery } from '@tanstack/react-query'
import { getNodeCalls, getReviewNode } from '../../api/api'
import type { GraphNode, LlmCall } from '../../api/types'
import {
  Badge, Button, CopyButton, Drawer, EmptyState, ErrorState, LoadingState, StatusBadge,
} from '../../components/ui'
import { usePaginated } from '../../lib/hooks'
import { formatCost, formatDuration, formatJson, formatNumber } from '../../lib/format'
import ui from '../../styles/ui.module.css'
import styles from './review.module.css'

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function ParamsSection({ value }: { value: unknown }) {
  const entries = isPlainObject(value) ? Object.entries(value) : null
  return (
    <section className={styles.drawerSection}>
      <h3 className={styles.sectionLabel}>参数</h3>
      {entries && entries.length ? (
        <dl className={`${ui.definitionGrid} ${styles.usageGrid}`}>
          {entries.map(([key, item]) => (
            <div key={key} style={{ display: 'contents' }}>
              <dt className="mono">{key}</dt>
              <dd>
                {typeof item === 'string' || typeof item === 'number' || typeof item === 'boolean'
                  ? <span className="mono">{String(item)}</span>
                  : <pre className={styles.paramValue}>{formatJson(item)}</pre>}
              </dd>
            </div>
          ))}
        </dl>
      ) : formatJson(value)
        ? <pre className={ui.codeBlock}>{formatJson(value)}</pre>
        : <span className="muted small">没有记录。</span>}
    </section>
  )
}

function JsonSection({ title, value, copy }: { title: string; value: unknown; copy?: boolean }) {
  const content = formatJson(value)
  return (
    <section className={styles.drawerSection}>
      <div className={styles.drawerSectionHead}>
        <h3 className={styles.sectionLabel}>{title}</h3>
        {copy && content ? <CopyButton value={content} label="复制 JSON" /> : null}
      </div>
      {content ? <pre className={`${ui.codeBlock} ${styles.nodeOutput}`}>{content}</pre> : <span className="muted small">没有记录。</span>}
    </section>
  )
}

function CallItem({ call }: { call: LlmCall }) {
  const request = formatJson(call.request)
  const response = formatJson(call.response)
  return (
    <details className={styles.call}>
      <summary className={styles.callSummary}>
        <b className="mono">#{call.turn ?? '?'}</b>
        <span className="mono muted">{formatNumber(call.input_tokens)} → {formatNumber(call.output_tokens)}</span>
        <span className="mono muted">{formatDuration(call.latency_ms)}</span>
        {call.provider || call.model ? <span className="muted">{[call.provider, call.model].filter(Boolean).join(' · ')}</span> : null}
        {call.error_kind ? <Badge tone="danger">错误: {call.error_kind}</Badge> : null}
        {call.stop_reason ? <span className={styles.callStop}><Badge plain>{call.stop_reason}</Badge></span> : null}
      </summary>
      <div className={styles.callBody}>
        {request || response ? (
          <>
            {request ? (
              <div className={styles.drawerSection}>
                <h4 className={styles.sectionLabel}>请求</h4>
                <pre className={`${ui.codeBlock} ${styles.codeBlockSmall}`}>{request}</pre>
              </div>
            ) : null}
            {response ? (
              <div className={styles.drawerSection}>
                <h4 className={styles.sectionLabel}>响应</h4>
                <pre className={`${ui.codeBlock} ${styles.codeBlockSmall}`}>{response}</pre>
              </div>
            ) : null}
          </>
        ) : <span className="muted small">没有保存请求或响应内容。</span>}
      </div>
    </details>
  )
}

export function NodeDrawer({
  reviewId, node, onClose,
}: { reviewId: string; node: GraphNode | null; onClose: () => void }) {
  const open = Boolean(node)
  const nodeId = node?.id
  const detail = useQuery({
    queryKey: ['review', reviewId, 'node', nodeId],
    queryFn: () => getReviewNode(reviewId, nodeId!),
    enabled: open && Boolean(nodeId),
  })
  const calls = usePaginated<LlmCall>(
    ['review', reviewId, 'calls', nodeId],
    (cursor) => nodeId ? getNodeCalls(reviewId, nodeId, cursor) : Promise.resolve({ calls: [] }),
    'calls',
    { enabled: open && Boolean(nodeId) },
  )

  const data = detail.data
  const current = data || node
  const attempt = current?.attempt
  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={node?.id || '节点详情'}
      description={node?.kind}
      footer={<Button variant="secondary" onClick={onClose}>关闭</Button>}
    >
      {current ? (
        <div className={styles.drawerMeta}>
          <StatusBadge status={current.status} />
          <span className={styles.nodeKind}>{current.kind}</span>
          <span>{current.provider || '—'}</span>
          {typeof attempt === 'number' && attempt > 0 ? <span>第 {attempt} 次尝试</span> : null}
          <span className="mono">{formatDuration(data?.duration_ms)}</span>
          <span className="mono">{formatCost(data?.usage?.cost_micros)}</span>
          {current.depends_on?.length ? <span>依赖 <code>{current.depends_on.join(', ')}</code></span> : null}
        </div>
      ) : null}
      {detail.isPending ? <LoadingState label="正在读取节点详情…" /> : null}
      {detail.isError ? <ErrorState error={detail.error} onRetry={() => { void detail.refetch() }} /> : null}
      {data?.error ? (
        <div className={styles.nodeError} role="alert">
          <strong>节点错误</strong>
          <pre>{data.error}</pre>
        </div>
      ) : null}
      {current?.status === 'failed' && !data?.error && !detail.isPending ? (
        <div className={styles.nodeError}>节点状态为失败，但服务端没有返回 error 字段。</div>
      ) : null}
      {data?.params !== undefined ? <ParamsSection value={data.params} /> : null}
      {data?.input !== undefined ? <JsonSection title="输入" value={data.input} /> : null}
      {current && !detail.isPending ? (
        data?.output !== undefined && data.output !== null
          ? <JsonSection title="输出" value={data.output} copy />
          : (
            <section className={styles.drawerSection}>
              <h3 className={styles.sectionLabel}>输出</h3>
              <EmptyState title="没有输出记录" description="服务端没有返回该节点的 output 字段。" />
            </section>
          )
      ) : null}
      {current ? (
        <section className={styles.drawerSection}>
          <h3 className={styles.sectionLabel}>LLM 调用 · {calls.items.length}{calls.hasNextPage ? '+' : ''} 轮</h3>
          {calls.isPending ? <LoadingState label="正在读取模型调用…" /> : null}
          {calls.isError ? <ErrorState error={calls.error} onRetry={calls.retry} /> : null}
          {calls.isSuccess && calls.items.length === 0 ? <span className="muted small">这是一段代码节点，没有模型调用。</span> : null}
          {calls.items.length ? (
            <div className={styles.callList}>
              {calls.items.map((call, index) => <CallItem key={call.id || `${call.turn}-${index}`} call={call} />)}
            </div>
          ) : null}
          {calls.hasNextPage ? (
            <Button variant="secondary" onClick={calls.fetchNextPage} loading={calls.isFetchingNextPage}>加载更多调用</Button>
          ) : null}
        </section>
      ) : null}
    </Drawer>
  )
}
