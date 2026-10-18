import assert from 'node:assert/strict';

// Refuse mixed generations; only the known synchronous psql call is replaced.
export function boundStartupPolicy(text, {python, runner}) {
  for (const value of [python, runner]) {
    assert.ok(/^\/[A-Za-z0-9_ .+/-]+$/.test(value), 'Invalid absolute executable path');
  }
  const before = '\tPGPASSWORD="${DATABASE_PASSWORD}" \\\n' +
    '\t\t/opt/homebrew/opt/postgresql@16/bin/psql \\\n' +
    '\t\t--host "${DATABASE_HOST}" \\\n' +
    '\t\t--port "${DATABASE_PORT}" \\\n' +
    '\t\t--username "${DATABASE_USER}" \\\n' +
    '\t\t--dbname "${DATABASE_DBNAME}" \\\n' +
    '\t\t--set ON_ERROR_STOP=1 \\\n' +
    '\t\t--file "${account_pool_policy}" >/dev/null';
  assert.equal(text.split(before).length, 2, 'Startup psql preimage missing or duplicated');
  let after = text.replace(before,
    `\t"${python}" -B "${runner}" \\\n` +
    '\t\t/opt/homebrew/opt/postgresql@16/bin/psql "${account_pool_policy}"');
  const readiness = '\t\tif /opt/homebrew/opt/postgresql@16/bin/pg_isready \\\n';
  assert.equal(after.split(readiness).length, 2, 'Database readiness preimage changed');
  after = after.replace(readiness, readiness + '\t\t\t--timeout 2 \\\n');
  for (const service of ['database', 'Redis']) {
    const oldMessage = `local Sub2API ${service} is unavailable after 120 seconds`;
    assert.equal(after.split(oldMessage).length, 2, 'Readiness diagnostic preimage changed');
    after = after.replace(oldMessage, `local Sub2API ${service} is unavailable after 60 bounded attempts`);
  }
  return after;
}
