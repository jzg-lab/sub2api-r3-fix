import { useManualQualityStore } from '@/stores/manualQuality'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

const { listAccounts, qualitySummaries } = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  qualitySummaries: vi.fn().mockResolvedValue([])
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag: vi.fn(),
      getBatchTodayStats: vi.fn().mockResolvedValue({ stats: {} }),
      getUpstreamBillingProbeSettings: vi.fn().mockResolvedValue({ enabled: true, interval_minutes: 30 }),
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn()
    },
    proxies: { getAll: vi.fn().mockResolvedValue([]) },
    groups: { getAll: vi.fn().mockResolvedValue([]) }
  }
}))

vi.mock('@/api/admin/manualQuality', () => ({ manualQualityAPI: { summaries: qualitySummaries } }))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

const DataTableStub = {
  props: ['columns', 'data'],
  emits: ['sort'],
  template: `
    <div data-test="data-table">
      <span v-for="column in columns" :key="column.key" :data-column="column.key">
        {{ column.sortable ? 'sortable' : 'fixed' }}
      </span>
      <div v-for="row in data" :key="row.id"><slot name="cell-health" :row="row" /></div>
      <button data-test="sort-priority" @click="$emit('sort', 'priority', 'desc')" />
    </div>
  `
}

function mountView() {
  return mount(AccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
        },
        DataTable: DataTableStub,
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        ManualQualityTestModal: { props: ['account'], template: '<div data-test="manual-quality-modal">{{ account.id }}</div>' },
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: true
      }
    }
  })
}

describe('admin AccountsView priority column preferences', () => {
  it('updates the health summary when a background test finishes after its dialog closes', async () => {
    listAccounts.mockResolvedValue({ items: [{ id: 7, name: 'upstream', platform: 'openai', type: 'apikey', status: 'active', credentials: {}, extra: {}, groups: [] }], total: 1, page: 1, page_size: 20, pages: 1 })
    qualitySummaries.mockResolvedValueOnce([{ account_id: 7, model: 'gpt-6.1-sol', kind: 'candy', run_id: 'background', started_at: '2026-11-03T00:00:00Z', status: 'running' }])
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.text()).toContain('admin.accounts.manualQuality.status.running')
    const tests = useManualQualityStore()
    tests.tasks['admin:7:gpt-6.1-sol:candy'] = {
      running: false, saved: true, error: '', controller: new AbortController(),
      result: { account_id: 7, model: 'gpt-6.1-sol', kind: 'candy', run_id: 'background', started_at: '2026-11-03T00:00:00Z', duration_ms: 1, status: 'completed', answer: '21', upstream_model: '', prompt: 'question', reference_answer: '21' }
    }
    await flushPromises()
    expect(wrapper.text()).toContain('gpt-6.1-sol')
    expect(wrapper.text()).toContain('admin.accounts.manualQuality.status.completed')
    wrapper.unmount()
  })

  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    listAccounts.mockReset().mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
      pages: 0
    })
  })

  it('opens manual quality testing for an API key account from the health column', async () => {
    listAccounts.mockResolvedValue({ items: [{id: 7, name: 'upstream', platform: 'openai', type: 'apikey', status: 'active', credentials: {}, extra: {}, groups: []}], total: 1, page: 1, page_size: 20, pages: 1 })
    const wrapper = mountView()
    await flushPromises()
    const button = wrapper.findAll('button').find(value => value.text() === 'admin.accounts.manualQuality.title')
    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.get('[data-test="manual-quality-modal"]').text()).toBe('7')
    expect(qualitySummaries).toHaveBeenCalledWith([7])
    wrapper.unmount()
  })

  it('shows priority as a sortable column for fresh preferences', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-column="priority"]').text()).toBe('sortable')

    await wrapper.get('[data-test="sort-priority"]').trigger('click')
    await flushPromises()

    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      expect.objectContaining({ sort_by: 'priority', sort_order: 'desc' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
  })

  it('preserves an existing preference that explicitly hides priority', async () => {
    localStorage.setItem('account-hidden-columns', JSON.stringify(['priority', 'today_stats']))
    localStorage.setItem('account-hidden-columns-version', 'scheduler-score-hidden-by-default')

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-column="priority"]').exists()).toBe(false)
    expect(JSON.parse(localStorage.getItem('account-hidden-columns') || '[]')).toEqual([
      'priority',
      'today_stats'
    ])
  })

  it('keeps priority visible while migrating older saved preferences', async () => {
    localStorage.setItem('account-hidden-columns', JSON.stringify(['today_stats']))

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-column="priority"]').text()).toBe('sortable')
    expect(JSON.parse(localStorage.getItem('account-hidden-columns') || '[]')).toEqual(
      expect.arrayContaining(['today_stats', 'scheduler_score'])
    )
    expect(JSON.parse(localStorage.getItem('account-hidden-columns') || '[]')).not.toContain('priority')
  })
})
