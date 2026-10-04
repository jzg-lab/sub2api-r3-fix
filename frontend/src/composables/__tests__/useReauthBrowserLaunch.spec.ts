import { effectScope, reactive, ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account } from '@/types'
import { useReauthSession } from '../useReauthSession'
import { useReauthBrowserLaunch } from '../useReauthBrowserLaunch'

const api = vi.hoisted(() => ({
  launchAuthBrowser: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: api } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => api }))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key.endsWith('.OPENAI_OAUTH_FIXED_EGRESS_REQUIRED') ? 'Fixed route required' : key
  })
}))

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => {
    resolve = yes
    reject = no
  })
  return { promise, resolve, reject }
}

function setup() {
  const scope = effectScope()
  const props = reactive({
    show: true,
    account: {
      id: 41, platform: 'openai', proxy_id: 21,
      updated_at: '2026-10-02T15:35:17.123456Z'
    } as Account
  })
  const oauth = { sessionId: ref('session-old'), generateAuthUrl: vi.fn() }
  oauth.generateAuthUrl.mockImplementation(async () => {
    oauth.sessionId.value = 'session-new'
    return true
  })
  const clear = vi.fn()
  const state = scope.run(() => {
    const session = useReauthSession(props)
    return { ...useReauthBrowserLaunch(session, oauth, clear), session }
  })!
  return { ...state, props, oauth, clear, scope }
}

const success = { launched: true, proxy_name: 'fixture-proxy' }
const expired = { reason: 'AUTH_BROWSER_SESSION_EXPIRED' }

