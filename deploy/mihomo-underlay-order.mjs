import assert from 'node:assert/strict';

// Only reorder existing underlays; fixed egress, credentials and listeners stay intact.
export function reorderUnderlay(config, {groupName, expectedOrder, nextOrder}) {
  assert.equal(typeof groupName, 'string', 'Missing group');
  assert.ok(Array.isArray(expectedOrder) && expectedOrder.length > 1, 'Missing preimage');
  assert.ok(Array.isArray(nextOrder), 'Missing order');
  assert.equal(new Set(expectedOrder).size, expectedOrder.length, 'Duplicate preimage');
  assert.equal(new Set(nextOrder).size, nextOrder.length, 'Duplicate order');
  assert.deepEqual([...nextOrder].sort(), [...expectedOrder].sort(), 'Underlay membership changed');
  const groups = config['proxy-groups']?.filter(group => group.name === groupName);
  assert.equal(groups?.length, 1, 'Ambiguous group');
  assert.equal(groups[0].type, 'fallback', 'Group must retain native fallback');
  assert.deepEqual(groups[0].proxies, expectedOrder, 'Stale group preimage');
  for (const name of expectedOrder) {
    assert.equal(config.proxies?.filter(proxy => proxy.name === name).length, 1, 'Missing or ambiguous underlay');
  }
  const fixed = config.proxies?.filter(proxy => proxy['dialer-proxy'] === groupName);
  assert.ok(fixed?.length > 0, 'No fixed egress consumer');
  assert.ok(fixed.every(proxy => proxy.type === 'socks5'), 'Unexpected egress consumer');
  const next = structuredClone(config);
  next['proxy-groups'].find(group => group.name === groupName).proxies = [...nextOrder];
  return next;
}
