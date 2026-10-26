import type { ChallengeStatus, Finding, FindingDecision, ReviewDetail } from '../../api/types'
import { labelForStatus } from '../../lib/format'

export type VerdictCardTone = 'PASS' | 'BLOCKING' | 'INCOMPLETE' | 'FAILED' | 'CANCELLED' | 'PENDING'

// A failed or cancelled run never takes the PASS style, even if a verdict was
// recorded before the run stopped.
export function verdictCardState(review: Pick<ReviewDetail, 'status' | 'verdict'>): { tone: VerdictCardTone; mark: string } {
  const verdict = review.verdict || ''
  if (review.status === 'failed' || review.status === 'cancelled') {
    const tone = review.status === 'failed' ? 'FAILED' : 'CANCELLED'
    const mark = verdict && verdict !== 'PASS' ? verdict : labelForStatus(review.status)
    return { tone, mark }
  }
  if (verdict === 'PASS' || verdict === 'BLOCKING' || verdict === 'INCOMPLETE') return { tone: verdict, mark: verdict }
  if (verdict) return { tone: 'INCOMPLETE', mark: verdict }
  return { tone: 'PENDING', mark: labelForStatus(review.status) }
}

export interface FindingFunnel {
  proposed: number
  challenge: Record<ChallengeStatus, number>
  decision: Record<FindingDecision, number>
}

// Counts come from the findings loaded on the page, so they follow the
// current filter and pagination.
export function findingFunnel(findings: Finding[]): FindingFunnel {
  const counts: FindingFunnel = {
    proposed: findings.length,
    challenge: { confirmed: 0, refuted: 0, uncertain: 0, skipped: 0 },
    decision: { publish: 0, drop: 0, mark: 0, downgrade: 0 },
  }
  for (const item of findings) {
    const challenge = item.challenge_status && item.challenge_status in counts.challenge ? item.challenge_status : 'skipped'
    counts.challenge[challenge] += 1
    if (item.decision && item.decision in counts.decision) counts.decision[item.decision] += 1
  }
  return counts
}

const STAGES: Array<[string, (kind: string) => boolean]> = [
  ['规划', (kind) => kind === 'plan' || kind === 'reasoner'],
  ['提出', (kind) => kind === 'code-review' || kind === 'review' || kind.startsWith('review.')],
  ['挑战', (kind) => kind === 'verify'],
  ['裁决', (kind) => kind === 'adjudicate' || kind === 'synthesize'],
  ['发布', (kind) => kind === 'publish' || kind.startsWith('publish.') || kind === 'summary' || kind === 'discussion' || kind === 'commit_status'],
]

export function stageForKind(kind: string): string | undefined {
  return STAGES.find(([, match]) => match(kind))?.[0]
}

// Lane header: zero-padded layer number plus the stage most of the layer's
// nodes belong to. Unknown kinds leave only the number.
export function laneLabel(layer: number, kinds: string[]): string {
  const number = String(layer + 1).padStart(2, '0')
  const tally = new Map<string, number>()
  for (const kind of kinds) {
    const stage = stageForKind(kind)
    if (stage) tally.set(stage, (tally.get(stage) || 0) + 1)
  }
  let best: string | undefined
  let bestCount = 0
  for (const [stage, count] of tally) {
    if (count > bestCount) {
      best = stage
      bestCount = count
    }
  }
  return best ? `${number} ${best}` : number
}
