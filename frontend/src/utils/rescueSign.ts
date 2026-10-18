export interface RescueSignInfo {
  capturedAt: number
  estRemainingSec: number | null
  basis: string
}

export function parseRescueSignInfo(
  bridge: { status_json?: string | null } | null | undefined,
): Record<number, RescueSignInfo> {
  const out: Record<number, RescueSignInfo> = {}
  if (!bridge?.status_json) return out
  try {
    const parsed = JSON.parse(bridge.status_json)
    if (!Array.isArray(parsed?.prober?.accounts)) return out
    for (const acc of parsed.prober.accounts) {
      if (!Number.isSafeInteger(acc?.account_id) || acc.account_id <= 0 ||
          typeof acc.sign_captured_at !== 'string') continue
      const capturedAt = Date.parse(acc.sign_captured_at)
      // Go's zero time parses successfully in JS, but is not a captured sign.
      if (!Number.isFinite(capturedAt) || capturedAt <= 0) continue
      const basis = acc.estimate_basis === 'measured' || acc.estimate_basis === 'fallback'
        ? acc.estimate_basis : 'none'
      const remaining = acc.estimated_remaining_seconds
      out[acc.account_id] = {
        capturedAt,
        estRemainingSec: basis !== 'none' && Number.isFinite(remaining) && remaining >= 0
          ? remaining : null,
        basis,
      }
    }
  } catch {
    // Missing or malformed observations must not imply an expired sign.
  }
  return out
}

export function signRemainingSeconds(info: RescueSignInfo, checkedAt: string | undefined, now: number): number | null {
  if (info.estRemainingSec == null) return null
  const anchor = checkedAt ? Date.parse(checkedAt) : NaN
  if (!Number.isFinite(anchor) || anchor <= 0) return null
  return Math.max(0, info.estRemainingSec - Math.max(0, now - anchor) / 1000)
}
