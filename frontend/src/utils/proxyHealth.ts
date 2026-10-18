import type { Proxy, ProxyQualityCheckResult } from '@/types'

export interface ProxyExitObservation {
  success: boolean
  latency_ms?: number
  message?: string
  ip_address?: string
  country?: string
  country_code?: string
  region?: string
  city?: string
}

export function applyProxyExitObservation(target: Proxy, result: ProxyExitObservation) {
  target.latency_status = result.success ? 'success' : 'failed'
  target.latency_ms = result.success ? result.latency_ms : undefined
  target.ip_address = result.success ? result.ip_address : undefined
  target.country = result.success ? result.country : undefined
  target.country_code = result.success ? result.country_code : undefined
  target.region = result.success ? result.region : undefined
  target.city = result.success ? result.city : undefined
  target.latency_message = result.message
}

export function proxyQualityExitObservation(result: ProxyQualityCheckResult): ProxyExitObservation {
  const base = result.items.find(item => item.target === 'base_connectivity')
  return {
    success: base?.status === 'pass' && Boolean(result.exit_ip),
    latency_ms: result.base_latency_ms,
    message: base?.message || result.summary,
    ip_address: result.exit_ip,
    country: result.country,
    country_code: result.country_code,
  }
}
