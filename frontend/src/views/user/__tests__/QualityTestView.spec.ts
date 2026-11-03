import { loadLocaleMessages } from '@/i18n'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import QualityTestView from '../QualityTestView.vue'
import { useManualQualityStore } from '@/stores/manualQuality'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'
import type { User } from '@/types'

const api = vi.hoisted(() => ({ templates: vi.fn(), run: vi.fn(), keys: vi.fn() }))
vi.mock('@/api/qualityTest', () => ({ qualityTestTemplates: api.templates, runUserQualityTest: api.run }))
vi.mock('@/api/keys', () => ({ keysAPI: { list: api.keys }, default: {} }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))
const templates = [
  { kind: 'candy', prompt: 'default candy', reference_answer: '21' },
  { kind: 'pelican', prompt: 'draw SVG pelican', reference_answer: '' }
]
const result = { ...templates[0], model: 'gpt-6.1-sol', answer: '21', status: 'completed', started_at: '2026-11-03T00:00:00Z', duration_ms: 1, run_id: 'run', account_id: 0, upstream_model: '' }
function mountPage(realKeyControl = false) {
  return mount(QualityTestView, { global: { stubs: {
    AppLayout: { template: '<div><slot /></div>' },
    RouterLink: { template: '<a><slot /></a>' },
    Teleport: true,
    Select: realKeyControl ? false : { props: ['modelValue', 'options', 'id'], emits: ['update:modelValue'], template: '<select :id="id" :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option v-for="item in options" :key="item.value" :value="item.value">{{ item.label }}</option></select>' }
  } } })
}
const button = (wrapper: ReturnType<typeof mountPage>, key: string) => wrapper.findAll('button').find(value => value.text() === key)!
beforeEach(async () => {
  await loadLocaleMessages('en')
  vi.resetAllMocks(); localStorage.clear(); setActivePinia(createPinia())
  const auth = useAuthStore()
  auth.user = { id: 7, role: 'user' } as User; auth.token = 'jwt'
  api.templates.mockResolvedValue(templates)
  api.keys.mockResolvedValue({ items: [{ id: 2, name: 'My key', key: 'sk-own', user_id: 7, status: 'active', group: { name: 'My plan', rate_multiplier: 2 } }], total: 1 })
  api.run.mockResolvedValue({ result })
  await flushPromises()
})

