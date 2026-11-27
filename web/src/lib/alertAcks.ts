// Overview alert acks (#656). The server stores each user's acks as opaque
// key/fingerprint pairs; this module computes both in the browser. A
// single-object alert is read while its current fingerprint equals the stored
// one, so a state change makes it reappear. Aggregate alerts store a
// finished_at watermark instead and count only hits that finished after it.
// Fingerprints never carry raw error text: errors are FNV-1a hashed.

/** Mirrors the server's key validation; alerts with other keys stay unackable. */
export const ALERT_KEY_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/

/** Stored acks keyed by alert key, valued by fingerprint. */
export type AlertAcks = ReadonlyMap<string, string>

export type TokenStage = 'warn' | 'danger'

const encoder = new TextEncoder()

/** fnv1a returns the 32-bit FNV-1a hash of the UTF-8 bytes as 8 hex digits. */
export function fnv1a(text: string): string {
  let hash = 0x811c9dc5
  for (const byte of encoder.encode(text)) {
    hash ^= byte
    hash = Math.imul(hash, 0x01000193)
  }
  return (hash >>> 0).toString(16).padStart(8, '0')
}

export function tokenStage(days: number): TokenStage {
  return days <= 0 ? 'danger' : 'warn'
}

// Project fingerprints include created_at: a deleted and re-registered
// project reuses its GitLab id, and the old ack must not hide the new alert.
export function tokenFingerprint(project: { created_at?: string; token_expires_at?: string }, stage: TokenStage): string {
  return `${project.created_at || ''}|${project.token_expires_at || ''}|${stage}`
}

// A failed refresh keeps last_refresh_at (only a successful refresh or
// onboarding advances it), so the value only changes across a recovery.
export function workspaceFingerprint(project: { created_at?: string; workspace?: { last_error?: string; last_refresh_at?: string } }): string {
  return `${project.created_at || ''}|${fnv1a(project.workspace?.last_error || '')}|${project.workspace?.last_refresh_at || ''}`
}

export function disabledFingerprint(project: { created_at?: string; revision?: number; updated_at?: string }): string {
  const version = typeof project.revision === 'number' ? `r${project.revision}` : `u${project.updated_at || ''}`
  return `${project.created_at || ''}|${version}`
}

/** A failed run never changes; a rerun creates a new workflow id. */
export const REVIEW_FAILURE_FINGERPRINT = 'failed'

export function publicationFingerprint(error?: string): string {
  return `failed|${fnv1a(error || '')}`
}

export function retryFingerprint(reason?: string): string {
  return fnv1a(reason || '')
}

function utcDate(now: number): string {
  return new Date(now).toISOString().slice(0, 10)
}

// Provider stats are a rolling 24h count without a last-error time, so the
// ack is approximate: it holds for the same UTC day while the count does not
// rise.
export function providerFingerprint(errors: number, now: number): string {
  return `${utcDate(now)}|${errors}`
}

export function providerAcked(stored: string | undefined, errors: number, now: number): boolean {
  if (!stored) return false
  const [date, count] = stored.split('|')
  const ackedErrors = Number(count)
  if (date !== utcDate(now) || count === undefined || !Number.isFinite(ackedErrors)) return false
  return errors <= ackedErrors
}

export function isAcked(acks: AlertAcks, key: string, fingerprint: string): boolean {
  return acks.get(key) === fingerprint
}

interface TimedHit {
  created_at?: string
  finished_at?: string
}

function finishedMs(hit: TimedHit): number {
  const finished = hit.finished_at ? Date.parse(hit.finished_at) : NaN
  if (Number.isFinite(finished)) return finished
  const created = hit.created_at ? Date.parse(hit.created_at) : NaN
  return Number.isFinite(created) ? created : 0
}

// watermarkFingerprint stores max(finished_at) of the hit set the user saw.
// created_at would lose a review created earlier that finishes later, and the
// ack time would swallow hits that finished after loading but before the
// click. The watermark never moves backwards.
export function watermarkFingerprint(hits: readonly TimedHit[], previous?: string): string {
  let max = 0
  for (const hit of hits) max = Math.max(max, finishedMs(hit))
  const prior = previous ? Date.parse(previous) : NaN
  if (Number.isFinite(prior)) max = Math.max(max, prior)
  return new Date(max).toISOString()
}

/** Hits that finished after the stored watermark; an unreadable one counts all. */
export function countSinceWatermark(hits: readonly TimedHit[], stored: string): number {
  const watermark = Date.parse(stored)
  if (!Number.isFinite(watermark)) return hits.length
  return hits.filter((hit) => finishedMs(hit) > watermark).length
}

export interface AggregateAckState {
  acked: boolean
  count: number
  /** The count may be larger than shown because more pages exist. */
  plus: boolean
  /** The count only includes hits since the last ack. */
  sinceAck: boolean
}

export function aggregateAckState(hits: readonly TimedHit[], hasMore: boolean, stored: string | undefined): AggregateAckState {
  if (stored === undefined) return { acked: false, count: hits.length, plus: hasMore, sinceAck: false }
  const fresh = countSinceWatermark(hits, stored)
  if (fresh === 0) return { acked: true, count: hits.length, plus: hasMore, sinceAck: false }
  // Unloaded pages are older by created_at; they can only add new hits when
  // every loaded hit is already new.
  return { acked: false, count: fresh, plus: hasMore && fresh === hits.length, sinceAck: true }
}
