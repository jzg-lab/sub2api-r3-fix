import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import PluginsView from '../PluginsView.vue'

const {
  listPlugins,
  uploadPlugin,
  enablePlugin,
  savePluginConfig,
  createUISession,
  stepUpRun,
  getPluginConfig,
  cancelStepUp,
} = vi.hoisted(() => ({
  listPlugins: vi.fn(),
  uploadPlugin: vi.fn(),
  enablePlugin: vi.fn(),
  savePluginConfig: vi.fn(),
  createUISession: vi.fn(),
  stepUpRun: vi.fn((action: () => Promise<unknown>) => action()),
  getPluginConfig: vi.fn(),
  cancelStepUp: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    plugins: {
      list: listPlugins,
      upload: uploadPlugin,
      enable: enablePlugin,
      disable: vi.fn(),
      remove: vi.fn(),
      getConfig: getPluginConfig,
      getStatus: vi.fn().mockResolvedValue({}),
      saveConfig: savePluginConfig,
      test: vi.fn().mockResolvedValue({ success: true, message: 'ok', latency_ms: 1 }),
      createUISession,
    },
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn(),
  }),
}))

vi.mock('@/composables/useStepUp', () => ({
  useStepUp: () => ({ run: stepUpRun, onCancel: cancelStepUp }),
  isStepUpBlocked: () => false,
  isStepUpCancelled: () => false,
  stepUpBlockReason: () => '',
}))

vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key }),
}))

const plugin = {
  id: 7,
  plugin_key: 'local.test.transport',
  name: 'Test Transport',
  version: '1.0.0',
  description: '',
  author: 'test',
  manifest: {
    schema_version: 1,
    id: 'local.test.transport',
    name: 'Test Transport',
    version: '1.0.0',
    requires: {
      sub2api: '>=0.1.0',
      plugin_protocol: 1,
      transport_api: 1,
      ui_bridge: 1,
    },
    capabilities: [],
    ui: { entrypoint: 'ui/index.html' },
  },
  binary_sha256: 'a'.repeat(64),
  signature_status: 'trusted' as const,
  state: 'disabled' as const,
  last_error: '',
  installed_at: '2026-08-22T00:00:00Z',
  updated_at: '2026-08-22T00:00:00Z',
  bindings: [
    {
      id: 1,
      plugin_id: 7,
      capability: 'openai.oauth.outbound_transport.v1',
      platform: 'openai',
      account_type: 'oauth',
      enabled: false,
      rollout_percent: 100,
    },
  ],
  compatibility: {
    compatible: true,
    tested: true,
    status: 'compatible' as const,
    message: '',
    current_sub2api_version: '0.1.0',
    required_sub2api_version: '>=0.1.0',
    recommended_sub2api_version: '0.1.0',
    plugin_protocol: 1,
    transport_api: 1,
    ui_bridge: 1,
  },
  runtime_healthy: false,
  runtime_message: '',
}

function mountView() {
  const wrapper = mount(PluginsView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        BaseDialog: { template: '<div><slot /></div>' },
        Icon: true,
        TotpStepUpDialog: true,
      },
    },
  })
  wrappers.push(wrapper)
  return wrapper
}

const wrappers: ReturnType<typeof mount>[] = []
afterEach(() => {
  wrappers.splice(0).forEach(wrapper => wrapper.unmount())
  document.body.innerHTML = ''
  vi.useRealTimers()
})

async function openUI(wrapper: ReturnType<typeof mountView>) {
  const button = wrapper.findAll('button').find(item => item.text().includes('admin.plugins.configure'))
  expect(button).toBeDefined()
  await button!.trigger('click')
  await flushPromises()
  const frame = wrapper.get('iframe')
  await frame.trigger('load')
  const frameWindow = (frame.element as HTMLIFrameElement).contentWindow!
  const post = vi.spyOn(frameWindow, 'postMessage')
  const send = (data: Record<string, unknown> = {}, source: MessageEventSource | null = frameWindow, origin = 'null') => {
    window.dispatchEvent(new MessageEvent('message', { source, origin, data: {
      source: 'sub2api-plugin-ui', bridge_token: 'bridge', type: 'config.save',
      request_id: 'ui-1', config: { enabled: true, probe_reasoning_effort: 'xhigh' }, ...data,
    } }))
  }
  return { frame, post, send }
}

