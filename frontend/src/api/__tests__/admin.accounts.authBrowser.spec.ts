import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({
  post: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { post }
}))

import { launchAuthBrowser } from '@/api/admin/accounts'

describe('admin auth browser launch API', () => {
  beforeEach(() => {
    post.mockReset()
  })

  it('allows the bounded launcher to exit before returning its result', async () => {
    const result = {
      launched: true,
      already_running: false,
      profile_tag: 'auth-example',
      proxy_name: 'local-bucket',
      exit_ingress: 'http://127.0.0.1:17921',
      output: 'launcher completed'
    }
    post.mockResolvedValue({ data: result })

    await expect(launchAuthBrowser('session-1')).resolves.toEqual(result)
    expect(post).toHaveBeenCalledTimes(1)
    expect(post).toHaveBeenCalledWith(
      '/admin/openai/launch-auth-browser',
      { session_id: 'session-1' },
      { timeout: 60000 }
    )
  })

  it('waits for the server result and preserves a launcher failure', async () => {
    let rejectResponse!: (reason: Error) => void
    post.mockReturnValue(new Promise((_, reject) => {
      rejectResponse = reject
    }))
    const settled = vi.fn()
    const pending = launchAuthBrowser('session-2')
    const observed = pending.then(settled, settled)
    await Promise.resolve()
    expect(settled).not.toHaveBeenCalled()

    const failure = new Error('auth browser launcher exited unsuccessfully: exit status 23')
    rejectResponse(failure)
    await expect(pending).rejects.toBe(failure)
    await observed
    expect(post).toHaveBeenCalledTimes(1)
  })

  it('preserves the distinct in-flight response without issuing another request', async () => {
    const result = {
      launched: false,
      already_running: true,
      output: 'launcher is already running for this authorization session'
    }
    post.mockResolvedValue({ data: result })

    await expect(launchAuthBrowser('session-3')).resolves.toEqual(result)
    expect(post).toHaveBeenCalledTimes(1)
  })
})
