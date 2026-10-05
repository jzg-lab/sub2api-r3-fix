import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { createServer, get as httpGet } from 'node:http'
import test from 'node:test'
import {
  isAuthorizationError, isRecoveryAccount, listenForAuthorization,
  recoverAccount, ReauthorizationError,
} from '../reauthorization.mjs'
import { hostRequest, localHostURL } from '../reauthorize.mjs'

const opaque = (length) => randomBytes(length / 2).toString('hex')
const account = () => ({
  id: 17, proxy_id: 13, platform: 'openai', type: 'oauth',
  status: 'error', error_message: 'upstream 401 token_revoked',
  schedulable: true, updated_at: '2026-10-04T01:02:03.123456Z',
})

function fixture() {
  const observed = account()
  const state = opaque(64)
  const id = opaque(32)
  const credentials = { access_token: opaque(32) }
  const callback = { code: opaque(32), state }
  const session = {
    session_id: id,
    auth_url: `https://auth.openai.com/oauth/authorize?state=${state}&redirect_uri=${encodeURIComponent('http://localhost:1455/auth/callback')}`,
  }
  const calls = []
  let closed = 0
  const context = {
    observed, credentials, state, id, callback, session, calls,
    current: structuredClone(observed),
    exchange: { proxy_id: 13, reauthorization_proof: opaque(32), credentials },
    launch: { launched: true },
    onCall: undefined,
    closed: () => closed,
  }
  context.request = async (path, body) => {
    calls.push({ path, body })
    const override = await context.onCall?.(path, body)
    if (override !== undefined) return override
    if (path.endsWith('/generate-auth-url')) return session
    if (path.endsWith('/launch-auth-browser')) return context.launch
    if (path.endsWith('/exchange-code')) return context.exchange
    if (path.endsWith('/apply-oauth-credentials')) {
      return { id: observed.id, updated_at: '2026-10-04T01:03:03.123456Z' }
    }
    return context.current
  }
  context.listen = async () => ({
    wait: async () => callback,
    close: async () => { closed++ },
  })
  context.run = () => recoverAccount({
    request: context.request, account: observed, listen: context.listen,
  })
  return context
}

test('bound recovery uses the initial revision and exact host proof credentials', async () => {
  const f = fixture()
  const result = await f.run()
  assert.equal(result.committed, true)
  const generated = f.calls.find((call) => call.path.endsWith('/generate-auth-url')).body
  const exchange = f.calls.find((call) => call.path.endsWith('/exchange-code')).body
  const committed = f.calls.find((call) => call.path.endsWith('/apply-oauth-credentials')).body
  assert.equal(generated.account_id, f.observed.id)
  assert.equal(generated.proxy_id, f.observed.proxy_id)
  assert.equal(exchange.session_id, f.id)
  assert.equal(exchange.proxy_id, f.observed.proxy_id)
  assert.equal(generated.expected_updated_at, f.observed.updated_at)
  assert.equal(committed.expected_updated_at, f.observed.updated_at)
  assert.ok(committed.credentials === f.credentials)
  assert.ok(committed.reauthorization_proof === f.exchange.reauthorization_proof)
  assert.deepEqual(Object.keys(committed).sort(),
    ['credentials', 'expected_updated_at', 'reauthorization_proof', 'type'])
  assert.equal(f.closed(), 1)
})

for (const boundary of ['before_launch', 'during_login', 'before_commit']) {
  for (const change of ['revision', 'active', 'proxy', 'deactivated', 'deleted']) {
    test(`${change} ${boundary} cannot replace credentials`, async () => {
      const f = fixture()
      const mutate = () => {
        if (change === 'revision') f.current.updated_at = '2026-10-04T01:02:03.123457Z'
        if (change === 'active') f.current.status = 'active'
        if (change === 'proxy') f.current.proxy_id = 14
        if (change === 'deactivated') f.current.error_message = 'account_deactivated 401'
        if (change === 'deleted') f.current = null
      }
      if (boundary === 'before_launch') mutate()
      f.onCall = (path) => {
        if (boundary === 'during_login' && path.endsWith('/launch-auth-browser')) mutate()
        if (boundary === 'before_commit' && path.endsWith('/exchange-code')) mutate()
      }
      await assert.rejects(f.run(), { code: 'REAUTH_ACCOUNT_CHANGED' })
      assert.equal(f.calls.some((call) => call.path.endsWith('/apply-oauth-credentials')), false)
      assert.equal(f.closed(), boundary === 'before_launch' ? 0 : 1)
    })
  }
}

