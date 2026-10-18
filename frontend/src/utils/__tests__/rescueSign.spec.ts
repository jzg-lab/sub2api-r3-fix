import { describe, expect, it } from 'vitest'
import { parseRescueSignInfo, signRemainingSeconds } from '../rescueSign'

const at = '2026-10-13T00:00:00Z'
const bridge = (accounts: unknown) => ({ status_json: JSON.stringify({ prober: { accounts } }) })
const sign = { account_id: 1, sign_captured_at: at, estimated_remaining_seconds: 60, estimate_basis: 'fallback' }

describe('rescue sign observations', () => {
  it('does not display missing, Go zero, invalid or malformed captures', () => {
    for (const captured of [null, undefined, '', '0001-01-01T00:00:00Z', 'bad']) {
      expect(parseRescueSignInfo(bridge([{ ...sign, sign_captured_at: captured }]))).toEqual({})
    }
    expect(parseRescueSignInfo(bridge({}))).toEqual({})
    expect(parseRescueSignInfo({ status_json: '{' })).toEqual({})
  })

  it('keeps unknown lifetime distinct from zero remaining lifetime', () => {
    for (const value of [-1, -2, null, '60']) {
      expect(parseRescueSignInfo(bridge([{ ...sign, estimated_remaining_seconds: value }]))[1]?.estRemainingSec).toBeNull()
    }
    expect(parseRescueSignInfo(bridge([{ ...sign, estimate_basis: 'none' }]))[1]?.estRemainingSec).toBeNull()
    expect(parseRescueSignInfo(bridge([{ ...sign, estimated_remaining_seconds: 0 }]))[1]?.estRemainingSec).toBe(0)
  })

  it('ages estimates from the snapshot without extending them for a future anchor', () => {
    const info = parseRescueSignInfo(bridge([sign]))[1]!
    const now = Date.parse(at)
    expect(signRemainingSeconds(info, at, now + 10_000)).toBe(50)
    expect(signRemainingSeconds(info, at, now + 70_000)).toBe(0)
    expect(signRemainingSeconds(info, at, now - 10_000)).toBe(60)
    expect(signRemainingSeconds(info, undefined, now)).toBeNull()
    expect(signRemainingSeconds(info, '0001-01-01T00:00:00Z', now)).toBeNull()
  })
})
