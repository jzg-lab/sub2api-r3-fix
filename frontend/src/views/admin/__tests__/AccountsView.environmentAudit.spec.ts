import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'
import AccountsView from '../AccountsView.vue'
import AccountEnvironmentAuditModal from '@/components/admin/AccountEnvironmentAuditModal.vue'

const { listAccounts } = vi.hoisted(() => ({ listAccounts: vi.fn() }))
vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag: vi.fn(),
      getBatchTodayStats: vi.fn().mockResolvedValue({ stats: {} }),
      getUpstreamBillingProbeSettings: vi.fn().mockResolvedValue({ enabled: true, interval_minutes: 30 })
    },
    proxies: { getAll: vi.fn().mockResolvedValue([]) },
    groups: { getAll: vi.fn().mockResolvedValue([]) }
  }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'fixture-only', isSimpleMode: false })
}))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))

describe('AccountsView environment audit entry', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset().mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
  })

  it('opens only from More Actions and closes without mutating an account', async () => {
    const wrapper = shallowMount(AccountsView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /></div>' },
          AccountTableActions: { template: '<div><slot name="after" /></div>' },
          Teleport: true
        }
      }
    })
    try {
      await flushPromises()
      const modal = wrapper.getComponent(AccountEnvironmentAuditModal)
      expect(modal.props('show')).toBe(false)
      const menu = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.moreActions'))
      expect(menu).toBeDefined()
      await menu!.trigger('click')
      const entry = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.environmentAudit.title'))
      expect(entry).toBeDefined()
      await entry!.trigger('click')
      expect(modal.props('show')).toBe(true)
      expect(wrapper.findAll('button').some(button => button.text().includes('admin.accounts.environmentAudit.title'))).toBe(false)
      modal.vm.$emit('close')
      await flushPromises()
      expect(modal.props('show')).toBe(false)
    } finally {
      wrapper.unmount()
    }
  })
})
