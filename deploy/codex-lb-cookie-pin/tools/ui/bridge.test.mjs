import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('../../ui/index.html', import.meta.url), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const defaults = {
  enabled: true, cookie_names: ['__cflb', '__oailb'], default_ttl_seconds: 240,
  refresh_before_seconds: 30, inject_scope: 'codex', reroll_on_faster_model: true,
  persist_kv: true, quality_probe_enabled: false, probe_interval_seconds: 900,
  probe_model: '', max_consecutive_probe_failures: 3, probe_backoff_seconds: 3600,
  probe_reasoning_effort: 'high', probe_min_reasoning_tokens: 1200,
  adaptive_probe_scheduling: false, probe_burst_until_passes: 6,
};

async function setup({ config = defaults, error, load = true, testFailure } = {}) {
  const elements = new Map();
  const messages = [];
  const windowListeners = new Map();
  const timers = new Map();
  let timerID = 0;
  const getElement = id => {
    if (!elements.has(id)) elements.set(id, {
      value: '', checked: false, textContent: '', className: '', innerHTML: '',
      addEventListener(type, handler) { this[type] = handler; },
    });
    return elements.get(id);
  };
  let reply;
  const parent = { postMessage(message) {
    messages.push(JSON.parse(JSON.stringify(message)));
    if (message.type === 'config.load' && load) reply(message, { ok: true, config });
    if (message.type === 'plugin.status') reply(message, { ok: true, result: {
      status_json: JSON.stringify({ enabled: true, prober: { enabled: false, model: '', probes: 0 } }),
    } });
    if (message.type === 'config.save') {
      if (error) reply(message, { ok: false, error });
      else if (!message.config) reply(message, { ok: false, error: 'invalid bridge config' });
      else reply(message, { ok: true, config: message.config });
    }
    if (message.type === 'config.test') reply(message, {
      ok: !testFailure, result: { success: !testFailure, message: testFailure || 'ok' },
    });
  } };
  const context = {
    parent, location: { hash: '#bridge_token=fixture' },
    window: { addEventListener(type, handler) { windowListeners.set(type, handler); } },
    document: { getElementById: getElement, documentElement: { scrollHeight: 700 } },
    setTimeout(handler) { timers.set(++timerID, handler); return timerID; },
    clearTimeout(id) { timers.delete(id); }, setInterval() {},
  };
  reply = (request, payload, overrides = {}) => windowListeners.get('message')({
    source: parent, data: {
      source: 'sub2api-plugin-host', bridge_token: 'fixture',
      request_id: request.request_id, ...payload, ...overrides,
    },
  });
  vm.runInNewContext(script, context);
  const settle = async () => { for (let i = 0; i < 5; i++) await Promise.resolve(); };
  await settle();
  return { getElement, messages, reply, parent, windowListeners, settle };
}

test('saves the probe switch in the native top-level config envelope', async () => {
  const ui = await setup();
  ui.getElement('quality_probe_enabled').checked = true;
  ui.getElement('save').click();
  await ui.settle();
  const save = ui.messages.find(message => message.type === 'config.save');
  assert.equal(save.config.quality_probe_enabled, true);
  assert.equal(save.params, undefined);
  assert.equal(ui.getElement('msg').className, 'msg ok');
});

test('preserves advanced saved settings not represented in the form', async () => {
  const ui = await setup();
  ui.getElement('save').click();
  await ui.settle();
  const save = ui.messages.find(message => message.type === 'config.save');
  for (const key of ['probe_reasoning_effort', 'probe_min_reasoning_tokens',
    'adaptive_probe_scheduling', 'probe_burst_until_passes']) {
    assert.equal(save.config[key], defaults[key]);
  }
});

test('clears an explicit probe model to follow business requests', async () => {
  const ui = await setup({ config: { ...defaults, probe_model: 'fixed-model' } });
  ui.getElement('probe_model').value = '';
  ui.getElement('save').click();
  await ui.settle();
  assert.equal(ui.messages.find(message => message.type === 'config.save').config.probe_model, '');
});

test('displays the native host string error and keeps the unsaved draft', async () => {
  const ui = await setup({ error: 'host rejected configuration' });
  ui.getElement('quality_probe_enabled').checked = true;
  ui.getElement('save').click();
  await ui.settle();
  assert.equal(ui.getElement('msg').textContent, '保存失败: host rejected configuration');
  assert.equal(ui.getElement('quality_probe_enabled').checked, true);
  assert.match(ui.getElement('status').innerHTML, /质量探针 — 关（跟随业务模型）/);
});

test('also displays an object-shaped error from a compatible host', async () => {
  const ui = await setup({ error: { message: 'config invalid' } });
  ui.getElement('save').click();
  await ui.settle();
  assert.equal(ui.getElement('msg').textContent, '保存失败: config invalid');
});

test('uses the diagnostic result message when a saved-config test fails', async () => {
  const ui = await setup({ testFailure: 'runtime unavailable' });
  ui.getElement('test').click();
  await ui.settle();
  assert.equal(ui.getElement('msg').textContent, '测试失败: runtime unavailable');
});

test('never saves an unloaded configuration', async () => {
  const ui = await setup({ load: false });
  ui.getElement('save').click();
  await ui.settle();
  assert.equal(ui.messages.some(message => message.type === 'config.save'), false);
  assert.match(ui.getElement('msg').textContent, /配置尚未加载/);
});

test('resize sends the height at the top level', async () => {
  const ui = await setup();
  const resize = ui.messages.find(message => message.type === 'ui.resize');
  assert.equal(resize.height, 700);
  assert.equal(resize.params, undefined);
});

test('ignores responses with the wrong source, token or request ID', async () => {
  const ui = await setup({ load: false });
  const request = ui.messages.find(message => message.type === 'config.load');
  ui.reply(request, { ok: true, config: defaults }, { bridge_token: 'wrong' });
  ui.reply(request, { ok: true, config: defaults }, { request_id: 'wrong' });
  ui.windowListeners.get('message')({ source: {}, data: {
    source: 'sub2api-plugin-host', bridge_token: 'fixture', request_id: request.request_id,
    ok: true, config: defaults,
  } });
  await ui.settle();
  assert.equal(ui.getElement('enabled').checked, false);
  ui.reply(request, { ok: true, config: defaults });
  await ui.settle();
  assert.equal(ui.getElement('enabled').checked, true);
});

test('does not replay a previous one-shot cookie drop while saving the switch', async () => {
  const ui = await setup({ config: { ...defaults, drop_account_ids: [41] } });
  ui.getElement('save').click();
  await ui.settle();
  assert.equal(ui.messages.find(message => message.type === 'config.save').config.drop_account_ids, undefined);
});
