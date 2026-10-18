import assert from 'node:assert/strict';
import {test} from 'node:test';
import {boundStartupPolicy} from './startup-policy-launcher.mjs';

const options = {python: '/opt/homebrew/bin/python3', runner: '/tmp/fixture/startup_policy.py'};
const launcher = [
  '#!/usr/bin/env bash\nset -euo pipefail',
  '\t\tif /opt/homebrew/opt/postgresql@16/bin/pg_isready \\',
  '\t\t\t--host "${DATABASE_HOST}"; then return 0; fi',
  'local Sub2API database is unavailable after 120 seconds',
  'local Sub2API Redis is unavailable after 120 seconds',
  '\tPGPASSWORD="${DATABASE_PASSWORD}" \\',
  '\t\t/opt/homebrew/opt/postgresql@16/bin/psql \\',
  '\t\t--host "${DATABASE_HOST}" \\',
  '\t\t--port "${DATABASE_PORT}" \\',
  '\t\t--username "${DATABASE_USER}" \\',
  '\t\t--dbname "${DATABASE_DBNAME}" \\',
  '\t\t--set ON_ERROR_STOP=1 \\',
  '\t\t--file "${account_pool_policy}" >/dev/null',
  'exec "${binary_path}"',
].join('\n');
test('bounds only the known startup call and readiness probe', () => {
  const next = boundStartupPolicy(launcher, options);
  assert.ok(next.includes('" -B "/tmp/fixture/startup_policy.py"'));
  assert.ok(next.includes('--timeout 2'));
  assert.equal(next.split('60 bounded attempts').length, 3);
  assert.ok(next.endsWith('exec "${binary_path}"'));
});
test('rejects repeated adoption and partial generations', () => {
  assert.throws(() => boundStartupPolicy(boundStartupPolicy(launcher, options), options));
  assert.throws(() => boundStartupPolicy(launcher.replace('--set ON_ERROR_STOP=1', '--set ON_ERROR_STOP=0'), options));
  assert.throws(() => boundStartupPolicy(launcher + launcher, options));
});
test('rejects shell interpolation in executable paths', () => {
  for (const runner of ['/tmp/$(touch injected)', '/tmp/`true`', '/tmp/"', 'relative', '/tmp/a\nb']) {
    assert.throws(() => boundStartupPolicy(launcher, {...options, runner}));
  }
});
