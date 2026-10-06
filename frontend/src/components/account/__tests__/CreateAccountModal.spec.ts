import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const {
  createAccountMock,
  probeUpstreamBillingMock,
  syncUpstreamModelsMock,
  showWarningMock,
  showErrorMock,
  showSuccessMock,
  generateAuthUrlMock,
  launchAuthBrowserMock,
  exchangeCodeMock,
  importCodexSessionMock,
  createOpenAICodexPATMock,
  authIsSimpleMode,
} = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  probeUpstreamBillingMock: vi.fn(),
  syncUpstreamModelsMock: vi.fn(),
  showWarningMock: vi.fn(),
  showErrorMock: vi.fn(),
  showSuccessMock: vi.fn(),
  generateAuthUrlMock: vi.fn(),
  launchAuthBrowserMock: vi.fn(),
  exchangeCodeMock: vi.fn(),
  importCodexSessionMock: vi.fn(),
  createOpenAICodexPATMock: vi.fn(),
  authIsSimpleMode: { value: true },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: showSuccessMock,
    showWarning: showWarningMock,
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return authIsSimpleMode.value
    },
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      probeUpstreamBilling: probeUpstreamBillingMock,
      syncUpstreamModels: syncUpstreamModelsMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      importCodexSession: importCodexSessionMock,
      createOpenAICodexPAT: createOpenAICodexPATMock,
      generateAuthUrl: generateAuthUrlMock,
      launchAuthBrowser: launchAuthBrowserMock,
      exchangeCode: exchangeCodeMock,
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([{ id: 12, name: 'profile-12' }]),
    },
  },
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import CreateAccountModal from '../CreateAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  props: {
    showManualOption: Boolean,
    showCodexSessionImportOption: Boolean,
    showAgentIdentityOption: Boolean,
    showCodexPatOption: Boolean,
    initialInputMethod: String,
    loading: Boolean,
    authBrowserLaunching: Boolean,
  },
  data: () => ({ inputMethod: 'manual', authCode: 'test-code', oauthState: 'test-state' }),
  methods: { reset() {} },
  emits: ['import-codex-session', 'import-codex-pat', 'generate-url', 'launch-auth-browser'],
  template: `
    <div>
      <button data-testid="import-codex-session" @click="$emit('import-codex-session', 'session-json')">session</button>
      <button data-testid="import-codex-pat" @click="$emit('import-codex-pat', 'pat-token')">pat</button>
      <button data-testid="generate-url" @click="$emit('generate-url')">authorize</button>
      <button data-testid="launch-auth-browser" @click="$emit('launch-auth-browser')">launch</button>
    </div>
  `,
})

const GroupSelectorStub = defineComponent({
  name: 'GroupSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
  },
  emits: ['update:modelValue'],
  template: `
    <button
      type="button"
      data-testid="select-pricing-groups"
      @click="$emit('update:modelValue', [1, 2])"
    >
      groups
    </button>
  `,
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
    platform: String,
    syncCredentials: Object,
  },
  emits: ['update:modelValue', 'upstream-synced'],
  template: `<button
    type="button"
    data-testid="model-whitelist-selector"
    @click="$emit('update:modelValue', ['public-glm']); $emit('upstream-synced')"
  >models</button>`,
})

function mountModal(groups: any[] = [], show = true) {
  return mount(CreateAccountModal, {
    props: { show, proxies: [], groups },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: OAuthAuthorizationFlowStub,
        ConfirmDialog: true,
        Select: true,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: GroupSelectorStub,
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        QuotaLimitCard: true,
      },
    },
  })
}

async function selectButtonByText(wrapper: ReturnType<typeof mountModal>, text: string) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

async function submitApiKeyAccount(
  platform: 'openai' | 'anthropic',
  enableLongContextBilling = false,
  disableUpstreamBillingProbe = false
) {
  const wrapper = mountModal()
  await selectButtonByText(wrapper, platform === 'openai' ? 'OpenAI' : 'admin.accounts.claudeConsole')
  if (platform === 'openai') {
    await selectButtonByText(wrapper, 'API Key')
  }
  await wrapper.get('form#create-account-form input[type="text"]').setValue(`${platform} account`)
  await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
  if (enableLongContextBilling) {
    await wrapper.get('[data-testid="openai-long-context-billing-toggle"]').trigger('click')
  }
  if (disableUpstreamBillingProbe) {
    await wrapper.get('[data-testid="upstream-billing-auto-probe"]').trigger('click')
  }
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  await flushPromises()
  return wrapper
}

