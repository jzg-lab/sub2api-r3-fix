import { effectScope, reactive, ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account } from '@/types'
import type { AuthBrowserLogin } from '@/api/admin/accounts'
import { useAutomaticReauth } from '../useAutomaticReauth'
import { useReauthSession } from '../useReauthSession'
import type { useOpenAIOAuth } from '../useOpenAIOAuth'

const api = vi.hoisted(() => ({ getById: vi.fn(), automateAuthBrowser: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: api } }))

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((yes) => { resolve = yes })
  return { promise, resolve }
}

function loginFixture(): AuthBrowserLogin {
  return { email: 'test@example.invalid', password: 'local-fixture', totp_secret: '' }
}

function setup() {
  const scope = effectScope()
  const props = reactive({
    show: true,
    account: {
      id: 41, platform: 'openai', type: 'oauth', proxy_id: 21,
      updated_at: '2026-10-06T01:00:00Z', reauthorization_revision: 'old-revision'
    } as Account
  })
  api.getById.mockResolvedValue({
    ...props.account, updated_at: '2026-10-06T02:00:00Z', reauthorization_revision: 'fresh-revision'
  })
  const oauth = {
    sessionId: ref('old-session'), oauthState: ref('old-state'),
    resetState: vi.fn(), generateAuthUrl: vi.fn(), exchangeAuthCode: vi.fn()
  }
  oauth.resetState.mockImplementation(() => { oauth.sessionId.value = ''; oauth.oauthState.value = '' })
  oauth.generateAuthUrl.mockImplementation(async () => {
    oauth.sessionId.value = 'bound-session'
    oauth.oauthState.value = 'bound-state'
    return true
  })
  oauth.exchangeAuthCode.mockResolvedValue({ reauthorization_proof: 'local-proof' })
  api.automateAuthBrowser.mockResolvedValue({
    launched: true, code: 'local-code', state: 'bound-state'
  })
  const apply = vi.fn().mockResolvedValue(undefined)
  const state = scope.run(() => {
    const session = useReauthSession(props)
    return {
      session,
      ...useAutomaticReauth(session, oauth as unknown as ReturnType<typeof useOpenAIOAuth>, apply)
    }
  })!
  return { ...state, props, oauth, scope, apply }
}

