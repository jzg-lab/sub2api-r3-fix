import { defineComponent, h, ref } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account } from '@/types'
import AdminModal from '../ReAuthAccountModal.vue'
import AccountModal from '@/components/account/ReAuthAccountModal.vue'

const api = vi.hoisted(() => ({
  exchangeCode: vi.fn(),
  exchangeAuthCode: vi.fn(),
  launchAuthBrowser: vi.fn(),
  applyOAuthCredentials: vi.fn(),
  generateAuthUrl: vi.fn(),
  sessionId: 'bound-session',
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({ adminAPI: { accounts: api } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => api }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))

function oauthClient() {
  return {
    authUrl: ref(''),
    sessionId: ref(api.sessionId),
    state: ref(''),
    oauthState: ref('bound-state'),
    loading: ref(false),
    error: ref(''),
    resetState: vi.fn(),
    generateAuthUrl: api.generateAuthUrl,
    exchangeAuthCode: api.exchangeAuthCode,
    buildCredentials: () => ({ access_token: 'fixture' }),
    buildExtraInfo: () => undefined
  }
}

vi.mock('@/composables/useAccountOAuth', () => ({ useAccountOAuth: oauthClient }))
vi.mock('@/composables/useOpenAIOAuth', () => ({ useOpenAIOAuth: oauthClient }))
vi.mock('@/composables/useGeminiOAuth', () => ({ useGeminiOAuth: oauthClient }))
vi.mock('@/composables/useAntigravityOAuth', () => ({ useAntigravityOAuth: oauthClient }))
vi.mock('@/composables/useGrokOAuth', () => ({ useGrokOAuth: oauthClient }))

const Dialog = defineComponent({
  setup(_, { slots }) {
    return () => h('div', [slots.default?.(), slots.footer?.()])
  }
})

const Flow = defineComponent({
  props: ['showAuthBrowserLaunch', 'authBrowserLaunching', 'authBrowserReady', 'showRefreshTokenOption'],
  emits: ['launch-auth-browser'],
  setup(_, { expose }) {
    expose({ reset: vi.fn(), authCode: 'fixture', oauthState: 'bound-state' })
    return () => h('div')
  }
})

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => {
    resolve = yes
    reject = no
  })
  return { promise, resolve, reject }
}

function account(id = 1): Account {
  return {
    id,
    name: `fixture-${id}`,
    platform: 'anthropic',
    type: 'oauth',
    updated_at: `2026-10-03T00:00:0${id}.123456Z`,
    credentials: {}
  } as Account
}

