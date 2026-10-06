import { createHmac } from 'node:crypto'
import { spawn } from 'node:child_process'
import { mkdir, mkdtemp, rm } from 'node:fs/promises'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { setTimeout as sleep } from 'node:timers/promises'

const fail = (code) => { throw new Error(code) }
const authOrigin = 'https://auth.openai.com'

export function totp(secret, milliseconds = Date.now(), digits = 6) {
  const normalized = String(secret).toUpperCase().replace(/[\s-]/g, '').replace(/=+$/, '')
  if (!/^[A-Z2-7]{16,205}$/.test(normalized) || ![6, 8].includes(digits) ||
      !Number.isFinite(milliseconds) || milliseconds < 0) fail('AUTH_TOTP_INVALID')
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = 0
  let value = 0
  const bytes = []
  for (const character of normalized) {
    value = (value << 5) | alphabet.indexOf(character)
    bits += 5
    if (bits >= 8) {
      bits -= 8
      bytes.push((value >> bits) & 255)
      value &= (1 << bits) - 1
    }
  }
  if (value !== 0) fail('AUTH_TOTP_INVALID')
  const key = Buffer.from(bytes)
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(Math.floor(milliseconds / 30000)))
  const hash = createHmac('sha1', key).update(counter).digest()
  key.fill(0)
  const offset = hash[hash.length - 1] & 15
  const number = (hash.readUInt32BE(offset) & 0x7fffffff) % (10 ** digits)
  hash.fill(0)
  return String(number).padStart(digits, '0')
}

export function authorizationBinding(value) {
  const url = new URL(value)
  if (url.origin !== authOrigin || url.pathname !== '/oauth/authorize' || url.username ||
      url.password || url.hash || url.searchParams.getAll('state').length !== 1 ||
      url.searchParams.getAll('redirect_uri').length !== 1 ||
      !/^[a-f0-9]{64}$/.test(url.searchParams.get('state') || '')) fail('AUTH_SESSION_INVALID')
  const redirect = new URL(url.searchParams.get('redirect_uri'))
  if (redirect.protocol !== 'http:' || !['localhost', '127.0.0.1', '[::1]'].includes(redirect.hostname) ||
      !redirect.port || Number(redirect.port) < 1024 || redirect.pathname !== '/auth/callback' ||
      redirect.username || redirect.password || redirect.search || redirect.hash) fail('AUTH_CALLBACK_INVALID')
  return { redirect, state: url.searchParams.get('state') }
}

export function parseCallback(value, binding) {
  const url = new URL(value)
  if (url.origin !== binding.redirect.origin || url.pathname !== binding.redirect.pathname) return null
  if (url.username || url.password || url.hash || url.searchParams.getAll('state').length !== 1 ||
      url.searchParams.get('state') !== binding.state) fail('AUTH_CALLBACK_INVALID')
  const codes = url.searchParams.getAll('code')
  if (url.searchParams.has('error')) fail('AUTH_REJECTED')
  if (codes.length !== 1 || !codes[0] || codes[0].length > 4096 ||
      /[\x00-\x20\x7f]/.test(codes[0])) fail('AUTH_CALLBACK_INVALID')
  return { code: codes[0], state: binding.state }
}

// Executed in the top-level page. Never fill third-party identity providers,
// payment forms, email/SMS verification, or CAPTCHA/security challenges.
export function loginStep(input, submitted, targetKey) {
  if (location.origin !== 'https://auth.openai.com') return { kind: 'manual' }
  const changesAccount = pathname =>
    /(?:^|\/)(?:create-account|signup|sign-up|reset-password|forgot-password)(?:\/|$)/i.test(pathname)
  if (changesAccount(location.pathname)) return { kind: 'manual' }
  const visible = (element) => element && !element.disabled && element.getClientRects().length > 0
  const find = (selector) => [...document.querySelectorAll(selector)].find(visible)
  const safeDestination = value => {
    try {
      const target = new URL(value || location.href, location.href)
      return target.origin === location.origin && !target.username && !target.password && !changesAccount(target.pathname)
    } catch { return false }
  }
  const safeSubmit = (button, form) =>
    (!form || safeDestination(form.action)) &&
    (!button.hasAttribute('formaction') || safeDestination(button.formAction))
  const text = document.body?.innerText || ''
  if (/captcha|verify you are human|security challenge/i.test(text)) return { kind: 'manual' }
  if (find('input[autocomplete="new-password"]')) return { kind: 'manual' }
  const password = find('input[type="password"][autocomplete="current-password"], input[type="password"][name="password"]')
  const email = find('input[type="email"], input[autocomplete="username"], input[name="username"]')
  const otp = /authenticator|authentication app/i.test(text)
    ? find('input[autocomplete="one-time-code"], input[name="code"], input[name="otp"]') : null
  const field = password || otp || email
  const kind = password ? 'password' : otp ? 'totp' : email ? 'email' : 'consent'
  const key = `${location.href.split('?')[0]}:${kind}`
  if (submitted.includes(key)) return { kind: 'waiting' }
  if (kind === 'consent') {
    if (!/\/consent(?:\/|$)/.test(location.pathname)) return { kind: 'manual' }
    const button = [...document.querySelectorAll('button')].find((element) =>
      visible(element) && /^(allow|authorize|continue)$/i.test(element.textContent.trim()))
    if (!button || !safeSubmit(button, button.form)) return { kind: 'manual' }
    if (!input) return { kind, key }
    if (targetKey !== key) return { kind: 'manual' }
    button.click()
    return { kind, key }
  }
  const form = field.form
  if (!form || !safeDestination(form.action)) return { kind: 'manual' }
  const submit = [...form.elements].find(element =>
    visible(element) && ['BUTTON', 'INPUT'].includes(element.tagName) && element.type === 'submit')
  if (!submit || !safeSubmit(submit, form)) return { kind: 'manual' }
  if (!input) return { kind, key }
  if (targetKey !== key) return { kind: 'manual' }
  const value = kind === 'password' ? input.password : kind === 'totp' ? input.code : input.email
  if (!value) return { kind: 'manual' }
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set
  setter.call(field, value)
  field.dispatchEvent(new Event('input', { bubbles: true }))
  field.dispatchEvent(new Event('change', { bubbles: true }))
  submit.click()
  return { kind, key }
}

