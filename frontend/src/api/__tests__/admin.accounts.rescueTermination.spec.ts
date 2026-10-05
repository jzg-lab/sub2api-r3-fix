import { describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { post } }))
import { terminateOpenAIRescue } from '@/api/admin/accounts'

describe('rescue termination API', () => {
  it('uses the dedicated command and returns its result', async () => {
    post.mockResolvedValueOnce({ data: { account_id: 42, terminated: true } })
    expect(await terminateOpenAIRescue(42)).toEqual({ account_id: 42, terminated: true })
    expect(post).toHaveBeenCalledWith('/admin/openai/accounts/42/rescue/terminate')
  })

  it('passes storage errors to the caller', async () => {
    post.mockRejectedValueOnce(new Error('cannot restore groups'))
    await expect(terminateOpenAIRescue(42)).rejects.toThrow('cannot restore groups')
  })
})