describe('automatic reauthorization', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    vi.stubGlobal('location', { protocol: 'http:', hostname: '127.0.0.1' })
  })
  afterEach(() => vi.unstubAllGlobals())

  it('uses the original account/proxy, fresh identity revision and one-use proof', async () => {
    const test = setup()
    const login = loginFixture()
    await test.start(login)
    expect(api.getById).toHaveBeenCalledWith(41)
    expect(test.oauth.generateAuthUrl).toHaveBeenCalledWith(21, undefined, {
      accountId: 41, expectedUpdatedAt: '2026-10-06T02:00:00Z',
      expectedAuthorizationRevision: 'fresh-revision'
    })
    expect(test.oauth.exchangeAuthCode).toHaveBeenCalledWith(
      'local-code', 'bound-session', 'bound-state', 21
    )
    expect(test.apply).toHaveBeenCalledOnce()
    expect(test.apply.mock.calls[0][0].account.id).toBe(41)
    expect(Object.values(login).every(value => value === '')).toBe(true)
    expect(test.running.value).toBe(false)
    expect(test.session.busy.value).toBe(false)
    test.scope.stop()
  })

  it('serializes against other authorization actions and never replays input', async () => {
    const pending = deferred<{ launched: boolean; code: string; state: string }>()
    const test = setup()
    api.automateAuthBrowser.mockReturnValue(pending.promise)
    const first = test.start(loginFixture())
    await vi.waitFor(() => expect(api.automateAuthBrowser).toHaveBeenCalledOnce())
    const duplicate = loginFixture()
    await test.start(duplicate)
    const manual = vi.fn()
    await test.session.run(manual)
    expect(manual).not.toHaveBeenCalled()
    expect(Object.values(duplicate).every(value => value === '')).toBe(true)
    expect(api.automateAuthBrowser).toHaveBeenCalledOnce()
    pending.resolve({ launched: true, code: 'local-code', state: 'bound-state' })
    await first
    expect(test.apply).toHaveBeenCalledOnce()
    test.scope.stop()
  })

  it.each(['cancel', 'close', 'account', 'proxy', 'unmount'])(
    'aborts and ignores a late callback after %s', async (change) => {
      const pending = deferred<{ launched: boolean; code: string; state: string }>()
      const test = setup()
      api.automateAuthBrowser.mockReturnValue(pending.promise)
      const login = loginFixture()
      const operation = test.start(login)
      await vi.waitFor(() => expect(api.automateAuthBrowser).toHaveBeenCalledOnce())
      const signal = api.automateAuthBrowser.mock.calls[0][2] as AbortSignal
      if (change === 'cancel') test.cancel()
      if (change === 'close') test.props.show = false
      if (change === 'account') test.props.account = { ...test.props.account, id: 42 }
      if (change === 'proxy') test.props.account = { ...test.props.account, proxy_id: 22 }
      if (change === 'unmount') test.scope.stop()
      expect(signal.aborted).toBe(true)
      expect(Object.values(login).every(value => value === '')).toBe(true)
      pending.resolve({ launched: true, code: 'local-code', state: 'bound-state' })
      await operation
      expect(test.apply).not.toHaveBeenCalled()
      expect(test.oauth.exchangeAuthCode).not.toHaveBeenCalled()
      expect(test.error.value).toBe('')
      test.scope.stop()
    }
  )

  it.each(['account read', 'session creation', 'code exchange'])(
    'does not start the next phase when canceled during %s', async (phase) => {
      const test = setup()
      const pending = deferred<unknown>()
      if (phase === 'account read') api.getById.mockReturnValue(pending.promise)
      if (phase === 'session creation') test.oauth.generateAuthUrl.mockReturnValue(pending.promise)
      if (phase === 'code exchange') test.oauth.exchangeAuthCode.mockReturnValue(pending.promise)
      const login = loginFixture()
      const operation = test.start(login)
      if (phase === 'session creation') await vi.waitFor(() => expect(test.oauth.generateAuthUrl).toHaveBeenCalled())
      if (phase === 'code exchange') await vi.waitFor(() => expect(test.oauth.exchangeAuthCode).toHaveBeenCalled())
      test.cancel()
      expect(Object.values(login).every(value => value === '')).toBe(true)
      pending.resolve(phase === 'account read' ? test.props.account :
        phase === 'session creation' ? true : { reauthorization_proof: 'local-proof' })
      await operation
      expect(test.apply).not.toHaveBeenCalled()
      if (phase !== 'code exchange') expect(api.automateAuthBrowser).not.toHaveBeenCalled()
      test.scope.stop()
    }
  )

  it.each(['wrong state', 'missing code', 'already running', 'missing proof', 'request error'])(
    'fails closed without exposing request data: %s', async (fault) => {
      const test = setup()
      if (fault === 'wrong state') api.automateAuthBrowser.mockResolvedValue({ launched: true, code: 'x', state: 'foreign' })
      if (fault === 'missing code') api.automateAuthBrowser.mockResolvedValue({ launched: true, state: 'bound-state' })
      if (fault === 'already running') api.automateAuthBrowser.mockResolvedValue({ already_running: true })
      if (fault === 'missing proof') test.oauth.exchangeAuthCode.mockResolvedValue({})
      if (fault === 'request error') api.automateAuthBrowser.mockRejectedValue(new Error('DO_NOT_DISPLAY_REQUEST'))
      await test.start(loginFixture())
      expect(test.apply).not.toHaveBeenCalled()
      expect(test.error.value).not.toBe('')
      expect(test.error.value).not.toContain('DO_NOT_DISPLAY_REQUEST')
      test.scope.stop()
    }
  )

  it('does not send login input over a remote plaintext connection', async () => {
    const test = setup()
    vi.stubGlobal('location', { protocol: 'http:', hostname: 'lan.example.invalid' })
    const login = loginFixture()
    await test.start(login)
    expect(api.getById).not.toHaveBeenCalled()
    expect(api.automateAuthBrowser).not.toHaveBeenCalled()
    expect(test.error.value).toContain('HTTPS')
    expect(Object.values(login).every(value => value === '')).toBe(true)
    test.scope.stop()
  })
})
