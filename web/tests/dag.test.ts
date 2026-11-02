import { describe, expect, it } from 'vitest'
import { COLUMN_GAP, LANE_HEADER, NODE_HEIGHT, NODE_WIDTH, PADDING, ROW_GAP, layoutDag } from '../src/lib/dag'

function previousCanvasHeight(rows: number): number {
  return PADDING * 2 + LANE_HEADER + rows * NODE_HEIGHT + Math.max(0, rows - 1) * ROW_GAP
}

describe('layoutDag', () => {
  it('places a dependency chain in successive layers', () => {
    const layout = layoutDag(
      [
        { id: 'a', kind: 'x', status: 'completed' },
        { id: 'b', kind: 'x', status: 'running', depends_on: ['a'] },
        { id: 'c', kind: 'x', status: 'pending' },
      ],
      [
        { from: 'a', to: 'b' },
        { from: 'b', to: 'c' },
      ],
    )
    const byId = new Map(layout.nodes.map((node) => [node.id, node]))
    expect(byId.get('a')?.layer).toBe(0)
    expect(byId.get('b')?.layer).toBe(1)
    expect(byId.get('c')?.layer).toBe(2)
    expect(byId.get('b')?.x).toBe(PADDING + NODE_WIDTH + COLUMN_GAP)
    expect(layout.edges).toHaveLength(2)
    expect(layout.edges[0].path).toContain('M ')
  })

  it('adds about 5% canvas height for one row without moving the node horizontally', () => {
    const layout = layoutDag([{ id: 'a', kind: 'x', status: 'completed' }], [])
    const node = layout.nodes[0]

    expect(node.x).toBe(PADDING)
    expect(layout.paddingY).toBeGreaterThan(PADDING)
    expect(node.y).toBe(layout.paddingY + LANE_HEADER)
    expect(layout.height - (node.y + NODE_HEIGHT)).toBe(layout.paddingY)
    expect(layout.height).toBeGreaterThanOrEqual(Math.round(previousCanvasHeight(1) * 1.04))
    expect(layout.height).toBeLessThanOrEqual(Math.round(previousCanvasHeight(1) * 1.06))
  })

  it('keeps the row grid and gives a multi-row graph the same bottom clearance', () => {
    const layout = layoutDag(
      [
        { id: 'a', kind: 'x', status: 'completed' },
        { id: 'b', kind: 'x', status: 'completed' },
        { id: 'c', kind: 'x', status: 'completed', depends_on: ['a', 'b'] },
      ],
      [],
    )
    const byId = new Map(layout.nodes.map((node) => [node.id, node]))

    expect(byId.get('b')!.y - byId.get('a')!.y).toBe(NODE_HEIGHT + ROW_GAP)
    expect(byId.get('a')!.x).toBe(PADDING)
    expect(byId.get('c')!.x).toBe(PADDING + NODE_WIDTH + COLUMN_GAP)
    expect(layout.height - (byId.get('b')!.y + NODE_HEIGHT)).toBe(layout.paddingY)
    expect(layout.paddingY).toBeGreaterThanOrEqual(20)
    expect(layout.height).toBeGreaterThanOrEqual(Math.round(previousCanvasHeight(2) * 1.04))
    expect(layout.height).toBeLessThanOrEqual(Math.round(previousCanvasHeight(2) * 1.06))
  })

  it('keeps edge endpoints aligned with the shifted nodes', () => {
    const layout = layoutDag(
      [
        { id: 'a', kind: 'x', status: 'completed' },
        { id: 'b', kind: 'x', status: 'completed', depends_on: ['a'] },
      ],
      [],
    )
    const from = layout.nodes.find((node) => node.id === 'a')!
    const to = layout.nodes.find((node) => node.id === 'b')!

    expect(layout.edges[0].path).toContain(`M ${from.x + NODE_WIDTH} ${from.y + NODE_HEIGHT / 2}`)
    expect(layout.edges[0].path).toContain(`${to.x} ${to.y + NODE_HEIGHT / 2}`)
  })

  it('puts disconnected components in the first layer and does not crash', () => {
    const layout = layoutDag(
      [
        { id: 'a', kind: 'x', status: 'completed' },
        { id: 'alone', kind: 'x', status: 'completed' },
      ],
      [],
    )
    expect(layout.nodes.find((node) => node.id === 'alone')?.layer).toBe(0)
    expect(layout.layerCount).toBe(1)
    expect(layout.cycleNodes).toEqual([])
  })

  it('ignores unknown dependencies and reports them', () => {
    const layout = layoutDag([{ id: 'a', kind: 'x', status: 'completed', depends_on: ['missing'] }], [])
    expect(layout.nodes[0].layer).toBe(0)
    expect(layout.unknownEdges).toBe(1)
  })

  it('terminates on cycles and keeps every node', () => {
    const layout = layoutDag(
      [
        { id: 'a', kind: 'x', status: 'completed', depends_on: ['c'] },
        { id: 'b', kind: 'x', status: 'completed', depends_on: ['a'] },
        { id: 'c', kind: 'x', status: 'completed', depends_on: ['b'] },
        { id: 'root', kind: 'x', status: 'completed' },
      ],
      [],
    )
    expect(layout.nodes).toHaveLength(4)
    expect(layout.cycleNodes.sort()).toEqual(['a', 'b', 'c'])
    for (const node of layout.nodes) {
      if (layout.cycleNodes.includes(node.id)) expect(node.layer).toBe(1)
    }
  })

  it('preserves node ids containing dots and de-duplicates edges', () => {
    const layout = layoutDag(
      [
        { id: 'review.std.p1', kind: 'x', status: 'completed' },
        { id: 'verify.f7', kind: 'x', status: 'completed', depends_on: ['review.std.p1'] },
      ],
      [{ from: 'review.std.p1', to: 'verify.f7' }, { from: 'review.std.p1', to: 'verify.f7' }],
    )
    expect(layout.nodes.map((node) => node.id)).toEqual(['review.std.p1', 'verify.f7'])
    expect(layout.edges).toHaveLength(1)
    expect(layout.nodes.find((node) => node.id === 'verify.f7')?.layer).toBe(1)
  })

  it('handles an empty graph', () => {
    const layout = layoutDag([], [])
    expect(layout.nodes).toEqual([])
    expect(layout.edges).toEqual([])
    expect(layout.cycleNodes).toEqual([])
    expect(layout.height).toBeGreaterThan(previousCanvasHeight(1))
  })
})