test('mutating the supplied account cannot move the captured version forward', async () => {
  const f = fixture()
  f.onCall = (path) => {
    if (path.endsWith('/launch-auth-browser')) {
      f.current.updated_at = '2026-10-04T01:03:00Z'
      f.observed.updated_at = f.current.updated_at
    }
  }
  await assert.rejects(f.run(), { code: 'REAUTH_ACCOUNT_CHANGED' })
  assert.equal(f.calls.some((call) => call.path.endsWith('/exchange-code')), false)
})

for (const change of ['runtime', 'authorization', 'missing revision']) {
  test(`identity-bound recovery handles ${change} updates during login`, async () => {
    const f = fixture()
    const revision = `oauth-v1:${opaque(64)}`
    f.observed.reauthorization_revision = revision
    f.current.reauthorization_revision = revision
    f.onCall = (path) => {
      if (path.endsWith('/launch-auth-browser')) {
        f.current.updated_at = '2026-10-04T01:03:00Z'
        if (change === 'authorization') f.current.reauthorization_revision = `oauth-v1:${opaque(64)}`
        if (change === 'missing revision') delete f.current.reauthorization_revision
      }
    }
    if (change === 'runtime') {
      assert.equal((await f.run()).committed, true)
      const generated = f.calls.find((call) => call.path.endsWith('/generate-auth-url')).body
      assert.equal(generated.expected_authorization_revision, revision)
      const committed = f.calls.find((call) => call.path.endsWith('/apply-oauth-credentials')).body
      assert.equal(committed.expected_updated_at, f.observed.updated_at)
    } else {
      await assert.rejects(f.run(), { code: 'REAUTH_ACCOUNT_CHANGED' })
      assert.equal(f.calls.some((call) => call.path.endsWith('/apply-oauth-credentials')), false)
    }
  })
}

test('explicit rescue-account reauthorization does not send scheduling or rescue changes', async () => {
  const f = fixture()
  f.current.schedulable = f.observed.schedulable = false
  assert.equal(isRecoveryAccount(f.observed), true)
  await f.run()
  const body = f.calls.at(-1).body
  assert.equal('schedulable' in body, false)
  assert.equal('extra' in body, false)
})

for (const change of ['missing_proof', 'wrong_proxy', 'detached_tokens', 'array_credentials']) {
  test(`${change} cannot use the replacement endpoint`, async () => {
    const f = fixture()
    if (change === 'missing_proof') delete f.exchange.reauthorization_proof
    if (change === 'wrong_proxy') f.exchange.proxy_id++
    if (change === 'detached_tokens') delete f.exchange.credentials
    if (change === 'array_credentials') f.exchange.credentials = []
    await assert.rejects(f.run(), { code: 'REAUTH_PROOF_REQUIRED' })
    assert.equal(f.calls.some((call) => call.path.endsWith('/apply-oauth-credentials')), false)
    assert.equal(f.closed(), 1)
  })
}

for (const stage of ['exchange-code', 'apply-oauth-credentials']) {
  for (const error of [
    new Error('untrusted response detail'),
    new ReauthorizationError('REAUTH_HOST_HTTP_500'),
    new ReauthorizationError('REAUTH_HOST_HTTP_408'),
  ]) {
    test(`${stage} ambiguous failure is never retried`, async () => {
      const f = fixture()
      f.onCall = (path) => { if (path.endsWith(`/${stage}`)) throw error }
      await assert.rejects(f.run(), {
        code: stage === 'exchange-code' ? 'REAUTH_EXCHANGE_OUTCOME_UNKNOWN' : 'REAUTH_COMMIT_OUTCOME_UNKNOWN',
      })
      assert.equal(f.calls.filter((call) => call.path.endsWith(`/${stage}`)).length, 1)
      assert.equal(f.closed(), 1)
    })
  }
}

