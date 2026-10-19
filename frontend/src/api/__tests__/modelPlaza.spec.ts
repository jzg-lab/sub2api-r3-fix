import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../client'
import { refreshModelPlaza } from '../modelPlaza'
afterEach(() => vi.restoreAllMocks())
describe('plaza conditional requests', () => {
  it('accepts 304, keeps the caller ETag and bounds the foreground refresh request', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({ status: 304, data: '', headers: { etag: 'version-1' } })
    const result = await refreshModelPlaza('version-1')
    expect(result).toEqual({ data: null, etag: 'version-1' })
    const options = get.mock.calls[0][1]!
    expect(options.headers).toEqual({ 'If-None-Match': 'version-1' })
    expect(options.timeout).toBeLessThanOrEqual(2000)
    expect(options.validateStatus!(304)).toBe(true)
    expect(options.validateStatus!(403)).toBe(false)
  })
})
