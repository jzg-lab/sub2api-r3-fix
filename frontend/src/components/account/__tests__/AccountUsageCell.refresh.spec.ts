import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account, AccountUsageInfo } from '@/types'

const { getUsage } = vi.hoisted(() => ({ getUsage: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getUsage } } }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

const account = (id: number) => ({
  id, platform: 'openai', type: 'oauth', extra: {}, credentials: {}
}) as Account
const usage = (percent: number) => ({
  five_hour: { utilization: percent, resets_at: null, remaining_seconds: 0 }
}) as AccountUsageInfo
const stubs = {
  UsageProgressBar: {
    props: ['utilization'],
    template: '<span data-test="quota">{{ utilization }}</span>'
  },
  OpenAIQuotaResetCell: { template: '<div><slot name="pre-actions" /></div>' }
}
function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => { resolve = done })
  return { promise, resolve }
}

describe('AccountUsageCell refresh consistency', () => {
  beforeEach(() => {
    getUsage.mockReset()
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: () => ({
        matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn()
      })
    })
  })

  it('passes force to the API when no source is specified', async () => {
    getUsage.mockResolvedValue(usage(12))
    const wrapper = mount(AccountUsageCell, {
      props: { account: account(90101), manualRefreshToken: 0 },
      global: { stubs }
    })
    await flushPromises()
    await wrapper.setProps({ manualRefreshToken: 1 })
    await flushPromises()
    expect(getUsage).toHaveBeenLastCalledWith(90101, undefined, true)
    wrapper.unmount()
  })

  it('does not overwrite a forced refresh with an older response', async () => {
    const old = deferred<AccountUsageInfo>()
    getUsage.mockReturnValueOnce(old.promise).mockResolvedValueOnce(usage(72))
    const wrapper = mount(AccountUsageCell, {
      props: { account: account(90102), manualRefreshToken: 0 },
      global: { stubs }
    })
    await flushPromises()
    await wrapper.setProps({ manualRefreshToken: 1 })
    await flushPromises()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('72')
    old.resolve(usage(12))
    await flushPromises()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('72')
    wrapper.unmount()
  })

  it('keeps a replaced account isolated from queued old responses', async () => {
    const old = deferred<AccountUsageInfo>()
    getUsage.mockReturnValueOnce(old.promise).mockResolvedValueOnce(usage(44))
    const wrapper = mount(AccountUsageCell, {
      props: { account: account(90103) },
      global: { stubs }
    })
    await flushPromises()
    await wrapper.setProps({ account: account(90104) })
    await flushPromises()
    old.resolve(usage(99))
    await flushPromises()
    expect(getUsage).toHaveBeenLastCalledWith(90104)
    expect(wrapper.get('[data-test="quota"]').text()).toBe('44')
    wrapper.unmount()
  })

  it('refreshes through the parent when usage is batch managed', async () => {
    const requestBatchedUsage = vi.fn()
    const current = account(90105)
    const wrapper = mount(AccountUsageCell, {
      props: { account: current, batchedUsage: usage(12), requestBatchedUsage },
      global: { stubs }
    })
    await flushPromises()
    requestBatchedUsage.mockClear()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(requestBatchedUsage).toHaveBeenCalledWith(current, { force: true })
    expect(getUsage).not.toHaveBeenCalled()
    await wrapper.setProps({ batchedUsage: usage(73) })
    expect(wrapper.get('[data-test="quota"]').text()).toBe('73')
    wrapper.unmount()
  })

  it('preserves active-query intent for batch-managed Anthropic accounts', async () => {
    const requestBatchedUsage = vi.fn()
    const current = { ...account(90108), platform: 'anthropic' } as Account
    const wrapper = mount(AccountUsageCell, {
      props: { account: current, batchedUsage: usage(12), requestBatchedUsage },
      global: { stubs }
    })
    await flushPromises()
    requestBatchedUsage.mockClear()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(requestBatchedUsage).toHaveBeenCalledWith(current, { force: true, source: 'active' })
    wrapper.unmount()
  })

  it('does not hide a failed active refresh behind the previous snapshot', async () => {
    getUsage.mockResolvedValueOnce(usage(21)).mockRejectedValueOnce(new Error('offline'))
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    const wrapper = mount(AccountUsageCell, {
      props: { account: account(90106) },
      global: { stubs }
    })
    try {
      await flushPromises()
      await wrapper.get('button').trigger('click')
      await flushPromises()
      expect(getUsage).toHaveBeenLastCalledWith(90106, 'active', true)
      expect(wrapper.text()).toContain('common.error')
      expect(wrapper.get('[data-test="quota"]').text()).toBe('21')
    } finally {
      wrapper.unmount()
      consoleError.mockRestore()
    }
  })

  it('does not emit a late result after unmount', async () => {
    const pending = deferred<AccountUsageInfo>()
    getUsage.mockReturnValueOnce(pending.promise)
    const wrapper = mount(AccountUsageCell, {
      props: { account: account(90107) },
      global: { stubs }
    })
    await flushPromises()
    wrapper.unmount()
    pending.resolve(usage(93))
    await flushPromises()
    expect(wrapper.emitted('usage-loaded')).toBeUndefined()
  })

  it('does not clear the new account active-query state when the old query completes', async () => {
    const old = deferred<AccountUsageInfo>()
    const current = deferred<AccountUsageInfo>()
    getUsage.mockResolvedValueOnce(usage(11)).mockReturnValueOnce(old.promise)
      .mockResolvedValueOnce(usage(22)).mockReturnValueOnce(current.promise)
    const wrapper = mount(AccountUsageCell, {
      props: { account: account(90109) }, global: { stubs }
    })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await wrapper.setProps({ account: account(90110) })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    old.resolve(usage(99))
    await flushPromises()
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('22')
    current.resolve(usage(33))
    await flushPromises()
    expect(wrapper.get('button').attributes('disabled')).toBeUndefined()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('33')
    wrapper.unmount()
  })

  it('releases the active-query button when a newer manual refresh supersedes it', async () => {
    const old = deferred<AccountUsageInfo>()
    getUsage.mockResolvedValueOnce(usage(14)).mockReturnValueOnce(old.promise)
      .mockResolvedValueOnce(usage(45))
    const wrapper = mount(AccountUsageCell, {
      props: { account: account(90111), manualRefreshToken: 0 }, global: { stubs }
    })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await wrapper.setProps({ manualRefreshToken: 1 })
    await flushPromises()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('45')
    expect(wrapper.get('button').attributes('disabled')).toBeUndefined()
    old.resolve(usage(90))
    await flushPromises()
    expect(wrapper.get('[data-test="quota"]').text()).toBe('45')
    wrapper.unmount()
  })
})
