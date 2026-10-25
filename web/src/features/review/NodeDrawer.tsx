import { useQuery } from '@tanstack/react-query'
import { getNodeCalls, getReviewNode } from '../../api/api'
import type { GraphNode, LlmCall } from '../../api/types'
import { Badge, Button, Drawer, EmptyState, ErrorState, LoadingState, StatusBadge } from '../../components/ui'
import { usePaginated } from '../../lib/hooks'
import { formatDuration, formatJson, formatNumber } from '../../lib/format'
import styles from '../../pages/pages.module.css'

function JsonSection({ title, value }: { title: string; value: unknown }) {
  const content = formatJson(value)
  return (
    <section className={styles.drawerSection}>
      <h3 className={styles.drawerSectionTitle}>{title}</h3>
      {content ? <pre className="pre" style={preStyle}>{content}</pre> : <span className="muted small">没有记录。</span>}
    </section>
  )
}

function CallItem({ call }: { call: LlmCall }) {
  return (
    <article className={styles.callRow}>
      <div className={styles.callHeader}>
        <strong>第 {call.turn ?? '?'} 轮</strong>
        <span>{call.provider || '未知 provider'}{call.model ? ` · ${call.model}` : ''}</span>
        <span>{formatNumber(call.input_tokens)} / {formatNumber(call.output_tokens)} tokens</span>
        <span>{formatDuration(call.latency_ms)}</span>
        {call.stop_reason ? <span className="muted">stop: {call.stop_reason}</span> : null}
        {call.error_kind ? <span style={{ color: 'var(--danger-fg)' }}>错误: {call.error_kind}</span> : null}
      </div>
      {(call.request !== undefined || call.response !== undefined) ? (
        <details className={styles.callDetails}>
          <summary>展开请求与响应</summary>
          <div className={styles.detailsStack}>
            <JsonSection title="请求" value={call.request} />
            <JsonSection title="响应" value={call.response} />
          </div>
        </details>
      ) : <span className="muted small">没有保存请求或响应内容。</span>}
    </article>
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
  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={node?.id || '节点详情'}
      description={node?.kind}
      footer={<Button variant="secondary" onClick={onClose}>关闭</Button>}
    >
      {detail.isPending ? <LoadingState label="正在读取节点详情…" /> : null}
      {detail.isError ? <ErrorState error={detail.error} onRetry={() => { void detail.refetch() }} /> : null}
      {current ? (
        <>
          <div className={styles.docMeta}>
            <span>类型 <code>{current.kind}</code></span>
            <span>状态 <StatusBadge status={current.status} /></span>
            <span>provider <code>{current.provider || '—'}</code></span>
            <span>尝试 <span className="tabular">{current.attempt ?? '—'}</span></span>
            <span>用时 <span className="tabular">{(current as { duration_ms?: number }).duration_ms !== undefined ? formatDuration((current as { duration_ms?: number }).duration_ms) : '—'}</span></span>
            <span>依赖 <code>{current.depends_on?.length ? current.depends_on.join(', ') : '—'}</code></span>
          </div>
          {data?.error ? (
            <div className={styles.warning} role="alert">
              <strong>节点错误</strong>
              <pre className="pre" style={{ marginTop: 6 }}>{data.error}</pre>
            </div>
          ) : null}
          {data?.params !== undefined ? <JsonSection title="参数" value={data.params} /> : null}
          {data?.input !== undefined ? <JsonSection title="输入" value={data.input} /> : null}
          <JsonSection title="输出" value={data?.output} />
          <section className={styles.drawerSection}>
            <h3 className={styles.drawerSectionTitle}>模型调用（{calls.items.length}{calls.hasNextPage ? '+' : ''}）</h3>
            {calls.isPending ? <LoadingState label="正在读取模型调用…" /> : null}
            {calls.isError ? <ErrorState error={calls.error} onRetry={calls.retry} /> : null}
            {calls.isSuccess && calls.items.length === 0 ? <span className="muted small">这是一段代码节点，没有模型调用。</span> : null}
            <div className={styles.list}>
              {calls.items.map((call, index) => <CallItem key={call.id || `${call.turn}-${index}`} call={call} />)}
            </div>
            {calls.hasNextPage ? (
              <Button variant="secondary" onClick={calls.fetchNextPage} loading={calls.isFetchingNextPage}>加载更多调用</Button>
            ) : null}
          </section>
        </>
      ) : null}
      {current && !data?.output && !detail.isPending ? (
        <EmptyState title="没有输出记录" description="服务端没有返回该节点的 output 字段。" />
      ) : null}
      {current?.status === 'failed' && !data?.error && !detail.isPending ? (
        <span className={styles.warning}>节点状态为失败，但服务端没有返回 error 字段。</span>
      ) : null}
    </Drawer>
  )
}

const preStyle = {
  margin: 0,
  padding: 12,
  background: 'var(--code-bg)',
  color: 'var(--code-fg)',
  borderRadius: 8,
  maxHeight: 280,
  overflow: 'auto',
} as const