describe('管理员插件页二次验证', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    stepUpRun.mockImplementation((action: () => Promise<unknown>) => action())
    listPlugins.mockResolvedValue([plugin])
    uploadPlugin.mockResolvedValue(plugin)
    enablePlugin.mockResolvedValue(plugin)
    savePluginConfig.mockResolvedValue({ enabled: true })
    getPluginConfig.mockResolvedValue({})
    createUISession.mockResolvedValue({
      url: '/api/v1/plugin-ui/token/index.html#bridge_token=bridge',
      bridge_token: 'bridge',
      ui_bridge_version: 1,
      expires_at: '2026-08-22T01:00:00Z',
    })
  })

  it('启用插件通过 step-up 控制器执行', async () => {
    const wrapper = mountView()
    await flushPromises()

    const button = wrapper.findAll('button').find((item) => item.text().includes('admin.plugins.enable'))
    expect(button).toBeDefined()
    await button!.trigger('click')
    await flushPromises()

    expect(stepUpRun).toHaveBeenCalledTimes(1)
    expect(enablePlugin).toHaveBeenCalledWith(7, 100, false)
  })

  it('上传插件通过 step-up 控制器执行', async () => {
    const wrapper = mountView()
    await flushPromises()
    const input = wrapper.get('input[type="file"]')
    Object.defineProperty(input.element, 'files', {
      configurable: true,
      value: [new File(['plugin'], 'transport.s2plugin', { type: 'application/zip' })],
    })

    await input.trigger('change')
    await flushPromises()

    expect(stepUpRun).toHaveBeenCalledTimes(1)
    expect(uploadPlugin).toHaveBeenCalledTimes(1)
  })

  it('顶层配置通过二次验证且沙箱不授予同源权限', async () => {
    const wrapper = mountView()
    await flushPromises()
    const ui = await openUI(wrapper)
    expect(ui.frame.attributes('sandbox')).toBe('allow-scripts')
    ui.send()
    await flushPromises()
    expect(stepUpRun).toHaveBeenCalledTimes(1)
    expect(savePluginConfig).toHaveBeenCalledWith(7, { enabled: true, probe_reasoning_effort: 'xhigh' })
    expect(ui.post).toHaveBeenCalledWith(expect.objectContaining({ ok: true, request_id: 'ui-1' }), '*')
  })

  it('无效信封和畸形配置不触发保存', async () => {
    const wrapper = mountView()
    await flushPromises()
    const ui = await openUI(wrapper)
    ui.send({}, window)
    ui.send({}, undefined, 'https://untrusted.invalid')
    ui.send({ bridge_token: 'wrong' })
    ui.send({ request_id: '' })
    for (const config of [null, [], 'wrong']) {
      ui.send({ config })
      await flushPromises()
    }
    expect(savePluginConfig).not.toHaveBeenCalled()
    expect(stepUpRun).not.toHaveBeenCalled()
    expect(ui.post).toHaveBeenCalledWith(expect.objectContaining({ ok: false, error: 'admin.plugins.bridgeRejected' }), '*')
  })

  it('服务端错误原文回传而非吞成通用失败', async () => {
    savePluginConfig.mockRejectedValue(new Error('fixture validation failure'))
    const wrapper = mountView()
    await flushPromises()
    const ui = await openUI(wrapper)
    ui.send()
    await flushPromises()
    expect(ui.post).toHaveBeenCalledWith(expect.objectContaining({ ok: false, error: 'fixture validation failure' }), '*')
  })

  it('同一未决请求与另一敏感操作不会重复进入二次验证', async () => {
    stepUpRun.mockImplementation(() => new Promise(() => {}))
    const wrapper = mountView()
    await flushPromises()
    const ui = await openUI(wrapper)
    ui.send()
    ui.send()
    ui.send({ type: 'config.test', request_id: 'ui-2' })
    await flushPromises()
    expect(stepUpRun).toHaveBeenCalledTimes(1)
    expect(ui.post).toHaveBeenCalledWith(expect.objectContaining({ ok: false, request_id: 'ui-2' }), '*')
  })

  it('iframe 导航后，旧二次验证不能继续写配置', async () => {
    let resume!: () => Promise<unknown>
    stepUpRun.mockImplementation(action => new Promise((resolve, reject) => {
      resume = async () => {
        try { resolve(await action()) } catch (error) { reject(error) }
      }
    }))
    const wrapper = mountView()
    await flushPromises()
    const ui = await openUI(wrapper)
    ui.send()
    await ui.frame.trigger('load')
    await resume()
    await flushPromises()
    expect(savePluginConfig).not.toHaveBeenCalled()
    expect(ui.post).not.toHaveBeenCalled()
    expect(cancelStepUp).toHaveBeenCalledTimes(1)
  })

  it('导航后重复 request_id 不会收到旧页面的延迟配置', async () => {
    let oldResponse!: (value: Record<string, unknown>) => void
    getPluginConfig.mockImplementationOnce(() => new Promise(resolve => { oldResponse = resolve }))
    const wrapper = mountView()
    await flushPromises()
    const ui = await openUI(wrapper)
    ui.send({ type: 'config.load' })
    await ui.frame.trigger('load')
    getPluginConfig.mockResolvedValue({ generation: 'new' })
    ui.send({ type: 'config.load' })
    await flushPromises()
    oldResponse({ generation: 'old' })
    await flushPromises()
    expect(ui.post).toHaveBeenCalledTimes(1)
    expect(ui.post).toHaveBeenCalledWith(expect.objectContaining({ config: { generation: 'new' } }), '*')
  })

  it('超时后的二次验证回调不能继续保存', async () => {
    vi.useFakeTimers()
    let resume!: () => Promise<unknown>
    stepUpRun.mockImplementation(action => new Promise((resolve, reject) => {
      resume = async () => {
        try { resolve(await action()) } catch (error) { reject(error) }
      }
    }))
    const wrapper = mountView()
    await flushPromises()
    const ui = await openUI(wrapper)
    ui.send()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(cancelStepUp).toHaveBeenCalledTimes(1)
    await resume()
    await flushPromises()
    expect(savePluginConfig).not.toHaveBeenCalled()
    expect(ui.post).not.toHaveBeenCalled()
  })

  it('旧请求在页面关闭后才要求二次验证，不再弹出过期验证', async () => {
    let rejectSave!: (error: unknown) => void
    let stepUpError: unknown
    savePluginConfig.mockImplementation(() => new Promise((_, reject) => { rejectSave = reject }))
    stepUpRun.mockImplementation(async action => {
      try { return await action() } catch (error) { stepUpError = error; throw error }
    })
    const wrapper = mountView()
    await flushPromises()
    const ui = await openUI(wrapper)
    ui.send()
    await ui.frame.trigger('load')
    rejectSave({ reason: 'STEP_UP_REQUIRED', message: 'fixture challenge' })
    await flushPromises()
    expect(stepUpError).toBeInstanceOf(Error)
    expect(stepUpError).not.toHaveProperty('reason', 'STEP_UP_REQUIRED')
    expect(ui.post).not.toHaveBeenCalled()
  })

  it('旧 session 异步返回不能覆盖后来打开的插件页面', async () => {
    let finishSession!: (session: unknown) => void
    createUISession.mockImplementationOnce(() => new Promise(resolve => { finishSession = resolve }))
    const wrapper = mountView()
    await flushPromises()
    const button = wrapper.findAll('button').find(item => item.text().includes('admin.plugins.configure'))!
    await button.trigger('click')
    await button.trigger('click')
    await flushPromises()
    finishSession({
      url: '/old-fixture', bridge_token: 'old', ui_bridge_version: 1, expires_at: '2099-01-01T00:00:00Z',
    })
    await flushPromises()
    expect(wrapper.get('iframe').attributes('src')).not.toContain('old-fixture')
  })
})
