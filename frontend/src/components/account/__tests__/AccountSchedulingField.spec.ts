import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountSchedulingField from '../AccountSchedulingField.vue'
import type { Account } from '@/types'

const { bulkUpdate, showError, showSuccess } = vi.hoisted(() => ({
  bulkUpdate: vi.fn(), showError: vi.fn(), showSuccess: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { bulkUpdate } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const account = { id: 1, concurrency: 7, priority: 8 } as Account
const mountField = (field: 'concurrency' | 'priority' = 'concurrency') => mount(AccountSchedulingField, {
  props: { account, field }, global: { stubs: { Icon: true } }
})

describe('AccountSchedulingField', () => {
  beforeEach(() => {
    bulkUpdate.mockReset().mockResolvedValue({ success: 1, failed: 0, results: [] })
    showError.mockReset()
    showSuccess.mockReset()
  })

  it.each(['concurrency', 'priority'] as const)('saves only the selected %s field', async field => {
    const wrapper = mountField(field)
    await wrapper.get('button').trigger('click')
    await wrapper.get('input').setValue(field === 'priority' ? 0 : 75)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    const value = field === 'priority' ? 0 : 75
    expect(bulkUpdate).toHaveBeenCalledWith([1], { [field]: value })
    expect(wrapper.emitted('updated')?.[0]?.[0]).toEqual({ ...account, [field]: value })
    expect(wrapper.find('input').exists()).toBe(false)
    expect(wrapper.emitted('editing')).toEqual([[true], [false]])
    wrapper.unmount()
  })

  it('does not let refreshed row props overwrite an active draft; escape cancels', async () => {
    const wrapper = mountField()
    await wrapper.get('button').trigger('click')
    await wrapper.get('input').setValue(75)
    await wrapper.setProps({ account: { ...account, concurrency: 9 } })
    expect(wrapper.get<HTMLInputElement>('input').element.value).toBe('75')
    await wrapper.get('input').trigger('keydown', { key: 'Escape' })
    expect(bulkUpdate).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('9')
    wrapper.unmount()
  })

  it.each(['', '0', '-1', '1.5'])('rejects invalid concurrency %s', async value => {
    const wrapper = mountField()
    await wrapper.get('button').trigger('click')
    await wrapper.get('input').setValue(value)
    await wrapper.get('form').trigger('submit')
    expect(bulkUpdate).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.accounts.schedulingInvalid')
    wrapper.unmount()
  })

  it('preserves the draft after a partial failure', async () => {
    bulkUpdate.mockResolvedValueOnce({ success: 0, failed: 1, results: [{ success: false, error: 'account missing' }] })
    const wrapper = mountField()
    await wrapper.get('button').trigger('click')
    await wrapper.get('input').setValue(75)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.get<HTMLInputElement>('input').element.value).toBe('75')
    expect(wrapper.emitted('updated')).toBeUndefined()
    expect(showError).toHaveBeenCalledWith('account missing')
    wrapper.unmount()
  })

  it('prevents duplicate saves and cancellation while a request is pending', async () => {
    let resolve!: (value: unknown) => void
    bulkUpdate.mockReturnValueOnce(new Promise(done => { resolve = done }))
    const wrapper = mountField()
    await wrapper.get('button').trigger('click')
    await wrapper.get('input').setValue(75)
    await wrapper.get('form').trigger('submit')
    await wrapper.get('form').trigger('submit')
    await wrapper.get('input').trigger('keydown', { key: 'Escape' })
    expect(bulkUpdate).toHaveBeenCalledTimes(1)
    expect(wrapper.get<HTMLInputElement>('input').element.disabled).toBe(true)
    resolve({ success: 1, failed: 0, results: [] })
    await flushPromises()
    expect(wrapper.find('input').exists()).toBe(false)
    wrapper.unmount()
  })
})
