import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useAccountOAuth } from '../useAccountOAuth'
import { useGeminiOAuth } from '../useGeminiOAuth'
import { useAntigravityOAuth } from '../useAntigravityOAuth'
import { useGrokOAuth } from '../useGrokOAuth'

const mocks = vi.hoisted(() => {
  const provider = () => ({
    generateAuthUrl: vi.fn(),
    exchangeCode: vi.fn(),
    refreshAntigravityToken: vi.fn(),
    refreshGrokToken: vi.fn(),
    validateSSOToken: vi.fn(),
    authorizePassword: vi.fn()
  })
  return {
    accounts: provider(),
    gemini: provider(),
    antigravity: provider(),
    grok: provider(),
    showError: vi.fn()
  }
})
vi.mock('@/api/admin', () => ({ adminAPI: mocks }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

const providers = [
  { name: 'accounts', create: useAccountOAuth },
  { name: 'gemini', create: useGeminiOAuth },
  { name: 'antigravity', create: useAntigravityOAuth },
  { name: 'grok', create: useGrokOAuth }
] as const

describe.each(providers)('$name OAuth request lifecycle', ({ name, create }) => {
  beforeEach(() => vi.resetAllMocks())

  const generate = (client: ReturnType<typeof create>) =>
    name === 'accounts'
      ? (client as ReturnType<typeof useAccountOAuth>).generateAuthUrl('oauth', 1)
      : (client as ReturnType<typeof useGeminiOAuth>).generateAuthUrl(1)

  it.each(['resolve', 'reject'] as const)('ignores a %s after reset without clearing newer loading', async (outcome) => {
    const old = deferred<Record<string, string>>()
    const next = deferred<Record<string, string>>()
    mocks[name].generateAuthUrl.mockReturnValueOnce(old.promise).mockReturnValueOnce(next.promise)
    const client = create()
    const first = generate(client)
    client.resetState()
    const second = generate(client)
    if (outcome === 'resolve') old.resolve({ auth_url: 'old', session_id: 'old', state: 'old' })
    else old.reject(new Error('old request failed'))
    expect(await first).toBe(false)
    expect(client.authUrl.value).toBe('')
    expect(client.sessionId.value).toBe('')
    expect(client.error.value).toBe('')
    expect(client.loading.value).toBe(true)
    expect(mocks.showError).not.toHaveBeenCalled()
    next.resolve({ auth_url: 'new', session_id: 'new', state: 'new' })
    expect(await second).toBe(true)
    expect(client.sessionId.value).toBe('new')
    expect(client.loading.value).toBe(false)
  })

  it('discards an exchanged credential after reset', async () => {
    const pending = deferred<Record<string, unknown>>()
    mocks[name].exchangeCode.mockReturnValue(pending.promise)
    const client = create()
    let request: Promise<unknown>
    if (name === 'accounts') {
      const accountClient = client as ReturnType<typeof useAccountOAuth>
      accountClient.authCode.value = 'fixture'
      accountClient.sessionId.value = 'fixture'
      request = accountClient.exchangeAuthCode('oauth', 1)
    } else {
      request = (client as ReturnType<typeof useGeminiOAuth>).exchangeAuthCode({
        code: 'fixture', sessionId: 'fixture', state: 'fixture', proxyId: 1
      })
    }
    client.resetState()
    pending.resolve({})
    expect(await request).toBeNull()
    expect(client.error.value).toBe('')
    expect(mocks.showError).not.toHaveBeenCalled()
  })
})

describe('manual OAuth input cancellation', () => {
  beforeEach(() => vi.resetAllMocks())

  it.each(['cookie', 'antigravity-refresh', 'grok-refresh', 'grok-sso', 'grok-password'] as const)(
    'discards %s results after reset',
    async (method) => {
      const pending = deferred<Record<string, unknown>>()
      const claude = useAccountOAuth()
      const antigravity = useAntigravityOAuth()
      const grok = useGrokOAuth()
      const cases = {
        cookie: {
          api: mocks.accounts.exchangeCode,
          invoke: () => claude.cookieAuth('oauth', 'fixture'),
          reset: claude.resetState
        },
        'antigravity-refresh': {
          api: mocks.antigravity.refreshAntigravityToken,
          invoke: () => antigravity.validateRefreshToken('fixture'),
          reset: antigravity.resetState
        },
        'grok-refresh': {
          api: mocks.grok.refreshGrokToken,
          invoke: () => grok.validateRefreshToken('fixture'),
          reset: grok.resetState
        },
        'grok-sso': {
          api: mocks.grok.validateSSOToken,
          invoke: () => grok.validateSSOToken('fixture'),
          reset: grok.resetState
        },
        'grok-password': {
          api: mocks.grok.authorizePassword,
          invoke: () => grok.authorizePassword('fixture'),
          reset: grok.resetState
        }
      }
      const selected = cases[method]
      selected.api.mockReturnValue(pending.promise)
      const request = selected.invoke()
      selected.reset()
      pending.resolve({})
      expect(await request).toBeNull()
    }
  )
})