export class BrowserPipe {
  constructor(child) {
    this.child = child
    this.next = 0
    this.pending = new Map()
    this.buffer = ''
    child.stdio[4].setEncoding('utf8')
    child.stdio[4].on('data', (chunk) => {
      this.buffer += chunk
      if (this.buffer.length > 2 ** 20) return this.close()
      let separator
      while ((separator = this.buffer.indexOf('\0')) >= 0) {
        const raw = this.buffer.slice(0, separator)
        this.buffer = this.buffer.slice(separator + 1)
        let message
        try { message = JSON.parse(raw) } catch { this.close(); return }
        if (message.id) {
          const pending = this.pending.get(message.id)
          if (pending) {
            this.pending.delete(message.id)
            clearTimeout(pending.timer)
            if (message.error) {
              const navigation = pending.method === 'Runtime.evaluate' &&
                /execution context.*destroyed|cannot find context|inspected target navigated/i.test(message.error.message || '')
              pending.reject(new Error(navigation ? 'AUTH_BROWSER_NAVIGATION' : 'AUTH_BROWSER_PROTOCOL_ERROR'))
            }
            else pending.resolve(message.result)
          }
        } else {
          this.onEvent?.(message)
        }
      }
    })
    child.once('exit', () => this.close())
    child.once('error', () => this.close())
    child.stdio[3].on('error', () => this.close())
    child.stdio[4].on('error', () => this.close())
  }
  call(method, params = {}, sessionId) {
    if (this.closed) return Promise.reject(new Error('AUTH_BROWSER_CLOSED'))
    const id = ++this.next
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id)
        reject(new Error('AUTH_BROWSER_PROTOCOL_TIMEOUT'))
      }, 5000)
      this.pending.set(id, { resolve, reject, timer, method })
      this.child.stdio[3].write(JSON.stringify({ id, method, params, sessionId }) + '\0')
    })
  }
  close() {
    this.closed = true
    for (const { reject, timer } of this.pending.values()) {
      clearTimeout(timer)
      reject(new Error('AUTH_BROWSER_CLOSED'))
    }
    this.pending.clear()
  }
}

export async function advanceLogin(pipe, sessionId, login, submitted) {
  const evaluate = async (input, targetKey) => {
    try {
      const response = await pipe.call('Runtime.evaluate', {
        expression: `(${loginStep.toString()})(${JSON.stringify(input)},${JSON.stringify(input ? [] : [...submitted])},${JSON.stringify(targetKey ?? null)})`,
        returnByValue: true,
      }, sessionId)
      if (response.exceptionDetails) fail('AUTH_PAGE_SCRIPT_FAILED')
      return response.result?.value
    } catch (error) {
      if (error.message === 'AUTH_BROWSER_NAVIGATION') return undefined
      throw error
    }
  }
  const next = await evaluate(null)
  if (!next?.key || !['email', 'password', 'totp', 'consent'].includes(next.kind)) return
  if (submitted.has(next.key)) return
  const input = {}
  if (next.kind === 'email') input.email = login.email
  if (next.kind === 'password') input.password = login.password
  if (next.kind === 'totp') {
    if (!login.totp_secret) return
    input.code = totp(login.totp_secret)
  }
  // Register before submitting: navigation may lose the response after a
  // successful click. An uncertain submission must never be replayed.
  submitted.add(next.key)
  try {
    await evaluate(input, next.key)
  } finally {
    for (const key of Object.keys(input)) input[key] = ''
  }
}