test('host CAS conflict is preserved without rereading a newer version and retrying', async () => {
  const f = fixture()
  f.onCall = (path) => {
    if (path.endsWith('/apply-oauth-credentials')) throw new ReauthorizationError('REAUTH_HOST_HTTP_409')
  }
  await assert.rejects(f.run(), { code: 'REAUTH_HOST_HTTP_409' })
  assert.equal(f.calls.at(-1).path.endsWith('/apply-oauth-credentials'), true)
})

for (const updatedAt of [
  '2026-10-04T01:02:03.123456Z',
  '2026-10-04T09:02:03.123456+08:00',
  '2026-10-04T01:02:03.123455Z',
  '2026-10-04T01:02:03.123Z',
  '2026-10-04T01:02:03.123456000Z',
  '2026-10-04T01:02:03.1234560001Z',
  '2026-10-04T01:03:03',
  '2027-02-30T01:03:03Z',
  '2026-10-04T24:00:00Z',
  'not-a-timestamp',
]) {
  test(`unchanged or invalid commit revision ${updatedAt} cannot report success`, async () => {
    const f = fixture()
    f.onCall = (path) => {
      if (path.endsWith('/apply-oauth-credentials')) {
        return { id: f.observed.id, updated_at: updatedAt }
      }
    }
    await assert.rejects(f.run(), { code: 'REAUTH_COMMIT_OUTCOME_UNKNOWN' })
    assert.equal(f.calls.filter((call) => call.path.endsWith('/apply-oauth-credentials')).length, 1)
    assert.equal(f.closed(), 1)
  })
}

for (const updatedAt of [
  '2026-10-04T01:02:03.123457Z',
  '2026-10-04T09:02:03.123457+08:00',
  '2026-10-04T01:02:03.123456001Z',
]) {
  test(`sub-millisecond commit revision ${updatedAt} is accepted without rounding`, async () => {
    const f = fixture()
    f.onCall = (path) => {
      if (path.endsWith('/apply-oauth-credentials')) {
        return { id: f.observed.id, updated_at: updatedAt }
      }
    }
    assert.equal((await f.run()).committed, true)
    assert.equal(f.closed(), 1)
  })
}

for (const updatedAt of ['not-a-timestamp', '2026-02-30T00:00:00Z', '2026-10-04T01:02:03']) {
  test(`invalid initial revision ${updatedAt} cannot start recovery`, async () => {
    const f = fixture()
    f.observed.updated_at = updatedAt
    await assert.rejects(f.run(), { code: 'REAUTH_ACCOUNT_NOT_ELIGIBLE' })
    assert.equal(f.calls.length, 0)
    assert.equal(f.closed(), 0)
  })
}

test('failed browser launch does not proceed to exchange', async () => {
  const f = fixture()
  f.launch.launched = false
  await assert.rejects(f.run(), { code: 'REAUTH_BROWSER_NOT_LAUNCHED' })
  assert.equal(f.calls.some((call) => call.path.endsWith('/exchange-code')), false)
  assert.equal(f.closed(), 1)
})

test('callback bind conflict stops before launching any browser', async () => {
  const f = fixture()
  f.listen = async () => { throw new ReauthorizationError('REAUTH_CALLBACK_UNAVAILABLE') }
  await assert.rejects(f.run(), { code: 'REAUTH_CALLBACK_UNAVAILABLE' })
  assert.equal(f.calls.some((call) => call.path.endsWith('/launch-auth-browser')), false)
})

test('an aborted operation cannot launch a browser', async () => {
  const f = fixture()
  await assert.rejects(recoverAccount({
    request: f.request, account: f.observed, signal: AbortSignal.abort(), listen: f.listen,
  }), { code: 'REAUTH_INTERRUPTED' })
  assert.equal(f.calls.length, 0)
})

