import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn()
  })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      const messages: Record<string, string> = {
        'admin.accounts.oauth.openai.failedToExchangeCode': 'OpenAI 授权码兑换失败',
        'admin.accounts.oauth.openai.errors.OPENAI_OAUTH_PROXY_REQUIRED':
          '未设置代理，当前服务器无法直连 OpenAI，导致 OpenAI OAuth 请求失败。请先选择可访问 OpenAI 的代理后重试；如果授权码已失效，请重新生成授权链接。'
      }
      return messages[key] ?? key
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      generateAuthUrl: vi.fn(),
      exchangeCode: vi.fn(),
      refreshOpenAIToken: vi.fn()
    }
  }
}))

import { useOpenAIOAuth } from '@/composables/useOpenAIOAuth'
import { adminAPI } from '@/api/admin'

beforeEach(() => vi.clearAllMocks())

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

async function bindOAuthSession(
  oauth: ReturnType<typeof useOpenAIOAuth>,
  proxyId = 7,
  sessionId = 'session-id'
) {
  vi.mocked(adminAPI.accounts.generateAuthUrl).mockResolvedValueOnce({
    auth_url: 'https://example.test/?state=state',
    session_id: sessionId
  })
  expect(await oauth.generateAuthUrl(proxyId)).toBe(true)
}

describe('useOpenAIOAuth.buildCredentials', () => {
  it('should keep client_id when token response contains it', () => {
    const oauth = useOpenAIOAuth()
    const creds = oauth.buildCredentials({
      access_token: 'at',
      refresh_token: 'rt',
      client_id: 'app_test_client',
      expires_at: 1700000000
    })

    expect(creds.client_id).toBe('app_test_client')
    expect(creds.access_token).toBe('at')
    expect(creds.refresh_token).toBe('rt')
  })

  it('should keep legacy behavior when client_id is missing', () => {
    const oauth = useOpenAIOAuth()
    const creds = oauth.buildCredentials({
      access_token: 'at',
      refresh_token: 'rt',
      expires_at: 1700000000
    })

    expect(Object.prototype.hasOwnProperty.call(creds, 'client_id')).toBe(false)
    expect(creds.access_token).toBe('at')
    expect(creds.refresh_token).toBe('rt')
  })

  it('should keep ChatGPT subscription expiration from token response', () => {
    const oauth = useOpenAIOAuth()
    const creds = oauth.buildCredentials({
      access_token: 'at',
      refresh_token: 'rt',
      expires_at: 1700000000,
      plan_type: 'team',
      subscription_expires_at: '2026-07-20T19:22:48+00:00'
    })

    expect(creds.plan_type).toBe('team')
    expect(creds.subscription_expires_at).toBe('2026-07-20T19:22:48+00:00')
  })
})

