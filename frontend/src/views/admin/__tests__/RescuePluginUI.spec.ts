import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import { resolve } from 'node:path'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const html = readFileSync(resolve(process.cwd(), '../deploy/codex-lb-cookie-pin/ui/index.html'), 'utf8')
const script = html.match(/<script>([\s\S]*?)<\/script>/)![1]
const initialConfig = {
  enabled: true, cookie_names: ['__cflb', '__oailb'], default_ttl_seconds: 240,
  refresh_before_seconds: 30, inject_scope: 'codex', reroll_on_faster_model: true,
  persist_kv: true, quality_probe_enabled: true, probe_interval_seconds: 900,
  probe_model: 'model-fixture', max_consecutive_probe_failures: 3,
  probe_backoff_seconds: 3600, probe_reasoning_effort: 'xhigh',
  probe_min_reasoning_tokens: 1600, adaptive_probe_scheduling: false,
  probe_schedule_margin_seconds: 60, probe_burst_interval_seconds: 45,
  probe_burst_until_passes: 9, probe_burst_max_probes: 20,
}

function startUI() {
  document.body.innerHTML = html.replace(/<script>[\s\S]*?<\/script>/, '')
  const parent = { postMessage: vi.fn() }
  const events = new Map<string, (event: unknown) => void>()
  runInNewContext(script, {
    document, parent, location: { hash: '#bridge_token=fixture' },
    window: { addEventListener: (name: string, handler: (event: unknown) => void) => events.set(name, handler) },
    setTimeout, clearTimeout, setInterval, clearInterval,
  })
  const requests = (type: string) => parent.postMessage.mock.calls
    .map(([message]) => message).filter(message => message.type === type)
  const respond = async (request: Record<string, unknown>, payload: Record<string, unknown>, source: unknown = parent) => {
    events.get('message')!({ source, data: {
      source: 'sub2api-plugin-host', bridge_token: 'fixture', request_id: request.request_id,
      ...payload,
    } })
    await vi.advanceTimersByTimeAsync(0)
  }
  const load = (config = initialConfig) => respond(requests('config.load')[0], { ok: true, config })
  return { requests, respond, load, events }
}

const field = (id: string) => document.getElementById(id) as HTMLInputElement
const message = () => document.getElementById('msg')!.textContent

describe('救号插件实际包内页面', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    vi.clearAllTimers()
    vi.useRealTimers()
    document.body.innerHTML = ''
  })

  it('配置加载前及加载失败时不允许破坏性覆盖', async () => {
    const ui = startUI()
    expect(field('save').disabled).toBe(true)
    field('save').click()
    expect(ui.requests('config.save')).toHaveLength(0)
    await ui.respond(ui.requests('config.load')[0], { ok: false, error: 'fixture load rejected' })
    expect(field('save').disabled).toBe(true)
    expect(message()).toContain('fixture load rejected')
  })

  it('发送顶层 config，保留隐藏的 xhigh/密集档/false 配置，并消费规范化回包', async () => {
    const ui = startUI()
    await ui.load()
    field('probe_interval_seconds').value = '300'
    field('drop_account_ids').value = '123'
    field('save').click()
    field('save').click()
    const requests = ui.requests('config.save')
    expect(requests).toHaveLength(1)
    expect(requests[0]).not.toHaveProperty('params')
    expect(requests[0].config).toMatchObject({ ...initialConfig, probe_interval_seconds: 300, drop_account_ids: [123] })
    await ui.respond(requests[0], { ok: true, config: { ...initialConfig, probe_burst_until_passes: 10, drop_account_ids: [123] } })
    expect(field('drop_account_ids').value).toBe('')
    field('save').click()
    expect(ui.requests('config.save')[1].config).toMatchObject({ probe_burst_until_passes: 10, probe_reasoning_effort: 'xhigh' })
    expect(ui.requests('config.save')[1].config.drop_account_ids).toBeUndefined()
  })

  it.each([
    [{ error: 'fixture permission denied' }, 'fixture permission denied'],
    [{ error: { message: 'fixture invalid range' } }, 'fixture invalid range'],
    [{ result: { success: false, message: 'fixture health failure' } }, 'fixture health failure'],
  ])('展示宿主真实错误且保留草稿 %j', async (payload, expected) => {
    const ui = startUI()
    await ui.load()
    field('probe_model').value = 'edited-fixture'
    field('save').click()
    await ui.respond(ui.requests('config.save')[0], { ok: false, ...payload })
    expect(message()).toContain(expected)
    expect(field('probe_model').value).toBe('edited-fixture')
    expect(field('save').disabled).toBe(false)
  })

  it('二次验证超过旧超时仍可收到结果，期间禁止重复敏感操作', async () => {
    const ui = startUI()
    await ui.load()
    field('save').click()
    await vi.advanceTimersByTimeAsync(31_000)
    expect(field('save').disabled).toBe(true)
    expect(field('test').disabled).toBe(true)
    await ui.respond(ui.requests('config.save')[0], { ok: true, config: initialConfig })
    expect(message()).toBe('已保存')
    expect(field('save').disabled).toBe(false)
  })

  it('错误的来源/token/request_id 和重复回包不能冒充加载成功', async () => {
    const ui = startUI()
    const request = ui.requests('config.load')[0]
    await ui.respond(request, { ok: true, config: initialConfig }, {})
    await ui.respond(request, { ok: true, config: initialConfig, bridge_token: 'wrong' })
    await ui.respond(request, { ok: true, config: initialConfig, request_id: 'unknown' })
    expect(field('save').disabled).toBe(true)
    await ui.load()
    await ui.respond(request, { ok: true, config: { ...initialConfig, probe_interval_seconds: 7000 } })
    expect(field('probe_interval_seconds').value).toBe('900')
  })

  it('resize 为顶层无回包通知，关闭时清除轮询和未决请求', async () => {
    const ui = startUI()
    ui.events.get('resize')!({})
    expect(ui.requests('ui.resize')[0]).toHaveProperty('height')
    expect(ui.requests('ui.resize')[0]).not.toHaveProperty('request_id')
    ui.events.get('pagehide')!({})
    await vi.advanceTimersByTimeAsync(0)
    expect(vi.getTimerCount()).toBe(0)
  })
})
