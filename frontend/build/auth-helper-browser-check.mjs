import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { pathToFileURL } from 'node:url'
import { after, before, test } from 'node:test'
import { advanceLogin, loginStep, totp } from '../../deploy/auth-browser/automate.mjs'

const require = createRequire(import.meta.url)
const modulePath = process.env.PLAYWRIGHT_MODULE || require.resolve('playwright')
const playwright = await import(pathToFileURL(modulePath).href)
const { chromium } = playwright.default || playwright
const origin = 'https://auth.openai.com'
const login = {
  email: 'browser-fixture@example.invalid',
  password: ['synthetic', 'browser', 'fixture'].join('-'),
  totp_secret: 'A'.repeat(32),
}
let browser

before(async () => {
  browser = await chromium.launch({
    headless: true,
    ...(process.env.BROWSER_CHANNEL ? { channel: process.env.BROWSER_CHANNEL } : {}),
  })
})
after(async () => { await browser?.close() })

const form = (field, button = '<button type="submit">Continue</button>', action = '/next') =>
  `<form action="${action}" method="post">${field}${button}</form>`
const emailField = '<input name="username" type="email" autocomplete="username">'
const passwordField = '<input name="password" type="password" autocomplete="current-password">'
const otpField = '<p>Authentication app</p><input name="code" autocomplete="one-time-code">'
const document = body => `<!doctype html><html><body>${body}</body></html>`

async function isolatedPage(html, pathname = '/log-in', pageOrigin = origin) {
  const context = await browser.newContext({ serviceWorkers: 'block' })
  const page = await context.newPage()
  page.setDefaultTimeout(5000)
  const submissions = []
  await context.route('**/*', async route => {
    const request = route.request()
    if (request.method() !== 'GET') submissions.push(request.url())
    // Every request is fulfilled locally, including negative foreign origins.
    await route.fulfill({ contentType: 'text/html', body: document(html) })
  })
  await page.goto(pageOrigin + pathname)
  const cdp = await context.newCDPSession(page)
  const pipe = {
    async call(method, params) {
      try { return await cdp.send(method, params) }
      catch (error) {
        if (/execution context.*destroyed|cannot find context/i.test(error.message)) {
          throw new Error('AUTH_BROWSER_NAVIGATION')
        }
        throw error
      }
    },
  }
  return { context, page, pipe, submissions }
}

test('real DOM submits email, password, TOTP and consent once without external traffic', async () => {
  const fixture = await isolatedPage('')
  const { context, page, pipe } = fixture
  const stages = []
  await context.unrouteAll()
  await context.route('**/*', async route => {
    const request = route.request()
    const url = new URL(request.url())
    assert.equal(url.origin, origin)
    const values = new URLSearchParams(request.postData() || '')
    const stage = url.pathname
    if (request.method() === 'POST') {
      stages.push(stage)
      if (stage === '/password') assert.ok(values.get('username') === login.email)
      if (stage === '/totp') assert.ok(values.get('password') === login.password)
      if (stage === '/consent') {
        assert.ok([-30000, 0, 30000].some(offset =>
          values.get('code') === totp(login.totp_secret, Date.now() + offset)))
      }
    }
    const body = {
      '/email': form(emailField, undefined, '/password'),
      '/password': form(passwordField, undefined, '/totp'),
      '/totp': form(otpField, undefined, '/consent'),
      '/consent': form('', '<button type="submit">Allow</button>', '/done'),
      '/done': '<p>Fixture completed</p>',
    }[stage]
    assert.ok(body, 'unexpected request in isolated flow')
    await route.fulfill({ contentType: 'text/html', body: document(body) })
  })
  try {
    await page.goto(origin + '/email')
    const submitted = new Set()
    for (const next of ['/password', '/totp', '/consent', '/done']) {
      await advanceLogin(pipe, 'fixture', login, submitted)
      await page.waitForURL(origin + next)
      await page.waitForLoadState('domcontentloaded')
    }
    assert.deepEqual(stages, ['/password', '/totp', '/consent', '/done'])
    assert.equal(submitted.size, 4)
  } finally {
    await context.close()
  }
})

for (const tc of [
  { name: 'foreign origin', html: form(passwordField), origin: 'https://foreign.invalid' },
  { name: 'foreign form action', html: form(passwordField, undefined, 'https://foreign.invalid/submit') },
  { name: 'foreign button action', html: form(passwordField, '<button type="submit" formaction="https://foreign.invalid/submit">Continue</button>') },
  { name: 'hidden input', html: form('<input type="email" style="display:none">') },
  { name: 'disabled input', html: form('<input type="email" disabled>') },
  { name: 'challenge', html: '<p>Verify you are human</p>' + form(passwordField) },
  { name: 'email OTP is not TOTP', html: form('<p>Check your email</p><input name="code" autocomplete="one-time-code">') },
  { name: 'signup', pathname: '/create-account/password', html: form(passwordField) },
  { name: 'new password', html: form('<input type="password" name="password" autocomplete="new-password">') },
  { name: 'reset password', pathname: '/reset-password', html: form(passwordField) },
  { name: 'unrecognized consent page', html: '<button>Allow</button>' },
  { name: 'foreign consent action', pathname: '/consent', html: form('', '<button type="submit">Allow</button>', 'https://foreign.invalid/submit') },
]) {
  test(`real DOM does not submit on ${tc.name}`, async () => {
    const fixture = await isolatedPage(tc.html, tc.pathname, tc.origin)
    try {
      const result = await fixture.page.evaluate(`(${loginStep.toString()})(null, [], null)`)
      assert.equal(result.kind, 'manual')
      assert.equal(fixture.submissions.length, 0)
    } finally {
      await fixture.context.close()
    }
  })
}

test('real DOM recognizes an implicit submit button and ignores hidden duplicate fields', async () => {
  const fixture = await isolatedPage(form(
    '<input name="username" type="email" style="display:none">' + emailField,
    '<button>Continue</button>',
  ))
  try {
    const result = await fixture.page.evaluate(`(${loginStep.toString()})(null, [], null)`)
    assert.equal(result.kind, 'email')
  } finally {
    await fixture.context.close()
  }
})

test('real DOM refuses a stale target and replay without typing anything', async () => {
  const fixture = await isolatedPage(form(emailField))
  try {
    const stale = await fixture.page.evaluate(`(${loginStep.toString()})(${JSON.stringify(login)}, [], "stale")`)
    assert.equal(stale.kind, 'manual')
    const replay = await fixture.page.evaluate(`(${loginStep.toString()})(null, ["${origin}/log-in:email"], null)`)
    assert.equal(replay.kind, 'waiting')
    assert.equal(await fixture.page.locator('input').inputValue(), '')
    assert.equal(fixture.submissions.length, 0)
  } finally {
    await fixture.context.close()
  }
})