async function openCodexImportStep(
  longContextToggleClicks = 0,
  tlsToggleClicks = 0,
  tlsProfileId?: string
) {
  const wrapper = mountModal()
  await selectButtonByText(wrapper, 'OpenAI')
  await flushPromises()
  for (let click = 0; click < longContextToggleClicks; click += 1) {
    await wrapper.get('[data-testid="openai-long-context-billing-toggle"]').trigger('click')
  }
  for (let click = 0; click < tlsToggleClicks; click += 1) {
    await wrapper.get('[data-testid="create-openai-tls-fingerprint-toggle"]').trigger('click')
  }
  if (tlsProfileId) {
    const profile = wrapper.get('[data-testid="create-openai-tls-fingerprint-profile"]')
    await profile.setValue(tlsProfileId)
    expect((profile.element as HTMLSelectElement).value).toBe(tlsProfileId)
  }
  await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex import')
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  return wrapper
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

async function startOAuthFlow(wrapper: ReturnType<typeof mountModal>) {
  await selectButtonByText(wrapper, 'OpenAI')
  await wrapper.get('form#create-account-form input[type="text"]').setValue('OAuth account')
  wrapper.findComponent({ name: 'ProxySelector' }).vm.$emit('update:modelValue', 23)
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  await wrapper.get('[data-testid="generate-url"]').trigger('click')
  await flushPromises()
}

function authSubmit(wrapper: ReturnType<typeof mountModal>) {
  return wrapper.findAll('button').find(button =>
    /admin\.accounts\.oauth\.(completeAuth|verifying)/.test(button.text())
  )!
}

describe('CreateAccountModal local concurrency', () => {
  it('mounts closed and resets safely across repeated open and close cycles', async () => {
    const wrapper = mountModal([], false)
    await flushPromises()
    expect(wrapper.find('form#create-account-form').exists()).toBe(false)

    for (let cycle = 0; cycle < 2; cycle += 1) {
      await wrapper.setProps({ show: true })
      await flushPromises()
      const name = wrapper.get('form#create-account-form input[type="text"]')
      expect((name.element as HTMLInputElement).value).toBe('')
      await name.setValue('unsaved account')
      await selectButtonByText(wrapper, 'OpenAI')
      await wrapper.setProps({ show: false })
      await flushPromises()
      expect(wrapper.find('form#create-account-form').exists()).toBe(false)
    }
    expect(wrapper.emitted('created')).toBeUndefined()
    wrapper.unmount()
  })

  it('keeps custom concurrency after platform changes and restores the default on reopening', async () => {
    const wrapper = mountModal()
    const limit = () => wrapper.get('[data-testid="account-concurrency"]')
    expect((limit().element as HTMLInputElement).value).toBe('5')
    expect(wrapper.get<HTMLInputElement>('[data-tour="account-form-priority"]').element.value).toBe('2')
    expect((limit().element as HTMLInputElement).readOnly).toBe(false)
    await limit().setValue(75)
    await selectButtonByText(wrapper, 'OpenAI')
    expect((limit().element as HTMLInputElement).value).toBe('75')
    await selectButtonByText(wrapper, 'Grok')
    expect((limit().element as HTMLInputElement).value).toBe('75')
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect((limit().element as HTMLInputElement).value).toBe('5')
    expect(wrapper.get<HTMLInputElement>('[data-tour="account-form-priority"]').element.value).toBe('2')
    wrapper.unmount()
  })

  it('submits the selected concurrency and priority when creating an API key account', async () => {
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'openai', type: 'apikey' })
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('custom account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-key')
    await wrapper.get('[data-testid="account-concurrency"]').setValue(75)
    await wrapper.get('[data-tour="account-form-priority"]').setValue(0)
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(createAccountMock).toHaveBeenCalledWith(expect.objectContaining({ concurrency: 75, priority: 0 }))
    wrapper.unmount()
  })
})

