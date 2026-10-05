import { createServer } from 'node:http'

export class ReauthorizationError extends Error {
  constructor(code) {
    super(code)
    this.name = 'ReauthorizationError'
    this.code = code
  }
}

const fail = (code) => { throw new ReauthorizationError(code) }
const positiveID = (value) => Number.isSafeInteger(value) && value > 0
const hexID = (value, length) =>
  typeof value === 'string' && new RegExp(`^[a-f0-9]{${length}}$`).test(value)

function revisionInstant(value) {
  if (typeof value !== 'string') return null
  const match = /^(\d{4}-\d{2}-\d{2})T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.(\d{1,9}))?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$/.exec(value)
  if (!match) return null
  const milliseconds = Date.parse(value)
  const day = Date.parse(`${match[1]}T00:00:00Z`)
  if (!Number.isFinite(milliseconds) || !Number.isFinite(day) ||
      new Date(day).toISOString().slice(0, 10) !== match[1]) return null
  // Account CAS revisions retain sub-millisecond precision in PostgreSQL.
  const remainder = (match[2] || '').padEnd(9, '0').slice(3)
  return BigInt(milliseconds) * 1000000n + BigInt(remainder)
}

// Classification adapted from the supplied MIT-licensed onboarding worker.
// Unlike that worker, this path never imports detached credentials.
export function isAuthorizationError(message) {
  const text = String(message || '').toLowerCase()
  if (/account_deactivated|account_deleted|deactivated_workspace|account suspended|account banned|insufficient_scope|unauthorized_client|invalid_client/.test(text)) return false
  return /\b401\b|token_revoked|token_invalidated|invalid_grant|refresh_token_reused|refresh_token_expired|refresh token expired|token invalid|token expired|refresh_token missing|authentication token/.test(text)
}

export function isRecoveryAccount(account) {
  return account?.platform === 'openai' && account.type === 'oauth' &&
    account.status === 'error' && !account.parent_account_id &&
    isAuthorizationError(account.error_message)
}

function captureAccount(account) {
  const instant = revisionInstant(account?.updated_at)
  if (!isRecoveryAccount(account) || !positiveID(account.id) ||
      !positiveID(account.proxy_id) || instant === null ||
      (account.reauthorization_revision !== undefined &&
        !/^oauth-v1:[a-f0-9]{64}$/.test(account.reauthorization_revision))) {
    fail('REAUTH_ACCOUNT_NOT_ELIGIBLE')
  }
  // Do not hold a mutable account object or refresh this revision at upload.
  return Object.freeze({
    id: account.id,
    proxyID: account.proxy_id,
    revision: account.updated_at,
    authorizationRevision: account.reauthorization_revision,
    instant,
  })
}

function assertCurrent(account, binding) {
  if (!isRecoveryAccount(account) || account.id !== binding.id ||
      account.proxy_id !== binding.proxyID ||
      (binding.authorizationRevision
        ? account.reauthorization_revision !== binding.authorizationRevision
        : account.updated_at !== binding.revision)) {
    fail('REAUTH_ACCOUNT_CHANGED')
  }
}

function callbackTarget(value) {
  let target
  try { target = new URL(value) } catch { fail('REAUTH_CALLBACK_INVALID') }
  if (target.protocol !== 'http:' ||
      !['localhost', '127.0.0.1', '[::1]'].includes(target.hostname) ||
      target.username || target.password || target.search || target.hash ||
      !target.port || Number(target.port) < 1024 ||
      target.pathname !== '/auth/callback') {
    fail('REAUTH_CALLBACK_INVALID')
  }
  return target
}

function authorizationSession(value) {
  let authURL
  try { authURL = new URL(value?.auth_url) } catch { fail('REAUTH_SESSION_INVALID') }
  if (!hexID(value?.session_id, 32) ||
      authURL.origin !== 'https://auth.openai.com' ||
      authURL.pathname !== '/oauth/authorize' || authURL.username ||
      authURL.password || authURL.hash ||
      authURL.searchParams.getAll('state').length !== 1 ||
      authURL.searchParams.getAll('redirect_uri').length !== 1 ||
      !hexID(authURL.searchParams.get('state'), 64)) {
    fail('REAUTH_SESSION_INVALID')
  }
  return Object.freeze({
    id: value.session_id,
    state: authURL.searchParams.get('state'),
    redirectURI: callbackTarget(authURL.searchParams.get('redirect_uri')).href,
  })
}

