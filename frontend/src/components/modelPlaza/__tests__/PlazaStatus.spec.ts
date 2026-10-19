import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import type { MonitorCoverage, MonitorHealth, MonitorMatrixRow, MonitorMetric } from '@/api/channelMonitorV2'
import PlazaStatus from '../PlazaStatus.vue'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

const coverage: MonitorCoverage = {
  requested_start: '2026-10-19T00:00:00Z', requested_end: '2026-10-20T00:00:00Z',
  coverage_start: '2026-10-19T00:00:00Z', data_through: '2026-10-19T23:55:00Z',
  computed_at: '2026-10-19T23:56:00Z', aggregation_lag_seconds: 60,
  coverage_complete: true, bucket_seconds: 3600,
}
const health: MonitorHealth = {
  overall: 'healthy', error_rate: 'healthy', ttft: 'unknown', cache: 'unknown', minimum_sample: 20,
}
const metrics: MonitorMetric = {
  request_count: 1, success_requests: 1, error_requests: 0, success_rate: 1, error_rate: 0,
  token_count: 0, rpm: 0, tpm: 0, cache_rate: 0, cache_rate_numerator: 0, cache_rate_denominator: 0,
  ttft: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
  duration: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
}
function row(hours: number[], m = metrics, h = health): MonitorMatrixRow {
  return {
    platform: 'openai', group_id: 1, model: 'gpt-6.1', metrics: m, health: h,
    buckets: hours.map(hour => ({
      bucket_start: new Date(Date.parse(coverage.requested_start) + hour * 3600_000).toISOString(),
      metrics: m, health: h,
    })),
  }
}

describe('model plaza success timeline', () => {
  it.each([false, true])('has no artificial gaps when every hour has requests (expanded=%s)', expanded => {
    const wrapper = mount(PlazaStatus, { props: { coverage, row: row(Array.from({ length: 24 }, (_, i) => i)), expanded } })
    expect(wrapper.findAll('.plaza-timeline i')).toHaveLength(24)
    expect(wrapper.findAll('.plaza-timeline .healthy')).toHaveLength(24)
    expect(wrapper.findAll('.plaza-timeline .unknown')).toHaveLength(0)
    expect(wrapper.find('.plaza-status-label b').text()).toBe('100.0%')
    wrapper.unmount()
  })

  it.each([
    [1, 0, 'healthy', '100.0%'],
    [10, 0.1, 'warning', '90.0%'],
    [1, 1, 'critical', '0.0%'],
  ] as const)('colors %s requests with error rate %s without a minimum sample gate', (count, errorRate, state, label) => {
    const m = { ...metrics, request_count: count, success_rate: 1 - errorRate, error_rate: errorRate }
    const h = { ...health, overall: 'unknown', error_rate: 'unknown' } as MonitorHealth
    const wrapper = mount(PlazaStatus, { props: { coverage, row: row([0], m, h) } })
    expect(wrapper.find('.plaza-timeline i').classes()).toContain(state)
    expect(wrapper.find('.plaza-status-label b').text()).toBe(label)
    expect(wrapper.findAll('.plaza-timeline .unknown')).toHaveLength(23)
    wrapper.unmount()
  })

  it('uses availability even when counts are hidden and the blended health disagrees', () => {
    const m = { ...metrics, request_count: 0, success_requests: 0 }
    const wrapper = mount(PlazaStatus, { props: { coverage, row: row([0], m, { ...health, overall: 'critical' }) } })
    expect(wrapper.find('.plaza-timeline i').classes()).toContain('healthy')
    expect(wrapper.find('.plaza-status-label b').text()).toBe('100.0%')
    wrapper.unmount()
  })

  it('keeps real gaps gray and colors the same slot when a request arrives', async () => {
    const wrapper = mount(PlazaStatus, { props: { coverage, row: row([-1, 0, 2, 24]) } })
    const bars = wrapper.findAll('.plaza-timeline i')
    expect(bars).toHaveLength(24)
    expect(bars[0].classes()).toContain('healthy')
    expect(bars[1].classes()).toContain('unknown')
    expect(bars[2].classes()).toContain('healthy')
    expect(bars[23].classes()).toContain('unknown')
    await wrapper.setProps({ row: row([0, 1, 2]) })
    expect(wrapper.findAll('.plaza-timeline i')[1].classes()).toContain('healthy')
    expect(wrapper.findAll('.plaza-timeline .healthy')).toHaveLength(3)
    wrapper.unmount()
  })

  it('does not interpret an empty bucket as 100% success', () => {
    const m = { ...metrics, request_count: 0, success_requests: 0, success_rate: 0 }
    const h = { ...health, overall: 'unknown', error_rate: 'unknown' } as MonitorHealth
    const wrapper = mount(PlazaStatus, { props: { coverage, row: row([0], m, h) } })
    expect(wrapper.findAll('.plaza-timeline .unknown')).toHaveLength(24)
    expect(wrapper.find('.plaza-status-label b').text()).toBe('—')
    wrapper.unmount()
  })
})
