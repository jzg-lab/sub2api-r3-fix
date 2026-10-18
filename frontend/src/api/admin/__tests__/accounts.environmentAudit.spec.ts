import { beforeEach, describe, expect, it, vi } from 'vitest'
import { accountsAPI, getEnvironmentAudit } from '../accounts'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get } }))

describe('environment audit API', () => {
  beforeEach(() => get.mockReset())

  it('uses a read-only request with caller-owned cancellation', async () => {
    const controller = new AbortController()
    const data = { accounts: [], collisions: [], import_batches: [], generated_at: '2026-10-11T12:00:00+08:00' }
    get.mockResolvedValueOnce({ data })
    expect(await accountsAPI.getEnvironmentAudit(controller.signal)).toBe(data)
    expect(get).toHaveBeenCalledTimes(1)
    expect(get).toHaveBeenCalledWith('/admin/accounts/environment-audit', { signal: controller.signal })
  })

  it('preserves failures instead of turning them into an empty success', async () => {
    const error = new Error('read failed')
    get.mockRejectedValueOnce(error)
    await expect(getEnvironmentAudit()).rejects.toBe(error)
  })
})
