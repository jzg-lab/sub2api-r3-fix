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
  it('applies the effective rate only in the after-multiplier mode', () => {
    const offer = catalogue(plazaFixture().groups)[0].offers[0]
    expect(formatPrice(displayPrice(3e-6, offer, false), 1e6)).toBe('$1.5')
    expect(formatPrice(displayPrice(3e-6, offer, true), 1e6)).toBe('$3')
    offer.model.current_rate_multiplier = 0
    expect(displayPrice(3e-6, offer, false)).toBe(0)
    delete offer.model.current_rate_multiplier
    expect(displayPrice(3e-6, offer, false)).toBeNull()
    expect(displayPrice(3e-6, offer, true)).toBe(3e-6)
    expect(displayPrice(0, offer, true)).toBe(0)
    expect(displayPrice(null, offer, true)).toBeNull()
    expect(displayPrice(NaN, offer, true)).toBeNull()
  })
  it('shows missing prices even when an official reference exists', () => {
    const offer = catalogue(plazaFixture().groups)[0].offers[0]
    offer.model.pricing = null
    const wrapper = mount(PlazaPriceGrid, { props: { offer, beforeMultiplier: true } })
    expect(wrapper.findAll('strong').map(node => node.text())).toEqual(['—', '—', '—', '—'])
    wrapper.unmount()
  })
  it.each(['per_request', 'image', 'video'] as const)('shows both %s prices without a token scale', async billingMode => {
    const offer = catalogue(plazaFixture().groups)[0].offers[0]
    offer.model.pricing!.billing_mode = billingMode
    offer.model.pricing!.per_request_price = 2
    const wrapper = mount(PlazaPriceGrid, { props: { offer, beforeMultiplier: false } })
    expect(wrapper.find('strong').text()).toBe('$1')
    await wrapper.setProps({ beforeMultiplier: true })
    expect(wrapper.find('strong').text()).toBe('$2')
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
  it('only offers groups with models from the selected provider and clears an incompatible group', async () => {
    const data = plazaFixture()
    data.groups[0].models.push({ ...data.groups[0].models[0], name: 'grok-1', platform: 'grok' })
    const anthropic = structuredClone(data.groups[0])
    anthropic.id = 2; anthropic.name = 'Claude group'; anthropic.platform = 'anthropic'
    anthropic.models = [{ ...anthropic.models[0], name: 'claude-1', platform: 'anthropic' }]
    const openai = structuredClone(data.groups[0])
    openai.id = 3; openai.name = 'GPT group'; openai.models = [openai.models[0]]
    data.groups.push(anthropic, openai)
    const wrapper = mount(ModelPlazaContent, { props: { response: data, loading: false } })
    const provider = (name: string) => wrapper.findAll('.plaza-provider-chips button')
      .find(button => button.find(`[data-provider="${name}"]`).exists())!
    const groups = () => wrapper.findAll('.plaza-filter-section')[1].findAll('button')
    expect(groups().map(button => button.text())).toHaveLength(4)

    await provider('grok').trigger('click')
    expect(groups().map(button => button.text()).join(' ')).toContain('Standard')
    expect(groups()).toHaveLength(2)
    expect(wrapper.findAll('.plaza-model-card')).toHaveLength(1)

    await provider('anthropic').trigger('click')
    await groups()[1].trigger('click')
    expect(groups()[1].attributes('aria-pressed')).toBe('true')
    await provider('openai').trigger('click')
    expect(groups()).toHaveLength(3)
    expect(groups()[0].attributes('aria-pressed')).toBe('true')
    expect(wrapper.findAll('.plaza-model-card')).toHaveLength(1)

    await wrapper.find('.plaza-provider-chips button').trigger('click')
    expect(groups()).toHaveLength(4)
    await groups()[3].trigger('click')
    await wrapper.setProps({ response: { ...data, groups: data.groups.slice(0, 2) } })
    expect(groups()[0].attributes('aria-pressed')).toBe('true')
    expect(groups()).toHaveLength(3)
    wrapper.unmount()
  })

  it('sorts by the displayed price and uses the same tiers before and after the multiplier', async () => {
    const data = plazaFixture()
    const model = data.groups[0].models[0]
    const pricing = model.pricing!
    pricing.intervals = [
      { ...pricing, min_tokens: 0, max_tokens: 200000, tier_label: 'Base tier', input_price: 6e-6, cache_write_1h_price: 2e-6 },
      { ...pricing, min_tokens: 200000, max_tokens: null, tier_label: 'Long tier', input_price: 12e-6, cache_write_1h_price: 4e-6 }
    ]
    const other = structuredClone(model)
    other.name = 'gpt-other'
    other.pricing!.intervals[0].input_price = 8e-6
    other.current_rate_multiplier = 0.2
    data.groups[0].models.push(other)
    const wrapper = mount(ModelPlazaContent, { props: { response: data, loading: false }, global: { stubs: { BaseDialog: { props: ['show'], template: '<section v-if="show" class="test-dialog"><slot /></section>' } } } })
    await wrapper.find('.plaza-toolbar select').setValue('price')
    expect(wrapper.find('.plaza-model-heading h2').text()).toBe('gpt-other')
    await wrapper.findAll('.plaza-card-price-detail')[1].trigger('click')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$3')
    await wrapper.find('.plaza-price-switch button').trigger('click')
    expect(wrapper.find('.plaza-model-heading h2').text()).toBe('gpt-5')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$6')
    const rows = wrapper.findAll('.test-dialog tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('Base tier')
    expect(rows[0].findAll('td').at(-1)?.text()).toBe('$2')
    expect(rows[1].findAll('td')[1].text()).toBe('$12')
    expect(rows[1].findAll('td').at(-1)?.text()).toBe('$4')
    wrapper.unmount()
  })
  it('keeps the price mode and group selected while refreshing open pricing details', async () => {
    const data = plazaFixture()
    const second = structuredClone(data.groups[0])
    second.id = 2; second.name = 'VIP'; second.models[0].current_rate_multiplier = 0.2
    data.groups.push(second)
    const wrapper = mount(ModelPlazaContent, { props: { response: data, loading: false }, global: { stubs: { BaseDialog: { props: ['show'], template: '<section v-if="show" class="test-dialog"><slot /></section>' } } } })
    expect(wrapper.findAll('.plaza-model-card')).toHaveLength(1)
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$1.5')
    await wrapper.find('.plaza-price-switch button').trigger('click')
    await wrapper.find('.plaza-model-meta select').setValue('2')
    expect(wrapper.find('.plaza-price-grid strong').text()).toBe('$3')
    await wrapper.find('.plaza-card-price-detail').trigger('click')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$3')
    const next = structuredClone(data)
    next.groups[1].models[0].pricing!.input_price = 8e-6
    next.groups[1].models[0].official_pricing!.input_price = 7e-6
    await wrapper.setProps({ response: next })
    expect(wrapper.find('.plaza-model-meta select').element).toHaveProperty('value', '2')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$8')
    expect(wrapper.find('.plaza-price-switch button').attributes('aria-pressed')).toBe('true')
    await wrapper.findAll('.plaza-price-switch button')[1].trigger('click')
    expect(wrapper.find('.test-dialog .plaza-price-grid strong').text()).toBe('$1.6')
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
