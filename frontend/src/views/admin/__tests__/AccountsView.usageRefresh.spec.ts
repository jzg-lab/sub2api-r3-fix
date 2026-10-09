import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import AccountsView from '../AccountsView.vue'
import AccountRecentStatsCell from '@/components/account/AccountRecentStatsCell.vue'
import AccountUsageCell from '@/components/account/AccountUsageCell.vue'
import AccountStatusIndicator from '@/components/account/AccountStatusIndicator.vue'
import type { Account, AccountUsageInfo } from '@/types'

const { getUsage, getBatchUsage, list, getBatchTodayStats } = vi.hoisted(() => ({
  getUsage: vi.fn(), getBatchUsage: vi.fn(), list: vi.fn(), getBatchTodayStats: vi.fn()
}))
vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list, listWithEtag: async () => ({ notModified: true }),
      getUsage, getBatchUsage,
      listOpenAIAccountHealth: async () => ({ accounts: [] }),
      getBatchTodayStats,
      getUpstreamBillingProbeSettings: async () => ({ enabled: false })
    },
    proxies: { getAll: async () => [] },
    groups: { getAll: async () => [] }
  }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ token: null }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

const current = {
  id: 90201, name: 'usage-test', platform: 'anthropic', type: 'oauth',
  extra: {}, credentials: {}, status: 'active', schedulable: true,
  created_at: '2026-09-15T00:00:00Z', updated_at: '2026-09-15T00:00:00Z'
} as Account
const usage = (percent: number) => ({
  source: 'active',
  five_hour: { utilization: percent, resets_at: null, remaining_seconds: 0 }
}) as AccountUsageInfo
const batch = (percent: number) => ({ usage: { [current.id]: usage(percent) }, errors: {} })
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: Error) => void
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail })
  return { promise, resolve, reject }
}
let wrapper: VueWrapper
let desktop = true
const viewportListeners = new Set<(event: MediaQueryListEvent) => void>()
async function setDesktop(matches: boolean) {
  desktop = matches
  for (const listener of viewportListeners) listener({ matches } as MediaQueryListEvent)
  await settle()
}
async function settle() {
  await flushPromises()
  await vi.advanceTimersByTimeAsync(1)
  await flushPromises()
}
function mountView(platform = 'anthropic') {
  list.mockResolvedValue({
    items: [{ ...current, platform }], total: 1, page: 1, page_size: 20, pages: 1
  })
  wrapper = mount(AccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="table" /></div>' },
        DataTable: {
          props: ['data'],
          template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-recent_stats" :row="row" /><slot name="cell-status" :row="row" /><slot name="cell-usage" :row="row" /></div></div>'
        },
        UsageProgressBar: {
          props: ['utilization'], template: '<span data-test="quota">{{ utilization }}</span>'
        },
        OpenAIQuotaResetCell: { template: '<div><slot name="pre-actions" /></div>' },
        Pagination: true, ConfirmDialog: true, AccountActionMenu: true,
        ImportDataModal: true, ReAuthAccountModal: true, AccountTestModal: true,
        AccountStatsModal: true, ScheduledTestsPanel: true, SyncFromCrsModal: true,
        TempUnschedStatusModal: true, ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true, CreateAccountModal: true,
        EditAccountModal: true, BulkEditAccountModal: true
      }
    }
  })
}
function request(options?: { force?: boolean; source?: 'active' }) {
  const cell = wrapper.getComponent(AccountUsageCell)
  return cell.props('requestBatchedUsage')!(cell.props('account'), options)
}

