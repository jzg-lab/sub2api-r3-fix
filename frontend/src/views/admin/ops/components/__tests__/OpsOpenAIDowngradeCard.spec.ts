import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive } from 'vue'
import OpsOpenAIDowngradeCard from '../OpsOpenAIDowngradeCard.vue'
import OpenAIDowngradeView from '@/views/admin/OpenAIDowngradeView.vue'

const getDashboard = vi.fn()
const settings = reactive({ opsMonitoringEnabled: true, fetch: vi.fn() })
vi.mock('@/api/admin/ops', () => ({ default: {}, opsAPI: { getOpenAIDowngradeDashboard: (...args: unknown[]) => getDashboard(...args) } }))
vi.mock('@/stores/adminSettings', () => ({ useAdminSettingsStore: () => settings }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))

const empty = { buckets: [], account_stats: [], recent_events: [], probe_count_24h: 0, success_count_24h: 0, degraded_account_count: 0, at_capacity_bucket_count: 0 }
beforeEach(() => { vi.useFakeTimers(); getDashboard.mockReset(); settings.opsMonitoringEnabled = true })
afterEach(() => vi.useRealTimers())

describe('OpenAI probe standalone page', () => {
  it('shows failure without fake zero statistics, supports retry and cleans up polling', async () => {
    getDashboard.mockRejectedValueOnce(new Error('storage unavailable')).mockResolvedValue(empty)
    const wrapper = mount(OpsOpenAIDowngradeCard)
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('storage unavailable')
    expect(wrapper.text()).not.toContain('admin.ops.openaiDowngrade.summary')
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('common.noData')
    const signal = getDashboard.mock.calls[1][0].signal as AbortSignal
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(120_000)
    expect(getDashboard).toHaveBeenCalledTimes(2)
  })

  it('does not query or poll when ops monitoring is disabled', async () => {
    settings.opsMonitoringEnabled = false
    const wrapper = mount(OpenAIDowngradeView, { global: { stubs: { AppLayout: { template: '<div><slot /></div>' } } } })
    await flushPromises()
    expect(wrapper.text()).toContain('admin.ops.openaiDowngrade.disabled')
    await vi.advanceTimersByTimeAsync(60_000)
    expect(getDashboard).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('ignores a response after unmount', async () => {
    let resolve!: (value: typeof empty) => void
    getDashboard.mockReturnValue(new Promise(done => { resolve = done }))
    const wrapper = mount(OpsOpenAIDowngradeCard)
    const signal = getDashboard.mock.calls[0][0].signal as AbortSignal
    wrapper.unmount()
    resolve(empty)
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(vi.getTimerCount()).toBe(0)
  })
})
