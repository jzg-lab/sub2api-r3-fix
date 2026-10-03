import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))

vi.mock('@/api/client', () => ({
  apiClient: { post }
}))

import { applyOAuthCredentials } from '@/api/admin/accounts'

describe('atomic account reauthorization API', () => {
  beforeEach(() => {
    post.mockReset()
  })

  it('submits the captured revision without losing timestamp precision', async () => {
    const payload = {
      type: 'oauth' as const,
      credentials: {},
      extra: { privacy_mode: true },
      expected_updated_at: '2026-10-02T15:35:17.123456Z'
    }
    const account = { id: 42, updated_at: '2026-10-02T15:36:18.654321Z' }
    post.mockResolvedValue({ data: account })

    await expect(applyOAuthCredentials(42, payload)).resolves.toEqual(account)
    expect(post).toHaveBeenCalledTimes(1)
    expect(post).toHaveBeenCalledWith(
      '/admin/accounts/42/apply-oauth-credentials',
      payload
    )
  })

  it('preserves a stale-account conflict without retrying or clearing errors', async () => {
    const conflict = {
      response: { status: 409, data: { reason: 'OAUTH_REAUTH_STALE_ACCOUNT' } }
    }
    post.mockRejectedValue(conflict)

    await expect(
      applyOAuthCredentials(42, {
        type: 'oauth',
        credentials: {},
        expected_updated_at: '2026-10-02T15:35:17.123456Z'
      })
    ).rejects.toBe(conflict)
    expect(post).toHaveBeenCalledTimes(1)
  })
})
