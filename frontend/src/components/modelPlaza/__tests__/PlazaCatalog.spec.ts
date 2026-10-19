import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import ModelPlazaContent from '../ModelPlazaContent.vue'
import PlazaPriceGrid from '../PlazaPriceGrid.vue'
import PlazaStatus from '../PlazaStatus.vue'
import { catalogue, displayPrice, formatPrice, monitorKey } from '../plazaPricing'
import { plazaFixture } from './plazaFixture'
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))

describe('plaza billing display', () => {
  it('uses the server billing rate and live recharge conversion, preserving zero and missing prices', () => {
    const data = plazaFixture()
    const offer = catalogue(data.groups)[0].offers[0]
    expect(displayPrice(3e-6, offer, 'CNY', 10)).toBeCloseTo(1.5e-7)
    expect(formatPrice(displayPrice(3e-6, offer, 'CNY', 10), 1e6, 'CNY')).toBe('¥0.15')
    expect(formatPrice(displayPrice(3e-6, offer, 'USD', 10), 1e6)).toBe('$0.0223881')
    expect(displayPrice(3e-6, offer, 'USD', 10)! * 6.7).toBeCloseTo(displayPrice(3e-6, offer, 'CNY', 10)!, 14)
    expect(displayPrice(3e-6, offer, 'USD', 20)).toBeCloseTo(7.5e-8 / 6.7, 14)
    expect(displayPrice(3e-6, offer, 'CNY', 20)).toBeCloseTo(7.5e-8)
    expect(displayPrice(0, offer, 'CNY', 10)).toBe(0)
    expect(displayPrice(null, offer, 'CNY', 10)).toBeNull()
    for (const conversion of [undefined, 0, -1, NaN, Infinity]) {
      expect(displayPrice(3e-6, offer, 'CNY', conversion)).toBeNull()
      expect(displayPrice(3e-6, offer, 'USD', conversion)).toBeNull()
    }
    offer.model.current_rate_multiplier = 0
    expect(displayPrice(3e-6, offer, 'USD', 10)).toBe(0)
    delete offer.model.current_rate_multiplier
    expect(displayPrice(3e-6, offer, 'USD', 10)).toBeNull()
  })
  it('replaces the main price with official prices in the selected real currency', async () => {
    const offer = catalogue(plazaFixture().groups)[0].offers[0]
    const wrapper = mount(PlazaPriceGrid, { props: { offer, currency: 'CNY', conversion: 10, showOriginal: false } })
    expect(wrapper.find('strong').text()).toBe('¥0.15')
    await wrapper.setProps({ showOriginal: true })
    expect(wrapper.find('strong').text()).toBe('¥33.5')
    expect(wrapper.text()).not.toContain('¥0.15')
    expect(wrapper.find('.plaza-original').exists()).toBe(false)
    await wrapper.setProps({ currency: 'USD' })
    expect(wrapper.find('strong').text()).toBe('$5')
    await wrapper.setProps({ conversion: 20 })
    expect(wrapper.find('strong').text()).toBe('$5')
    await wrapper.setProps({ showOriginal: false })
    expect(wrapper.find('strong').text()).toBe('$0.011194')
    wrapper.unmount()
  })
  it('keeps official prices independent of billing rates and preserves missing and zero values', () => {
    const offer = catalogue(plazaFixture().groups)[0].offers[0]
    delete offer.model.current_rate_multiplier
    expect(displayPrice(5, offer, 'CNY', undefined, true)).toBe(33.5)
    expect(displayPrice(5, offer, 'USD', undefined, true)).toBe(5)
    expect(displayPrice(0, offer, 'CNY', 10, true)).toBe(0)
    expect(displayPrice(null, offer, 'CNY', 10, true)).toBeNull()
    expect(displayPrice(NaN, offer, 'USD', 10, true)).toBeNull()
  })
  it('does not substitute a sale price when the official price is missing', () => {
    const offer = catalogue(plazaFixture().groups)[0].offers[0]
    offer.model.official_pricing = null
    const wrapper = mount(PlazaPriceGrid, { props: { offer, currency: 'CNY', conversion: 10, showOriginal: true } })
    expect(wrapper.findAll('strong').map(node => node.text())).toEqual(['—', '—', '—', '—'])
    wrapper.unmount()
  })
  it('converts per-request charges without a token scale or misleading official token prices', async () => {
    const offer = catalogue(plazaFixture().groups)[0].offers[0]
    offer.model.pricing!.billing_mode = 'per_request'
    offer.model.pricing!.per_request_price = 2
    const wrapper = mount(PlazaPriceGrid, { props: { offer, currency: 'CNY', conversion: 10, showOriginal: false } })
    expect(wrapper.find('strong').text()).toBe('¥0.1')
    await wrapper.setProps({ currency: 'USD' })
    expect(wrapper.find('strong').text()).toBe('$0.0149254')
    await wrapper.setProps({ showOriginal: true })
    expect(wrapper.find('strong').text()).toBe('—')
    wrapper.unmount()
  })
  it('keeps same-name models on different platforms isolated', () => {
    const data = plazaFixture()
    data.groups[0].models.push({ ...data.groups[0].models[0], platform: 'grok' })
    expect(catalogue(data.groups)).toHaveLength(2)
    expect(monitorKey('openai', 1, 'same')).not.toBe(monitorKey('grok', 1, 'same'))
    expect(monitorKey('openai', 1, 'same')).not.toBe(monitorKey('openai', 2, 'same'))
  })
})

