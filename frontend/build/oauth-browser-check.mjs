import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { createServer } from 'vite'
import vue from '@vitejs/plugin-vue'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
process.chdir(root)
const require = createRequire(import.meta.url)
const playwrightPath = process.env.PLAYWRIGHT_MODULE || require.resolve('playwright')
const playwright = await import(pathToFileURL(playwrightPath).href)
const { chromium } = playwright.default || playwright
const output = resolve(root, '../output/playwright/oauth')
const proxy = { id: 23, name: 'Fixed fixture route', protocol: 'socks5', host: '127.0.0.1', port: 19023 }
const account = {
  id: 71, name: 'Browser fixture account', platform: 'openai', type: 'oauth',
  proxy_id: proxy.id, credentials: {}, status: 'error', schedulable: true,
  updated_at: '2026-10-04T00:00:00.123456Z'
}
const entry = `
import { createApp, h, ref } from 'vue'
import { createPinia } from 'pinia'
import i18n, { initI18n } from '/src/i18n/index.ts'
import { useAppStore } from '/src/stores/app.ts'
import { apiClient } from '/src/api/client.ts'
import Create from '/src/components/account/CreateAccountModal.vue'
import Account from '/src/components/account/ReAuthAccountModal.vue'
import Admin from '/src/components/admin/account/ReAuthAccountModal.vue'
import '/src/style.css'
await initI18n()
const pinia = createPinia()
createApp({
  setup() {
    const show = ref(true)
    const current = ref(${JSON.stringify(account)})
    const mode = new URLSearchParams(location.search).get('mode') || 'create'
    const events = []
    const store = useAppStore(pinia)
    window.fixture = { events, switchAccount: () => {
      current.value = { ...current.value, id: 72, name: 'Newer fixture account',
        updated_at: '2026-10-04T00:00:01.123456Z' }
    }, toasts: () => store.toasts, pending: 0 }
    apiClient.interceptors.request.use(config => {
      window.fixture.pending += 1
      return config
    })
    apiClient.interceptors.response.use(response => {
      window.fixture.pending -= 1
      return response
    }, error => {
      window.fixture.pending -= 1
      return Promise.reject(error)
    })
    return () => h(mode === 'create' ? Create : mode === 'account' ? Account : Admin, {
      show: show.value, account: current.value,
      proxies: [${JSON.stringify(proxy)}], groups: [],
      onClose: () => { events.push('close'); show.value = false },
      onCreated: () => events.push('created'),
      onReauthorized: value => events.push('reauthorized:' + value.id)
    })
  }
}).use(pinia).use(i18n).mount('#app')
`
const server = await createServer({
  root, configFile: false, envFile: false,
  resolve: { alias: { '@': resolve(root, 'src') } },
  plugins: [
    vue(),
    {
      name: 'isolated-oauth-browser-fixture',
      resolveId(id) {
        if (id === '/oauth-browser-fixture.js') return '\0oauth-browser-fixture'
      },
      load(id) {
        if (id === '\0oauth-browser-fixture') return entry
      },
      configureServer(instance) {
        instance.middlewares.use((request, response, next) => {
          const pathname = new URL(request.url, 'http://fixture.invalid').pathname
          if (pathname === '/') {
            response.setHeader('Content-Type', 'text/html')
            response.end('<!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"></head><body><div id="app"></div><script type="module" src="/oauth-browser-fixture.js"></script></body></html>')
          } else if (pathname.startsWith('/api/') || pathname.startsWith('/v1/')) {
            response.statusCode = 503
            response.end('Unmocked fixture request')
          } else next()
        })
      }
    }
  ],
  // No production proxy or .env is loaded by this fixture.
  server: { host: '127.0.0.1', port: 0 },
  logLevel: 'error'
})
let browser
const results = []
const deferred = () => {
  let resolve
  const promise = new Promise(done => { resolve = done })
  return { promise, resolve }
}