// The only browser is the host's existing fixed-egress launcher. This listener
// receives its loopback redirect; it does not log URLs, codes or credentials.
export async function listenForAuthorization({ redirectURI, state, signal }) {
  const target = callbackTarget(redirectURI)
  if (!hexID(state, 64)) fail('REAUTH_SESSION_INVALID')
  signal?.throwIfAborted()
  let settle
  let settled = false
  const outcome = new Promise((resolve) => { settle = resolve })
  const finish = (result) => {
    if (settled) return
    settled = true
    settle(result)
  }
  const server = createServer({ maxHeaderSize: 8192 }, (request, response) => {
    response.setHeader('Cache-Control', 'no-store')
    response.setHeader('Referrer-Policy', 'no-referrer')
    response.setHeader('Content-Type', 'text/plain; charset=utf-8')
    response.setHeader('Content-Security-Policy', "default-src 'none'; frame-ancestors 'none'")
    let url
    try { url = new URL(request.url, target.origin) } catch {
      response.writeHead(400).end('Invalid callback')
      return
    }
    const valid = request.method === 'GET' && request.headers.host === target.host &&
      url.origin === target.origin && url.pathname === target.pathname &&
      url.searchParams.getAll('state').length === 1 &&
      url.searchParams.get('state') === state
    if (!valid) {
      response.writeHead(400).end('Invalid callback')
      return
    }
    if (settled) {
      response.writeHead(409).end('Callback already received')
      return
    }
    const codes = url.searchParams.getAll('code')
    const errors = url.searchParams.getAll('error')
    if (errors.length === 1 && codes.length === 0) {
      response.writeHead(400).end('Authorization was not completed')
      finish({ error: new ReauthorizationError('REAUTH_AUTHORIZATION_REJECTED') })
      return
    }
    if (errors.length || codes.length !== 1 || !codes[0].trim() ||
        codes[0].length > 4096 || /[\x00-\x20\x7f]/.test(codes[0])) {
      response.writeHead(400).end('Invalid callback')
      return
    }
    response.end('Authorization received. Return to Sub2API for the result.')
    finish({ value: { code: codes[0], state } })
  })
  server.headersTimeout = 5000
  server.requestTimeout = 5000
  server.keepAliveTimeout = 1000
  server.timeout = 5000
  server.maxConnections = 8
  server.on('clientError', (_error, socket) => { socket.destroy() })
  const onAbort = () => finish({ error: new ReauthorizationError('REAUTH_CANCELLED') })
  signal?.addEventListener('abort', onAbort, { once: true })
  const close = async () => {
    signal?.removeEventListener('abort', onAbort)
    finish({ error: new ReauthorizationError('REAUTH_CANCELLED') })
    await new Promise((resolve) => {
      server.close(resolve)
      server.closeAllConnections()
    })
  }
  try {
    await new Promise((resolve, reject) => {
      server.once('error', reject)
      server.listen(Number(target.port), target.hostname === '[::1]' ? '::1' : '127.0.0.1', () => {
        server.removeListener('error', reject)
        resolve()
      })
    })
    server.on('error', () => finish({ error: new ReauthorizationError('REAUTH_CALLBACK_FAILED') }))
    if (signal?.aborted) onAbort()
  } catch {
    await close()
    fail('REAUTH_CALLBACK_UNAVAILABLE')
  }
  return {
    wait: async () => {
      const result = await outcome
      if (result.error) throw result.error
      return result.value
    },
    close,
  }
}

// One explicit account per invocation, no periodic scanning, route fallback,
// password store, or automatic replay after an ambiguous exchange/commit.
export async function recoverAccount({
  request, account, signal, listen = listenForAuthorization,
}) {
  const deadline = AbortSignal.timeout(5 * 60 * 1000)
  signal = signal ? AbortSignal.any([signal, deadline]) : deadline
  const binding = captureAccount(account)
  let listener
  let stage = 'prepare'
  const call = async (path, body) => {
    signal?.throwIfAborted()
    return request(path, body, signal)
  }
  const checkCurrent = async () => {
    assertCurrent(await call(`/api/v1/admin/accounts/${binding.id}`), binding)
  }
  try {
    await checkCurrent()
    const session = authorizationSession(await call('/api/v1/admin/openai/generate-auth-url', {
      account_id: binding.id,
      expected_updated_at: binding.revision,
      ...(binding.authorizationRevision
        ? { expected_authorization_revision: binding.authorizationRevision } : {}),
      proxy_id: binding.proxyID,
    }))
    listener = await listen({ ...session, signal })
    const launch = await call('/api/v1/admin/openai/launch-auth-browser', { session_id: session.id })
    if (launch?.launched !== true) fail('REAUTH_BROWSER_NOT_LAUNCHED')
    const callback = await listener.wait()
    if (!callback?.code || callback.state !== session.state) fail('REAUTH_CALLBACK_INVALID')
    await checkCurrent()
    stage = 'exchange'
    const result = await call('/api/v1/admin/openai/exchange-code', {
      session_id: session.id,
      proxy_id: binding.proxyID,
      code: callback.code,
      state: callback.state,
      redirect_uri: session.redirectURI,
    })
    stage = 'prepare'
    if (result?.proxy_id !== binding.proxyID || !hexID(result?.reauthorization_proof, 32) ||
        !result.credentials || typeof result.credentials !== 'object' ||
        Array.isArray(result.credentials)) {
      fail('REAUTH_PROOF_REQUIRED')
    }
    await checkCurrent()
    stage = 'commit'
    const committed = await call(`/api/v1/admin/accounts/${binding.id}/apply-oauth-credentials`, {
      type: 'oauth',
      expected_updated_at: binding.revision,
      reauthorization_proof: result.reauthorization_proof,
      credentials: result.credentials,
    })
    const committedInstant = revisionInstant(committed?.updated_at)
    if (committed?.id !== binding.id || committedInstant === null ||
        committedInstant <= binding.instant) {
      fail('REAUTH_COMMIT_OUTCOME_UNKNOWN')
    }
    return { account_id: committed.id, updated_at: committed.updated_at, committed: true }
  } catch (error) {
    if (stage === 'commit' || stage === 'exchange') {
      if (!(error instanceof ReauthorizationError) ||
          /^REAUTH_HOST_HTTP_5/.test(error.code) ||
          error.code === 'REAUTH_HOST_HTTP_408' ||
          error.code === 'REAUTH_HOST_RESPONSE_INVALID') {
        fail(stage === 'commit' ? 'REAUTH_COMMIT_OUTCOME_UNKNOWN' : 'REAUTH_EXCHANGE_OUTCOME_UNKNOWN')
      }
    }
    if (error instanceof ReauthorizationError) throw error
    fail(stage === 'commit' ? 'REAUTH_COMMIT_OUTCOME_UNKNOWN' :
      stage === 'exchange' ? 'REAUTH_EXCHANGE_OUTCOME_UNKNOWN' : 'REAUTH_INTERRUPTED')
  } finally {
    await listener?.close()
  }
}
