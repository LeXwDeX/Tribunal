import type { GraphNode } from '../api/types'

export const REVIEW_STAGES = ['规划', '提出', '挑战', '裁决', '发布'] as const

// Node kinds equal block names (internal/review/blocks.go). Blocks without a
// lane, such as gather or onboarding, do not move the track.
function stageForKind(kind: string): number | null {
  if (kind === 'plan' || kind === 'reasoner') return 0
  if (kind === 'code-review' || kind === 'review' || kind.startsWith('review.')) return 1
  if (kind === 'verify') return 2
  if (kind === 'adjudicate' || kind === 'synthesize') return 3
  if (kind === 'publish' || kind.startsWith('publish.')) return 4
  return null
}

// deriveReviewStage returns the index of the current lane, or null when no
// node maps to one; callers then omit the track instead of guessing.
export function deriveReviewStage(nodes?: GraphNode[]): number | null {
  if (!nodes?.length) return null
  let completed = -1
  let running: number | null = null
  for (const node of nodes) {
    const stage = stageForKind(node.kind)
    if (stage === null) continue
    if (node.status === 'completed') completed = Math.max(completed, stage)
    else if (node.status === 'running') running = running === null ? stage : Math.min(running, stage)
  }
  if (completed >= 0) return Math.min(completed + 1, REVIEW_STAGES.length - 1)
  return running
}
