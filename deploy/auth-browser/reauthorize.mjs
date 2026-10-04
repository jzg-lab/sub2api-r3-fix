import { pathToFileURL } from 'node:url'
import { parseArgs } from 'node:util'
import { recoverAccount, ReauthorizationError } from './reauthorization.mjs'

export function localHostURL(value) {
  const url = new URL(value)
  if (url.protocol !== 'http:' || !['127.0.0.1', '[::1]'].includes(url.hostname) ||
      !url.port || url.username || url.password || url.pathname !== '/' ||
      url.search || url.hash) {
    throw new ReauthorizationError('REAUTH_LOCAL_HOST_REQUIRED')
  }
  return url.origin
}

export function hostRequest(baseURL, adminSession) {
  const origin = localHostURL(baseURL)
  if (typeof adminSession !== 'string' || !adminSession.trim() ||
      /[\x00-\x20\x7f]/.test(adminSession) || adminSession.length > 16384) {
    throw new ReauthorizationError('REAUTH_ADMIN_SESSION_REQUIRED')
  }
  return async (path, body, signal) => {
    if (!path.startsWith('/api/v1/admin/') || path.startsWith('//')) {
      throw new ReauthorizationError('REAUTH_PATH_INVALID')
    }
    const response = await fetch(origin + path, {
      method: body === undefined ? 'GET' : 'POST',
      redirect: 'error',
      headers: { Authorization: `Bearer ${adminSession}`, 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(60000)]) : AbortSignal.timeout(60000),
    })
    // Backend error bodies can include account details. Never echo them.
    if (!response.ok) {
      await response.body?.cancel()
      throw new ReauthorizationError(`REAUTH_HOST_HTTP_${response.status}`)
    }
    const payload = await response.json()
    if (payload?.code !== 0 || !payload.data) {
      throw new ReauthorizationError('REAUTH_HOST_RESPONSE_INVALID')
    }
    return payload.data
  }
}

async function main() {
  const { values } = parseArgs({
    options: { 'base-url': { type: 'string' }, 'account-id': { type: 'string' } },
    allowPositionals: false,
  })
  const id = Number(values['account-id'])
  if (!Number.isSafeInteger(id) || id <= 0) throw new ReauthorizationError('REAUTH_ACCOUNT_ID_REQUIRED')
  const baseURL = localHostURL(values['base-url'])
  if (process.stdin.isTTY) throw new ReauthorizationError('REAUTH_ADMIN_SESSION_STDIN_REQUIRED')
  const abort = new AbortController()
  const stop = () => abort.abort()
  process.once('SIGINT', stop)
  process.once('SIGTERM', stop)
  const timer = setTimeout(stop, 5 * 60 * 1000)
  const onAbort = () => process.stdin.destroy()
  abort.signal.addEventListener('abort', onAbort, { once: true })
  try {
    let input = ''
    for await (const chunk of process.stdin) {
      input += chunk.toString('utf8')
      if (input.length > 16384) throw new ReauthorizationError('REAUTH_INPUT_TOO_LARGE')
    }
    const request = hostRequest(baseURL, JSON.parse(input).admin_session)
    input = ''
    const account = await request(`/api/v1/admin/accounts/${id}`, undefined, abort.signal)
    const result = await recoverAccount({ request, account, signal: abort.signal })
    process.stdout.write(`${JSON.stringify(result)}\n`)
  } finally {
    clearTimeout(timer)
    abort.signal.removeEventListener('abort', onAbort)
    process.removeListener('SIGINT', stop)
    process.removeListener('SIGTERM', stop)
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    const code = error instanceof ReauthorizationError ? error.code : 'REAUTH_FAILED'
    process.stderr.write(`${code}\n`)
    process.exitCode = 1
  })
}
