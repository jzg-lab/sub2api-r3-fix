import assert from 'node:assert/strict';
import test from 'node:test';
import {reorderUnderlay} from './mihomo-underlay-order.mjs';

const fixture = () => ({
  'proxy-groups': [{name: 'fixed-underlay', type: 'fallback',
    proxies: ['quic-a', 'quic-b', 'tcp'], interval: 60, url: 'https://example.test/health'}],
  proxies: [
    {name: 'quic-a', type: 'hysteria2', server: 'a.test'},
    {name: 'quic-b', type: 'hysteria2', server: 'b.test'},
    {name: 'tcp', type: 'trojan', server: 'c.test'},
    {name: 'fixed', type: 'socks5', server: '192.0.2.1', username: 'fixture',
      password: 'fixture', 'dialer-proxy': 'fixed-underlay'},
  ],
  listeners: [{name: 'account-entry', port: 17932, proxy: 'fixed'}],
});
const policy = {groupName: 'fixed-underlay',
  expectedOrder: ['quic-a', 'quic-b', 'tcp'], nextOrder: ['tcp', 'quic-a', 'quic-b']};

test('only existing group order changes; input and all fixed exits remain identical', () => {
  const before = fixture(), snapshot = structuredClone(before);
  const after = reorderUnderlay(before, policy);
  assert.deepEqual(before, snapshot);
  assert.deepEqual(after['proxy-groups'][0].proxies, policy.nextOrder);
  after['proxy-groups'][0].proxies = policy.expectedOrder;
  assert.deepEqual(after, before);
});

test('rollback preserves every other setting', () => {
  const before = fixture();
  const next = reorderUnderlay(before, policy);
  const restored = reorderUnderlay(next, {...policy,
    expectedOrder: policy.nextOrder, nextOrder: policy.expectedOrder});
  assert.deepEqual(restored, before);
});

for (const [name, change] of [
  ['stale generation', (c, p) => {c['proxy-groups'][0].proxies.reverse();}],
  ['new route', (c, p) => {p.nextOrder[0] = 'new-route';}],
  ['duplicate route', (c, p) => {p.nextOrder[0] = p.nextOrder[1];}],
  ['missing route', (c) => {c.proxies.shift();}],
  ['ambiguous route', (c) => {c.proxies.push({...c.proxies[0]});}],
  ['different group behavior', (c) => {c['proxy-groups'][0].type = 'select';}],
  ['no fixed consumer', (c) => {c.proxies.pop();}],
  ['unexpected fixed consumer', (c) => {c.proxies.at(-1).type = 'http';}],
  ['ambiguous group', (c) => {c['proxy-groups'].push({...c['proxy-groups'][0]});}],
]) {
  test(`reject ${name}`, () => {
    const config = fixture(), proposed = structuredClone(policy);
    change(config, proposed);
    assert.throws(() => reorderUnderlay(config, proposed));
  });
}
