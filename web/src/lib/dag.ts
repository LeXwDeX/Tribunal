import type { GraphEdge, GraphNode } from '../api/types'

export const NODE_WIDTH = 192
export const NODE_HEIGHT = 64
// Column pitch 220 and row pitch 84 match the v2 console prototype.
export const COLUMN_GAP = 28
export const ROW_GAP = 20
// Horizontal inset. The vertical inset grows with the canvas height.
export const PADDING = 16
// Room above the first row for the lane headers.
export const LANE_HEADER = 28

export interface PositionedNode extends GraphNode {
  x: number
  y: number
  layer: number
  row: number
}

export interface PositionedEdge extends GraphEdge {
  key: string
  path: string
  implicit: boolean
}

export interface DagLayout {
  nodes: PositionedNode[]
  edges: PositionedEdge[]
  width: number
  height: number
  paddingY: number
  layerCount: number
  cycleNodes: string[]
  unknownEdges: number
}

function edgeKey(from: string, to: string): string {
  return `${from}\u0000${to}`
}

export function layoutDag(inputNodes: GraphNode[], inputEdges: GraphEdge[]): DagLayout {
  const nodes = inputNodes || []
  const byId = new Map<string, GraphNode>()
  const order = new Map<string, number>()
  nodes.forEach((node, index) => {
    if (!byId.has(node.id)) {
      byId.set(node.id, node)
      order.set(node.id, index)
    }
  })

  const dependencySets = new Map<string, Set<string>>()
  nodes.forEach((node) => dependencySets.set(node.id, new Set()))
  const allEdges: Array<{ from: string; to: string; implicit: boolean }> = []
  const seenEdges = new Set<string>()
  let unknownEdges = 0

  const addEdge = (from: string, to: string, implicit: boolean) => {
    if (!byId.has(from) || !byId.has(to)) {
      unknownEdges += 1
      return
    }
    if (from === to) return
    const key = edgeKey(from, to)
    if (!seenEdges.has(key)) {
      seenEdges.add(key)
      allEdges.push({ from, to, implicit })
    }
    dependencySets.get(to)?.add(from)
  }

  for (const edge of inputEdges || []) addEdge(edge.from, edge.to, false)
  for (const node of nodes) {
    for (const dependency of node.depends_on || []) addEdge(dependency, node.id, true)
  }

  const indegree = new Map<string, number>()
  const dependents = new Map<string, string[]>()
  for (const node of nodes) {
    indegree.set(node.id, dependencySets.get(node.id)?.size || 0)
    dependents.set(node.id, [])
  }
  for (const node of nodes) {
    const deps = dependencySets.get(node.id)
    if (!deps) continue
    for (const dep of deps) dependents.get(dep)?.push(node.id)
  }
  for (const list of dependents.values()) {
    list.sort((a, b) => (order.get(a) || 0) - (order.get(b) || 0))
  }

  const layers = new Map<string, number>()
  const queue: string[] = []
  for (const node of nodes) {
    if ((indegree.get(node.id) || 0) === 0) {
      queue.push(node.id)
      layers.set(node.id, 0)
    }
  }
  queue.sort((a, b) => (order.get(a) || 0) - (order.get(b) || 0))

  let cursor = 0
  const processed = new Set<string>()
  while (cursor < queue.length) {
    const id = queue[cursor]
    cursor += 1
    if (processed.has(id)) continue
    processed.add(id)
    const layer = layers.get(id) || 0
    for (const dependent of dependents.get(id) || []) {
      layers.set(dependent, Math.max(layers.get(dependent) ?? 0, layer + 1))
      const next = (indegree.get(dependent) || 0) - 1
      indegree.set(dependent, next)
      if (next === 0) queue.push(dependent)
    }
  }

  const cycleNodes = nodes.filter((node) => !processed.has(node.id)).map((node) => node.id)
  let maxLayer = 0
  for (const layer of layers.values()) maxLayer = Math.max(maxLayer, layer)
  if (cycleNodes.length) {
    for (const id of cycleNodes) layers.set(id, maxLayer + 1)
    maxLayer += 1
  }

  const grouped = new Map<number, string[]>()
  for (const node of nodes) {
    const layer = layers.get(node.id) ?? 0
    const row = grouped.get(layer) || []
    row.push(node.id)
    grouped.set(layer, row)
  }

  const positioned: PositionedNode[] = []
  let maxRows = 1
  const layerCount = maxLayer + 1
  for (const ids of grouped.values()) maxRows = Math.max(maxRows, ids.length)
  const baseHeight = PADDING * 2 + LANE_HEADER + maxRows * NODE_HEIGHT + Math.max(0, maxRows - 1) * ROW_GAP
  // Split a 5% height increase, measured on the previous canvas height, between top and bottom.
  const paddingY = PADDING + Math.round(baseHeight * 0.025)
  const height = baseHeight + (paddingY - PADDING) * 2
  for (const node of nodes) {
    const layer = layers.get(node.id) ?? 0
    const ids = grouped.get(layer) || []
    const row = ids.indexOf(node.id)
    positioned.push({
      ...node,
      layer,
      row,
      x: PADDING + layer * (NODE_WIDTH + COLUMN_GAP),
      y: paddingY + LANE_HEADER + row * (NODE_HEIGHT + ROW_GAP),
    })
  }

  const positions = new Map(positioned.map((node) => [node.id, node]))
  const edges: PositionedEdge[] = []
  for (const edge of allEdges) {
    const from = positions.get(edge.from)
    const to = positions.get(edge.to)
    if (!from || !to) continue
    const startX = from.x + NODE_WIDTH
    const startY = from.y + NODE_HEIGHT / 2
    const endX = to.x
    const endY = to.y + NODE_HEIGHT / 2
    const middle = (startX + endX) / 2
    edges.push({
      from: edge.from,
      to: edge.to,
      key: `${edge.from}->${edge.to}`,
      implicit: edge.implicit,
      path: `M ${startX} ${startY} C ${middle} ${startY}, ${middle} ${endY},${endX} ${endY}`,
    })
  }

  return {
    nodes: positioned,
    edges,
    width: PADDING * 2 + layerCount * NODE_WIDTH + Math.max(0, layerCount - 1) * COLUMN_GAP,
    height,
    paddingY,
    layerCount,
    cycleNodes,
    unknownEdges,
  }
}