describe('plaza interactions', () => {
  it('switches tier details and sorting to the same official price source', async () => {
    const data = plazaFixture()
    const model = data.groups[0].models[0]
    const official = model.official_pricing!
    official.intervals = [
      { ...official, min_tokens: 0, max_tokens: 200000, tier_label: 'Official base', input_price: 6e-6, cache_write_1h_price: 2e-6, per_request_price: null },
      { ...official, min_tokens: 200000, max_tokens: null, tier_label: 'Official long', input_price: 12e-6, cache_write_1h_price: 4e-6, per_request_price: null }
    ]
    const other = structuredClone(model)
    other.name = 'gpt-other'
    other.pricing!.input_price = 8e-6
    other.official_pricing!.intervals![0].input_price = 2e-6
    data.groups[0].models.push(other)
    const wrapper = mount(ModelPlazaContent, { props: { response: data, loading: false }, global: { stubs: { BaseDialog: { props: ['show'], template: '<section v-if="show" class="test-dialog"><slot /></section>' } } } })
    await wrapper.find('.plaza-toolbar select').setValue('price')
    expect(wrapper.find('.plaza-model-heading h2').text()).toBe('gpt-5')
    await wrapper.find('.plaza-original-toggle').trigger('click')
    expect(wrapper.find('.plaza-model-heading h2').text()).toBe('gpt-other')
    await wrapper.findAll('.plaza-card-price-detail')[1].trigger('click')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('¥40.2')
    const rows = wrapper.findAll('.test-dialog tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('Official base')
    expect(rows[0].text()).toContain('¥13.4')
    expect(rows[1].text()).toContain('¥80.4')
    expect(rows[1].text()).toContain('¥26.8')
    await wrapper.findAll('.plaza-segmented button')[1].trigger('click')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$6')
    expect(wrapper.findAll('.test-dialog tbody tr')[1].text()).toContain('$12')
    wrapper.unmount()
  })
  it('switches currency, original prices, group and search; refresh retains controls and updates detail', async () => {
    const data = plazaFixture()
    const second = structuredClone(data.groups[0])
    second.id = 2; second.name = 'VIP'; second.models[0].current_rate_multiplier = 0.2
    data.groups.push(second)
    const wrapper = mount(ModelPlazaContent, { props: { response: data, loading: false }, global: { stubs: { BaseDialog: { props: ['show'], template: '<section v-if="show" class="test-dialog"><slot /></section>' } } } })
    expect(wrapper.findAll('.plaza-model-card')).toHaveLength(1)
    expect(wrapper.find('.plaza-price-grid').text()).toContain('¥0.15')
    await wrapper.find('.plaza-original-toggle').trigger('click')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('¥33.5')
    await wrapper.findAll('.plaza-segmented button')[1].trigger('click')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$5')
    await wrapper.find('.plaza-model-meta select').setValue('2')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$5')
    await wrapper.find('.plaza-card-price-detail').trigger('click')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$5')
    const next = structuredClone(data)
    next.groups[1].models[0].pricing!.input_price = 8e-6
    next.groups[1].models[0].official_pricing!.input_price = 7e-6
    await wrapper.setProps({ response: next })
    expect(wrapper.find('.plaza-model-meta select').element).toHaveProperty('value', '2')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$7')
    expect(wrapper.find('.plaza-original-toggle').attributes('aria-pressed')).toBe('true')
    await wrapper.find('.plaza-original-toggle').trigger('click')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$0.0238806')
    expect(wrapper.findAll('button').some(button => ['/1K', '/1M'].includes(button.text()))).toBe(false)
    await wrapper.find('.plaza-search input').setValue('not-found')
    expect(wrapper.findAll('.plaza-model-card')).toHaveLength(0)
    await wrapper.setProps({ error: true })
    expect(wrapper.find('.plaza-stale').exists()).toBe(true)
    wrapper.unmount()
  })
  it('does not fabricate a healthy status or TPS without monitor data', () => {
    const wrapper = mount(PlazaStatus)
    expect(wrapper.findAll('.plaza-timeline .unknown')).toHaveLength(24)
    expect(wrapper.find('.plaza-status-label b').text()).toBe('—')
    expect(wrapper.findAll('.plaza-metric strong').map(node => node.text())).toEqual(['—', '—'])
    wrapper.unmount()
  })
})
