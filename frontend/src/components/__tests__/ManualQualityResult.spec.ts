import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import ManualQualityResult from '../common/ManualQualityResult.vue'
import type { QualityResult } from '@/api/admin/manualQuality'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const result: QualityResult = {
  kind: 'pelican', model: 'gpt-6.1-sol', prompt: 'draw an animation', reference_answer: '',
  account_id: 0, run_id: 'one', started_at: '2026-11-03T00:00:00Z', duration_ms: 1,
  status: 'completed', upstream_model: '', answer: '<svg><circle r="10"/></svg><script>requestAnimationFrame(() => {})</script>'
}

describe('ManualQualityResult', () => {
  it('isolates playback, destroys it on stop and recreates it on replay or a new result', async () => {
    const wrapper = mount(ManualQualityResult, { props: { result } })
    const frame = wrapper.get('iframe')
    expect(wrapper.get('.aspect-video').classes()).toContain('max-w-xl')
    expect(frame.attributes('sandbox')).toBe('allow-scripts')
    expect(frame.attributes('referrerpolicy')).toBe('no-referrer')
    expect(frame.attributes('srcdoc')).toContain('requestAnimationFrame')
    expect(wrapper.find('script').exists()).toBe(false)
    const original = frame.element
    const button = (name: string) => wrapper.findAll('button').find(value => value.text().endsWith(name))!
    await button('replay').trigger('click')
    expect(wrapper.get('iframe').element).not.toBe(original)
    await button('stopPreview').trigger('click')
    expect(wrapper.find('iframe').exists()).toBe(false)
    expect(wrapper.text()).toContain('previewStopped')
    await button('replay').trigger('click')
    expect(wrapper.find('iframe').exists()).toBe(true)
    await button('stopPreview').trigger('click')
    await wrapper.setProps({ result: { ...result, run_id: 'two', answer: '<svg><rect width="10" height="10"/></svg>' } })
    expect(wrapper.find('iframe').exists()).toBe(true)
    wrapper.unmount()
  })
  it('keeps incomplete output as text and never executes it', () => {
    const wrapper = mount(ManualQualityResult, { props: { result: { ...result, status: 'failed' } } })
    expect(wrapper.find('iframe').exists()).toBe(false)
    expect(wrapper.get('pre').text()).toBe(result.answer)
    wrapper.unmount()
  })
})
