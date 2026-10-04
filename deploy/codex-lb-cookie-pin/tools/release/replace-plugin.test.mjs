import assert from 'node:assert/strict';
import test from 'node:test';
import { readNativeResponse, replacePlugin } from './replace-plugin.mjs';

test('native response preserves an upload rejection and its digest', async () => {
  const response = new Response(JSON.stringify({ code: 'invalid_package', message: 'signature missing' }),
    { status: 400 });
  await assert.rejects(readNativeResponse(response, '/admin/plugins/upload'), error =>
    error.httpStatus === 400 && error.code === 'invalid_package' &&
    error.outcomeUnknown === false &&
    error.installation_message === 'signature missing' && error.responseSha256.length === 64);
});

test('non-JSON HTTP errors are not replaced by JSON parse exceptions', async () => {
  await assert.rejects(readNativeResponse(new Response('gateway unavailable', { status: 503 }),
    '/admin/plugins/upload'), error => /HTTP 503/.test(error.message) &&
    error.httpStatus === 503 && error.responseSha256.length === 64);
});

test('successful native response unwraps data', async () => {
  assert.deepEqual(await readNativeResponse(new Response('{"data":{"id":7}}'), '/admin/plugins'),
    { id: 7 });
});

test('lost response body is an ambiguous outcome', async () => {
  await assert.rejects(readNativeResponse({
    status: 200, text: async () => { throw new Error('socket closed'); }
  }, '/admin/plugins/upload'), error => error.outcomeUnknown === true);
});

for (const reply of ['null', '[]', '"unexpected"', '<html>wrong endpoint</html>']) {
  test(`malformed successful reply is ambiguous: ${reply}`, async () => {
    await assert.rejects(readNativeResponse(new Response(reply), '/admin/plugins/upload'),
      error => error.httpStatus === 200 && error.outcomeUnknown === true);
  });
}

for (const status of [408, 500, 502, 503, 504]) {
  test(`HTTP ${status} cannot authorize concurrent recovery`, async () => {
    await assert.rejects(readNativeResponse(new Response('{"code":"unavailable"}', { status }),
      '/admin/plugins/upload'), error => error.httpStatus === status &&
      error.outcomeUnknown === true);
  });
}

function fixture(overrides = {}) {
  const previous = { id: 7, version: '0.3.6', sha256: 'old', config_sha256: 'same' };
  const candidate = { version: '0.3.7', sha256: 'new' };
  let state = {
    id: 7, version: previous.version, artifact_sha256: previous.sha256,
    config_sha256: 'same', state: 'enabled', runtime_healthy: true,
    bindings: [{ enabled: true, rollout_percent: 100 }]
  };
  const calls = [];
  const records = [];
  const ownership = { generation: 'release-1', holder: 'writer-1' };
  const options = {
    previous, candidate,
    fence: { ...ownership, assertCurrent: async () => ({ ...ownership }) },
    preflight: async () => { calls.push('preflight'); },
    inspect: async () => structuredClone(state),
    disable: async () => {
      calls.push('disable');
      state.state = 'disabled';
      state.runtime_healthy = false;
      state.bindings[0].enabled = false;
    },
    upload: async artifact => {
      calls.push(`upload:${artifact.version}`);
      state.version = artifact.version;
      state.artifact_sha256 = artifact.sha256;
      return { id: state.id, version: state.version };
    },
    enable: async () => {
      calls.push('enable');
      state.state = 'enabled';
      state.runtime_healthy = true;
      state.bindings[0].enabled = true;
    },
    record: value => records.push(value),
    ...overrides
  };
  return { options, calls, records, ownership, state: () => state };
}

for (const fence of [undefined, {}, { generation: '', holder: 'writer-1' },
  { generation: 'release-1', holder: 'writer-1' }]) {
  test(`missing deployment fence fails before any operation: ${JSON.stringify(fence)}`, async () => {
    const f = fixture({ fence });
    await assert.rejects(replacePlugin(f.options), /fence is required/);
    assert.deepEqual(f.calls, []);
  });
}

for (const field of ['generation', 'holder']) {
  test(`stale ${field} fails even when artifact and configuration hashes match`, async () => {
    const f = fixture();
    f.ownership[field] = 'superseded';
    await assert.rejects(replacePlugin(f.options), error => error.code === 'DEPLOYMENT_FENCE_LOST');
    assert.deepEqual(f.calls, []);
  });
}

test('every native callback receives the immutable generation and holder binding', async () => {
  const f = fixture();
  for (const name of ['preflight', 'inspect', 'disable', 'upload', 'enable']) {
    const original = f.options[name];
    f.options[name] = async (...args) => {
      const binding = args.at(-1);
      assert.deepEqual(binding, f.ownership);
      assert.equal(Object.isFrozen(binding), true);
      return original(...args);
    };
  }
  await replacePlugin(f.options);
});

