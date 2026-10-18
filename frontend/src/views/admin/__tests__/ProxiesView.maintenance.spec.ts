import { flushPromises, shallowMount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProxiesView from '../ProxiesView.vue'

const { removeProxy, batchDelete, updateProxy, showError, list, checkProxyQuality } = vi.hoisted(() => ({
  removeProxy: vi.fn(), batchDelete: vi.fn(), updateProxy: vi.fn(), showError: vi.fn(), list: vi.fn(),
  checkProxyQuality: vi.fn(),
}))
vi.mock('@/api/admin', () => ({
  adminAPI: { proxies: {
    delete: removeProxy, batchDelete, update: updateProxy, list, checkProxyQuality,
    getAllWithCount: vi.fn().mockResolvedValue([]),
  } },
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess: vi.fn(), showInfo: vi.fn() }),
}))
vi.mock('@/composables/useSwipeSelect', () => ({ useSwipeSelect: vi.fn() }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key }),
}))

const proxy = {
  id: 7, name: 'fixture', protocol: 'socks5', host: 'proxy.example.test', port: 1080,
  status: 'active', account_count: 0, created_at: '2026-01-01T00:00:00Z',
  expires_at: '2027-02-03T12:34:56Z',
}
const wrappers: ReturnType<typeof shallowMount>[] = []
async function view() {
  const wrapper = shallowMount(ProxiesView)
  wrappers.push(wrapper)
  await flushPromises()
  return wrapper.vm as unknown as {
    handleDelete: (value: typeof proxy) => void
    handleEdit: (value: typeof proxy) => void
    confirmDelete: () => Promise<void>
    confirmBatchDelete: () => Promise<void>
    handleUpdateProxy: () => Promise<void>
    select: (id: number) => void
    selectedProxyIds: Set<number>
    editForm: { status: string; expires_at: string }
    handleQualityCheck: (value: typeof proxy) => Promise<void>
    runBatchProxyQualityChecks: (ids: number[]) => Promise<{ failed: number }>
    proxies: Array<typeof proxy & {
      ip_address?: string; country?: string; city?: string; latency_status?: string
      quality_status?: string; quality_score?: number; quality_checked?: number
    }>
  }
}
beforeEach(() => {
  vi.clearAllMocks()
  vi.spyOn(console, 'error').mockImplementation(() => {})
  list.mockResolvedValue({ items: [{ ...proxy }], total: 1, pages: 1 })
})
afterEach(() => {
  wrappers.splice(0).forEach(wrapper => wrapper.unmount())
  vi.restoreAllMocks()
})

