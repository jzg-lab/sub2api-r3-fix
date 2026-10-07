import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import UserDashboardStats from '../UserDashboardStats.vue'
import UserDashboardModels from '../UserDashboardModels.vue'
import type { UserDashboardStats as Stats } from '@/api/usage'
import type { ModelStat } from '@/types'

vi.mock('vue-i18n', async (original) => ({ ...await original<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
vi.mock('vue-chartjs', () => ({ Doughnut: {
  props: ['data', 'options'], template: '<div class="chart-data">{{ JSON.stringify(data) }}</div>',
} }))

const stats = { total_tokens: 1_890_000_000, total_actual_cost: 50, total_cost: 100,
  today_actual_cost: 2, today_cost: 4, total_api_keys: 6, active_api_keys: 5,
  by_platform: [{ platform: 'openai', total_actual_cost: 40, today_actual_cost: 1, total_requests: 20, total_tokens: 300 }],
} as Stats

describe('dashboard summary preserves existing data', () => {
  it('retains cumulative cost, platform differences and disabled quotas in the new layout', async () => {
    const wrapper = mount(UserDashboardStats, { props: { stats, balance: 98.75, isSimple: false,
      platformQuotas: [{ platform: 'openai', daily_limit_usd: 0 }],
    } })
    expect(wrapper.text()).toContain('$98.75')
    expect(wrapper.text()).toContain('1.89B')
    expect(wrapper.text()).toContain('dashboard.platformOther')
    expect(wrapper.text()).toContain('$10.0000')
    expect(wrapper.text()).toContain('dashboard.platformQuota.disabled')
    await wrapper.setProps({ isSimple: true })
    expect(wrapper.find('.signal-metric--balance').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('dashboard.platformQuota.disabled')
    expect(wrapper.text()).toContain('$50.0000 / $100.0000')
    wrapper.unmount()
  })

  it('uses the same model token totals and handles empty or invalid data without NaN', async () => {
    const wrapper = mount(UserDashboardModels, { props: { models: [
      { model: 'small', total_tokens: 20 }, { model: 'large', total_tokens: 80 },
      { model: 'unknown', total_tokens: NaN },
    ] as ModelStat[], startDate: '2026-10-01', endDate: '2026-10-08', loading: false } })
    expect(wrapper.findAll('li')[0].text()).toContain('large80.0%')
    expect(JSON.parse(wrapper.get('.chart-data').text()).datasets[0].data).toEqual([80, 20])
    await wrapper.setProps({ models: [{ model: 'zero', total_tokens: 0 }] as ModelStat[] })
    expect(wrapper.find('.chart-data').exists()).toBe(false)
    expect(wrapper.text()).toContain('dashboard.noDataAvailable')
    expect(wrapper.text()).not.toContain('NaN')
    wrapper.unmount()
  })
})
