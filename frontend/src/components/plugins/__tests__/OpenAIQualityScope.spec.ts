import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import OpenAIQualityScope from '../OpenAIQualityScope.vue'

const { getSettings, setSettings, getAll } = vi.hoisted(() => ({
  getSettings: vi.fn(), setSettings: vi.fn(), getAll: vi.fn()
}))
vi.mock('@/api/admin/openaiOperations', () => ({ openaiOperationsAPI: { getSettings, setSettings } }))
vi.mock('@/api/admin/groups', () => ({ groupsAPI: { getAll } }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('quality scheduling scope', () => {
  it('saves selected groups and explicit empty scope, preserves other settings, and reports errors', async () => {
    const settings = { quality_protected_group_ids: null, recovery: { enabled: true } }
    getSettings.mockResolvedValue(settings)
    getAll.mockResolvedValue([{ id: 7, name: 'protected' }, { id: 8, name: 'ordinary' }])
    setSettings.mockResolvedValue(settings)
    const wrapper = mount(OpenAIQualityScope, { global: { stubs: { Icon: true } } })
    await flushPromises()
    expect(wrapper.get('select').element.value).toBe('all')
    await wrapper.get('select').setValue('selected')
    await wrapper.get('input[value="7"]').setValue(true)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(setSettings).toHaveBeenLastCalledWith({ ...settings, quality_protected_group_ids: [7] })
    await wrapper.get('input[value="7"]').setValue(false)
    setSettings.mockRejectedValueOnce({ message: 'save rejected' })
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('save rejected')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(setSettings).toHaveBeenLastCalledWith({ ...settings, quality_protected_group_ids: [] })
    await wrapper.get('select').setValue('all')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(setSettings).toHaveBeenLastCalledWith(settings)
    wrapper.unmount()
  })
})