test('banned, nested and active accounts are not automatic authorization failures', () => {
  assert.equal(isAuthorizationError('invalid_client 401'), false)
  assert.equal(isAuthorizationError('account_deactivated 401 token_revoked'), false)
  assert.equal(isAuthorizationError('upstream 503'), false)
  assert.equal(isRecoveryAccount({ ...account(), parent_account_id: 1 }), false)
  assert.equal(isRecoveryAccount({ ...account(), status: 'active' }), false)
})

for (const url of [
  'http://example.com:18420', 'http://localhost:18420', 'http://127.0.0.1:18420/?query=1',
  'http://127.0.0.1:18420/other', 'https://127.0.0.1:18420',
]) {
  test(`host address rejects ${url}`, () => {
    assert.throws(() => localHostURL(url), { code: 'REAUTH_LOCAL_HOST_REQUIRED' })
  })
}

async function httpFixture(handler) {
  const server = createServer(handler)
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const port = server.address().port
  return {
    origin: `http://127.0.0.1:${port}`,
    close: () => new Promise((resolve) => { server.close(resolve); server.closeAllConnections() }),
  }
}

test('HTTP client uses native envelope and refuses redirects without forwarding the admin session', async () => {
  let destinationHits = 0
  const destination = await httpFixture((_request, response) => { destinationHits++; response.end('{}') })
  const source = await httpFixture((request, response) => {
    if (request.url.endsWith('/accounts/17')) {
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ code: 0, data: { id: 17 } }))
    } else {
      response.writeHead(302, { Location: `${destination.origin}/capture` }).end()
    }
  })
  try {
    const request = hostRequest(source.origin, opaque(32))
    assert.equal((await request('/api/v1/admin/accounts/17')).id, 17)
    await assert.rejects(request('/api/v1/admin/openai/exchange-code', {}))
    assert.equal(destinationHits, 0)
  } finally {
    await source.close()
    await destination.close()
  }
})

test('real loopback callback rejects wrong state, duplicate code and host spoofing, then accepts once', async () => {
  const reserve = await httpFixture((_request, response) => response.end())
  const redirectURI = `${reserve.origin}/auth/callback`
  await reserve.close()
  const state = opaque(64)
  const listener = await listenForAuthorization({ redirectURI, state })
  try {
    const get = (query, options) => fetch(`${redirectURI}?${query}`, options)
    assert.equal((await get('state=incorrect&code=synthetic')).status, 400)
    assert.equal((await get(`state=${state}&code=a&code=b`)).status, 400)
    const spoofStatus = await new Promise((resolve, reject) => {
      httpGet(`${redirectURI}?state=${state}&code=synthetic`, { headers: { Host: 'example.com' } }, (response) => {
        response.resume()
        resolve(response.statusCode)
      }).on('error', reject)
    })
    assert.equal(spoofStatus, 400)
    const code = opaque(32)
    const response = await get(`state=${state}&code=${code}`)
    assert.equal(response.status, 200)
    const text = await response.text()
    assert.equal(text.includes(code), false)
    assert.equal(response.headers.get('Cache-Control'), 'no-store')
    const received = await listener.wait()
    assert.ok(received.code === code && received.state === state)
    assert.equal((await get(`state=${state}&code=${code}`)).status, 409)
  } finally { await listener.close() }
})

test('loopback denial and cancellation settle without an unhandled promise rejection', async () => {
  for (const mode of ['denied', 'cancelled']) {
    const reserve = await httpFixture((_request, response) => response.end())
    const redirectURI = `${reserve.origin}/auth/callback`
    await reserve.close()
    const controller = new AbortController()
    const state = opaque(64)
    const listener = await listenForAuthorization({ redirectURI, state, signal: controller.signal })
    try {
      if (mode === 'denied') {
        const response = await fetch(`${redirectURI}?state=${state}&error=access_denied`)
        assert.equal(response.status, 400)
      } else {
        controller.abort()
      }
      await assert.rejects(listener.wait(), {
        code: mode === 'denied' ? 'REAUTH_AUTHORIZATION_REJECTED' : 'REAUTH_CANCELLED',
      })
    } finally { await listener.close() }
  }
})
