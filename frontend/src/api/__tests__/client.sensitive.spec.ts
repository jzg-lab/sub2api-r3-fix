import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import axios, { AxiosError, type AxiosInstance, type InternalAxiosRequestConfig } from 'axios'

vi.mock('@/i18n', () => ({ getLocale: () => 'zh-CN' }))

describe('transient interactive request boundary', () => {
  let client: AxiosInstance
  const marker = ['fixture', 'transient', 'input'].join('-')

  beforeEach(async () => {
    localStorage.clear()
    localStorage.setItem('refresh_token', marker)
    vi.resetModules()
    client = (await import('@/api/client')).apiClient
  })
  afterEach(() => vi.restoreAllMocks())

  it.each([401, 403, 409, 429, 500, 502, 0])('does not replay or expose failed input (status %s)', async (status) => {
    const refresh = vi.spyOn(axios, 'post')
    let captured: InternalAxiosRequestConfig | undefined
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => {
      captured = config
      throw new AxiosError(marker, 'ERR_BAD_RESPONSE', config, undefined,
        status ? { config, status, statusText: marker, headers: {}, data: { message: marker } } : undefined)
    })
    client.defaults.adapter = adapter
    const error = await client.post('/admin/openai/launch-auth-browser',
      { transientInput: marker }, { sensitiveNoReplay: true }).catch(error => error)
    expect(error).toBeInstanceOf(Error)
    expect(error.message).not.toContain(marker)
    expect(error.config).toBeUndefined()
    expect(error.response).toBeUndefined()
    expect(captured?.data).toBeUndefined()
    expect(adapter).toHaveBeenCalledTimes(1)
    expect(refresh).not.toHaveBeenCalled()
  })

  it.each([0, 400])('clears the body on HTTP success and sanitizes application errors (code %s)', async (code) => {
    let captured: InternalAxiosRequestConfig | undefined
    client.defaults.adapter = async config => {
      captured = config
      return { config, status: 200, statusText: 'OK', headers: {}, data: {
        code, message: marker, metadata: { reflected: marker }, data: { launched: true }
      } }
    }
    const pending = client.post('/admin/openai/launch-auth-browser',
      { transientInput: marker }, { sensitiveNoReplay: true })
    if (code === 0) {
      expect((await pending).data).toEqual({ launched: true })
    } else {
      const error = await pending.catch(error => error)
      expect(error).toBeInstanceOf(Error)
      expect(error.message).not.toContain(marker)
      expect(error.metadata).toBeUndefined()
    }
    expect(captured?.data).toBeUndefined()
  })
})
