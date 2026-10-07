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
const modulePath = process.env.PLAYWRIGHT_MODULE || require.resolve('playwright')
const imported = await import(pathToFileURL(modulePath).href)
const { chromium } = imported.default || imported
const output = resolve(root, '../output/playwright/concurrency')
const entry = `
import { createApp, h, ref } from 'vue'
import { createPinia } from 'pinia'
import i18n, { initI18n } from '/src/i18n/index.ts'
import Cell from '/src/components/account/AccountCapacityCell.vue'
import Modal from '/src/components/account/AccountConcurrencyModal.vue'
import '/src/style.css'
await initI18n()
i18n.global.locale.value = 'en'
createApp({
  setup() {
    const account = ref({ id: 71, name: 'Concurrency fixture', platform: 'openai',
      type: 'oauth', concurrency: 50, current_concurrency: 2 })
    const editing = ref(null)
    return () => h('main', { class: 'p-4' }, [
      h('table', { class: 'w-full text-left' }, [
        h('thead', [h('tr', [h('th', 'Account'), h('th', 'Capacity')])]),
        h('tbody', [h('tr', [
          h('td', account.value.name),
          h('td', [h(Cell, { account: account.value,
            onEditConcurrency: () => { editing.value = account.value } })])
        ])])
      ]),
      h(Modal, { account: editing.value, onClose: () => { editing.value = null },
        onUpdated: updated => { account.value = { ...account.value, ...updated } } })
    ])
  }
}).use(createPinia()).use(i18n).mount('#app')
`
const server = await createServer({
  root, configFile: false, envFile: false,
  cacheDir: resolve(output, 'vite-cache'),
  resolve: { alias: {
    '@': resolve(root, 'src'),
    'vue-i18n': 'vue-i18n/dist/vue-i18n.runtime.esm-bundler.js'
  } },
  define: { __INTLIFY_JIT_COMPILATION__: true },
  optimizeDeps: { esbuildOptions: { define: { __INTLIFY_JIT_COMPILATION__: 'true' } } },
  plugins: [vue(), {
    name: 'isolated-concurrency-fixture',
    resolveId(id) {
      if (id === '/concurrency-fixture.js') return '\0concurrency-fixture'
    },
    load(id) {
      if (id === '\0concurrency-fixture') return entry
    },
    configureServer(instance) {
      instance.middlewares.use((request, response, next) => {
        const pathname = new URL(request.url, 'http://fixture.invalid').pathname
        if (pathname === '/') {
          response.setHeader('Content-Type', 'text/html')
          response.end('<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body><div id="app"></div><script type="module" src="/concurrency-fixture.js"></script></body></html>')
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
  const port = server.httpServer.address().port
  browser = await chromium.launch({
    headless: true,
    ...(process.env.BROWSER_CHANNEL ? { channel: process.env.BROWSER_CHANNEL } : {})
  })
  for (const width of [1280, 375]) {
    const context = await browser.newContext({ viewport: { width, height: 800 }, locale: 'en-US' })
    const page = await context.newPage()
    const errors = []
    const updates = []
    let failNext = false
    page.on('pageerror', error => {
      errors.push(error.message)
      console.error(`Fixture page error: ${error.message}`)
    })
    page.on('console', message => {
      if (message.type() === 'error') console.error(`Fixture console error: ${message.text()}`)
    })
    await page.route('**/api/v1/admin/accounts/71', async route => {
      assert.equal(route.request().method(), 'PUT')
      const body = route.request().postDataJSON()
      assert.deepEqual(Object.keys(body), ['concurrency'])
      updates.push(body)
      if (failNext) {
        failNext = false
        await route.fulfill({ status: 409, json: { code: 409, message: 'Fixture save conflict' } })
      } else {
        await route.fulfill({ json: { code: 0, data: { id: 71, concurrency: body.concurrency } } })
      }
    })
    await page.goto(`http://127.0.0.1:${port}`)
    const button = page.getByRole('button', { name: 'Edit concurrency limit' })
    await button.click({ timeout: 10000 }).catch(async error => {
      await page.screenshot({ path: resolve(output, `failure-${width}.png`) })
      console.error(await page.locator('body').innerText())
      console.error(await page.getByRole('button').evaluateAll(buttons =>
        buttons.map(button => ({ label: button.getAttribute('aria-label'), title: button.title }))))
      throw error
    })
    const dialog = page.getByRole('dialog')
    const input = dialog.locator('#account-concurrency-limit')
    assert.equal(await input.inputValue(), '50')
    const save = dialog.getByRole('button', { name: 'Save', exact: true })
    for (const invalid of ['', '0', '-1', '1.5', '10001']) {
      await input.fill(invalid)
      assert.equal(await save.isDisabled(), true)
    }
    assert.equal(updates.length, 0)
    await input.fill('3')
    await page.screenshot({ path: resolve(output, `edit-${width}.png`), animations: 'disabled' })
    const rect = await dialog.locator('.modal-content').boundingBox()
    assert.ok(rect && rect.x >= 0 && rect.x + rect.width <= width + 1)
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await save.click()
    await dialog.waitFor({ state: 'hidden' })
    assert.deepEqual(updates, [{ concurrency: 3 }])
    await button.click()
    assert.equal(await input.inputValue(), '3')
    await input.fill('4')
    failNext = true
    await save.click()
    await dialog.getByRole('alert').waitFor()
    assert.equal(await input.inputValue(), '4')
    assert.equal(await dialog.isVisible(), true)
    await save.click()
    await dialog.waitFor({ state: 'hidden' })
    assert.deepEqual(updates, [{ concurrency: 3 }, { concurrency: 4 }, { concurrency: 4 }])
    await button.click()
    assert.equal(await input.inputValue(), '4')
    await page.keyboard.press('Escape')
    await dialog.waitFor({ state: 'hidden' })
    assert.deepEqual(errors, [])
    results.push({ width, passed: true, updates: updates.length })
    await context.close()
  }
  await writeFile(resolve(output, 'result.json'), JSON.stringify({ productionAccess: false, results }, null, 2))
  console.log(JSON.stringify({ productionAccess: false, results }))
} finally {
  await browser?.close()
  await server.close()
}
