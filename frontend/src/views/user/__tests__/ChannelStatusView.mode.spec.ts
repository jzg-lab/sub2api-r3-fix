import { describe, expect, it, vi, beforeEach } from 'vitest'
import { defineComponent, h } from 'vue'
import { mount } from '@vue/test-utils'

const isV1 = vi.fn(() => false)
const route = { query: {} as Record<string, string> }
vi.mock('vue-router', () => ({ useRoute: () => route }))
vi.mock('../ChannelStatusOverview.vue', () => ({
  default: defineComponent({ name: 'ChannelStatusOverview', setup: () => () => h('div', { 'data-testid': 'cards' }) }),
}))

vi.mock('@/utils/featureFlags', () => ({
  isChannelMonitorV1Mode: () => isV1(),
}))

vi.mock('../ChannelStatusV1View.vue', () => ({
  default: defineComponent({ name: 'ChannelStatusV1View', setup: () => () => h('div', { 'data-testid': 'v1' }) }),
}))
vi.mock('../ChannelStatusV2View.vue', () => ({
  default: defineComponent({ name: 'ChannelStatusV2View', setup: () => () => h('div', { 'data-testid': 'v2' }) }),
}))

import ChannelStatusView from '../ChannelStatusView.vue'

describe('ChannelStatusView mode switch', () => {
  beforeEach(() => {
    isV1.mockReset()
    route.query = {}
  })

  it('renders cards for a new V2 visit', () => {
    isV1.mockReturnValue(false)
    const wrapper = mount(ChannelStatusView)
    expect(wrapper.find('[data-testid="cards"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="v1"]').exists()).toBe(false)
  })

  it('renders V1 when in v1 mode', () => {
    isV1.mockReturnValue(true)
    const wrapper = mount(ChannelStatusView)
    expect(wrapper.find('[data-testid="v1"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="v2"]').exists()).toBe(false)
  })
})

it.each([{ monitor_view: 'details' }, { monitor_view: 'v2' }, { model: 'gpt-5' }, { tab: 'errors' }])('preserves an analysis bookmark %j', (query) => {
  isV1.mockReturnValue(false)
  route.query = query
  const wrapper = mount(ChannelStatusView)
  expect(wrapper.find('[data-testid="v2"]').exists()).toBe(true)
  expect(wrapper.find('[data-testid="cards"]').exists()).toBe(false)
})

it('lets cards retain analysis filters without reopening analysis', () => {
  isV1.mockReturnValue(false)
  route.query = { monitor_view: 'cards', model: 'gpt-5' }
  expect(mount(ChannelStatusView).find('[data-testid="cards"]').exists()).toBe(true)
})