async function scenario(mode, width, behavior = 'success') {
  const context = await browser.newContext({ viewport: { width, height: 900 }, locale: 'en-US' })
  const page = await context.newPage()
  page.setDefaultTimeout(20000)
  const errors = []
  const requests = []
  const gate = deferred()
  const launchStarted = deferred()
  const exchangeStarted = deferred()
  const persistenceStarted = deferred()
  let launchCount = 0
  let sessionCount = 0
  page.on('pageerror', error => errors.push(error.message))
  await context.route('**/*', async route => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.origin !== server.resolvedUrls.local[0].replace(/\/$/, '')) {
      errors.push(`Unexpected non-fixture request: ${url.origin}`)
      await route.abort()
      return
    }
    if (!url.pathname.startsWith('/api/')) return route.continue()
    const body = request.postDataJSON()
    requests.push({ path: url.pathname, method: request.method(), body })
    const reply = (data, status = 200) => route.fulfill({
      status, contentType: 'application/json',
      body: JSON.stringify(status === 200 ? { code: 0, data } : data)
    })
    if (url.pathname.endsWith('/generate-auth-url')) {
      sessionCount += 1
      return reply({
        session_id: `fixture-session-${sessionCount}`,
        auth_url: `https://auth.example.invalid/authorize?state=fixture-state-${sessionCount}`
      })
    }
    if (url.pathname.endsWith('/launch-auth-browser')) {
      launchCount += 1
      launchStarted.resolve()
      if (['automatic-cancel', 'automatic-switch', 'automatic-duplicate'].includes(behavior)) await gate.promise
      if (behavior === 'launcher-failure' || behavior === 'automatic-failure') {
        return reply({ code: 'OPENAI_AUTH_BROWSER_UNAVAILABLE', message: 'Fixture launcher unavailable' }, 503)
      }
      if (behavior === 'launcher-running' || behavior === 'launcher-not-launched' || behavior === 'automatic-running') {
        return reply({ launched: false, already_running: behavior !== 'launcher-not-launched' })
      }
      if (behavior.startsWith('automatic-')) {
        return reply({ launched: true, code: 'fixture-code', state: `fixture-state-${sessionCount}` })
      }
      return reply({ launched: true, already_running: false })
    }
    if (url.pathname === `/api/v1/admin/accounts/${account.id}` && request.method() === 'GET') return reply(account)
    if (url.pathname.endsWith('/exchange-code')) {
      exchangeStarted.resolve()
      if (behavior === 'switch-during-exchange' || behavior === 'duplicate-exchange') await gate.promise
      return reply({
        access_token: 'synthetic-browser-fixture', refresh_token: 'synthetic-browser-fixture',
        token_type: 'Bearer', expires_in: 3600, expires_at: 2000000000,
        proxy_id: proxy.id,
        initial_authorization_proof: 'fixture-initial-proof',
        reauthorization_proof: 'fixture-reauth-proof'
      })
    }
    if (url.pathname === '/api/v1/admin/accounts' && request.method() === 'POST') {
      persistenceStarted.resolve()
      if (behavior === 'duplicate-write') await gate.promise
      if (behavior === 'write-failure') return reply({ code: 'CONFLICT', message: 'Fixture proof expired' }, 409)
      return reply({ ...account, ...body })
    }
    if (url.pathname.endsWith('/apply-oauth-credentials')) {
      persistenceStarted.resolve()
      if (behavior === 'duplicate-write') await gate.promise
      return reply(account)
    }
    if (url.pathname.endsWith('/web-search-emulation')) return reply({ enabled: false, providers: [] })
    if (url.pathname.endsWith('/tls-fingerprint-profiles')) return reply([])
    if (url.pathname.endsWith('/antigravity/default-model-mapping')) return reply([])
    if (url.pathname.endsWith('/settings')) return reply({})
    if (url.pathname.endsWith('/check-mixed-channel-risk')) return reply({ has_risk: false })
    errors.push(`Unexpected fixture API: ${request.method()} ${url.pathname}`)
    return reply({ message: 'Unexpected fixture API' }, 500)
  })
  const submitted = page.getByRole('button', { name: 'Complete Authorization', exact: true })
  try {
    await page.goto(`${server.resolvedUrls.local[0]}?mode=${mode}`)
    if (behavior.startsWith('automatic-')) {
      await page.getByLabel('账号邮箱', { exact: true }).fill('fixture@example.invalid')
      await page.getByLabel('账号密码', { exact: true }).fill(['fixture', 'only'].join('-'))
      const start = page.getByRole('button', { name: '自动重新授权并覆盖', exact: true })
      await start.click()
      await launchStarted.promise
      assert.equal(await page.getByLabel('账号邮箱', { exact: true }).inputValue(), '')
      assert.equal(await page.getByLabel('账号密码', { exact: true }).inputValue(), '')
      if (behavior === 'automatic-duplicate') {
        assert.equal(await start.isDisabled(), true)
        await start.evaluate(element => { element.click(); element.click() })
        gate.resolve()
      } else if (behavior === 'automatic-cancel') {
        await page.getByRole('button', { name: '取消自动授权', exact: true }).click()
        gate.resolve()
      } else if (behavior === 'automatic-switch') {
        await page.evaluate(() => window.fixture.switchAccount())
        gate.resolve()
      }
      const succeeded = ['automatic-success', 'automatic-duplicate'].includes(behavior)
      if (succeeded) {
        await page.waitForFunction(() => window.fixture.events.length >= 2)
        assert.deepEqual((await page.evaluate(() => window.fixture.events)).sort(),
          [`reauthorized:${account.id}`, 'close'].sort())
      } else {
        await page.waitForFunction(() => window.fixture.pending === 0)
        await page.getByRole('button', { name: '自动重新授权并覆盖', exact: true }).waitFor()
        assert.deepEqual(await page.evaluate(() => window.fixture.events), [])
        assert.equal(requests.filter(item => item.path.endsWith('/exchange-code')).length, 0)
        assert.equal(requests.filter(item => item.method === 'POST' && item.path.includes('/accounts/')).length, 0)
        if (behavior === 'automatic-failure' || behavior === 'automatic-running') {
          assert.equal(await page.getByRole('alert').count() > 0, true)
        }
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false)
        await page.screenshot({ path: resolve(output, `${mode}-${width}-${behavior}.png`), fullPage: true })
      }
      assert.equal(launchCount, 1)
      const generated = requests.find(item => item.path.endsWith('/generate-auth-url'))
      assert.equal(generated.body.account_id, account.id)
      assert.equal(generated.body.proxy_id, proxy.id)
      assert.deepEqual(errors, [])
      results.push({ mode, width, behavior, status: 'passed' })
      console.log(`PASS ${mode} ${width} ${behavior}`)
      return
    }
    if (mode === 'create') {
      await page.getByRole('button', { name: 'OpenAI', exact: true }).click()
      await page.locator('form#create-account-form input[type=text]').first().fill('Browser fixture create')
      await page.getByRole('button', { name: 'No Proxy', exact: true }).click()
      await page.getByText(proxy.name, { exact: true }).click()
      await page.getByRole('button', { name: 'Next', exact: true }).click()
    }
    await page.getByRole('button', { name: 'Generate Auth URL', exact: true }).click()
    const launch = page.getByTitle('Launch the activation browser with the bucket proxy bound to this authorization')
    await launch.waitFor()
    assert.equal(await page.getByTitle('Copy URL', { exact: true }).count(), 0)
    assert.equal(await page.locator('input[readonly][value^="https://auth.example.invalid"]').count(), 0)
    await page.locator('textarea').last().fill('http://localhost/callback?code=fixture-code&state=fixture-state-1')
    assert.equal(await submitted.isDisabled(), true, 'A callback alone is not evidence of a pinned browser launch')
    await launch.click()
    await page.waitForFunction(() => !document.querySelector('[aria-busy=true]'))
    assert.equal(launchCount, 1)
    assert.equal(await page.locator('textarea').last().inputValue(), '', 'Launching must discard a pre-launch callback')
    const generated = requests.find(item => item.path.endsWith('/generate-auth-url'))
    assert.equal(generated.body.proxy_id, proxy.id)
    if (mode !== 'create') {
      assert.equal(generated.body.account_id, account.id)
      assert.equal(generated.body.expected_updated_at, account.updated_at)
    }
    const launched = requests.find(item => item.path.endsWith('/launch-auth-browser'))
    assert.equal(launched.body.session_id, 'fixture-session-1')
    if (['launcher-failure', 'launcher-running', 'launcher-not-launched'].includes(behavior)) {
      await page.locator('textarea').last().fill('http://localhost/callback?code=fixture-code&state=fixture-state-1')
      assert.equal(await submitted.isDisabled(), true, 'An unsuccessful or in-progress launch must stay locked')
      assert.equal(sessionCount, 1, 'Unavailable launcher must not silently change the session or route')
      assert.equal(requests.filter(item => item.path.endsWith('/exchange-code')).length, 0)
      assert.equal((await page.evaluate(() => window.fixture.events)).length, 0)
      if (behavior !== 'launcher-running') {
        assert.equal((await page.evaluate(() => window.fixture.toasts())).some(toast => toast.type === 'error'), true)
      }
    } else {
      if (behavior === 'regenerate') {
        await page.getByRole('button', { name: 'Regenerate', exact: true }).click()
        await page.waitForFunction(() => window.fixture.pending === 0)
        await launch.waitFor()
        assert.equal(sessionCount, 2)
        await page.locator('textarea').last().fill('http://localhost/callback?code=fixture-code&state=fixture-state-2')
        assert.equal(await submitted.isDisabled(), true, 'A replacement session needs its own successful launch')
        await launch.click()
        await page.waitForFunction(() => !document.querySelector('[aria-busy=true]'))
        assert.equal(launchCount, 2)
      }
      await page.locator('textarea').last().fill(`http://localhost/callback?code=fixture-code&state=fixture-state-${sessionCount}`)
      assert.equal(await submitted.isDisabled(), false)
      await page.screenshot({ path: resolve(output, `${mode}-${width}-${behavior}.png`), fullPage: true })
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)
      assert.equal(overflow, false, 'OAuth dialog must not overflow the viewport')
      await submitted.click()
      if (behavior === 'switch-during-exchange') {
        await exchangeStarted.promise
        await page.evaluate(() => window.fixture.switchAccount())
        gate.resolve()
        await page.getByRole('button', { name: 'Generate Auth URL', exact: true }).waitFor()
        await page.waitForFunction(() => window.fixture.pending === 0)
        await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))))
        assert.deepEqual(await page.evaluate(() => window.fixture.events), [])
      } else {
        if (behavior === 'duplicate-exchange' || behavior === 'duplicate-write') {
          await (behavior === 'duplicate-exchange' ? exchangeStarted.promise : persistenceStarted.promise)
          const primary = page.locator('button.btn-primary').last()
          assert.equal(await primary.isDisabled(), true)
          // Native click dispatch on a disabled DOM button must not create a second request.
          await primary.evaluate(element => { element.click(); element.click() })
          gate.resolve()
        }
        if (behavior === 'write-failure') {
          await page.locator('form#create-account-form').waitFor()
          assert.deepEqual(await page.evaluate(() => window.fixture.events), [])
          assert.equal((await page.evaluate(() => window.fixture.toasts())).some(toast => toast.type === 'error'), true)
          assert.equal(await submitted.count(), 0)
        } else {
          await page.waitForFunction(() => window.fixture.events.length >= 2)
          assert.deepEqual((await page.evaluate(() => window.fixture.events)).sort(),
            [mode === 'create' ? 'created' : `reauthorized:${account.id}`, 'close'].sort())
        }
      }
      const exchanges = requests.filter(item => item.path.endsWith('/exchange-code'))
      assert.equal(exchanges.length, 1)
      assert.deepEqual(exchanges[0].body, {
        session_id: `fixture-session-${sessionCount}`, code: 'fixture-code',
        state: `fixture-state-${sessionCount}`, proxy_id: proxy.id
      })
      const writes = requests.filter(item => item.method === 'POST' &&
        (item.path.endsWith('/apply-oauth-credentials') || item.path === '/api/v1/admin/accounts'))
      assert.equal(writes.length, behavior === 'switch-during-exchange' ? 0 : 1)
      if (writes.length) {
        if (mode === 'create') {
          assert.equal(writes[0].body.initial_authorization_proof, 'fixture-initial-proof')
          assert.equal(writes[0].body.proxy_id, proxy.id)
        } else {
          assert.equal(writes[0].path, `/api/v1/admin/accounts/${account.id}/apply-oauth-credentials`)
          assert.equal(writes[0].body.expected_updated_at, account.updated_at)
          assert.equal(writes[0].body.reauthorization_proof, 'fixture-reauth-proof')
          assert.equal('proxy_id' in writes[0].body, false)
        }
      }
    }
    assert.deepEqual(errors, [])
    results.push({ mode, width, behavior, status: 'passed' })
    console.log(`PASS ${mode} ${width} ${behavior}`)
  } catch (error) {
    console.error(`FAIL ${mode} ${width} ${behavior}: ${error.message}`)
    console.error('Browser errors:', JSON.stringify(errors))
    console.error('Visible controls:', JSON.stringify(await page.locator('button').allTextContents()))
    throw error
  } finally {
    gate.resolve()
    await context.close()
  }
}