describe('AccountsView usage refresh ownership', () => {
  it('updates rate-limit status along with an active quota refresh', async () => {
    const exhausted = { ...usage(100), quota_rate_limit_reset_at: '2099-10-04T00:00:00Z' }
    getBatchUsage.mockResolvedValueOnce({ usage: { [current.id]: exhausted }, errors: {} })
    getUsage.mockResolvedValueOnce({ ...usage(0), quota_rate_limit_reset_at: null })
    mountView('openai')
    await settle()
    expect(wrapper.getComponent(AccountStatusIndicator).text()).toContain('admin.accounts.status.rateLimited')
    await wrapper.getComponent(AccountUsageCell).get('button').trigger('click')
    await settle()
    expect(wrapper.getComponent(AccountStatusIndicator).text()).toContain('admin.accounts.status.active')
    expect(wrapper.getComponent(AccountStatusIndicator).text()).not.toContain('admin.accounts.status.rateLimited')
  })

  beforeEach(() => {
    vi.useFakeTimers()
    vi.stubGlobal('IntersectionObserver', undefined)
    localStorage.clear()
    localStorage.setItem('account-auto-refresh-enabled', 'false')
    getUsage.mockReset().mockResolvedValue(usage(73))
    getBatchUsage.mockReset().mockResolvedValue(batch(12))
    list.mockReset()
 getBatchTodayStats.mockReset().mockResolvedValue({stats:{}})
    desktop = true
    viewportListeners.clear()
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: () => ({
        get matches() { return desktop },
        addEventListener: (_: string, listener: (event: MediaQueryListEvent) => void) => viewportListeners.add(listener),
        removeEventListener: (_: string, listener: (event: MediaQueryListEvent) => void) => viewportListeners.delete(listener)
      })
    })
  })
  afterEach(() => {
    wrapper?.unmount()
    vi.useRealTimers()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('shows recent account metrics from the existing batch request', async () => {
    getBatchTodayStats.mockResolvedValue({ stats: {}, recent_stats: { [current.id]: {
      attempts: 20, successes: 19, failures: 1, success_rate: 0.95, cache_hit_rate: 0.6, ttft_avg_ms: 1500
    } } })
    mountView()
    await settle()
    expect(getBatchTodayStats).toHaveBeenCalledTimes(1)
    const cell = wrapper.getComponent(AccountRecentStatsCell)
    expect(cell.text()).toContain('95.00%')
    expect(cell.text()).toContain('60.00%')
    expect(cell.text()).toContain('1.50 s')
  })

  it('does not display stale metrics when the statistics store is unavailable', async () => {
    getBatchTodayStats.mockResolvedValue({ stats: {}, recent_stats_unavailable: true, recent_stats: { [current.id]: {
      attempts: 20, successes: 20, success_rate: 1
    } } })
    mountView()
    await settle()
    const cell = wrapper.getComponent(AccountRecentStatsCell)
    expect(cell.text()).toContain('admin.accounts.recentStats.unavailable')
    expect(cell.text()).not.toContain('100.00%')
  })

  it('uses the single-account active API for the Anthropic button and updates the parent cache', async () => {
    mountView()
    await settle()
    await wrapper.getComponent(AccountUsageCell).get('button').trigger('click')
    await settle()
    expect(getUsage).toHaveBeenCalledWith(current.id, 'active', true)
    expect(getBatchUsage).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
    request()
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
    expect(getBatchUsage).toHaveBeenCalledTimes(1)
  })

  it('uses the single-account active API for an OpenAI refresh instead of the batch route', async () => {
    mountView('openai')
    await settle()
    await wrapper.getComponent(AccountUsageCell).get('button').trigger('click')
    await settle()
    expect(getUsage).toHaveBeenCalledWith(current.id, 'active', true)
    expect(getBatchUsage).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
  })

  it('does not allow a pending passive batch to overwrite the newer active result', async () => {
    const stale = deferred<ReturnType<typeof batch>>()
    getBatchUsage.mockReturnValueOnce(stale.promise)
    mountView()
    await settle()
    request({ force: true, source: 'active' })
    await settle()
    stale.resolve(batch(8))
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
  })

  it('keeps ordinary cache reads from clearing a forced request in flight', async () => {
    mountView('openai')
    await settle()
    const pending = deferred<ReturnType<typeof batch>>()
    getBatchUsage.mockReturnValueOnce(pending.promise)
    request({ force: true })
    await settle()
    request()
    await settle()
    expect(getBatchUsage).toHaveBeenCalledTimes(2)
    expect(wrapper.getComponent(AccountUsageCell).props('batchedUsageLoading')).toBe(true)
    pending.resolve(batch(67))
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('67')
  })

  it('preserves the snapshot on an active failure and permits a retry', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    getUsage.mockRejectedValueOnce(new Error('offline')).mockResolvedValueOnce(usage(66))
    mountView()
    await settle()
    await wrapper.getComponent(AccountUsageCell).get('button').trigger('click')
    await settle()
    expect(wrapper.getComponent(AccountUsageCell).text()).toContain('Failed')
    expect(wrapper.get('[data-test="quota"]').text()).toBe('12')
    await wrapper.getComponent(AccountUsageCell).get('button').trigger('click')
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('66')
    expect(wrapper.getComponent(AccountUsageCell).props('batchedUsageError')).toBeNull()
  })

  it('ignores a stale failed active query after a newer forced batch has completed', async () => {
    const stale = deferred<AccountUsageInfo>()
    getUsage.mockReturnValueOnce(stale.promise)
    mountView()
    await settle()
    request({ force: true, source: 'active' })
    await settle()
    getBatchUsage.mockResolvedValueOnce(batch(65))
    request({ force: true })
    await settle()
    stale.reject(new Error('stale offline'))
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('65')
    expect(wrapper.getComponent(AccountUsageCell).props('batchedUsageError')).toBeNull()
    expect(wrapper.getComponent(AccountUsageCell).props('batchedUsageLoading')).toBe(false)
  })

  it('preserves the last snapshot and exposes per-account batch errors', async () => {
    mountView('openai')
    await settle()
    getBatchUsage.mockResolvedValueOnce({ usage: {}, errors: { [current.id]: 'unavailable' } })
    request({ force: true })
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('12')
    expect(wrapper.getComponent(AccountUsageCell).text()).toContain('unavailable')
    request()
    await settle()
    expect(getBatchUsage).toHaveBeenCalledTimes(3)
    expect(wrapper.getComponent(AccountUsageCell).props('batchedUsageError')).toBeNull()
  })

  it('does not treat a missing batch result as a successful empty snapshot', async () => {
    mountView('openai')
    await settle()
    getBatchUsage.mockResolvedValueOnce({ usage: {}, errors: {} })
    request({ force: true })
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('12')
    expect(wrapper.getComponent(AccountUsageCell).props('batchedUsageError')).toBe('Failed')
  })

  it('permits an active retry when the initial Anthropic batch failed without a snapshot', async () => {
    getBatchUsage.mockResolvedValueOnce({ usage: {}, errors: { [current.id]: 'unavailable' } })
    mountView()
    await settle()
    expect(wrapper.getComponent(AccountUsageCell).text()).toContain('unavailable')
    await wrapper.getComponent(AccountUsageCell).get('button').trigger('click')
    await settle()
    expect(getUsage).toHaveBeenCalledWith(current.id, 'active', true)
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
  })

  it('ignores an old desktop batch after a newer mobile result and returning to desktop', async () => {
    const stale = deferred<ReturnType<typeof batch>>()
    getBatchUsage.mockReturnValueOnce(stale.promise)
    mountView('openai')
    await settle()
    await setDesktop(false)
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
    stale.resolve(batch(8))
    await settle()
    await setDesktop(true)
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
  })

  it('does not reuse an old mobile cache after a forced desktop refresh', async () => {
    desktop = false
    mountView('openai')
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
    await setDesktop(true)
    getBatchUsage.mockResolvedValueOnce(batch(84))
    request({ force: true })
    await settle()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('84')
    getUsage.mockResolvedValueOnce(usage(91))
    await setDesktop(false)
    expect(getUsage).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="quota"]').text()).toBe('91')
    await setDesktop(true)
    expect(wrapper.get('[data-test="quota"]').text()).toBe('91')
  })

  it('invalidates in-flight desktop work when switching to mobile even if the mobile query fails', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    mountView('openai')
    await settle()
    const stale = deferred<ReturnType<typeof batch>>()
    getBatchUsage.mockReturnValueOnce(stale.promise)
    request({ force: true })
    await settle()
    getUsage.mockRejectedValueOnce(new Error('offline'))
    await setDesktop(false)
    stale.resolve(batch(8))
    await settle()
    expect(wrapper.getComponent(AccountUsageCell).props('batchedUsage')).toEqual(usage(12))
    expect(wrapper.getComponent(AccountUsageCell).props('batchedUsageLoading')).toBe(false)
  })
})