describe('useOpenAIOAuth.exchangeAuthCode', () => {
  it('returns the server assignment when it matches the immutable authorization proxy', async () => {
    vi.mocked(adminAPI.accounts.exchangeCode).mockResolvedValueOnce({ proxy_id: 7 })
    const oauth = useOpenAIOAuth()
    await bindOAuthSession(oauth)
    const result = await oauth.exchangeAuthCode('code', 'session-id', 'state', 7)
    expect(result?.proxy_id).toBe(7)
  })

  it.each([undefined, null, 0, -1, 1.5, '7'])('rejects an unbound exchange result: %s', async (proxy_id) => {
    vi.mocked(adminAPI.accounts.exchangeCode).mockResolvedValueOnce({ proxy_id })
    const oauth = useOpenAIOAuth()
    await bindOAuthSession(oauth)
    expect(await oauth.exchangeAuthCode('code', 'session-id', 'state', 7)).toBeNull()
    expect(oauth.error.value).not.toBe('')
  })

  it('does not return an exchange after the dialog is reset', async () => {
    const response = deferred<Record<string, unknown>>()
    vi.mocked(adminAPI.accounts.exchangeCode).mockReturnValueOnce(response.promise)
    const oauth = useOpenAIOAuth()
    await bindOAuthSession(oauth)
    const pending = oauth.exchangeAuthCode('code', 'session-id', 'state', 7)
    oauth.resetState()
    response.resolve({ proxy_id: 7 })
    expect(await pending).toBeNull()
    expect(oauth.error.value).toBe('')
    expect(oauth.loading.value).toBe(false)
  })

  it('does not clear loading or publish errors from a superseded request', async () => {
    const first = deferred<Record<string, unknown>>()
    const second = deferred<Record<string, unknown>>()
    vi.mocked(adminAPI.accounts.exchangeCode)
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise)
    const oauth = useOpenAIOAuth()
    await bindOAuthSession(oauth)
    const old = oauth.exchangeAuthCode('code', 'session-id', 'state', 7)
    const current = oauth.exchangeAuthCode('code', 'session-id', 'state', 7)
    first.reject(new Error('obsolete'))
    expect(await old).toBeNull()
    expect(oauth.error.value).toBe('')
    expect(oauth.loading.value).toBe(true)
    second.resolve({ proxy_id: 7 })
    expect((await current)?.proxy_id).toBe(7)
    expect(oauth.loading.value).toBe(false)
  })

  it('shows a clear proxy hint when code exchange fails without a proxy', async () => {
    vi.mocked(adminAPI.accounts.exchangeCode).mockRejectedValueOnce({
      status: 502,
      reason: 'OPENAI_OAUTH_PROXY_REQUIRED',
      message: 'OpenAI OAuth token exchange failed: no proxy is configured.'
    })
    const oauth = useOpenAIOAuth()
    await bindOAuthSession(oauth)

    const tokenInfo = await oauth.exchangeAuthCode('code', 'session-id', 'state', 7)

    expect(tokenInfo).toBeNull()
    expect(oauth.error.value).toBe(
      '未设置代理，当前服务器无法直连 OpenAI，导致 OpenAI OAuth 请求失败。请先选择可访问 OpenAI 的代理后重试；如果授权码已失效，请重新生成授权链接。'
    )
  })

  it('rejects a mutable form proxy that differs from the authorization session', async () => {
    const oauth = useOpenAIOAuth()
    await bindOAuthSession(oauth, 7)

    expect(await oauth.exchangeAuthCode('code', 'session-id', 'state', 8)).toBeNull()
    expect(adminAPI.accounts.exchangeCode).not.toHaveBeenCalled()
  })

  it('rejects a server proxy that differs from the authorization session', async () => {
    vi.mocked(adminAPI.accounts.exchangeCode).mockResolvedValueOnce({ proxy_id: 8 })
    const oauth = useOpenAIOAuth()
    await bindOAuthSession(oauth, 7)

    expect(await oauth.exchangeAuthCode('code', 'session-id', 'state', 7)).toBeNull()
    expect(oauth.error.value).not.toBe('')
  })
})

describe('useOpenAIOAuth session generation', () => {
  it('requires a valid proxy before generating an authorization URL', async () => {
    const oauth = useOpenAIOAuth()

    expect(await oauth.generateAuthUrl(null)).toBe(false)
    expect(adminAPI.accounts.generateAuthUrl).not.toHaveBeenCalled()
    expect(oauth.authorizationProxyId.value).toBeNull()
  })

  it('keeps the newer authorization when responses arrive in reverse order', async () => {
    type Result = Awaited<ReturnType<typeof adminAPI.accounts.generateAuthUrl>>
    const first = deferred<Result>()
    const second = deferred<Result>()
    vi.mocked(adminAPI.accounts.generateAuthUrl)
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise)
    const oauth = useOpenAIOAuth()
    const old = oauth.generateAuthUrl(7)
    const current = oauth.generateAuthUrl(8)
    second.resolve({ auth_url: 'https://example.test/?state=new', session_id: 'new' })
    expect(await current).toBe(true)
    first.resolve({ auth_url: 'https://example.test/?state=old', session_id: 'old' })
    expect(await old).toBe(false)
    expect(oauth.sessionId.value).toBe('new')
    expect(oauth.oauthState.value).toBe('new')
    expect(oauth.authorizationProxyId.value).toBe(8)
  })

  it('does not restore a discarded authorization after closing the dialog', async () => {
    type Result = Awaited<ReturnType<typeof adminAPI.accounts.generateAuthUrl>>
    const response = deferred<Result>()
    vi.mocked(adminAPI.accounts.generateAuthUrl).mockReturnValueOnce(response.promise)
    const oauth = useOpenAIOAuth()
    const pending = oauth.generateAuthUrl(7)
    oauth.resetState()
    response.resolve({ auth_url: 'https://example.test/?state=old', session_id: 'old' })
    expect(await pending).toBe(false)
    expect(oauth.sessionId.value).toBe('')
    expect(oauth.authUrl.value).toBe('')
    expect(oauth.authorizationProxyId.value).toBeNull()
  })

  it('does not return refresh credentials from a discarded dialog', async () => {
    type Result = Awaited<ReturnType<typeof adminAPI.accounts.refreshOpenAIToken>>
    const response = deferred<Result>()
    vi.mocked(adminAPI.accounts.refreshOpenAIToken).mockReturnValueOnce(response.promise)
    const oauth = useOpenAIOAuth()
    const pending = oauth.validateRefreshToken('fixture', 7)
    oauth.resetState()
    response.resolve({ proxy_id: 7 } as Result)
    expect(await pending).toBeNull()
    expect(oauth.loading.value).toBe(false)
  })
})
