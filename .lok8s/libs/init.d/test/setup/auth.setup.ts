import { test as setup } from '@playwright/test'
import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'
import { config, caps } from '../utils/config'
import { TOKEN_FILE } from '../utils/oidc'

/**
 * Authentication setup — produces the storageState consumed by the authed
 * projects (so `*.auth.spec.ts` start logged in) AND, when your IdP issues a
 * browser-readable token, captures it to TOKEN_FILE so headless API specs can
 * send a real bearer.
 *
 * Entirely gated on `caps.canAuth`: when login isn't usable yet this writes an
 * EMPTY storageState (and no token file) and the dependent authed specs skip
 * themselves. That keeps `--list` and the unauth/public suite green without a
 * live login. The empty-state files are written unconditionally so projects
 * that reference `storageState` never fail to start.
 *
 * ── Make this yours ──────────────────────────────────────────────────────────
 * Replace `loginViaUi()` below with your real login (ideally a page object, e.g.
 * the worked-example specs' POM). The `captureAccessToken()` helper assumes an
 * oidc-client-ts-style sessionStorage entry (`oidc.user:*`); adjust the key/shape
 * for your auth library, or delete it if you only need cookie-based sessions.
 */

const USER_STATE = 'test-results/.auth/user.json'
const ADMIN_STATE = 'test-results/.auth/admin.json'
const EMPTY_STATE = { cookies: [], origins: [] }

/** Fill an input and verify the value stuck (controlled-component race guard). */
async function fillStable(input: ReturnType<import('@playwright/test').Page['locator']>, value: string): Promise<void> {
  await input.waitFor({ state: 'visible', timeout: 15_000 })
  for (let i = 0; i < 4; i++) {
    await input.fill('')
    await input.fill(value)
    if (await input.inputValue().catch(() => '') === value) return
    await input.page().waitForTimeout(250)
  }
  await input.fill('')
  await input.pressSequentially(value, { delay: 20 })
}

/**
 * Minimal generic UI login: open the app, follow the redirect to the IdP portal,
 * submit username/password, and wait to land back on the app. This is a starting
 * point — swap in your real flow / page object.
 */
async function loginViaUi(page: import('@playwright/test').Page): Promise<void> {
  await page.goto(`${config.urls.app}/login`, { waitUntil: 'domcontentloaded' })
  const username = page.getByLabel(/username|email/i).or(page.locator('input[name="username"], input[type="email"]')).first()
  await username.waitFor({ state: 'visible', timeout: 30_000 })
  const password = page.getByLabel(/password/i).or(page.locator('input[type="password"]')).first()
  await fillStable(username, config.testUser.username)
  await fillStable(password, config.testUser.password)
  await page.getByRole('button', { name: /sign in|log in|login|continue/i })
    .or(page.locator('button[type="submit"]')).first().click()
  // Wait until we are back on the app host (past the IdP/callback hop).
  const appHost = new URL(config.urls.app).host
  await page.waitForURL(new RegExp(appHost.replace(/\./g, '\\.')), { timeout: 30_000 }).catch(() => {})
}

/** Pull an oidc-client-ts access token out of sessionStorage, if present. */
async function captureAccessToken(page: import('@playwright/test').Page): Promise<void> {
  const token = await page.evaluate(() => {
    for (let i = 0; i < sessionStorage.length; i++) {
      const key = sessionStorage.key(i)
      if (!key || !key.startsWith('oidc.user')) continue
      try {
        const parsed = JSON.parse(sessionStorage.getItem(key) || '{}')
        if (parsed && typeof parsed.access_token === 'string') return parsed.access_token as string
      }
      catch { /* not the entry we want */ }
    }
    return ''
  }).catch(() => '')

  if (token) {
    mkdirSync(dirname(TOKEN_FILE), { recursive: true })
    writeFileSync(TOKEN_FILE, token, 'utf8')
  }
}

setup('authenticate test user', async ({ page }) => {
  setup.setTimeout(120_000)

  if (!caps.canAuth) {
    mkdirSync('test-results/.auth', { recursive: true })
    writeFileSync(USER_STATE, JSON.stringify(EMPTY_STATE))
    writeFileSync(ADMIN_STATE, JSON.stringify(EMPTY_STATE))
    setup.skip(true, 'canAuth=false — login not exercisable yet (set LOK8S_TEST_CAN_AUTH=true)')
    return
  }

  await loginViaUi(page)
  await captureAccessToken(page)
  await page.context().storageState({ path: USER_STATE })
})

setup('authenticate admin user', async ({ page }) => {
  setup.setTimeout(120_000)
  if (!caps.canAuth) {
    setup.skip(true, 'canAuth=false — admin login not exercisable yet')
    return
  }
  // If admin and test user are the same account, this just re-captures state.
  await loginViaUi(page)
  await page.context().storageState({ path: ADMIN_STATE })
})