try {
  await mkdir(output, { recursive: true })
  await server.listen()
  browser = await chromium.launch({
    headless: true,
    ...(process.env.BROWSER_CHANNEL ? { channel: process.env.BROWSER_CHANNEL } : {})
  })
  for (const mode of ['create', 'account', 'admin']) {
    for (const width of [1440, 390]) await scenario(mode, width)
    for (const behavior of [
      'duplicate-exchange', 'duplicate-write', 'launcher-failure',
      'launcher-running', 'launcher-not-launched', 'regenerate'
    ]) {
      await scenario(mode, 1440, behavior)
    }
    if (mode !== 'create') await scenario(mode, 1440, 'switch-during-exchange')
    if (mode !== 'create') {
      for (const width of [1440, 390]) await scenario(mode, width, 'automatic-success')
      for (const behavior of ['automatic-duplicate', 'automatic-cancel', 'automatic-switch', 'automatic-failure', 'automatic-running']) {
        await scenario(mode, behavior === 'automatic-failure' ? 390 : 1440, behavior)
      }
    }
  }
  await scenario('create', 1440, 'write-failure')
  await writeFile(resolve(output, 'results.json'), JSON.stringify({
    scope: 'Real Chromium, Vue components and composables; mocked API only; no production or provider traffic',
    results
  }, null, 2) + '\n')
} finally {
  await browser?.close()
  await server.close()
}
