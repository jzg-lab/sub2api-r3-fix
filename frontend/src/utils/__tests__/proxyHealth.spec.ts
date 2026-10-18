import { describe, expect, it } from 'vitest'
import type { Proxy, ProxyQualityCheckResult } from '@/types'
import { applyProxyExitObservation, proxyQualityExitObservation } from '../proxyHealth'

describe('proxy exit evidence', () => {
  const previous = () => ({
    id: 7, ip_address: '192.0.2.1', country: 'old', country_code: 'US',
    region: 'old', city: 'old', latency_ms: 23, latency_status: 'success',
  } as Proxy)

  it.each(['fail', 'warn', 'challenge', 'missing'])('clears stale evidence after %s', status => {
    const target = previous()
    const result = {
      items: status === 'missing' ? [] : [{ target: 'base_connectivity', status }],
      summary: 'exit unknown',
    } as unknown as ProxyQualityCheckResult
    applyProxyExitObservation(target, proxyQualityExitObservation(result))
    expect(target).toMatchObject({ latency_status: 'failed', latency_message: 'exit unknown' })
    for (const key of ['ip_address', 'country', 'country_code', 'region', 'city', 'latency_ms']) {
      expect(target[key as keyof Proxy]).toBeUndefined()
    }
  })

  it('does not reuse old geography when only a new IP was measured', () => {
    const target = previous()
    applyProxyExitObservation(target, proxyQualityExitObservation({
      items: [{ target: 'base_connectivity', status: 'pass' }],
      exit_ip: '192.0.2.2', base_latency_ms: 12, summary: 'measured',
    } as ProxyQualityCheckResult))
    expect(target).toMatchObject({ ip_address: '192.0.2.2', latency_ms: 12, latency_status: 'success' })
    expect(target.country).toBeUndefined()
    expect(target.city).toBeUndefined()
  })

  it('does not confuse target availability with measured exit availability', () => {
    const target = previous()
    applyProxyExitObservation(target, proxyQualityExitObservation({
      items: [{ target: 'base_connectivity', status: 'pass' }, { target: 'openai', status: 'fail' }],
      exit_ip: '192.0.2.2', summary: 'upstream failed',
    } as ProxyQualityCheckResult))
    expect(target.latency_status).toBe('success')
    expect(target.ip_address).toBe('192.0.2.2')
  })

  it('fails closed on a missing IP even if the response labels it pass', () => {
    expect(proxyQualityExitObservation({
      items: [{ target: 'base_connectivity', status: 'pass' }],
    } as ProxyQualityCheckResult).success).toBe(false)
  })
})