for (const action of ['disable', 'upload', 'enable']) {
  test(`revocation during ${action} stops subsequent writes and automatic recovery`, async () => {
    const f = fixture();
    const original = f.options[action];
    f.options[action] = async (...args) => {
      const result = await original(...args);
      f.ownership.generation = 'release-2';
      return result;
    };
    await assert.rejects(replacePlugin(f.options), error =>
      error.code === 'DEPLOYMENT_FENCE_LOST' && error.recovery === 'reconcile_after_fence_loss');
    const expected = ['preflight', 'disable', 'upload:0.3.7', 'enable'];
    assert.deepEqual(f.calls, expected.slice(0, { disable: 2, upload: 3, enable: 4 }[action]));
    assert.equal(f.records[0].generation, 'release-1');
    assert.equal(f.records[0].holder, 'writer-1');
  });
}

test('revocation after a rejected upload cannot authorize recovery', async () => {
  const f = fixture();
  const failure = new Error('upload rejected');
  f.options.upload = async () => {
    f.ownership.holder = 'writer-2';
    throw failure;
  };
  await assert.rejects(replacePlugin(f.options), error => error === failure &&
    error.recovery === 'reconcile_after_fence_loss' &&
    error.recoveryError.code === 'DEPLOYMENT_FENCE_LOST');
  assert.deepEqual(f.calls, ['preflight', 'disable']);
});

test('revocation during the post-disable read prevents upload and recovery', async () => {
  const f = fixture();
  const inspect = f.options.inspect;
  f.options.inspect = async (...args) => {
    const state = await inspect(...args);
    if (state.state === 'disabled') f.ownership.generation = 'release-2';
    return state;
  };
  await assert.rejects(replacePlugin(f.options), error =>
    error.code === 'DEPLOYMENT_FENCE_LOST' && error.recovery === 'reconcile_after_fence_loss');
  assert.deepEqual(f.calls, ['preflight', 'disable']);
});

test('unavailable ownership authority fails closed before preflight', async () => {
  const f = fixture();
  f.options.fence.assertCurrent = async () => { throw new Error('lease unavailable'); };
  await assert.rejects(replacePlugin(f.options), error =>
    error.code === 'DEPLOYMENT_FENCE_LOST' && error.cause.message === 'lease unavailable');
  assert.deepEqual(f.calls, []);
});

for (const action of ['disable', 'upload', 'enable']) {
  test(`revocation during recovery ${action} prevents any following operation`, async () => {
    const f = fixture();
    const originalEnable = f.options.enable;
    f.options.enable = async (...args) => {
      await originalEnable(...args);
      if (f.state().version === '0.3.7') f.state().runtime_healthy = false;
    };
    const original = f.options[action];
    f.options[action] = async (...args) => {
      const recovering = action === 'disable'
        ? f.state().version === '0.3.7'
        : action === 'upload' ? args[0].version === '0.3.6' : f.state().version === '0.3.6';
      const result = await original(...args);
      if (recovering) f.ownership.holder = 'writer-2';
      return result;
    };
    await assert.rejects(replacePlugin(f.options), error =>
      /acceptance failed/.test(error.message) && error.recovery === 'reconcile_after_fence_loss');
    const expected = ['preflight', 'disable', 'upload:0.3.7', 'enable',
      'disable', 'upload:0.3.6', 'enable'];
    assert.deepEqual(f.calls, expected.slice(0, { disable: 5, upload: 6, enable: 7 }[action]));
  });
}

test('preflight completes before disable, install and enable', async () => {
  const f = fixture();
  assert.equal((await replacePlugin(f.options)).version, '0.3.7');
  assert.deepEqual(f.calls, ['preflight', 'disable', 'upload:0.3.7', 'enable']);
});

test('failed native preflight does not stop or recover the running plugin', async () => {
  const f = fixture({ preflight: async () => { throw new Error('untrusted'); } });
  await assert.rejects(replacePlugin(f.options), /untrusted/);
  assert.deepEqual(f.calls, []);
  assert.equal(f.state().state, 'enabled');
});

for (const change of ['identity', 'config', 'empty-bindings', 'unhealthy']) {
  test(`baseline ${change} drift fails without writes`, async () => {
    const f = fixture();
    if (change === 'identity') f.state().id++;
    if (change === 'config') f.state().config_sha256 = 'changed';
    if (change === 'empty-bindings') f.state().bindings = [];
    if (change === 'unhealthy') f.state().runtime_healthy = false;
    await assert.rejects(replacePlugin(f.options), /baseline changed/);
    assert.deepEqual(f.calls, ['preflight']);
  });
}

