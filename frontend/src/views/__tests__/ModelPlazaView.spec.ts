import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive, ref } from 'vue'
import ModelPlazaView from '../ModelPlazaView.vue'
import ModelPlazaContent from '@/components/modelPlaza/ModelPlazaContent.vue'
import { refreshModelPlaza } from '@/api/modelPlaza'
import { apiClient } from '@/api/client'
import { plazaFixture } from '@/components/modelPlaza/__tests__/plazaFixture'
const state = vi.hoisted(() => ({ auth: null as unknown, locale: null as unknown, monitorEnabled: false }))
vi.mock('@/api/modelPlaza', () => ({ refreshModelPlaza: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ fetchPublicSettings: vi.fn(), cachedPublicSettings: { channel_monitor_enabled: state.monitorEnabled } }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => state.auth }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ locale: state.locale, t: (key: string) => key }),
}))
const api = vi.mocked(refreshModelPlaza)
beforeEach(() => {
  vi.useFakeTimers()
  state.auth = reactive({ isAuthenticated: false, user: null as { id: number } | null })
  state.locale = ref('zh')
  state.monitorEnabled = false
  Object.defineProperty(document, 'hidden', { configurable: true, value: false })
  api.mockReset()
  api.mockResolvedValue({ data: plazaFixture(), etag: 'version-1' })
})
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })
const create = () => mount(ModelPlazaView, { global: { stubs: { PlazaNavBar: true, ModelPlazaContent: true } } })
describe('live model prices', () => {
  it('loads exact model statistics for signed-in plaza viewers', async () => {
    state.monitorEnabled = true
    state.auth = reactive({ isAuthenticated: true, user: { id: 1 } })
    const matrix = { coverage: {}, group_by: 'platform_group_model', items: [] }
    const get = vi.spyOn(apiClient, 'get').mockImplementation(async url => ({
      data: url.endsWith('/matrix') ? matrix : { config: { refresh_interval_seconds: 300 } },
    }))
    const wrapper = create()
    await flushPromises()
    expect(get).toHaveBeenCalledWith('/channel-monitor-v2/matrix', expect.objectContaining({
      params: expect.objectContaining({ range: '24h', group_by: 'platform_group_model', include_output_tps: true, preserve_model_names: true }),
    }))
    expect(wrapper.findComponent(ModelPlazaContent).props('monitor')).toEqual(matrix)
    wrapper.unmount()
  })
  it('revalidates at 3 seconds, keeps data on 304/failure and aborts on unmount', async () => {
    const wrapper = create()
    await flushPromises()
    expect(api).toHaveBeenCalledTimes(1)
    api.mockResolvedValueOnce({ data: null, etag: 'version-1' })
    await vi.advanceTimersByTimeAsync(3000)
    expect(api).toHaveBeenLastCalledWith('version-1', expect.any(AbortSignal))
    expect(wrapper.findComponent(ModelPlazaContent).props('response')).not.toBeNull()
    api.mockRejectedValueOnce({ status: 500 })
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.findComponent(ModelPlazaContent).props('error')).toBe(true)
    expect(wrapper.findComponent(ModelPlazaContent).props('response')).not.toBeNull()
    api.mockImplementationOnce(() => new Promise(() => {}))
    await vi.advanceTimersByTimeAsync(3000)
    const signal = api.mock.calls.at(-1)![1]!
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
    const calls = api.mock.calls.length
    await vi.advanceTimersByTimeAsync(6000)
    expect(api).toHaveBeenCalledTimes(calls)
  })
  it('removes revoked prices and invalidates the ETag on account changes', async () => {
    const wrapper = create()
    await flushPromises()
    api.mockRejectedValueOnce({ status: 403 })
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.findComponent(ModelPlazaContent).props('response')).toBeNull()
    const auth = state.auth as { user: { id: number } | null }
    auth.user = { id: 2 }
    await flushPromises()
    expect(api).toHaveBeenLastCalledWith(undefined, expect.any(AbortSignal))
    wrapper.unmount()
  })
  it('pauses hidden-page polling and refreshes immediately on visibility return', async () => {
    const wrapper = create()
    await flushPromises()
    Object.defineProperty(document, 'hidden', { configurable: true, value: true })
    await vi.advanceTimersByTimeAsync(9000)
    expect(api).toHaveBeenCalledTimes(1)
    Object.defineProperty(document, 'hidden', { configurable: true, value: false })
    document.dispatchEvent(new Event('visibilitychange'))
    await flushPromises()
    expect(api).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })
})
