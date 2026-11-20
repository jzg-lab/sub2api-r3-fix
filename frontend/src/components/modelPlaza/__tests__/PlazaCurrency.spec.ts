import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh/dashboard'
import en from '@/i18n/locales/en/dashboard'
import ModelPlazaContent from '../ModelPlazaContent.vue'
import { plazaFixture } from './plazaFixture'

// The app uses a runtime-only build; this test needs the compiler to render source messages.
vi.mock('vue-i18n', async () => {
  const { createRequire } = await import('node:module')
  return vi.importActual(createRequire(import.meta.url).resolve('vue-i18n/dist/vue-i18n.esm-bundler.js'))
})
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))

describe('model plaza price display', () => {
  it.each(['zh', 'en'] as const)('shows both prices in account USD without recharge conversion in %s', async locale => {
    const i18n = createI18n({ legacy: false, locale, messages: { zh, en }, missingWarn: false, fallbackWarn: false })
    const labels = (locale === 'zh' ? zh : en).modelPlaza.catalog
    const data = plazaFixture()
    const wrapper = mount(ModelPlazaContent, {
      props: { response: data, loading: false },
      global: {
        plugins: [i18n],
        stubs: { BaseDialog: { props: ['show'], template: '<section v-if="show" class="test-dialog"><slot /></section>' } },
      },
    })
    const [before, after] = wrapper.findAll('.plaza-price-switch button')
    expect(before.text()).toBe(labels.beforeMultiplier)
    expect(after.text()).toBe(labels.afterMultiplier)
    expect(after.attributes('aria-pressed')).toBe('true')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$1.5')
    expect(wrapper.find('.plaza-catalog-meta').text()).toContain(labels.usd)
    await wrapper.setProps({ response: { ...data, balance_recharge_multiplier: 20 } })
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$1.5')

    await before.trigger('click')
    expect(wrapper.find('.plaza-pricing-type').text()).toContain(labels.beforeMultiplier)
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$3')
    await wrapper.find('.plaza-card-price-detail').trigger('click')
    expect(wrapper.find('.test-dialog').text()).toContain(labels.beforeMultiplier)
    await wrapper.setProps({ response: { ...data, balance_recharge_multiplier: undefined } })
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$3')
    await after.trigger('click')
    expect(wrapper.find('.test-dialog').text()).toContain(labels.afterMultiplier)
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$1.5')
    expect(wrapper.text()).not.toMatch(/¥|CNY/)
    wrapper.unmount()
  })
})
