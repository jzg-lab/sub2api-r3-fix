import assert from 'node:assert/strict'
import { mkdir, writeFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { dirname, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { createServer } from 'vite'
import vue from '@vitejs/plugin-vue'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
process.chdir(root)
const require = createRequire(import.meta.url)
const imported = await import(pathToFileURL(process.env.PLAYWRIGHT_MODULE || require.resolve('playwright')).href)
const { chromium } = imported.default || imported
const output = resolve(root, '../output/playwright/environment-audit')
const entry = `
import { createApp, h, ref } from 'vue'
import { createPinia } from 'pinia'
import i18n, { initI18n } from '/src/i18n/index.ts'
import Modal from '/src/components/admin/AccountEnvironmentAuditModal.vue'
import '/src/style.css'
i18n.global.locale.value = new URLSearchParams(location.search).get('locale') || 'zh'
await initI18n()
createApp({
  setup() {
    const show = ref(false)
    return () => h('main', { class: 'p-4' }, [
      h('button', { id: 'open-audit', class: 'btn btn-secondary', onClick: () => { show.value = true } },
        i18n.global.t('admin.accounts.environmentAudit.title')),
      h(Modal, { show: show.value, onClose: () => { show.value = false } })
    ])
  }
}).use(createPinia()).use(i18n).mount('#app')
`
const data = {
  accounts: [13, 14, 15].map((id, index) => ({
    account_id: id,
    configured_mode: index === 1 ? 'off' : 'full',
    effective_mode: index === 1 ? 'off' : 'device',
    seed_present: index !== 1,
    identity_source: index === 1 ? 'unknown' : 'seed_derived',
    identity_generated_at: null,
    account_created_at: '2026-10-11T09:20:00+08:00',
    login_ip_recorded: index !== 1,
    proxy_id: index === 1 ? null : 13 + index
  })),
  collisions: [{ kind: 'duplicate_seed', account_ids: [13, 15] }],
  import_batches: [{ kind: 'same_import_minute_only', account_ids: [13, 14, 15] }],
  generated_at: '2026-10-11T19:00:00+08:00'
}
const server = await createServer({
  root, configFile: false, envFile: false,
  cacheDir: resolve(output, 'vite-cache'),
  resolve: { alias: { '@': resolve(root, 'src'), 'vue-i18n': 'vue-i18n/dist/vue-i18n.runtime.esm-bundler.js' } },
  define: { __INTLIFY_JIT_COMPILATION__: true },
  optimizeDeps: { esbuildOptions: { define: { __INTLIFY_JIT_COMPILATION__: 'true' } } },
  plugins: [vue(), {
    name: 'isolated-environment-audit-fixture',
    resolveId(id) { if (id === '/environment-audit-fixture.js') return '\0environment-audit-fixture' },
    load(id) { if (id === '\0environment-audit-fixture') return entry },
    configureServer(instance) {
      instance.middlewares.use((request, response, next) => {
        const pathname = new URL(request.url, 'http://fixture.invalid').pathname
        if (pathname === '/') {
          response.setHeader('Content-Type', 'text/html')
          response.end('<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body><div id="app"></div><script type="module" src="/environment-audit-fixture.js"></script></body></html>')
        } else if (pathname.startsWith('/api/') || pathname.startsWith('/v1/')) {
          response.statusCode = 503
          response.end('Unmocked fixture request')
        } else next()
      })
    }
  }],
  server: { host: '127.0.0.1', port: 0 },
  logLevel: 'error'
})
let browser
const results = []
try {
  await mkdir(output, { recursive: true })
  await server.listen()
  const origin = `http://127.0.0.1:${server.httpServer.address().port}`
  browser = await chromium.launch({ headless: true, ...(process.env.BROWSER_CHANNEL ? { channel: process.env.BROWSER_CHANNEL } : {}) })
  for (const [width, locale] of [[1280, 'zh'], [390, 'zh'], [320, 'en']]) {
    const context = await browser.newContext({ viewport: { width, height: 850 } })
    const page = await context.newPage()
    const errors = []
    const requests = []
    let failNext = false
    page.on('pageerror', error => errors.push(error.message))
    await page.route('**/*', async route => {
      const url = new URL(route.request().url())
      if (url.origin !== origin) return route.abort()
      if (url.pathname === '/api/v1/admin/accounts/environment-audit') {
        const method = route.request().method()
        requests.push(method)
        assert.equal(method, 'GET')
        if (failNext) {
          failNext = false
          return route.fulfill({ status: 503, json: { code: 503, message: 'fixture unavailable' } })
        }
        return route.fulfill({ json: { code: 0, data } })
      }
      if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/v1/')) {
        throw new Error(`Unexpected fixture API path: ${url.pathname}`)
      }
      return route.continue()
    })
    await page.goto(`${origin}/?locale=${locale}`)
    await page.locator('#open-audit').click()
    const dialog = page.getByRole('dialog')
    await dialog.locator('[data-account-id="13"]').waitFor()
    assert.equal(await dialog.locator('[data-account-id]').count(), 3)
    assert.equal(await dialog.locator('[data-testid="collisions"]').isVisible(), true)
    assert.equal(await dialog.locator('[data-testid="import-batches"]').isVisible(), true)
    assert.equal(await dialog.locator('[data-account-id="14"] [data-field="identity-generated"]').innerText(), locale === 'zh' ? '未知' : 'Unknown')
    assert.equal(await dialog.locator('input, select, form').count(), 0)
    const rect = await dialog.locator('.modal-content').boundingBox()
    assert.ok(rect && rect.x >= 0 && rect.x + rect.width <= width + 1)
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    const clipped = await dialog.locator('th, td, button').evaluateAll(elements =>
      elements.filter(element => element.scrollWidth > element.clientWidth + 1).map(element => element.tagName))
    assert.deepEqual(clipped, [])
    await page.screenshot({ path: resolve(output, `audit-${width}-${locale}.png`), animations: 'disabled' })
    failNext = true
    await dialog.locator('button[title]').click()
    await dialog.getByRole('alert').waitFor()
    assert.equal(await dialog.locator('[data-account-id]').count(), 0)
    assert.equal(await dialog.locator('[data-testid="empty"]').count(), 0)
    await dialog.locator('button[title]').click()
    await dialog.locator('[data-account-id="13"]').waitFor()
    assert.equal(await dialog.getByRole('alert').count(), 0)
    await page.keyboard.press('Escape')
    await dialog.waitFor({ state: 'hidden' })
    assert.equal(await page.locator('#open-audit').evaluate(element => element === document.activeElement), true)
    assert.deepEqual(requests, ['GET', 'GET', 'GET'])
    assert.deepEqual(errors, [])
    results.push({ width, locale, passed: true, readOnlyRequests: requests.length })
    await context.close()
  }
  const result = { productionAccess: false, results }
  await writeFile(resolve(output, 'result.json'), JSON.stringify(result, null, 2))
  console.log(JSON.stringify(result))
} finally {
  await browser?.close()
  await server.close()
}