describe('reauthorization browser launch', () => {
  beforeEach(() => vi.resetAllMocks())

  it('serializes browser launch with exchange and keeps the captured revision', async () => {
    const pending = deferred<typeof success>()
    api.launchAuthBrowser.mockReturnValue(pending.promise)
    const test = setup()
    const first = test.launch()
    await test.launch()
    const exchange = vi.fn()
    await test.session.run(exchange)
    expect(test.launching.value).toBe(true)
    expect(test.ready.value).toBe(false)
    expect(exchange).not.toHaveBeenCalled()
    expect(api.launchAuthBrowser).toHaveBeenCalledOnce()
    expect(api.launchAuthBrowser).toHaveBeenCalledWith('session-old')
    pending.resolve(success)
    await first
    expect(test.launching.value).toBe(false)
    expect(test.ready.value).toBe(true)
    expect(api.showSuccess).toHaveBeenCalledOnce()
    await test.session.run(async (operation) => {
      expect(operation.expectedUpdatedAt).toBe('2026-10-02T15:35:17.123456Z')
    })
    test.scope.stop()
  })

  it.each([
    'AUTH_BROWSER_SESSION_NOT_FOUND',
    'AUTH_BROWSER_SESSION_EXPIRED',
    'AUTH_BROWSER_SESSION_INVALID',
    'AUTH_BROWSER_PROXY_ROUTE_STALE'
  ])('refreshes %s only once with the same proxy and clears the old callback', async (reason) => {
    api.launchAuthBrowser.mockRejectedValueOnce({ reason }).mockResolvedValueOnce(success)
    const test = setup()
    await test.launch()
    expect(test.clear).toHaveBeenCalledOnce()
    expect(test.oauth.generateAuthUrl).toHaveBeenCalledOnce()
    expect(test.oauth.generateAuthUrl).toHaveBeenCalledWith(21, undefined, {
      accountId: 41,
      expectedUpdatedAt: '2026-10-02T15:35:17.123456Z'
    })
    expect(api.launchAuthBrowser.mock.calls).toEqual([['session-old'], ['session-new']])
    expect(api.showError).not.toHaveBeenCalled()
    expect(test.launching.value).toBe(false)
    expect(test.ready.value).toBe(true)
    test.scope.stop()
  })

  it('does not loop when the refreshed session also fails', async () => {
    api.launchAuthBrowser.mockRejectedValue(expired)
    const test = setup()
    await test.launch()
    expect(api.launchAuthBrowser).toHaveBeenCalledTimes(2)
    expect(test.oauth.generateAuthUrl).toHaveBeenCalledOnce()
    expect(api.showError).toHaveBeenCalledOnce()
    test.scope.stop()
  })

  it.each([
    'AUTH_BROWSER_PROXY_UNAVAILABLE', 'AUTH_BROWSER_LAUNCH_FAILED',
    'OPENAI_OAUTH_LOGIN_IP_UNKNOWN', 'OPENAI_OAUTH_LOGIN_IP_CHANGED',
    'OPENAI_OAUTH_LOGIN_IP_UNAVAILABLE', 'OPENAI_OAUTH_PROXY_MISMATCH'
  ])(
    'does not regenerate or bypass a non-session failure: %s',
    async (reason) => {
      api.launchAuthBrowser.mockRejectedValue({ reason, message: 'fixture failure' })
      const test = setup()
      await test.launch()
      expect(api.launchAuthBrowser).toHaveBeenCalledOnce()
      expect(test.oauth.generateAuthUrl).not.toHaveBeenCalled()
      expect(api.showError).toHaveBeenCalledWith(expect.stringContaining('fixture failure'))
      test.scope.stop()
    }
  )

  it('localizes fixed-route rejection without generating an unbound session', async () => {
    api.launchAuthBrowser.mockRejectedValue({
      reason: 'OPENAI_OAUTH_FIXED_EGRESS_REQUIRED', message: 'raw backend message'
    })
    const test = setup()
    await test.launch()
    expect(test.oauth.generateAuthUrl).not.toHaveBeenCalled()
    expect(api.showError).toHaveBeenCalledWith(expect.stringContaining('Fixed route required'))
    test.scope.stop()
  })

  it.each(['close', 'reopen', 'account', 'proxy', 'unmount'] as const)(
    'ignores a late result after %s',
    async (change) => {
      const pending = deferred<typeof success>()
      api.launchAuthBrowser.mockReturnValue(pending.promise)
      const test = setup()
      const launch = test.launch()
      if (change === 'close') test.props.show = false
      if (change === 'reopen') {
        test.props.show = false
        test.props.show = true
      }
      if (change === 'account') test.props.account = { ...test.props.account, id: 42 }
      if (change === 'proxy') test.props.account = { ...test.props.account, proxy_id: 22 }
      if (change === 'unmount') test.scope.stop()
      pending.reject(expired)
      await launch
      expect(test.oauth.generateAuthUrl).not.toHaveBeenCalled()
      expect(api.showSuccess).not.toHaveBeenCalled()
      expect(api.showError).not.toHaveBeenCalled()
      expect(test.launching.value).toBe(false)
      expect(test.ready.value).toBe(false)
      test.scope.stop()
    }
  )

  it('does not let a stale completion clear a newer launch spinner', async () => {
    const old = deferred<typeof success>()
    const current = deferred<typeof success>()
    api.launchAuthBrowser.mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise)
    const test = setup()
    const first = test.launch()
    test.props.show = false
    test.props.show = true
    const second = test.launch()
    old.resolve(success)
    await first
    expect(test.launching.value).toBe(true)
    expect(api.showSuccess).not.toHaveBeenCalled()
    current.resolve(success)
    await second
    expect(test.launching.value).toBe(false)
    expect(api.showSuccess).toHaveBeenCalledOnce()
    test.scope.stop()
  })

  it('stops when regeneration fails', async () => {
    api.launchAuthBrowser.mockRejectedValue(expired)
    const test = setup()
    test.oauth.generateAuthUrl.mockResolvedValue(false)
    await test.launch()
    expect(api.launchAuthBrowser).toHaveBeenCalledOnce()
    expect(test.launching.value).toBe(false)
    test.scope.stop()
  })

  it.each([
    { launched: false, already_running: true, expected: 'success' },
    { launched: false, output: 'fixture failed', expected: 'error' }
  ])('interprets a non-launched response as $expected', async ({ expected, ...result }) => {
    api.launchAuthBrowser.mockResolvedValue(result)
    const test = setup()
    await test.launch()
    expect(expected === 'success' ? api.showSuccess : api.showError).toHaveBeenCalledOnce()
    expect(test.oauth.generateAuthUrl).not.toHaveBeenCalled()
    expect(test.ready.value).toBe(false)
    test.scope.stop()
  })

  it.each(['session', 'close', 'reopen', 'account', 'proxy', 'unmount'] as const)(
    'invalidates successful browser evidence after %s',
    async (change) => {
      api.launchAuthBrowser.mockResolvedValue(success)
      const test = setup()
      await test.launch()
      expect(test.ready.value).toBe(true)
      if (change === 'session') {
        test.oauth.sessionId.value = 'replacement'
        test.oauth.sessionId.value = 'session-old'
      }
      if (change === 'close') test.props.show = false
      if (change === 'reopen') {
        test.props.show = false
        test.props.show = true
      }
      if (change === 'account') test.props.account = { ...test.props.account, id: 42 }
      if (change === 'proxy') test.props.account = { ...test.props.account, proxy_id: 22 }
      if (change === 'unmount') test.scope.stop()
      expect(test.ready.value).toBe(false)
      test.scope.stop()
    }
  )

  it.each(['success', 'expired'] as const)(
    'ignores a late %s when the OAuth session changed during launch',
    async (outcome) => {
      const pending = deferred<typeof success>()
      api.launchAuthBrowser.mockReturnValue(pending.promise)
      const test = setup()
      const launch = test.launch()
      test.oauth.sessionId.value = 'replacement'
      if (outcome === 'success') pending.resolve(success)
      else pending.reject(expired)
      await launch
      expect(test.ready.value).toBe(false)
      expect(test.oauth.generateAuthUrl).not.toHaveBeenCalled()
      expect(api.showSuccess).not.toHaveBeenCalled()
      expect(api.showError).not.toHaveBeenCalled()
      test.scope.stop()
    }
  )

  it('revokes previous evidence while another launch is only in progress', async () => {
    api.launchAuthBrowser.mockResolvedValueOnce(success).mockResolvedValueOnce({
      launched: false, already_running: true
    })
    const test = setup()
    await test.launch()
    expect(test.ready.value).toBe(true)
    await test.launch()
    expect(test.ready.value).toBe(false)
    expect(test.clear).toHaveBeenCalledTimes(2)
    test.scope.stop()
  })

  it('requires a session and the OpenAI platform', async () => {
    const test = setup()
    test.oauth.sessionId.value = ''
    await test.launch()
    expect(api.showError).toHaveBeenCalledOnce()
    test.props.account = { ...test.props.account, platform: 'anthropic' }
    test.oauth.sessionId.value = 'session-other'
    await test.launch()
    expect(api.launchAuthBrowser).not.toHaveBeenCalled()
    test.scope.stop()
  })
})
