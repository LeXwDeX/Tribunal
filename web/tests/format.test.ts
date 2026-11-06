import { describe, expect, it } from 'vitest'
import { formatDuration, formatJson, formatTokens, labelForTrigger, safeExternalUrl } from '../src/lib/format'
import { formatRatio, statNumber } from '../src/lib/stats'

describe('display formatting', () => {
  it('labels the startup catch-up trigger', () => {
    expect(labelForTrigger('catch_up')).toBe('启动补评')
    expect(labelForTrigger('mr_update')).toBe('MR 更新')
  })

  it('distinguishes unavailable statistics from a real zero', () => {
    expect(statNumber({ runs: 0 }, 'runs')).toBe(0)
    expect(statNumber({ runs: null }, 'runs')).toBeNull()
    expect(statNumber({}, 'runs')).toBeNull()
    expect(formatRatio(null)).toBe('不可用')
    expect(formatRatio(0)).toBe('0.0%')
    expect(formatRatio(0.25)).toBe('25.0%')
    expect(formatRatio(1)).toBe('100.0%')
    for (const invalid of [-1, 25, NaN, Infinity]) {
      expect(formatRatio(invalid)).toBe('不可用')
    }
  })

  it('formats durations and tokens', () => {
    expect(formatDuration(48_000)).toBe('48s')
    expect(formatDuration(134_000)).toBe('2m 14s')
    expect(formatTokens(182_340, 12_011)).toContain('182.3k')
  })

  it('only allows http(s) external URLs', () => {
    expect(safeExternalUrl('https://gitlab.example.com/x')).toBe('https://gitlab.example.com/x')
    expect(safeExternalUrl('javascript:alert(1)')).toBeUndefined()
    expect(safeExternalUrl('data:text/html,x')).toBeUndefined()
  })

  it('serializes values to text without generating HTML', () => {
    expect(formatJson({ a: 1 })).toBe('{\n  "a": 1\n}')
    expect(formatJson('<b>x</b>')).toBe('<b>x</b>')
  })
})