export async function automate({ chrome, profileRoot, profileTag, authURL, proxy, login, signal }) {
  const binding = authorizationBinding(authURL)
  const ingress = new URL(proxy)
  if (!login || !/^[a-zA-Z0-9._-]{1,100}$/.test(profileTag) ||
      !['http:', 'https:', 'socks5:'].includes(ingress.protocol) ||
      !['127.0.0.1', 'localhost', '[::1]'].includes(ingress.hostname) ||
      !ingress.port || ingress.username || ingress.password || ingress.search || ingress.hash ||
      !['', '/'].includes(ingress.pathname) || typeof login.email !== 'string' ||
      !/^[^\s@]+@[^\s@]+$/.test(login.email) || login.email.length > 254 ||
      typeof login.password !== 'string' || !login.password || login.password.length > 4096 ||
      login.password.includes('\0') ||
      (login.totp_secret != null && typeof login.totp_secret !== 'string')) fail('AUTH_INPUT_INVALID')
  if (login.totp_secret) totp(login.totp_secret)
  signal?.throwIfAborted()
  await mkdir(profileRoot, { recursive: true, mode: 0o700 })
  const profile = await mkdtemp(join(profileRoot, `${profileTag}-auto-`))
  let child
  let pipe
  let watchdog
  const stop = () => child?.kill('SIGTERM')
  try {
    child = spawn(chrome, [
      `--user-data-dir=${profile}`, `--proxy-server=${proxy}`,
      '--remote-debugging-pipe', '--no-first-run', '--no-default-browser-check',
      '--incognito', '--disable-sync', '--disable-extensions', '--disable-quic',
      '--disable-features=PasswordManagerOnboarding,PasswordLeakDetection',
      '--force-webrtc-ip-handling-policy=disable_non_proxied_udp',
      '--lang=en-US', '--accept-lang=en-US', 'about:blank',
    ], { stdio: ['ignore', 'ignore', 'ignore', 'pipe', 'pipe'], env: { ...process.env, TZ: 'America/New_York' } })
    pipe = new BrowserPipe(child)
    signal?.addEventListener('abort', stop, { once: true })
    if (signal?.aborted) { stop(); signal.throwIfAborted() }
    const originalParent = process.ppid
    watchdog = setInterval(() => { if (process.ppid !== originalParent) stop() }, 1000)
    const { targetId } = await pipe.call('Target.createTarget', { url: 'about:blank' })
    const { sessionId } = await pipe.call('Target.attachToTarget', { targetId, flatten: true })
    await pipe.call('Page.enable', {}, sessionId)
    let callback
    let callbackError
    pipe.onEvent = (message) => {
      if (message.sessionId !== sessionId || message.method !== 'Fetch.requestPaused') return
      const { requestId, request } = message.params
      try {
        callback = parseCallback(request.url, binding)
        if (!callback || request.method !== 'GET') fail('AUTH_CALLBACK_INVALID')
      } catch { callbackError = new Error('AUTH_CALLBACK_INVALID') }
      // No callback code is sent to an unrelated process on the loopback port.
      void pipe.call('Fetch.failRequest', { requestId, errorReason: 'Aborted' }, sessionId).catch(() => {})
    }
    await pipe.call('Fetch.enable', { patterns: [{ urlPattern: `${binding.redirect.origin}/auth/callback*`, requestStage: 'Request' }] }, sessionId)
    await pipe.call('Page.navigate', { url: authURL }, sessionId)
    const submitted = new Set()
    const deadline = Date.now() + 4 * 60 * 1000
    while (!callback && !callbackError && Date.now() < deadline) {
      signal?.throwIfAborted()
      if (pipe.closed) fail('AUTH_BROWSER_CLOSED')
      await advanceLogin(pipe, sessionId, login, submitted)
      if (!callback) await sleep(400, undefined, { signal })
    }
    if (callbackError) throw callbackError
    if (!callback) fail('AUTH_INTERACTION_TIMEOUT')
    return callback
  } finally {
    login.password = login.totp_secret = ''
    clearInterval(watchdog)
    signal?.removeEventListener('abort', stop)
    pipe?.close()
    if (child?.pid && child.exitCode === null && child.signalCode === null) {
      await new Promise((resolve) => {
        const force = setTimeout(() => child.kill('SIGKILL'), 2000)
        child.once('exit', () => { clearTimeout(force); resolve() })
        stop()
      })
    }
    await rm(profile, { recursive: true, force: true })
  }
}

async function main() {
  const [chrome, profileRoot, profileTag, authURL, proxy] = process.argv.slice(2)
  let raw = ''
  for await (const chunk of process.stdin) {
    raw += chunk
    if (raw.length > 16 * 1024) fail('AUTH_INPUT_INVALID')
  }
  const login = JSON.parse(raw)
  raw = ''
  const controller = new AbortController()
  for (const name of ['SIGINT', 'SIGTERM']) process.once(name, () => controller.abort())
  const result = await automate({ chrome, profileRoot, profileTag, authURL, proxy, login, signal: controller.signal })
  process.stdout.write(JSON.stringify(result))
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(() => {
    // Never emit browser errors, callback URLs, credentials or the submitted DOM.
    process.stderr.write('Automatic authorization did not complete.\n')
    process.exitCode = 1
  })
}