describe('CreateAccountModal OpenAI authorization lifecycle', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'openai', type: 'oauth' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
    showErrorMock.mockReset()
    showSuccessMock.mockReset()
    generateAuthUrlMock.mockReset().mockResolvedValue({
      auth_url: 'https://auth.example.invalid/authorize?state=test-state',
      session_id: 'test-session',
    })
    launchAuthBrowserMock.mockReset().mockResolvedValue({
      launched: true,
      already_running: false,
      proxy_name: 'static-isp',
      output: '',
    })
    exchangeCodeMock.mockReset().mockResolvedValue({ proxy_id: 23, expires_in: 3600 })
  })

  it.each([
    'AUTH_BROWSER_SESSION_NOT_FOUND',
    'AUTH_BROWSER_SESSION_EXPIRED',
    'AUTH_BROWSER_SESSION_INVALID',
    'AUTH_BROWSER_PROXY_ROUTE_STALE',
  ])('refreshes %s once and launches only the replacement session', async (reason) => {
    generateAuthUrlMock
      .mockResolvedValueOnce({
        auth_url: 'https://auth.example.invalid/authorize?state=old-state',
        session_id: 'old-session',
      })
      .mockResolvedValueOnce({
        auth_url: 'https://auth.example.invalid/authorize?state=new-state',
        session_id: 'new-session',
      })
    launchAuthBrowserMock
      .mockRejectedValueOnce({ reason, message: 'authorization session must be replaced' })
      .mockResolvedValueOnce({
        launched: true,
        already_running: false,
        proxy_name: 'static-isp',
        output: '',
      })

    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await wrapper.get('[data-testid="launch-auth-browser"]').trigger('click')
    await flushPromises()

    expect(generateAuthUrlMock).toHaveBeenCalledTimes(2)
    expect(generateAuthUrlMock.mock.calls[1]).toEqual([
      '/admin/openai/generate-auth-url',
      { proxy_id: 23 },
    ])
    expect(launchAuthBrowserMock.mock.calls).toEqual([
      ['old-session'],
      ['new-session'],
    ])
    expect(showErrorMock).not.toHaveBeenCalled()
    expect(showSuccessMock).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it.each([
    ['AUTH_BROWSER_LAUNCH_FAILED', 500],
    ['AUTH_BROWSER_PROXY_UNAVAILABLE', 503],
    ['AUTH_BROWSER_LAUNCH_TIMEOUT', 504],
  ])('does not retry non-session launch failure %s', async (reason, status) => {
    launchAuthBrowserMock.mockRejectedValueOnce({
      reason,
      status,
      message: 'browser launch failed',
    })

    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await wrapper.get('[data-testid="launch-auth-browser"]').trigger('click')
    await flushPromises()

    expect(generateAuthUrlMock).toHaveBeenCalledTimes(1)
    expect(launchAuthBrowserMock).toHaveBeenCalledTimes(1)
    expect(launchAuthBrowserMock).toHaveBeenCalledWith('test-session')
    expect(showSuccessMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('refreshes a recoverable browser session at most once', async () => {
    generateAuthUrlMock
      .mockResolvedValueOnce({
        auth_url: 'https://auth.example.invalid/authorize?state=old-state',
        session_id: 'old-session',
      })
      .mockResolvedValueOnce({
        auth_url: 'https://auth.example.invalid/authorize?state=new-state',
        session_id: 'new-session',
      })
    launchAuthBrowserMock
      .mockRejectedValueOnce({
        reason: 'AUTH_BROWSER_SESSION_EXPIRED',
        message: 'old session expired',
      })
      .mockRejectedValueOnce({
        reason: 'AUTH_BROWSER_SESSION_EXPIRED',
        message: 'replacement session expired',
      })

    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await wrapper.get('[data-testid="launch-auth-browser"]').trigger('click')
    await flushPromises()

    expect(generateAuthUrlMock).toHaveBeenCalledTimes(2)
    expect(launchAuthBrowserMock.mock.calls).toEqual([
      ['old-session'],
      ['new-session'],
    ])
    expect(showSuccessMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('ignores a browser launch result after the modal is unmounted', async () => {
    const launch = deferred<{
      launched: boolean
      already_running: boolean
      proxy_name: string
      output: string
    }>()
    launchAuthBrowserMock.mockReturnValueOnce(launch.promise)

    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await wrapper.get('[data-testid="launch-auth-browser"]').trigger('click')
    await flushPromises()

    wrapper.unmount()
    launch.resolve({
      launched: true,
      already_running: false,
      proxy_name: 'static-isp',
      output: '',
    })
    await flushPromises()

    expect(showSuccessMock).not.toHaveBeenCalled()
    expect(showErrorMock).not.toHaveBeenCalled()
  })

  it('uses the exchange route and stays busy until account creation completes', async () => {
    const creation = deferred<unknown>()
    createAccountMock.mockReturnValueOnce(creation.promise)
    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await authSubmit(wrapper).trigger('click')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledWith(expect.objectContaining({ proxy_id: 23 }))
    expect(authSubmit(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.findComponent(OAuthAuthorizationFlowStub).props('loading')).toBe(true)
    await authSubmit(wrapper).trigger('click')
    expect(exchangeCodeMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock).toHaveBeenCalledTimes(1)

    creation.resolve({ id: 42 })
    await flushPromises()
    expect(wrapper.emitted('created')).toHaveLength(1)
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })

  it.each(['close', 'back', 'unmount'])('does not create after %s cancels an exchange', async (action) => {
    const exchange = deferred<unknown>()
    exchangeCodeMock.mockReturnValueOnce(exchange.promise)
    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await authSubmit(wrapper).trigger('click')
    if (action === 'close') wrapper.findComponent(BaseDialogStub).vm.$emit('close')
    if (action === 'back') await selectButtonByText(wrapper, 'common.back')
    if (action === 'unmount') wrapper.unmount()
    exchange.resolve({ proxy_id: 23 })
    await flushPromises()
    expect(createAccountMock).not.toHaveBeenCalled()
    expect(showErrorMock).not.toHaveBeenCalled()
    if (action !== 'unmount') wrapper.unmount()
  })

  it.each(['success', 'failure'])('does not let an old create %s change a new flow', async (outcome) => {
    const oldCreation = deferred<unknown>()
    const newCreation = deferred<unknown>()
    createAccountMock.mockReturnValueOnce(oldCreation.promise).mockReturnValueOnce(newCreation.promise)
    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await authSubmit(wrapper).trigger('click')
    await flushPromises()
    expect(createAccountMock).toHaveBeenCalledTimes(1)
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    generateAuthUrlMock.mockResolvedValueOnce({
      auth_url: 'https://auth.example.invalid/authorize?state=test-state',
      session_id: 'next-session',
    })
    await startOAuthFlow(wrapper)
    await authSubmit(wrapper).trigger('click')
    await flushPromises()
    expect(createAccountMock).toHaveBeenCalledTimes(2)

    if (outcome === 'success') oldCreation.resolve({ id: 42 })
    else oldCreation.reject(new Error('older creation failed'))
    await flushPromises()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(showErrorMock).not.toHaveBeenCalled()
    expect(showSuccessMock).not.toHaveBeenCalled()
    expect(wrapper.findComponent(OAuthAuthorizationFlowStub).props('loading')).toBe(true)
    expect(authSubmit(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('created')?.length ?? 0).toBe(outcome === 'success' ? 1 : 0)

    newCreation.resolve({ id: 43 })
    await flushPromises()
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })

  it('rejects an exchange without a server-assigned proxy before creating', async () => {
    exchangeCodeMock.mockResolvedValueOnce({ expires_in: 3600 })
    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await authSubmit(wrapper).trigger('click')
    await flushPromises()
    expect(createAccountMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalled()
    expect(wrapper.findComponent(OAuthAuthorizationFlowStub).props('loading')).toBe(false)
    wrapper.unmount()
  })

  it('rejects an exchange assigned to a different proxy than the authorization link', async () => {
    exchangeCodeMock.mockResolvedValueOnce({ proxy_id: 17, expires_in: 3600 })
    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    await authSubmit(wrapper).trigger('click')
    await flushPromises()
    expect(createAccountMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalled()
    wrapper.unmount()
  })

  it('prevents proxy edits after the authorization session is created', async () => {
    const wrapper = mountModal()
    await startOAuthFlow(wrapper)
    expect(wrapper.findComponent({ name: 'ProxySelector' }).exists()).toBe(false)
    wrapper.unmount()
  })
})

describe('CreateAccountModal OpenAI long-context billing', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'openai', type: 'apikey' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
    showWarningMock.mockReset()
    importCodexSessionMock.mockReset().mockResolvedValue({
      created: 1,
      updated: 0,
      skipped: 0,
      failed: 0,
      errors: [],
      warnings: [],
    })
    createOpenAICodexPATMock.mockReset().mockResolvedValue({})
  })

  it('hides only the redundant account toggle when every selected group enables tier pricing', async () => {
    authIsSimpleMode.value = false
    const wrapper = mountModal([
      { id: 1, long_context_pricing_enabled: true },
      { id: 2, long_context_pricing_enabled: true },
    ])

    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="create-openai-ws-mode"]').exists()).toBe(true)
  })

  it('keeps the account toggle when any selected group disables tier pricing', async () => {
    authIsSimpleMode.value = false
    const wrapper = mountModal([
      { id: 1, long_context_pricing_enabled: true },
      { id: 2, long_context_pricing_enabled: false },
    ])

    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="create-openai-ws-mode"]').exists()).toBe(true)
  })

  it('sends false explicitly for normal OpenAI account creation by default', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('persists upstream model metadata after creating an account from preview', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenCode account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledOnce()
    expect(syncUpstreamModelsMock).toHaveBeenCalledWith(42)
  })

  it('includes the current concrete model mapping in preview credentials', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await flushPromises()

    expect(wrapper.getComponent(ModelWhitelistSelectorStub).props('syncCredentials')).toMatchObject({
      model_mapping: { 'public-glm': 'public-glm' }
    })
  })

  it('runs formal capability sync after creating an account with explicit mappings', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Mapped account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await selectButtonByText(wrapper, 'admin.accounts.modelMapping')
    await selectButtonByText(wrapper, 'admin.accounts.addMapping')
    await wrapper.get('input[placeholder="admin.accounts.requestModel"]').setValue('public-glm')
    await wrapper.get('input[placeholder="admin.accounts.actualModel"]').setValue('glm-5.3')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock.mock.calls[0]?.[0]?.credentials?.model_mapping).toEqual({
      'public-glm': 'glm-5.3'
    })
    expect(syncUpstreamModelsMock).toHaveBeenCalledWith(42)
  })

  it('warns when post-create capability metadata remains incomplete', async () => {
    syncUpstreamModelsMock.mockResolvedValue({
      models: ['x-preview-f-free'],
      warnings: [{ code: 'upstream_model_metadata_incomplete', message: 'metadata incomplete' }],
    })
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenCode account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(showWarningMock).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsMetadataIncomplete'
    )
  })

  // namespace 摊平是仅 OAuth 的兼容开关：API Key 走 chat completions 回退桥时由桥自行摊平
  it('shows the Codex namespace flatten toggle only for OpenAI OAuth accounts', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')

    expect(wrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(
      true
    )

    await selectButtonByText(wrapper, 'API Key')
    expect(wrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(
      false
    )
  })

  it('enables upstream billing probes by default for new OpenAI API key accounts', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(true)
  })

  it('waits for the initial upstream billing probe before refreshing the account list', async () => {
    let resolveProbe: (() => void) | undefined
    probeUpstreamBillingMock.mockImplementationOnce(
      () => new Promise<void>((resolve) => {
        resolveProbe = resolve
      })
    )

    const wrapper = await submitApiKeyAccount('openai')

    expect(probeUpstreamBillingMock).toHaveBeenCalledWith(42)
    expect(wrapper.emitted('created')).toBeUndefined()

    resolveProbe?.()
    await flushPromises()

    expect(wrapper.emitted('created')).toHaveLength(1)
  })

  it('sends an explicit disabled state when the create toggle is turned off', async () => {
    await submitApiKeyAccount('openai', false, true)

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(false)
    expect(probeUpstreamBillingMock).not.toHaveBeenCalled()
  })

  it('submits adaptive Kimi protocol endpoints', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Kimi adaptive')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-kimi')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://api.moonshot.cn/v1',
      api_base_urls: {
        chat_completions: 'https://api.moonshot.cn/v1',
        anthropic: 'https://api.moonshot.cn/anthropic',
        responses: 'https://api.moonshot.cn/v1'
      }
    })
  })

  it('submits adaptive Kimi Coding Plan Responses endpoint', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await selectButtonByText(wrapper, 'admin.accounts.cnProviders.accountMode.coding')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Kimi coding')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-kimi-coding')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'coding',
      api_protocol: 'adaptive',
      base_url: 'https://api.kimi.com/coding/v1',
      api_base_urls: {
        chat_completions: 'https://api.kimi.com/coding/v1',
        anthropic: 'https://api.kimi.com/coding',
        responses: 'https://api.kimi.com/coding/v1'
      }
    })
  })

  it('uses the edited adaptive Chat endpoint when previewing upstream models', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await wrapper
      .get('[data-testid="cn-adaptive-base-url-chat_completions"]')
      .setValue('https://relay.example.com/v1')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-relay')

    expect(wrapper.getComponent(ModelWhitelistSelectorStub).props('syncCredentials')).toMatchObject({
      platform: 'kimi',
      type: 'apikey',
      base_url: 'https://relay.example.com/v1',
      api_key: 'sk-relay'
    })
  })

  it('exposes Agent Identity in the OpenAI authorization methods', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenAI account')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')

    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    expect(flow.props('showManualOption')).toBe(true)
    expect(flow.props('showCodexSessionImportOption')).toBe(true)
    expect(flow.props('showAgentIdentityOption')).toBe(true)
    expect(flow.props('showCodexPatOption')).toBe(true)
    expect(flow.props('initialInputMethod')).toBe('manual')
  })

  it.each([
    ['camelCase', { authMode: 'agentIdentity', agentIdentity: { agentRuntimeId: 'runtime' } }],
    ['nested identity without auth_mode', { agent_identity: { agent_runtime_id: 'runtime' } }],
  ])('accepts backend-compatible %s Agent Identity imports', async (_name, content) => {
    const wrapper = await openCodexImportStep()
    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    flow.vm.inputMethod = 'agent_identity'

    flow.vm.$emit('import-codex-session', JSON.stringify(content))
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
  })

  it('sends true explicitly when OpenAI long-context billing is enabled', async () => {
    await submitApiKeyAccount('openai', true)

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('omits the OpenAI setting for non-OpenAI account creation', async () => {
    await submitApiKeyAccount('anthropic')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
    // 上游倍率探测已放宽到全部 API-key 平台：非 OpenAI 平台与 OpenAI 一致，默认开启。
    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(true)
  })

  it('sends an explicit disabled state when the non-OpenAI create toggle is turned off', async () => {
    await submitApiKeyAccount('anthropic', false, true)

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(false)
  })

  it('antigravity upstream 创建默认携带上游倍率探测开关', async () => {
    // antigravity upstream 走独立创建 helper，
    // 也必须与其余 API-key 平台一样默认开启探测并传递开关。
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Antigravity')
    await selectButtonByText(wrapper, 'admin.accounts.types.antigravityApikey')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('antigravity relay')
    const baseInput = wrapper
      .findAll('input')
      .find((candidate) => candidate.attributes('placeholder') === 'https://cloudcode-pa.googleapis.com')
    expect(baseInput).toBeDefined()
    await baseInput?.setValue('https://relay.example')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-upstream')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0]?.[0]
    expect(payload?.platform).toBe('antigravity')
    expect(payload?.type).toBe('apikey')
    expect(payload?.upstream_billing_probe_enabled).toBe(true)
    // 创建成功后前端立即发起一次首探（与其他 apikey 平台一致）。
    expect(probeUpstreamBillingMock).toHaveBeenCalledWith(42)
  })

  it('leaves Codex session import billing ownership to the backend', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('defaults Codex session imports to single-machine multi-window fingerprinting', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.codex_fingerprint_mode).toBe('machine')
  })

  it('enables TLS fingerprinting by default for OpenAI OAuth imports', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.enable_tls_fingerprint).toBe(true)
    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra).not.toHaveProperty(
      'tls_fingerprint_profile_id'
    )
  })

  it('sends a selected TLS fingerprint profile for OpenAI OAuth imports', async () => {
    const wrapper = await openCodexImportStep(0, 0, '12')
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.enable_tls_fingerprint).toBe(true)
    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.tls_fingerprint_profile_id).toBe(12)
  })

  it('sends an explicit TLS fingerprinting opt-out for OpenAI OAuth imports', async () => {
    const wrapper = await openCodexImportStep(0, 1)
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.enable_tls_fingerprint).toBe(false)
    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra).not.toHaveProperty(
      'tls_fingerprint_profile_id'
    )
  })

  it('enables TLS fingerprinting by default for new Anthropic OAuth accounts', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Anthropic')

    expect(wrapper.get('[data-testid="create-tls-fingerprint-toggle"]').attributes('aria-checked')).toBe(
      'true'
    )
  })

  it('shows Codex long-context billing and TLS fingerprinting for new OpenAI OAuth accounts', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')

    expect(wrapper.get('[data-testid="openai-long-context-billing-toggle"]').attributes('aria-checked')).toBe(
      'false'
    )
    expect(wrapper.get('[data-testid="create-openai-tls-fingerprint-toggle"]').attributes('aria-checked')).toBe(
      'true'
    )
  })

  it('leaves Codex PAT import billing ownership to the backend', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock).toHaveBeenCalledTimes(1)
    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('sends explicit true for Codex session import after the toggle is enabled', async () => {
    const wrapper = await openCodexImportStep(1)
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('sends explicit false for Codex session import after the toggle is changed back', async () => {
    const wrapper = await openCodexImportStep(2)
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('sends explicit true for Codex PAT import after the toggle is enabled', async () => {
    const wrapper = await openCodexImportStep(1)
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('sends explicit false for Codex PAT import after the toggle is changed back', async () => {
    const wrapper = await openCodexImportStep(2)
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })
})
