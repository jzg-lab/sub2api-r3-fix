import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AccountRecentStatsCell from '../AccountRecentStatsCell.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const stats = {
  successes: 9, failures: 1, attempts: 10, success_rate: 0.9,
  cache_hit_rate: 0.8, ttft_avg_ms: 1250, latency_samples: 9,
  last_observed_at: 1, consecutive_failures: 0
}

describe('AccountRecentStatsCell', () => {
  it('shows success samples, cache rate and first token latency', () => {
    const wrapper = mount(AccountRecentStatsCell, { props: { stats } })
    expect(wrapper.text()).toContain('90.00% (9/10)')
    expect(wrapper.text()).toContain('80.00%')
    expect(wrapper.text()).toContain('1.25 s')
    expect(wrapper.text()).not.toContain('fewSamples')
  })

  it('distinguishes unknown data from a measured zero', () => {
    expect(mount(AccountRecentStatsCell).text()).toContain('noData')
    const wrapper = mount(AccountRecentStatsCell, { props: { stats: { ...stats, successes: 0, success_rate: 0, cache_hit_rate: null, ttft_avg_ms: null } } })
    expect(wrapper.text()).toContain('0.00% (0/10)')
    expect(wrapper.text()).toContain('—')
  })

  it('marks sparse samples and does not show stale statistics after a fetch error', () => {
    expect(mount(AccountRecentStatsCell, { props: { stats: { ...stats, attempts: 1, successes: 1 } } }).text()).toContain('fewSamples')
    const wrapper = mount(AccountRecentStatsCell, { props: { stats, unavailable: true } })
    expect(wrapper.text()).toContain('unavailable')
    expect(wrapper.text()).not.toContain('90.00%')
  })
})
