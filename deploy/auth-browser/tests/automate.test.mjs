import test from 'node:test'
import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { PassThrough } from 'node:stream'
import { mkdtemp, readdir, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { advanceLogin, automate, authorizationBinding, BrowserPipe, parseCallback, totp } from '../automate.mjs'

const state = 'a'.repeat(64)
const redirect = 'http://localhost:1455/auth/callback'
const authURL = 'https://auth.openai.com/oauth/authorize?' + new URLSearchParams({ state, redirect_uri: redirect })
const callback = (query) => `${redirect}?${new URLSearchParams(query)}`

test('callback requires the exact loopback target, unique state and a single bounded code', () => {
  const binding = authorizationBinding(authURL)
  assert.deepEqual(parseCallback(callback({ state, code: 'fixture-code' }), binding), { state, code: 'fixture-code' })
  assert.equal(parseCallback('https://example.invalid/auth/callback', binding), null)
  for (const value of [
    callback({ state: 'b'.repeat(64), code: 'fixture-code' }),
    callback({ state, code: '' }), callback({ state, error: 'access_denied' }),
    callback({ state, code: 'fixture-code' }) + '&code=another',
    callback({ state, code: 'fixture-code' }) + '&state=' + state,
    callback({ state, code: 'contains space' }), callback({ state, code: 'x'.repeat(4097) }),
  ]) assert.throws(() => parseCallback(value, binding), /AUTH_/)
  for (const target of ['https://example.invalid/auth/callback', 'http://localhost/auth/callback',
    'http://127.0.0.1:1455/other', 'http://localhost:1455/auth/callback?existing=1']) {
    const invalid = new URL(authURL)
    invalid.searchParams.set('redirect_uri', target)
    assert.throws(() => authorizationBinding(invalid), /AUTH_CALLBACK_INVALID/)
  }
})

test('TOTP matches RFC 6238 SHA1 vectors and rejects malformed input', () => {
  // Published RFC test vector, not an account secret.
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  const bits = [...Buffer.from('12345678901234567890')].map(v => v.toString(2).padStart(8, '0')).join('')
  const fixture = bits.match(/.{5}/g).map(v => alphabet[parseInt(v, 2)]).join('')
  assert.equal(totp(fixture, 59000, 8), '94287082')
  assert.equal(totp(fixture, 1111111109000, 8), '07081804')
  assert.equal(totp(fixture, 59000), '287082')
  for (const value of ['', 'bad!', 'A'.repeat(206), 'A'.repeat(16) + 'B']) {
    assert.throws(() => totp(value), /AUTH_TOTP_INVALID/)
  }
})

test('an uncertain submission is registered before CDP and cannot be replayed', async () => {
  const submitted = new Set()
  const stage = { key: '/log-in:email', kind: 'email' }
  let submissions = 0
  const pipe = {
    async call(_method, params) {
      if (params.expression.includes(')(null,')) return { result: { value: stage } }
      assert.equal(submitted.has(stage.key), true)
      submissions++
      throw new Error('AUTH_BROWSER_NAVIGATION')
    },
  }
  const login = { email: 'fixture@example.invalid' }
  await advanceLogin(pipe, 'fixture-session', login, submitted)
  await advanceLogin(pipe, 'fixture-session', login, submitted)
  assert.equal(submissions, 1)
})

test('read-only navigation races may be retried but non-navigation failures are surfaced', async () => {
  const submitted = new Set()
  const pipe = { call: async () => { throw new Error('AUTH_BROWSER_NAVIGATION') } }
  await advanceLogin(pipe, 'fixture-session', {}, submitted)
  assert.equal(submitted.size, 0)
  pipe.call = async () => { throw new Error('AUTH_BROWSER_PROTOCOL_TIMEOUT') }
  await assert.rejects(advanceLogin(pipe, 'fixture-session', {}, submitted), /AUTH_BROWSER_PROTOCOL_TIMEOUT/)
  pipe.call = async () => ({ exceptionDetails: {} })
  await assert.rejects(advanceLogin(pipe, 'fixture-session', {}, submitted), /AUTH_PAGE_SCRIPT_FAILED/)
})

test('CDP distinguishes navigation from protocol failure and closes pending work on exit', async () => {
  const child = new EventEmitter()
  child.stdio = [null, null, null, new PassThrough(), new PassThrough()]
  const pipe = new BrowserPipe(child)
  const pending = pipe.call('Runtime.evaluate')
  child.stdio[4].write(JSON.stringify({ id: 1, error: { message: 'Execution context was destroyed.' } }) + '\0')
  await assert.rejects(pending, /AUTH_BROWSER_NAVIGATION/)
  const closed = pipe.call('Page.enable')
  child.emit('exit')
  await assert.rejects(closed, /AUTH_BROWSER_CLOSED/)
  await assert.rejects(pipe.call('Page.enable'), /AUTH_BROWSER_CLOSED/)
})

test('missing browser fails without hanging and removes only the temporary profile', { timeout: 5000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), 'reauth-fixture-'))
  const login = { email: 'fixture@example.invalid', password: 'fixture-only', totp_secret: '' }
  try {
    await assert.rejects(automate({
      chrome: join(root, 'missing-browser'), profileRoot: root, profileTag: 'fixture',
      authURL, proxy: 'http://127.0.0.1:17931', login,
    }), /AUTH_BROWSER_CLOSED/)
    assert.deepEqual(await readdir(root), [])
    assert.equal(login.password, '')
    assert.equal(login.totp_secret, '')
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})

test('pre-canceled work and non-loopback ingress never start a browser', async () => {
  const controller = new AbortController()
  controller.abort()
  const options = {
    chrome: '/must-not-run', profileRoot: '/must-not-create', profileTag: 'fixture',
    authURL, proxy: 'http://127.0.0.1:17931',
    login: { email: 'fixture@example.invalid', password: 'fixture-only' },
  }
  await assert.rejects(automate({ ...options, signal: controller.signal }), { name: 'AbortError' })
  await assert.rejects(automate({ ...options, proxy: 'http://example.invalid:8080' }), /AUTH_INPUT_INVALID/)
})
