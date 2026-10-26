import { useMemo, useState } from 'react'
import type { GraphEdge, GraphNode } from '../../api/types'
import { COLUMN_GAP, layoutDag, NODE_HEIGHT, NODE_WIDTH, PADDING } from '../../lib/dag'
import { labelForStatus, statusTone } from '../../lib/format'
import { EmptyState } from '../../components/ui'
import { laneLabel } from './reviewView'
import styles from './review.module.css'

export function nodeStatusTone(status: string) {
  return statusTone(status)
}

export function ExecutionGraph({
  nodes, edges, selectedId, onSelect,
}: {
  nodes: GraphNode[]
  edges: GraphEdge[]
  selectedId?: string
  onSelect: (node: GraphNode) => void
}) {
  const layout = useMemo(() => layoutDag(nodes, edges), [nodes, edges])
  const [hotId, setHotId] = useState<string | null>(null)
  const lanes = useMemo(() => {
    const kinds = Array.from({ length: layout.layerCount }, () => [] as string[])
    for (const node of layout.nodes) kinds[node.layer]?.push(node.kind)
    return kinds.map((list, layer) => laneLabel(layer, list))
  }, [layout])

  if (!nodes.length) {
    return <EmptyState title="执行图尚未生成" description="评审开始执行后，节点会按依赖关系显示在这里。" />
  }

  return (
    <>
      <div className={styles.graphViewport}>
        <div className={styles.graphCanvas} style={{ width: layout.width, height: layout.height }}>
          {lanes.map((label, layer) => (
            <span
              key={layer}
              className={styles.graphLane}
              aria-hidden="true"
              style={{ left: PADDING + layer * (NODE_WIDTH + COLUMN_GAP), top: PADDING }}
            >
              {label}
            </span>
          ))}
          <svg className={styles.graphEdges} width={layout.width} height={layout.height} aria-hidden="true">
            {layout.edges.map((edge) => (
              <path
                key={edge.key}
                d={edge.path}
                className={hotId && (edge.from === hotId || edge.to === hotId) ? styles.edgeHot : undefined}
              />
            ))}
          </svg>
          {layout.nodes.map((node) => {
            const selected = node.id === selectedId
            const status = labelForStatus(node.status)
            return (
              <button
                key={node.id}
                type="button"
                className={[styles.nodeCard, selected ? styles.nodeCardSelected : ''].filter(Boolean).join(' ')}
                data-status={node.status}
                style={{ left: node.x, top: node.y, width: NODE_WIDTH, height: NODE_HEIGHT }}
                onClick={() => onSelect(node)}
                onMouseEnter={() => setHotId(node.id)}
                onMouseLeave={() => setHotId(null)}
                onFocus={() => setHotId(node.id)}
                onBlur={() => setHotId(null)}
                aria-pressed={selected}
                aria-label={`节点 ${node.id}，类型 ${node.kind}，状态 ${status}`}
                title={node.id}
              >
                <span className={styles.nodeHead}>
                  <span className={styles.nodeId}>{node.id}</span>
                  <span className={styles.nodeStatus}>{status}</span>
                </span>
                <span className={styles.nodeFooter}>
                  <span className={styles.nodeKind}>{node.kind}</span>
                  <span>
                    {typeof node.attempt === 'number' && node.attempt > 1
                      ? <span className={styles.nodeAttempt}>第 {node.attempt} 次 · </span>
                      : null}
                    {node.provider || '—'}
                  </span>
                </span>
              </button>
            )
          })}
        </div>
      </div>
      <p className={styles.graphHint}>
        点击节点查看参数、输出和模型调用。共 {layout.nodes.length} 个节点，
        {layout.edges.length} 条连接；第 1 层为无依赖节点。
        {layout.cycleNodes.length ? ` 检测到 ${layout.cycleNodes.length} 个循环依赖节点，已放置在末层。` : ''}
        {layout.unknownEdges ? ` 忽略了 ${layout.unknownEdges} 条指向未知节点的连接。` : ''}
      </p>
    </>
  )
}
