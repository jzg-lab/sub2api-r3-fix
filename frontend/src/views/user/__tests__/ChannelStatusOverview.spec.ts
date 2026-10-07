import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'
import ChannelStatusOverview from '../ChannelStatusOverview.vue'

const snapshot = vi.fn()
const matrix = vi.fn()
const auth = { isAdmin: false }
vi.mock('@/api/channelMonitorV2', () => ({ getSnapshot: (...args: unknown[]) => snapshot(...args), getMatrix: (...args: unknown[]) => matrix(...args) }))
vi.mock('@/api/groups', () => ({ default: { getAvailable: async () => [], getUserGroupRates: async () => ({}) } }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => auth }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key, locale: { value: 'en' } }) }))

const health = { overall: 'healthy', error_rate: 'healthy', ttft: 'healthy', cache: 'healthy', score: 95, minimum_sample: 20 }
const metrics = { request_count: 0, cache_rate_denominator: 0, error_rate: 0.02, cache_rate: 0.5, ttft: { p50_ms: 500 } }
const coverage = { requested_start: '2026-10-08T00:00:00Z', requested_end: '2026-10-08T00:03:00Z',
  data_through: '2026-10-08T00:03:00Z', computed_at: '2026-10-08T00:03:00Z', bucket_seconds: 60, coverage_complete: true, aggregation_lag_seconds: 0 }
const snap = { coverage, metrics, health, config: { refresh_interval_seconds: 60 } }
const data = (name: string) => ({ coverage, items: [{ platform: 'openai', group_id: 7, group_name: name, metrics, health, buckets: [] }] })
async function render(url = '/monitor') {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/monitor', component: ChannelStatusOverview }] })
  await router.push(url)
  await router.isReady()
  const wrapper = mount(RouterView, { global: { plugins: [router], stubs: {
    AppLayout: { template: '<div><slot /></div>' },
    ChannelStatusCard: { props: ['row'], template: '<div class="test-card">{{ row.group_name }}</div>' },
  } } })
  await flushPromises()
  return { wrapper, router }
}
beforeEach(() => {
  vi.useFakeTimers()
  snapshot.mockReset().mockResolvedValue(snap)
  matrix.mockReset().mockResolvedValue(data('Current group'))
  auth.isAdmin = false
})
afterEach(() => vi.useRealTimers())

describe('channel overview data flow', () => {
  it.each([false, true])('uses the existing role scope (admin=%s) and retains filters in the analysis link', async (admin) => {
    auth.isAdmin = admin
    const { wrapper } = await render('/monitor?range=24h&platform=openai&group=7&model=gpt-5')
    expect(matrix).toHaveBeenCalledWith({ range: '24h', platforms: ['openai'], groupIds: [7], models: ['gpt-5'] }, 'platform_group', admin, expect.any(AbortSignal))
    const link = wrapper.get('a').attributes('href')
    expect(link).toContain('monitor_view=details')
    expect(link).toContain('model=gpt-5')
    wrapper.unmount()
  })

  it('ignores obsolete responses after range changes and clears both timers on unmount', async () => {
    let finishOld!: (value: ReturnType<typeof data>) => void
    matrix.mockImplementationOnce(() => new Promise(done => { finishOld = done }))
    const { wrapper, router } = await render()
    const oldSignal = matrix.mock.calls[0][3] as AbortSignal
    await router.replace('/monitor?range=24h')
    await flushPromises()
    expect(oldSignal.aborted).toBe(true)
    expect(wrapper.get('.test-card').text()).toBe('Current group')
    finishOld(data('Obsolete group'))
    await flushPromises()
    expect(wrapper.text()).not.toContain('Obsolete group')
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(matrix).toHaveBeenCalledTimes(2)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('shows a query failure and retries without claiming there are no groups', async () => {
    snapshot.mockRejectedValueOnce(new Error('query failed'))
    const { wrapper } = await render()
    expect(wrapper.get('[role="alert"]').text()).toContain('query failed')
    expect(wrapper.text()).not.toContain('common.noData')
    await vi.advanceTimersByTimeAsync(60_000)
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.get('.test-card').text()).toBe('Current group')
    wrapper.unmount()
  })
})
