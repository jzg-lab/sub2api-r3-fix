import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import type { Account } from '@/types'
import AccountConcurrencyModal from '../AccountConcurrencyModal.vue'
import AccountCapacityCell from '../AccountCapacityCell.vue'

const { update, showSuccess } = vi.hoisted(() => ({ update: vi.fn(), showSuccess: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { update } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const account = { id: 7, name: 'fixture', concurrency: 3, current_concurrency: 2 } as Account
const mountModal = () => mount(AccountConcurrencyModal, {
  props: { account },
  global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } }
})

describe('account concurrency controls', () => {
  beforeEach(() => vi.clearAllMocks())

  it('opens from the capacity column without changing row selection', async () => {
    const wrapper = mount(AccountCapacityCell, { props: { account } })
    await wrapper.get('button').trigger('click')
    expect(wrapper.emitted('edit-concurrency')).toHaveLength(1)
    expect(wrapper.text()).toContain('3')
  })

  it('saves only the selected limit and emits the server result', async () => {
    const wrapper = mountModal()
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('3')
    const result = { ...account, concurrency: 1 }
    update.mockResolvedValueOnce(result)
    await wrapper.get('input').setValue('1')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(update).toHaveBeenCalledTimes(1)
    expect(update).toHaveBeenCalledWith(7, { concurrency: 1 })
    expect(wrapper.emitted('updated')?.[0]).toEqual([result])
    expect(wrapper.emitted('close')).toHaveLength(1)
  })

  it.each(['', '0', '-1', '1.5', '10001'])('rejects invalid limit %s', async value => {
    const wrapper = mountModal()
    await wrapper.get('input').setValue(value)
    await wrapper.get('form').trigger('submit')
    expect(update).not.toHaveBeenCalled()
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeDefined()
  })

  it('keeps the draft after failure and permits retry', async () => {
    const wrapper = mountModal()
    update.mockRejectedValueOnce(new Error('offline'))
    await wrapper.get('input').setValue('4')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.emitted('updated')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('4')
    update.mockResolvedValueOnce({ ...account, concurrency: 4 })
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.emitted('updated')).toHaveLength(1)
  })

  it('blocks duplicate submissions and cancel while saving', async () => {
    let finish!: (value: Account) => void
    update.mockReturnValueOnce(new Promise<Account>(resolve => { finish = resolve }))
    const wrapper = mountModal()
    await wrapper.get('form').trigger('submit')
    await wrapper.get('form').trigger('submit')
    await wrapper.get('button[type="button"]').trigger('click')
    expect(update).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('close')).toBeUndefined()
    finish(account)
    await flushPromises()
    expect(wrapper.emitted('close')).toHaveLength(1)
  })

  it('does not overwrite a draft on polling but resets for another account', async () => {
    const wrapper = mountModal()
    await wrapper.get('input').setValue('9')
    await wrapper.setProps({ account: { ...account, current_concurrency: 1 } })
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('9')
    await wrapper.setProps({ account: { ...account, id: 8, concurrency: 5 } })
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('5')
  })
})
