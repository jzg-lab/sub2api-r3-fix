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
function mountPage() {
  return mount(QualityTestView, { global: { stubs: {
    AppLayout: { template: '<div><slot /></div>' },
    RouterLink: { template: '<a><slot /></a>' },
    Select: { props: ['modelValue', 'options', 'id'], emits: ['update:modelValue'], template: '<select :id="id" :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option v-for="item in options" :key="item.value" :value="item.value">{{ item.label }}</option></select>' }
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
})