describe('代理维护与历史关联保护', () => {
  it.each(['single', 'batch'])('clears a previous measured exit on failed %s quality check', async mode => {
    checkProxyQuality.mockResolvedValue({
      items: [{ target: 'base_connectivity', status: 'fail', message: 'exit unknown' }],
      exit_ip: '', failed_count: 1, challenge_count: 0, warn_count: 0,
      score: 0, grade: 'F', checked_at: 123, summary: 'exit unknown',
    })
    const vm = await view()
    Object.assign(vm.proxies[0], { ip_address: '192.0.2.1', country: 'old', city: 'old', quality_status: 'healthy' })
    if (mode === 'single') await vm.handleQualityCheck(proxy)
    else expect((await vm.runBatchProxyQualityChecks([7])).failed).toBe(1)
    expect(vm.proxies[0]).toMatchObject({ latency_status: 'failed', quality_status: 'failed' })
    expect(vm.proxies[0].ip_address).toBeUndefined()
    expect(vm.proxies[0].country).toBeUndefined()
    expect(vm.proxies[0].city).toBeUndefined()
  })

  it.each(['single', 'batch'])('does not display old healthy evidence after an API failure in %s', async mode => {
    checkProxyQuality.mockRejectedValue(new Error('fixture transport interruption'))
    const vm = await view()
    Object.assign(vm.proxies[0], {
      ip_address: '192.0.2.1', quality_status: 'healthy', quality_score: 100, quality_checked: 100,
    })
    if (mode === 'single') await vm.handleQualityCheck(proxy)
    else expect((await vm.runBatchProxyQualityChecks([7])).failed).toBe(1)
    expect(vm.proxies[0].latency_status).toBe('failed')
    expect(vm.proxies[0].ip_address).toBeUndefined()
    expect(vm.proxies[0].quality_status).toBeUndefined()
    expect(vm.proxies[0].quality_score).toBeUndefined()
    expect(vm.proxies[0].quality_checked).toBeUndefined()
  })

  it('历史关联删除冲突显示停用/续期的具体提示', async () => {
    removeProxy.mockRejectedValue({ reason: 'OPENAI_OAUTH_PROXY_BINDING_PROTECTED', message: 'protected' })
    const vm = await view()
    vm.handleDelete(proxy)
    await vm.confirmDelete()
    expect(removeProxy).toHaveBeenCalledWith(7)
    expect(showError).toHaveBeenCalledWith('admin.proxies.historyBindingProtected')
  })

  it('已有当前账号关联时不发删除请求', async () => {
    const vm = await view()
    vm.handleDelete({ ...proxy, account_count: 1 })
    await vm.confirmDelete()
    expect(removeProxy).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.proxies.deleteBlockedInUse')
  })

  it('优先显示服务器原因而不是 Axios 通用状态错误', async () => {
    removeProxy.mockRejectedValue({
      message: 'Request failed with status code 409',
      response: { data: { message: 'fixture server rejection' } },
    })
    const vm = await view()
    vm.handleDelete(proxy)
    await vm.confirmDelete()
    expect(showError).toHaveBeenCalledWith('fixture server rejection')
  })

  it('兼容未规范化的历史关联冲突', async () => {
    removeProxy.mockRejectedValue({
      response: { data: { reason: 'OPENAI_OAUTH_PROXY_BINDING_PROTECTED' } },
    })
    const vm = await view()
    vm.handleDelete(proxy)
    await vm.confirmDelete()
    expect(showError).toHaveBeenCalledWith('admin.proxies.historyBindingProtected')
  })

  it('保留批量删除失败项的选中状态并展示具体原因', async () => {
    batchDelete.mockResolvedValue({ deleted_ids: [8], skipped: [{ id: 7, reason: 'fixture historical reference' }] })
    const vm = await view()
    vm.select(7)
    vm.select(8)
    await vm.confirmBatchDelete()
    expect(showError).toHaveBeenCalledWith('#7: fixture historical reference')
    expect([...vm.selectedProxyIds]).toEqual([7])
  })

  it('停用使用原连接参数并保留规范化 API 错误', async () => {
    updateProxy.mockRejectedValue({ message: 'fixture maintenance rejection' })
    const vm = await view()
    vm.handleEdit(proxy)
    vm.editForm.status = 'inactive'
    await vm.handleUpdateProxy()
    expect(updateProxy).toHaveBeenCalledWith(7, expect.objectContaining({
      status: 'inactive', host: proxy.host, port: proxy.port,
      expires_at: Date.parse(proxy.expires_at) / 1000,
    }))
    expect(showError).toHaveBeenCalledWith('fixture maintenance rejection')
  })

  it.each([
    ['2027-03-03', Date.parse('2027-03-03') / 1000],
    ['', null],
  ])('允许明确续期或移除有效期：%s', async (date, expected) => {
    updateProxy.mockResolvedValue(proxy)
    const vm = await view()
    vm.handleEdit(proxy)
    vm.editForm.expires_at = date as string
    await vm.handleUpdateProxy()
    expect(updateProxy).toHaveBeenCalledWith(7, expect.objectContaining({
      expires_at: expected, host: proxy.host, port: proxy.port, status: 'active',
    }))
  })
})
