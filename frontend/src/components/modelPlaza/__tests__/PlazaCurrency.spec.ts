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

describe('model plaza currency explanations', () => {
  it.each(['zh', 'en'] as const)('distinguishes account charges, CNY conversion and official USD in %s', async locale => {
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
    const buttons = () => wrapper.find('.plaza-toolbar-actions .plaza-segmented').findAll('button')
    const notice = () => wrapper.find('.plaza-notice').text()
    expect(buttons()[1].text()).toBe(labels.usd)
    expect(notice()).toBe(i18n.global.t('modelPlaza.catalog.conversion', { rate: 10 }))
    expect(notice()).toContain('$10')
    expect(wrapper.find('.plaza-notice').element.compareDocumentPosition(wrapper.find('.plaza-model-grid').element) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('¥0.15')

    await buttons()[1].trigger('click')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$1.5')
    expect(wrapper.find('.plaza-catalog-meta').text()).toContain(labels.usd)
    expect(wrapper.find('.plaza-pricing-type').text()).toContain(labels.usd)
    await wrapper.find('.plaza-original-toggle').trigger('click')
    expect(buttons()[1].text()).toBe(labels.officialUsd)
    expect(wrapper.find('.plaza-pricing-type').text()).toContain(labels.officialUsd)
    expect(notice()).toBe(i18n.global.t('modelPlaza.catalog.referenceHint', { fx: '6.70' }))
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$5')
    await wrapper.find('.plaza-card-price-detail').trigger('click')
    expect(wrapper.find('.test-dialog').text()).toContain(labels.officialUsd)
    await wrapper.find('.plaza-original-toggle').trigger('click')
    expect(wrapper.find('.test-dialog').text()).toContain(labels.usd)
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$1.5')

    await wrapper.setProps({ response: { ...data, balance_recharge_multiplier: 20 } })
    expect(notice()).toContain('$20')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$1.5')
    await buttons()[0].trigger('click')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('¥0.075')
    await wrapper.setProps({ response: { ...data, balance_recharge_multiplier: undefined } })
    expect(notice()).toBe(labels.conversionUnavailable)
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('—')
    await buttons()[1].trigger('click')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$1.5')
    wrapper.unmount()
  })
})