describe.each([
  ['admin', AdminModal],
  ['account', AccountModal]
] as const)('%s reauthorization lifecycle', (_, component) => {
  beforeEach(() => {
    vi.resetAllMocks()
    api.sessionId = 'bound-session'
  })

  function setup() {
    const wrapper = mount(component, {
      props: { show: true, account: account() },
      global: {
        stubs: { BaseDialog: Dialog, OAuthAuthorizationFlow: Flow, Icon: true }
      }
    })
    const actions = wrapper.vm.$.setupState as unknown as {
      handleCookieAuth: (input: string) => Promise<void>
      handleClose: () => void
      handleGenerateUrl: () => Promise<void>
      handleLaunchAuthBrowser: () => Promise<void>
      handleExchangeCode: () => Promise<void>
      handleValidateRefreshToken: (input: string) => Promise<void>
    }
    return { wrapper, actions }
  }

  it('passes the one-shot server proof with the same account revision', async () => {
    const { wrapper, actions } = setup()
    const original = { ...account(), platform: 'openai' as const, proxy_id: 7 }
    await wrapper.setProps({ account: original })
    api.exchangeAuthCode.mockResolvedValue({ reauthorization_proof: 'bound-proof' })
    api.applyOAuthCredentials.mockResolvedValue(original)
    api.launchAuthBrowser.mockResolvedValue({ launched: true })
    await actions.handleLaunchAuthBrowser()
    await actions.handleExchangeCode()
    expect(api.applyOAuthCredentials).toHaveBeenCalledWith(original.id, expect.objectContaining({
      reauthorization_proof: 'bound-proof',
      expected_updated_at: original.updated_at
    }))
    expect(wrapper.emitted('reauthorized')).toEqual([[original]])
    wrapper.unmount()
  })

  it.each(['missing', 'failed', 'in-progress', 'rejected'] as const)(
    'rejects programmatic submission with %s browser evidence',
    async (result) => {
      const { wrapper, actions } = setup()
      await wrapper.setProps({ account: { ...account(), platform: 'openai', proxy_id: 7 } })
      if (result === 'rejected') api.launchAuthBrowser.mockRejectedValue({ reason: 'AUTH_BROWSER_LAUNCH_FAILED' })
      else api.launchAuthBrowser.mockResolvedValue({
        launched: false, already_running: result === 'in-progress'
      })
      if (result !== 'missing') await actions.handleLaunchAuthBrowser()
      await actions.handleExchangeCode()
      expect(api.exchangeAuthCode).not.toHaveBeenCalled()
      expect(api.applyOAuthCredentials).not.toHaveBeenCalled()
      expect(wrapper.findComponent(Flow).props('authBrowserReady')).toBe(false)
      wrapper.unmount()
    }
  )

  it('keeps the captured revision and revokes browser evidence on regeneration', async () => {
    const { wrapper, actions } = setup()
    const original = { ...account(), platform: 'openai' as const, proxy_id: 7 }
    await wrapper.setProps({ account: original })
    api.launchAuthBrowser.mockResolvedValue({ launched: true })
    await actions.handleLaunchAuthBrowser()
    await wrapper.setProps({ account: { ...original, updated_at: 'later-revision' } })
    await actions.handleGenerateUrl()
    expect(api.generateAuthUrl).toHaveBeenCalledWith(7, undefined, {
      accountId: original.id, expectedUpdatedAt: original.updated_at
    })
    await actions.handleExchangeCode()
    expect(api.exchangeAuthCode).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('does not expose unbound refresh-token import for OpenAI reauthorization', async () => {
    const { wrapper, actions } = setup()
    await wrapper.setProps({ account: { ...account(), platform: 'openai' as const } })
    expect(wrapper.findComponent(Flow).props('showRefreshTokenOption')).not.toBe(true)
    if (component === AdminModal) {
      await actions.handleValidateRefreshToken('fixture')
      expect(api.applyOAuthCredentials).not.toHaveBeenCalled()
      expect(api.showError).toHaveBeenCalled()
    }
    wrapper.unmount()
  })

  it('binds authorization generation to the original account, revision and proxy', async () => {
    const { wrapper, actions } = setup()
    const original = { ...account(), platform: 'openai' as const, proxy_id: 7 }
    await wrapper.setProps({ account: original })
    await actions.handleGenerateUrl()
    expect(api.generateAuthUrl).toHaveBeenCalledWith(7, undefined, {
      accountId: original.id, expectedUpdatedAt: original.updated_at
    })
    wrapper.unmount()
  })

  it('exposes the bound browser launcher only for OpenAI reauthorization', async () => {
    api.launchAuthBrowser.mockResolvedValue({ launched: true, already_running: false })
    const { wrapper } = setup()
    expect(wrapper.findComponent(Flow).props('showAuthBrowserLaunch')).toBe(false)
    await wrapper.setProps({ account: { ...account(), platform: 'openai' } })
    const flow = wrapper.findComponent(Flow)
    expect(flow.props('showAuthBrowserLaunch')).toBe(true)
    flow.vm.$emit('launch-auth-browser')
    await flushPromises()
    expect(api.launchAuthBrowser).toHaveBeenCalledWith('bound-session')
    expect(api.showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('does not launch without an account-bound authorization session', async () => {
    api.sessionId = ''
    const { wrapper } = setup()
    await wrapper.setProps({ account: { ...account(), platform: 'openai' } })
    wrapper.findComponent(Flow).vm.$emit('launch-auth-browser')
    await flushPromises()
    expect(api.showError).toHaveBeenCalledWith('授权会话缺失，请先重新生成授权链接')
    expect(api.launchAuthBrowser).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it.each(['switch', 'reopen', 'close', 'unmount'] as const)(
    'discards an old exchange after %s',
    async (change) => {
      const pending = deferred<Record<string, unknown>>()
      api.exchangeCode.mockReturnValue(pending.promise)
      api.applyOAuthCredentials.mockResolvedValue(account(2))
      const { wrapper, actions } = setup()
      const request = actions.handleCookieAuth('fixture')
      if (change === 'switch') await wrapper.setProps({ account: account(2) })
      if (change === 'reopen') {
        await wrapper.setProps({ show: false })
        await wrapper.setProps({ show: true })
      }
      if (change === 'close') actions.handleClose()
      if (change === 'unmount') wrapper.unmount()
      const closesBeforeCompletion = wrapper.emitted('close')?.length ?? 0
      pending.resolve({})
      await request
      expect(api.applyOAuthCredentials).not.toHaveBeenCalled()
      expect(api.showSuccess).not.toHaveBeenCalled()
      expect(api.showError).not.toHaveBeenCalled()
      expect(wrapper.emitted('reauthorized')).toBeUndefined()
      expect(wrapper.emitted('close')?.length ?? 0).toBe(closesBeforeCompletion)
      wrapper.unmount()
    }
  )

  it('does not close a newer dialog when an earlier database write finishes', async () => {
    const pending = deferred<Account>()
    api.exchangeCode.mockResolvedValue({})
    api.applyOAuthCredentials.mockReturnValue(pending.promise)
    const { wrapper, actions } = setup()
    const request = actions.handleCookieAuth('fixture')
    await flushPromises()
    expect(api.applyOAuthCredentials).toHaveBeenCalledWith(1, expect.objectContaining({
      expected_updated_at: account().updated_at
    }))
    await wrapper.setProps({ account: account(2) })
    pending.resolve(account())
    await request
    expect(wrapper.emitted('reauthorized')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(api.showSuccess).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('serializes submissions through exchange and credential persistence', async () => {
    const pending = deferred<Account>()
    api.exchangeCode.mockResolvedValue({})
    api.applyOAuthCredentials.mockReturnValue(pending.promise)
    const { wrapper, actions } = setup()
    const request = actions.handleCookieAuth('fixture')
    await flushPromises()
    const duplicate = actions.handleCookieAuth('duplicate')
    await flushPromises()
    const exchangeCount = api.exchangeCode.mock.calls.length
    const applyCount = api.applyOAuthCredentials.mock.calls.length
    pending.resolve(account())
    await Promise.all([request, duplicate])
    expect(exchangeCount).toBe(1)
    expect(applyCount).toBe(1)
    expect(wrapper.emitted('reauthorized')).toEqual([[account()]])
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })
})
