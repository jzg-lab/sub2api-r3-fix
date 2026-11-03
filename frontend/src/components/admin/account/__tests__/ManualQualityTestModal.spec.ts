import { loadLocaleMessages } from '@/i18n'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ManualQualityTestModal from '../ManualQualityTestModal.vue'
import { createPinia, setActivePinia } from 'pinia'
import { useAppStore } from '@/stores/app'
import type { Account } from '@/types'

const { api, models } = vi.hoisted(() => ({
  api: { templates: vi.fn(), save: vi.fn(), result: vi.fn(), run: vi.fn() }, models: vi.fn()
}))
vi.mock('@/api/admin/manualQuality', () => ({ manualQualityAPI: api }))
vi.mock('@/api/admin/accounts', () => ({ getAvailableModels: models, default: {} }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))
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
beforeEach(async () => {
  await loadLocaleMessages('en')
  vi.resetAllMocks()
  setActivePinia(createPinia())
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
    expect(api.run).toHaveBeenCalledWith(7, { kind: 'candy', model: 'gpt-6.1-sol', prompt: 'temporary question', reference_answer: '' }, expect.any(AbortSignal))
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
  it('prevents duplicate clicks, survives closing and notifies after completion', async () => {
    let finish!: (value: unknown) => void
    api.run.mockReturnValue(new Promise(resolve => { finish = resolve }))
    const wrapper = mountModal(); await flushPromises()
    await button(wrapper, 'run').trigger('click')
    expect(button(wrapper, 'running').attributes('disabled')).toBeDefined()
    expect(api.run).toHaveBeenCalledTimes(1)
    const signal = api.run.mock.calls[0][2] as AbortSignal
    wrapper.unmount()
    expect(signal.aborted).toBe(false)
    const reopened = mountModal(); await flushPromises()
    expect(button(reopened, 'running').attributes('disabled')).toBeDefined()
    finish({ result, saved: true }); await flushPromises()
    expect(reopened.text()).toContain('upstream-m')
    expect(useAppStore().toasts).toHaveLength(1)
    expect(useAppStore().toasts[0].duration).toBeUndefined()
    reopened.unmount()
  })
  it('shows a newer server result instead of an older result cached before reopening', async () => {
    const first = mountModal(); await flushPromises()
    await button(first, 'run').trigger('click'); await flushPromises()
    first.unmount()
    api.result.mockResolvedValue({ ...result, answer: 'newer server answer', started_at: '2026-11-03T01:00:00Z' })
    const reopened = mountModal(); await flushPromises()
    expect(reopened.text()).toContain('newer server answer')
    reopened.unmount()
  })
  it('retains an unsaved local answer when reopening finds an older saved result', async () => {
    api.run.mockResolvedValue({ result, saved: false, error: 'storage failed' })
    const first = mountModal(); await flushPromises()
    await button(first, 'run').trigger('click'); await flushPromises()
    first.unmount()
    api.result.mockResolvedValue({ ...result, answer: 'older saved answer', started_at: '2026-11-02T00:00:00Z' })
    const reopened = mountModal(); await flushPromises()
    expect(reopened.text()).toContain('storage failed')
    expect(reopened.text()).not.toContain('older saved answer')
    reopened.unmount()
  })
  it('loads the result scoped to the selected model and kind', async () => {
    api.result.mockResolvedValue(result)
    const wrapper = mountModal(); await flushPromises()
    expect(api.result).toHaveBeenLastCalledWith(7, 'gpt-6.1-sol', 'candy')
    await wrapper.get('#quality-kind').setValue('pelican'); await flushPromises()
    expect(api.result).toHaveBeenLastCalledWith(7, 'gpt-6.1-sol', 'pelican')
    expect((wrapper.get('#quality-prompt').element as HTMLTextAreaElement).value).toBe('pelican SVG')
    wrapper.unmount()
  })
})
