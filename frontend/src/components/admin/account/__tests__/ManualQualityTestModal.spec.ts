import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ManualQualityTestModal from '../ManualQualityTestModal.vue'
import type { Account } from '@/types'

const { api, models } = vi.hoisted(() => ({
  api: { templates: vi.fn(), save: vi.fn(), result: vi.fn(), run: vi.fn() }, models: vi.fn()
}))
vi.mock('@/api/admin/manualQuality', () => ({ manualQualityAPI: api }))
vi.mock('@/api/admin/accounts', () => ({ getAvailableModels: models }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const templates = [
  { kind: 'candy', prompt: 'candy default', reference_answer: '21' },
  { kind: 'pelican', prompt: 'pelican SVG', reference_answer: '' }
]
const result = { ...templates[0], account_id: 7, model: 'm', upstream_model: 'upstream-m', status: 'completed', answer: '21', started_at: '2026-11-03T00:00:00Z', duration_ms: 42 }
const mountModal = () => mount(ManualQualityTestModal, {
  props: { account: { id: 7, name: 'upstream' } as Account },
  global: { stubs: {
    BaseDialog: { template: '<div><slot /></div>' },
    Select: { props: ['modelValue', 'options', 'id'], emits: ['update:modelValue'], template: '<select :id="id" :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option v-for="item in options" :key="item.value" :value="item.value">{{ item.label }}</option></select>' }
  } }
})
function button(wrapper: ReturnType<typeof mountModal>, name: string) { return wrapper.findAll('button').find(value => value.text() === `admin.accounts.manualQuality.${name}`)! }
beforeEach(() => {
  vi.resetAllMocks()
  api.templates.mockResolvedValue({ templates, defaults: templates })
  api.result.mockResolvedValue(null)
  api.save.mockImplementation(async value => value)
  api.run.mockResolvedValue({ result, saved: true })
  models.mockResolvedValue([{ id: 'm', display_name: 'Model' }])
})
describe('ManualQualityTestModal', () => {
  it('opens and saves without running; editing clears the reference', async () => {
    const wrapper = mountModal(); await flushPromises()
    expect(api.run).not.toHaveBeenCalled()
    expect((wrapper.get('#quality-reference').element as HTMLInputElement).value).toBe('21')
    await wrapper.get('#quality-prompt').setValue('changed question')
    expect((wrapper.get('#quality-reference').element as HTMLInputElement).value).toBe('')
    await button(wrapper, 'save').trigger('click'); await flushPromises()
    expect(api.save).toHaveBeenCalledWith({ kind: 'candy', prompt: 'changed question', reference_answer: '' })
    expect(api.run).not.toHaveBeenCalled()
    await button(wrapper, 'restore').trigger('click')
    expect((wrapper.get('#quality-prompt').element as HTMLTextAreaElement).value).toBe('candy default')
    wrapper.unmount()
  })
  it('tests a draft without saving it and emits only saved results', async () => {
    const wrapper = mountModal(); await flushPromises()
    await wrapper.get('#quality-prompt').setValue('temporary question')
    await button(wrapper, 'run').trigger('click'); await flushPromises()
    expect(api.save).not.toHaveBeenCalled()
    expect(api.run).toHaveBeenCalledWith(7, { kind: 'candy', model: 'm', prompt: 'temporary question', reference_answer: '' }, expect.any(AbortSignal))
    expect(wrapper.emitted('result')?.[0]).toEqual([result])
    expect(wrapper.text()).toContain('upstream-m')
    wrapper.unmount()
  })
  it('keeps the answer visible when server persistence fails', async () => {
    api.run.mockResolvedValue({ result, saved: false, error: 'storage failed' })
    const wrapper = mountModal(); await flushPromises()
    await button(wrapper, 'run').trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('storage failed')
    expect(wrapper.text()).toContain('21')
    expect(wrapper.emitted('result')).toBeUndefined()
    wrapper.unmount()
  })
  it('prevents duplicate clicks and aborts an active request on unmount', async () => {
    api.run.mockReturnValue(new Promise(() => {}))
    const wrapper = mountModal(); await flushPromises()
    await button(wrapper, 'run').trigger('click')
    expect(button(wrapper, 'running').attributes('disabled')).toBeDefined()
    expect(api.run).toHaveBeenCalledTimes(1)
    const signal = api.run.mock.calls[0][2] as AbortSignal
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
  })
  it('loads the result scoped to the selected model and kind', async () => {
    api.result.mockResolvedValue(result)
    const wrapper = mountModal(); await flushPromises()
    expect(api.result).toHaveBeenLastCalledWith(7, 'm', 'candy')
    await wrapper.get('#quality-kind').setValue('pelican'); await flushPromises()
    expect(api.result).toHaveBeenLastCalledWith(7, 'm', 'pelican')
    expect((wrapper.get('#quality-prompt').element as HTMLTextAreaElement).value).toBe('pelican SVG')
    wrapper.unmount()
  })
})
