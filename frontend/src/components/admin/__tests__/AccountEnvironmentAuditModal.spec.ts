import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import type { AccountEnvironmentAudit } from '@/api/admin/accounts'
import AccountEnvironmentAuditModal from '../AccountEnvironmentAuditModal.vue'

const { getEnvironmentAudit } = vi.hoisted(() => ({ getEnvironmentAudit: vi.fn() }))
vi.mock('@/api/admin/accounts', () => ({ accountsAPI: { getEnvironmentAudit } }))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))

const wrappers: VueWrapper[] = []
const prefix = 'admin.accounts.environmentAudit.'
const refresh = 'button[aria-label="common.refresh"]'

function report(id = 13): AccountEnvironmentAudit {
  return {
    accounts: [{
      account_id: id,
      configured_mode: 'full',
      effective_mode: 'device',
      seed_present: true,
      installation_digest: 'digest-not-for-display',
      identity_source: 'seed_derived',
      identity_generated_at: null,
      account_created_at: '2026-10-11T09:20:00+08:00',
      login_ip_recorded: false,
      proxy_id: 15
    }],
    collisions: [],
    import_batches: [],
    generated_at: '2026-10-11T12:00:00+08:00'
  }
}

function deferred() {
  let resolve!: (value: AccountEnvironmentAudit) => void
  let reject!: (error: Error) => void
  const promise = new Promise<AccountEnvironmentAudit>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

function mountModal(show = true) {
  const wrapper = mount(AccountEnvironmentAuditModal, {
    props: { show },
    global: {
      stubs: {
        BaseDialog: {
          props: ['show'],
          emits: ['close'],
          template: '<div v-if="show"><button data-testid="close" @click="$emit(\'close\')" /><slot /></div>'
        }
      }
    }
  })
  wrappers.push(wrapper)
  return wrapper
}

describe('account environment audit', () => {
  beforeEach(() => getEnvironmentAudit.mockReset())
  afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()) })

  it('does not read records until opened and passes an abort signal', async () => {
    const wrapper = mountModal(false)
    expect(getEnvironmentAudit).not.toHaveBeenCalled()
    getEnvironmentAudit.mockResolvedValueOnce(report())
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(getEnvironmentAudit).toHaveBeenCalledTimes(1)
    expect(getEnvironmentAudit.mock.calls[0][0]).toBeInstanceOf(AbortSignal)
    expect(wrapper.find('[data-account-id="13"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('full')
    expect(wrapper.text()).toContain('device')
    expect(wrapper.text()).not.toContain('digest-not-for-display')
    expect(wrapper.find('input, select, form').exists()).toBe(false)
  })

  it('keeps missing provenance and login IP unknown instead of inferring from account creation', async () => {
    const data = report()
    data.accounts[0].identity_source = 'unknown'
    data.accounts[0].proxy_id = null
    getEnvironmentAudit.mockResolvedValueOnce(data)
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.get('[data-field="identity-generated"]').text()).toBe(prefix + 'unknown')
    expect(wrapper.get('[data-field="login-ip"]').text()).toBe(prefix + 'unknown')
    expect(wrapper.text()).not.toContain('undefined')
    expect(wrapper.text()).not.toContain('#null')
  })

  it('renders recorded provenance without equating a login IP record to a verified current exit', async () => {
    const data = report()
    data.accounts[0].identity_source = 'configured_device'
    data.accounts[0].identity_generated_at = '2026-10-10T09:00:00+08:00'
    data.accounts[0].seed_present = false
    data.accounts[0].login_ip_recorded = true
    getEnvironmentAudit.mockResolvedValueOnce(data)
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.text()).toContain(prefix + 'configuredDevice')
    expect(wrapper.text()).toContain(prefix + 'missing')
    expect(wrapper.get('[data-field="identity-generated"]').text()).not.toBe(prefix + 'unknown')
    expect(wrapper.get('[data-field="login-ip"]').text()).toBe(prefix + 'recorded')
  })

  it('separates actual identifier collisions from same-minute imports', async () => {
    const data = report()
    data.collisions = [
      { kind: 'duplicate_seed', account_ids: [13, 14] },
      { kind: 'duplicate_effective_installation', account_ids: [15, 16] }
    ]
    data.import_batches = [{ kind: 'same_import_minute_only', account_ids: [13, 14, 15, 16] }]
    getEnvironmentAudit.mockResolvedValueOnce(data)
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.get('[data-testid="collisions"]').text()).toContain(prefix + 'duplicateSeed')
    expect(wrapper.get('[data-testid="collisions"]').text()).toContain(prefix + 'duplicateInstallation')
    expect(wrapper.get('[data-testid="import-batches"]').text()).toContain('#13, #14, #15, #16')
    expect(wrapper.find('[data-testid="no-collisions"]').exists()).toBe(false)
  })

  it('does not label a same-minute import alone as an identity collision', async () => {
    const data = report()
    data.import_batches = [{ kind: 'same_import_minute_only', account_ids: [13, 14] }]
    getEnvironmentAudit.mockResolvedValueOnce(data)
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.find('[data-testid="collisions"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="no-collisions"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="import-batches"]').exists()).toBe(true)
  })

  it('removes stale results on refresh failure and allows one fresh retry', async () => {
    getEnvironmentAudit.mockResolvedValueOnce(report())
    const wrapper = mountModal()
    await flushPromises()
    getEnvironmentAudit.mockRejectedValueOnce(new Error('server-internal-details'))
    await wrapper.get(refresh).trigger('click')
    expect(wrapper.find('[data-account-id]').exists()).toBe(false)
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe(prefix + 'loadFailed')
    expect(wrapper.text()).not.toContain('server-internal-details')
    expect(wrapper.find('[data-testid="empty"]').exists()).toBe(false)
    getEnvironmentAudit.mockResolvedValueOnce(report(14))
    await wrapper.get(refresh).trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.find('[data-account-id="14"]').exists()).toBe(true)
    expect(getEnvironmentAudit).toHaveBeenCalledTimes(3)
  })

  it.each([undefined, {}, { accounts: [], collisions: [], import_batches: null, generated_at: 'now' }])(
    'fails closed for an invalid response envelope %#',
    async payload => {
      getEnvironmentAudit.mockResolvedValueOnce(payload)
      const wrapper = mountModal()
      await flushPromises()
      expect(wrapper.find('[role="alert"]').exists()).toBe(true)
      expect(wrapper.find('[data-testid="empty"]').exists()).toBe(false)
      expect(wrapper.find('[data-testid="no-collisions"]').exists()).toBe(false)
    }
  )

  it('shows a legitimate empty response separately from failure', async () => {
    getEnvironmentAudit.mockResolvedValueOnce({ ...report(), accounts: [] })
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.find('[data-testid="empty"]').exists()).toBe(true)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="no-collisions"]').exists()).toBe(false)
  })

  it.each(['resolve', 'reject'] as const)('ignores a stale %s and preserves the reopened request loading state', async outcome => {
    const old = deferred()
    const next = deferred()
    getEnvironmentAudit.mockReturnValueOnce(old.promise).mockReturnValueOnce(next.promise)
    const wrapper = mountModal()
    const oldSignal = getEnvironmentAudit.mock.calls[0][0] as AbortSignal
    await wrapper.setProps({ show: false })
    expect(oldSignal.aborted).toBe(true)
    await wrapper.setProps({ show: true })
    if (outcome === 'resolve') old.resolve(report(13))
    else old.reject(new Error('stale failure'))
    await flushPromises()
    expect(wrapper.find('[role="status"]').exists()).toBe(true)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.find('[data-account-id]').exists()).toBe(false)
    expect(wrapper.get(refresh).attributes('disabled')).toBeDefined()
    next.resolve(report(14))
    await flushPromises()
    expect(wrapper.find('[data-account-id="14"]').exists()).toBe(true)
  })

  it('does not overwrite a newer completed report with a late old response', async () => {
    const old = deferred()
    getEnvironmentAudit.mockReturnValueOnce(old.promise).mockResolvedValueOnce(report(14))
    const wrapper = mountModal()
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    old.resolve(report(13))
    await flushPromises()
    expect(wrapper.find('[data-account-id="14"]').exists()).toBe(true)
    expect(wrapper.find('[data-account-id="13"]').exists()).toBe(false)
  })

  it('cancels immediately on dialog close without waiting for the parent update', async () => {
    const pending = deferred()
    getEnvironmentAudit.mockReturnValueOnce(pending.promise)
    const wrapper = mountModal()
    const signal = getEnvironmentAudit.mock.calls[0][0] as AbortSignal
    await wrapper.get('[data-testid="close"]').trigger('click')
    expect(signal.aborted).toBe(true)
    expect(wrapper.emitted('close')).toHaveLength(1)
    pending.resolve(report())
    await flushPromises()
    expect(wrapper.find('[data-account-id]').exists()).toBe(false)
  })

  it('suppresses duplicate reads while loading and aborts on unmount', async () => {
    const pending = deferred()
    getEnvironmentAudit.mockReturnValueOnce(pending.promise)
    const wrapper = mountModal()
    await wrapper.get(refresh).trigger('click')
    await wrapper.get(refresh).trigger('click')
    expect(getEnvironmentAudit).toHaveBeenCalledTimes(1)
    const signal = getEnvironmentAudit.mock.calls[0][0] as AbortSignal
    wrapper.unmount()
    wrappers.pop()
    expect(signal.aborted).toBe(true)
    pending.reject(new Error('aborted'))
    await flushPromises()
  })
})
