import { describe, expect, it } from 'vitest'
import {
  ALERT_KEY_PATTERN,
  aggregateAckState,
  countSinceWatermark,
  disabledFingerprint,
  fnv1a,
  isAcked,
  providerAcked,
  providerFingerprint,
  publicationFingerprint,
  retryFingerprint,
  tokenFingerprint,
  tokenStage,
  watermarkFingerprint,
  workspaceFingerprint,
} from '../src/lib/alertAcks'

const DAY = 86_400_000

function acks(entries: Record<string, string>) {
  return new Map(Object.entries(entries))
}

describe('alert ack fingerprints', () => {
  it('hashes text with stable 32-bit FNV-1a over UTF-8', () => {
    expect(fnv1a('')).toBe('811c9dc5')
    expect(fnv1a('a')).toBe('e40c292c')
    expect(fnv1a('foobar')).toBe('bf9cf968')
    expect(fnv1a('工作区')).toMatch(/^[0-9a-f]{8}$/)
    expect(fnv1a('工作区')).not.toBe(fnv1a('工作'))
  })

  it('keeps fingerprints printable ASCII and keys within the server pattern', () => {
    const project = { created_at: '2026-01-01T00:00:00Z', workspace: { last_error: '克隆失败：网络错误', last_refresh_at: '2026-02-01T00:00:00Z' } }
    for (const value of [workspaceFingerprint(project), publicationFingerprint('原因'), retryFingerprint('重试')]) {
      expect(value).toMatch(/^[\x20-\x7e]{1,256}$/)
      expect(value).not.toContain('克隆')
    }
    expect(ALERT_KEY_PATTERN.test('review-0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b')).toBe(true)
    expect(ALERT_KEY_PATTERN.test('provider-user/model')).toBe(false)
  })

  it('splits token expiry into the two UI stages', () => {
    expect(tokenStage(30)).toBe('warn')
    expect(tokenStage(1)).toBe('warn')
    expect(tokenStage(0)).toBe('danger')
    expect(tokenStage(-3)).toBe('danger')
  })

  it('token alert stays acked while unchanged and reappears on a stage change or rotation', () => {
    const project = { created_at: '2026-01-01T00:00:00Z', token_expires_at: '2026-12-10T00:00:00Z' }
    const stored = acks({ 'token-42': tokenFingerprint(project, 'warn') })
    expect(isAcked(stored, 'token-42', tokenFingerprint(project, 'warn'))).toBe(true)
    expect(isAcked(stored, 'token-42', tokenFingerprint(project, 'danger'))).toBe(false)
    const rotated = { ...project, token_expires_at: '2027-01-10T00:00:00Z' }
    expect(isAcked(stored, 'token-42', tokenFingerprint(rotated, 'warn'))).toBe(false)
    const reRegistered = { ...project, created_at: '2026-06-01T00:00:00Z' }
    expect(isAcked(stored, 'token-42', tokenFingerprint(reRegistered, 'warn'))).toBe(false)
  })

  it('workspace alert reappears when the error, refresh time or base changes', () => {
    const project = { created_at: '2026-01-01T00:00:00Z', workspace: { last_error: 'clone failed', last_refresh_at: '2026-02-01T00:00:00Z', base_sha: 'aaa111' } }
    const stored = acks({ 'workspace-42': workspaceFingerprint(project) })
    expect(isAcked(stored, 'workspace-42', workspaceFingerprint({ ...project, workspace: { ...project.workspace } }))).toBe(true)
    expect(isAcked(stored, 'workspace-42', workspaceFingerprint({ ...project, workspace: { ...project.workspace, last_error: 'index failed' } }))).toBe(false)
    expect(isAcked(stored, 'workspace-42', workspaceFingerprint({ ...project, workspace: { ...project.workspace, last_refresh_at: '2026-03-01T00:00:00Z' } }))).toBe(false)
    expect(isAcked(stored, 'workspace-42', workspaceFingerprint({ ...project, created_at: '2026-05-01T00:00:00Z' }))).toBe(false)
    // A plain successful base refresh moves base_sha but not last_refresh_at;
    // recovering and failing again with the same error must reappear.
    expect(isAcked(stored, 'workspace-42', workspaceFingerprint({ ...project, workspace: { ...project.workspace, base_sha: 'bbb222' } }))).toBe(false)
  })

  it('disabled alert reappears after the project revision changes', () => {
    const project = { created_at: '2026-01-01T00:00:00Z', revision: 3, updated_at: '2026-01-02T00:00:00Z' }
    const stored = acks({ 'disabled-42': disabledFingerprint(project) })
    expect(isAcked(stored, 'disabled-42', disabledFingerprint({ ...project }))).toBe(true)
    expect(isAcked(stored, 'disabled-42', disabledFingerprint({ ...project, revision: 5 }))).toBe(false)
    // Older servers without revision fall back to updated_at.
    const legacy = { created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-02T00:00:00Z' }
    const legacyAcks = acks({ 'disabled-42': disabledFingerprint(legacy) })
    expect(isAcked(legacyAcks, 'disabled-42', disabledFingerprint(legacy))).toBe(true)
    expect(isAcked(legacyAcks, 'disabled-42', disabledFingerprint({ ...legacy, updated_at: '2026-01-03T00:00:00Z' }))).toBe(false)
  })

  it('publication and retry alerts reappear when the reason changes', () => {
    const stored = acks({
      'publication-wf': publicationFingerprint('merge request is not available'),
      'retry-wf': retryFingerprint('rate limited'),
    })
    expect(isAcked(stored, 'publication-wf', publicationFingerprint('merge request is not available'))).toBe(true)
    expect(isAcked(stored, 'publication-wf', publicationFingerprint('forbidden'))).toBe(false)
    expect(isAcked(stored, 'retry-wf', retryFingerprint('rate limited'))).toBe(true)
    expect(isAcked(stored, 'retry-wf', retryFingerprint('timeout'))).toBe(false)
    expect(isAcked(stored, 'missing', 'failed')).toBe(false)
  })

  it('provider alert stays acked the same UTC day unless errors rise, and reappears the next day', () => {
    const now = Date.parse('2026-11-27T10:00:00Z')
    const stored = providerFingerprint(5, now)
    expect(stored).toBe('2026-11-27|5')
    expect(providerAcked(stored, 5, now + 3_600_000)).toBe(true)
    expect(providerAcked(stored, 3, now + 3_600_000)).toBe(true)
    expect(providerAcked(stored, 6, now + 3_600_000)).toBe(false)
    expect(providerAcked(stored, 2, now + DAY)).toBe(false)
    expect(providerAcked(undefined, 2, now)).toBe(false)
    expect(providerAcked('garbage', 2, now)).toBe(false)
  })
})

describe('aggregate alert watermark', () => {
  // A was created first but finished after B. A created_at watermark would
  // lose A forever; the finished_at watermark keeps it.
  const b = { created_at: '2026-11-27T09:00:00Z', finished_at: '2026-11-27T09:10:00Z' }
  const a = { created_at: '2026-11-27T08:00:00Z', finished_at: '2026-11-27T09:30:00Z' }

  it('uses max(finished_at) of the seen hit set and counts later finishes', () => {
    const seen = watermarkFingerprint([b])
    expect(seen).toBe('2026-11-27T09:10:00.000Z')
    expect(countSinceWatermark([b], seen)).toBe(0)
    // A finishes after the ack; it is new even though it was created earlier.
    expect(countSinceWatermark([b, a], seen)).toBe(1)
    const reAcked = watermarkFingerprint([b, a], seen)
    expect(reAcked).toBe('2026-11-27T09:30:00.000Z')
    expect(countSinceWatermark([b, a], reAcked)).toBe(0)
  })

  it('keeps the watermark monotonic and falls back to created_at', () => {
    expect(watermarkFingerprint([b], '2026-11-28T00:00:00.000Z')).toBe('2026-11-28T00:00:00.000Z')
    expect(watermarkFingerprint([{ created_at: '2026-11-27T07:00:00Z' }])).toBe('2026-11-27T07:00:00.000Z')
    expect(countSinceWatermark([b], 'not a time')).toBe(1)
  })

  it('reports only new hits after an ack and keeps "+" only when every loaded hit is new', () => {
    expect(aggregateAckState([a, b], true, undefined)).toEqual({ acked: false, count: 2, plus: true, sinceAck: false })
    const seen = watermarkFingerprint([b])
    expect(aggregateAckState([a, b], true, seen)).toEqual({ acked: false, count: 1, plus: false, sinceAck: true })
    const older = '2026-11-27T00:00:00.000Z'
    expect(aggregateAckState([a, b], true, older)).toEqual({ acked: false, count: 2, plus: true, sinceAck: true })
    expect(aggregateAckState([a, b], false, older)).toEqual({ acked: false, count: 2, plus: false, sinceAck: true })
    const all = watermarkFingerprint([a, b])
    expect(aggregateAckState([a, b], true, all)).toEqual({ acked: true, count: 2, plus: true, sinceAck: false })
  })
})
