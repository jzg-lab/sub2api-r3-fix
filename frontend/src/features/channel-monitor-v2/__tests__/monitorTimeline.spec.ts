import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { monitorBucketStarts } from '../monitorTimeline'
import { monitorCardMetrics } from '../monitorFormat'
import ChannelStatusCard from '@/components/user/monitor/ChannelStatusCard.vue'
import type { MonitorCoverage, MonitorHealth, MonitorMetric } from '@/api/channelMonitorV2'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ locale: { value: 'en' }, t: (key: string, params?: Record<string, unknown>) =>
    key === 'channelMonitorV2.cards.multiplier' ? `Rate ${params?.value}x` : key }),
}))

const coverage: MonitorCoverage = {
  requested_start: '2026-10-08T00:00:00Z', requested_end: '2026-10-08T00:03:00Z',
  coverage_start: '2026-10-08T00:01:00Z', data_through: '2026-10-08T00:02:00Z',
  computed_at: '2026-10-08T00:03:00Z', aggregation_lag_seconds: 60,
  coverage_complete: false, bucket_seconds: 60,
}
const health: MonitorHealth = { overall: 'healthy', error_rate: 'healthy', ttft: 'healthy', cache: 'healthy', score: 100, minimum_sample: 20 }
const metrics: MonitorMetric = {
  request_count: 0, success_requests: 0, error_requests: 0, token_count: 0, rpm: 0, tpm: 0,
  error_rate: 0.02, cache_rate: 0.5, cache_rate_numerator: 0, cache_rate_denominator: 0,
  ttft: { sample_count: 0, p50_ms: 500, p95_ms: 900, avg_ms: 650 },
  duration: { sample_count: 0, p50_ms: 500, p95_ms: 900, avg_ms: 650 },
}

describe('channel cards with public, redacted metrics', () => {
  it('keeps valid rates and latency when absolute counts are redacted', () => {
    expect(monitorCardMetrics(metrics, health)).toEqual({ availability: '98.0%', cache: '50.0%', ttft: '500ms' })
    expect(monitorCardMetrics({ ...metrics, cache_rate: 0 }, health).cache).toBe('0.00%')
  })

  it('does not turn missing evidence into 100% availability or a zero cache rate', () => {
    expect(monitorCardMetrics({ ...metrics, error_rate: 0, ttft: { ...metrics.ttft, p50_ms: null } }, {
      ...health, overall: 'unknown', error_rate: 'unknown', cache: 'unknown', score: null,
    })).toEqual({ availability: '—', cache: '—', ttft: '—' })
  })

  it('aligns sparse history to the selected range, including interior and trailing gaps', async () => {
    const starts = monitorBucketStarts(coverage)
    expect(starts).toHaveLength(3)
    const wrapper = mount(ChannelStatusCard, {
      props: {
        row: { platform: 'openai', group_id: 7, group_name: 'Example', health, metrics,
          buckets: [{ bucket_start: starts[1], health, metrics }] },
        starts, multiplier: 0, countdown: 60,
      },

    })
    const bars = wrapper.findAll('.status-slot')
    expect(bars).toHaveLength(3)
    expect(bars[0].find('span').classes()).toContain('health-unknown')
    expect(bars[0].find('span').attributes('data-state')).toBe('unknown')
    expect(bars[1].find('span').attributes('data-state')).toBe('healthy')
    expect(bars[1].find('span').classes()).toContain('health-score10')
    expect(bars[2].find('span').classes()).toContain('health-unknown')
    expect(wrapper.text()).toContain('Rate 0x')
    await bars[1].trigger('focus')
    expect(wrapper.get('[role="tooltip"]').text()).toContain('98.0%')
    await bars[1].trigger('keydown', { key: 'Escape' })
    expect(wrapper.find('[role="tooltip"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('supports older coverage and rejects invalid dates', () => {
    expect(monitorBucketStarts({ ...coverage, requested_end: undefined })).toHaveLength(2)
    expect(monitorBucketStarts({ ...coverage, requested_start: 'invalid' })).toEqual([])
  })
})
