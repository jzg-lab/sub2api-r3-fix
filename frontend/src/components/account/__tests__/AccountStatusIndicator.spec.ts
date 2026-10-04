import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountStatusIndicator from '../AccountStatusIndicator.vue'
import type { Account, AccountUsageInfo } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

vi.mock('@/utils/format', async () => {
  const actual = await vi.importActual<typeof import('@/utils/format')>('@/utils/format')
  return {
    ...actual,
    formatCountdown: () => '1h'
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'antigravity',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

describe('AccountStatusIndicator', () => {
  it('shows the backend quota pause as a rate limit without a fabricated 429 marker', async () => {
    vi.useFakeTimers()
    const now = new Date('2026-10-04T00:00:00Z')
    vi.setSystemTime(now)
    const reset = new Date(now.getTime() + 2000).toISOString()
    const wrapper = mount(AccountStatusIndicator, { props: {
      account: makeAccount({ platform: 'openai', quota_rate_limit_reset_at: reset })
    } })
    try {
      expect(wrapper.get('.badge-warning').text()).toBe('admin.accounts.status.rateLimited')
      expect(wrapper.text()).toContain('admin.accounts.status.rateLimitedAutoResume')
      expect(wrapper.text()).not.toContain('429')
      await vi.advanceTimersByTimeAsync(3000)
      expect(wrapper.text()).toContain('admin.accounts.status.active')
      expect(wrapper.find('.badge-warning').exists()).toBe(false)
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })

  it('uses refreshed usage to clear an old quota limit and preserves a real 429', async () => {
    const reset = '2099-10-04T00:00:00Z'
    const account = makeAccount({ platform: 'openai', quota_rate_limit_reset_at: reset })
    const wrapper = mount(AccountStatusIndicator, { props: { account } })
    try {
      await wrapper.setProps({ usage: { quota_rate_limit_reset_at: null } as AccountUsageInfo })
      expect(wrapper.find('.badge-warning').exists()).toBe(false)
      await wrapper.setProps({ account: { ...account, rate_limit_reset_at: reset } })
      expect(wrapper.get('.badge-warning').text()).toBe('admin.accounts.status.rateLimited')
      expect(wrapper.text()).toContain('429')
    } finally {
      wrapper.unmount()
    }
  })

  it('adds a quota limit from refreshed usage and keeps error states visible', async () => {
    const account = makeAccount({ platform: 'openai' })
    const wrapper = mount(AccountStatusIndicator, { props: { account } })
    try {
      await wrapper.setProps({ usage: { quota_rate_limit_reset_at: '2099-10-04T00:00:00Z' } as AccountUsageInfo })
      expect(wrapper.get('.badge-warning').text()).toBe('admin.accounts.status.rateLimited')
      await wrapper.setProps({ account: { ...account, status: 'error' } })
      expect(wrapper.get('.badge-danger').text()).toBe('admin.accounts.status.error')
      await wrapper.setProps({ account: { ...account, status: 'inactive' } })
      expect(wrapper.text()).toContain('admin.accounts.status.inactive')
    } finally {
      wrapper.unmount()
    }
  })

  it('expires cooldown without waiting for an account refresh', async () => {
    vi.useFakeTimers()
    const now = new Date('2026-09-13T00:00:00Z')
    vi.setSystemTime(now)
    const wrapper = mount(AccountStatusIndicator, { props: { account: makeAccount({
      platform: 'openai', rate_limit_reset_at: new Date(now.getTime() + 2000).toISOString()
    }) } })
    try {
      expect(wrapper.find('.badge-warning').exists()).toBe(true)
      await vi.advanceTimersByTimeAsync(3000)
      expect(wrapper.find('.badge-warning').exists()).toBe(false)
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })
  it('Claude 5 模型限流时显示 Opus 和 Sonnet 的短别名', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          extra: {
            model_rate_limits: {
              'claude-opus-5': {
                rate_limited_at: '2026-07-28T00:00:00Z',
                rate_limit_reset_at: '2099-07-28T00:00:00Z'
              },
              'claude-sonnet-5': {
                rate_limited_at: '2026-07-28T00:00:00Z',
                rate_limit_reset_at: '2099-07-28T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('COpus5')
    expect(wrapper.text()).toContain('CSon5')
    expect(wrapper.text()).not.toContain('claude-sonnet-5')
  })

  it('Grok 账号额度限流时显示自动恢复时间而非临时不可调度', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 5,
          name: 'grok-free-1',
          platform: 'grok',
          rate_limited_at: '2026-07-11T12:00:00Z',
          rate_limit_reset_at: '2099-07-11T13:00:00Z',
          temp_unschedulable_until: '2099-07-11T12:30:00Z',
          temp_unschedulable_reason: 'legacy grok rate limited'
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.find('.badge-warning').text()).toBe('admin.accounts.status.rateLimited')
    expect(wrapper.text()).toContain('admin.accounts.status.rateLimitedAutoResume')
    expect(wrapper.text()).not.toContain('admin.accounts.status.tempUnschedulable')
  })

  it('模型限流 + overages 启用 + 无 AICredits key → 显示 ⚡ (credits_active)', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 1,
          name: 'ag-1',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('⚡')
    expect(wrapper.text()).toContain('CSon45')
  })

  it('模型限流 + overages 未启用 → 普通限流样式（无 ⚡）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 2,
          name: 'ag-2',
          extra: {
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).not.toContain('⚡')
  })

  it('AICredits key 生效 → 显示积分已用尽 (credits_exhausted)', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 3,
          name: 'ag-3',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'AICredits': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('admin.accounts.status.creditsExhausted')
  })

  it('模型限流 + overages 启用 + AICredits key 生效 → 普通限流样式（积分耗尽，无 ⚡）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 4,
          name: 'ag-4',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              },
              'AICredits': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 模型限流 + 积分耗尽 → 不应显示 ⚡
    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).not.toContain('⚡')
    // AICredits 积分耗尽状态应显示
    expect(wrapper.text()).toContain('admin.accounts.status.creditsExhausted')
  })
})