test('HTTP rejection resumes unchanged previous plugin without uploading it again', async () => {
  const f = fixture();
  const failure = Object.assign(new Error('HTTP 400'), {
    httpStatus: 400, installation_message: 'publisher not trusted', responseSha256: 'digest'
  });
  f.options.upload = async () => { f.calls.push('rejected-upload'); throw failure; };
  await assert.rejects(replacePlugin(f.options), error => error === failure &&
    error.recovery === 'previous_reenabled_without_upload');
  assert.deepEqual(f.calls, ['preflight', 'disable', 'rejected-upload', 'enable']);
  assert.equal(f.state().version, '0.3.6');
  assert.equal(f.state().state, 'enabled');
  assert.equal(f.records[0].http_status, 400);
  assert.equal(f.records[0].installation_message, 'publisher not trusted');
});

test('lost upload response never races a possibly executing native operation', async () => {
  const f = fixture();
  const failure = Object.assign(new Error('timeout'), { outcomeUnknown: true });
  f.options.upload = async () => { throw failure; };
  await assert.rejects(replacePlugin(f.options), error => error === failure &&
    error.recovery === 'reconcile_after_native_operation_quiescence');
  assert.deepEqual(f.calls, ['preflight', 'disable']);
});

test('enable failure after committed install restores exactly the old package', async () => {
  const f = fixture();
  const enable = f.options.enable;
  const failure = new Error('candidate cannot start');
  f.options.enable = async () => {
    if (f.state().version === '0.3.7') throw failure;
    return enable();
  };
  await assert.rejects(replacePlugin(f.options), error => error === failure &&
    error.recovery === 'previous_package_restored');
  assert.deepEqual(f.calls, ['preflight', 'disable', 'upload:0.3.7', 'upload:0.3.6', 'enable']);
  assert.equal(f.state().runtime_healthy, true);
});

test('unhealthy enabled candidate is disabled before restoring old bytes', async () => {
  const f = fixture();
  const enable = f.options.enable;
  f.options.enable = async () => {
    await enable();
    if (f.state().version === '0.3.7') f.state().runtime_healthy = false;
  };
  await assert.rejects(replacePlugin(f.options), /acceptance failed/);
  assert.deepEqual(f.calls, ['preflight', 'disable', 'upload:0.3.7', 'enable',
    'disable', 'upload:0.3.6', 'enable']);
});

test('native prepareRuntime failure state is normalized before restoring', async () => {
  const f = fixture();
  const enable = f.options.enable;
  f.options.enable = async () => {
    if (f.state().version === '0.3.7') {
      f.state().state = 'error';
      throw new Error('native runtime preparation failed');
    }
    return enable();
  };
  await assert.rejects(replacePlugin(f.options), error =>
    error.recovery === 'previous_package_restored');
  assert.deepEqual(f.calls, ['preflight', 'disable', 'upload:0.3.7',
    'disable', 'upload:0.3.6', 'enable']);
});

test('concurrent configuration drift prevents recovery overwrites', async () => {
  const f = fixture();
  f.options.upload = async () => {
    f.state().config_sha256 = 'concurrent-change';
    throw new Error('upload rejected');
  };
  await assert.rejects(replacePlugin(f.options), error =>
    error.recovery === 'recovery_failed' && /writes refused/.test(error.recoveryError.message));
  assert.deepEqual(f.calls, ['preflight', 'disable']);
});

test('recovery error never replaces original installation error', async () => {
  const f = fixture();
  const failure = new Error('original rejection');
  f.options.upload = async () => { throw failure; };
  f.options.enable = async () => { throw new Error('recovery error'); };
  await assert.rejects(replacePlugin(f.options), error =>
    error === failure && error.recoveryError.message === 'recovery error');
  assert.equal(f.records[0].error, 'original rejection');
});

for (const outcomeUnknown of [false, true]) {
  for (const asyncRecord of [false, true]) {
    test(`receipt failure preserves original error: unknown=${outcomeUnknown}, async=${asyncRecord}`, async () => {
      const failure = Object.assign(new Error('original upload error'), { outcomeUnknown });
      const recordError = new Error('receipt storage unavailable');
      const f = fixture({
        record: asyncRecord
          ? async () => { throw recordError; }
          : () => { throw recordError; },
        upload: async () => { throw failure; }
      });
      await assert.rejects(replacePlugin(f.options), error =>
        error === failure && error.recordError === recordError &&
        error.recovery === (outcomeUnknown
          ? 'reconcile_after_native_operation_quiescence'
          : 'previous_reenabled_without_upload'));
      assert.deepEqual(f.calls, outcomeUnknown
        ? ['preflight', 'disable'] : ['preflight', 'disable', 'enable']);
    });
  }
}