describe('QualityTestView', () => {
  it('uses the key creation group colors in the selection and searchable options without exposing keys', async () => {
    api.keys.mockResolvedValue({ items: [
      { id: 2, name: 'My key', key: 'sk-own', group: { name: 'OpenAI plan', platform: 'openai', subscription_type: 'standard', rate_multiplier: 2 } },
      { id: 3, name: 'Work key', key: 'sk-work', group: { name: 'Claude plan', platform: 'anthropic', subscription_type: 'subscription', rate_multiplier: 1.5 } },
      { id: 4, name: 'Ungrouped key', key: 'sk-ungrouped' }
    ], total: 3 })
    const wrapper = mountPage(true); await flushPromises()
    const trigger = () => wrapper.get('#self-quality-key')
    expect(trigger().text()).toContain('My key')
    expect(trigger().text()).toContain('OpenAI plan')
    expect(trigger().find('.bg-green-50').exists()).toBe(true)
    await trigger().trigger('click')
    expect(wrapper.findAll('[role="option"]')).toHaveLength(3)
    expect(wrapper.get('[role="option"] .bg-orange-100').text()).toContain('Claude plan')
    expect(wrapper.html()).not.toContain('sk-own')
    expect(wrapper.html()).not.toContain('sk-work')
    await wrapper.get('.select-search-input').setValue('Claude')
    expect(wrapper.findAll('[role="option"]')).toHaveLength(1)
    await wrapper.get('[role="option"]').trigger('click')
    expect(trigger().find('.bg-orange-100').exists()).toBe(true)
    expect(trigger().text()).toContain('Work key')
    expect(trigger().text()).toContain('1.5x')
    await button(wrapper, 'admin.accounts.manualQuality.run').trigger('click'); await flushPromises()
    expect(api.run).toHaveBeenCalledWith('sk-work', expect.any(Object), expect.any(AbortSignal))
    await trigger().trigger('click')
    await wrapper.get('.select-search-input').setValue('Ungrouped')
    await wrapper.get('[role="option"]').trigger('click')
    expect(trigger().text()).toBe('Ungrouped key')
    expect(trigger().find('.bg-green-50, .bg-orange-100').exists()).toBe(false)
    wrapper.unmount()
  })

  it('defaults to 6.1 sol and saves personal prompts without sending a request or persisting keys', async () => {
    const wrapper = mountPage(); await flushPromises()
    expect((wrapper.get('#self-quality-model').element as HTMLInputElement).value).toBe('gpt-6.1-sol')
    expect(wrapper.text()).toContain('My plan')
    await wrapper.get('#self-quality-prompt').setValue('my own question')
    expect((wrapper.get('#self-quality-reference').element as HTMLInputElement).value).toBe('')
    await button(wrapper, 'qualityTest.save').trigger('click')
    expect(localStorage.getItem('quality-test-templates:7')).toContain('my own question')
    expect(localStorage.getItem('quality-test-templates:7')).not.toContain('sk-own')
    expect(api.run).not.toHaveBeenCalled()
    wrapper.unmount()
    const reopened = mountPage(); await flushPromises()
    expect((reopened.get('#self-quality-prompt').element as HTMLInputElement).value).toBe('my own question')
    reopened.unmount()
  })

  it('continues after navigation, prevents duplicate starts and retains the result with a completion notice', async () => {
    let finish!: (value: unknown) => void
    api.run.mockReturnValue(new Promise(resolve => { finish = resolve }))
    const wrapper = mountPage(); await flushPromises()
    await button(wrapper, 'admin.accounts.manualQuality.run').trigger('click')
    const signal = api.run.mock.calls[0][2] as AbortSignal
    expect(api.run).toHaveBeenCalledWith('sk-own', expect.objectContaining({ model: 'gpt-6.1-sol', prompt: 'default candy' }), expect.any(AbortSignal))
    wrapper.unmount()
    const reopened = mountPage(); await flushPromises()
    expect(signal.aborted).toBe(false)
    expect(button(reopened, 'admin.accounts.manualQuality.running').attributes('disabled')).toBeDefined()
    expect(api.run).toHaveBeenCalledTimes(1)
    finish({ result }); await flushPromises()
    expect(reopened.get('#quality-question-panel').attributes('style')).toContain('display: none')
    expect(reopened.get('#quality-result-panel').attributes('style') || '').not.toContain('display: none')
    expect(reopened.text()).toContain('21')
    await button(reopened, 'qualityTest.backToQuestion').trigger('click')
    expect(reopened.get('#quality-question-panel').attributes('style') || '').not.toContain('display: none')
    expect((reopened.get('#self-quality-prompt').element as HTMLTextAreaElement).value).toBe('default candy')
    await button(reopened, 'qualityTest.result').trigger('click')
    expect(reopened.get('#quality-result-panel').attributes('style') || '').not.toContain('display: none')
    expect(useAppStore().toasts).toHaveLength(1)
    expect(useAppStore().toasts[0].type).toBe('success')
    reopened.unmount()
    useAuthStore().user = null; await flushPromises()
    expect(useAppStore().toasts).toHaveLength(0)
  })

  it('clears tasks and suppresses late results/notices when the user logs out', async () => {
    let finish!: (value: unknown) => void
    api.run.mockReturnValue(new Promise(resolve => { finish = resolve }))
    const wrapper = mountPage(); await flushPromises()
    await button(wrapper, 'admin.accounts.manualQuality.run').trigger('click')
    const signal = api.run.mock.calls[0][2] as AbortSignal
    wrapper.unmount()
    useAuthStore().user = null
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(useManualQualityStore().tasks).toEqual({})
    finish({ result }); await flushPromises()
    expect(useManualQualityStore().tasks).toEqual({})
    expect(useAppStore().toasts).toHaveLength(0)
  })

  it('removes animation playback while the question panel is visible', async () => {
    api.run.mockResolvedValue({ result: { ...result, kind: 'pelican', answer: '<svg><circle r="10"/></svg>' } })
    const wrapper = mountPage(); await flushPromises()
    await wrapper.findAll('button').find(value => value.text().includes('admin.accounts.manualQuality.pelican'))!.trigger('click')
    await button(wrapper, 'admin.accounts.manualQuality.run').trigger('click'); await flushPromises()
    expect(wrapper.find('iframe').exists()).toBe(true)
    await button(wrapper, 'qualityTest.backToQuestion').trigger('click')
    expect(wrapper.find('iframe').exists()).toBe(false)
    await button(wrapper, 'qualityTest.result').trigger('click')
    expect(wrapper.find('iframe').exists()).toBe(true)
    wrapper.unmount()
  })
})
